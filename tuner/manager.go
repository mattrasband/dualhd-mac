// SPDX-License-Identifier: GPL-2.0-or-later
// Copyright (C) 2026 Matt Rasband

// Package tuner shares the board's two frontends between viewers, and keeps
// the USB device connected: it opens the board when it appears, detects when
// it goes away (unplug, sleep/wake resets) and reopens it when it returns.
// Viewers of channels on the same RF transport share one tuner.
package tuner

import (
	"context"
	"errors"
	"fmt"
	"log"
	"sync"
	"time"

	"github.com/mattrasband/dualhd-mac/dualhd"
	"github.com/mattrasband/dualhd-mac/lgdt3306a"
)

var (
	ErrAllInUse = errors.New("all tuners in use")
	ErrNoLock   = errors.New("no signal lock")
	ErrNoDevice = errors.New("tuner device not connected")
)

const (
	lockTimeout   = 3 * time.Second
	statusEvery   = 5 * time.Second
	checkEvery    = 2 * time.Second // device health check / reopen interval
	detachTimeout = 5 * time.Second
	stallTimeout  = 5 * time.Second // no TS data while streaming
	subscriberBuf = 64              // chunks of ~48KB, ~1.2s of a full transport
)

type Manager struct {
	open func() (*dualhd.Board, error)
	// OnConnect, if set, is called (from Run's goroutine) each time the board
	// is opened, before it is used for streaming.
	OnConnect func(*dualhd.Board)

	mu     sync.Mutex
	board  *dualhd.Board // nil while disconnected
	slots  []*slot
	tuners int

	check  chan struct{} // nudges Run to health-check the device now
	failed error         // set when the device must be reopened
}

// NewManager returns a manager that opens boards with open; call Run to
// connect and maintain the device.
func NewManager(open func() (*dualhd.Board, error)) *Manager {
	return &Manager{open: open, tuners: 2, check: make(chan struct{}, 1)}
}

// Run maintains the device connection until ctx is cancelled.
func (m *Manager) Run(ctx context.Context) {
	var lastErr string
	t := time.NewTicker(checkEvery)
	defer t.Stop()
	for {
		m.mu.Lock()
		b := m.board
		m.mu.Unlock()

		if b == nil {
			b, err := m.open()
			if err != nil {
				if err.Error() != lastErr {
					log.Printf("tuner device unavailable: %v (retrying every %v)", err, checkEvery)
					lastErr = err.Error()
				}
			} else {
				lastErr = ""
				m.attach(b)
			}
		} else {
			m.mu.Lock()
			err := m.failed
			m.failed = nil
			m.mu.Unlock()
			if err == nil {
				err = b.Ping()
			}
			if err != nil {
				m.detach(err)
				continue // try to reopen straight away
			}
		}

		select {
		case <-ctx.Done():
			m.detach(ctx.Err())
			return
		case <-t.C:
		case <-m.check:
		}
	}
}

func (m *Manager) attach(b *dualhd.Board) {
	if m.OnConnect != nil {
		m.OnConnect(b)
	}
	m.mu.Lock()
	m.board = b
	m.slots = nil
	for _, fe := range b.Tuner {
		m.slots = append(m.slots, &slot{fe: fe, subs: map[*Sub]struct{}{}})
	}
	m.tuners = len(m.slots)
	m.mu.Unlock()
	log.Printf("tuner device connected (serial %s, %d tuners)", b.Serial(), len(b.Tuner))
}

// detach stops all streams and closes the board.
func (m *Manager) detach(cause error) {
	m.mu.Lock()
	b := m.board
	if b == nil {
		m.mu.Unlock()
		return
	}
	m.board = nil
	var waits []chan struct{}
	for _, s := range m.slots {
		if s.cancel != nil {
			s.cancel()
			waits = append(waits, s.done)
		}
	}
	m.slots = nil
	m.mu.Unlock()

	if !errors.Is(cause, context.Canceled) {
		log.Printf("tuner device lost: %v", cause)
	}
	timeout := time.After(detachTimeout)
	for _, w := range waits {
		select {
		case <-w:
		case <-timeout:
			// Closing the device under transfers libusb never completed
			// could block or crash; abandon the handle instead. A replugged
			// device is a new USB device, so reopening still works.
			log.Printf("streams did not stop; abandoning old device handle")
			return
		}
	}
	b.Close()
}

// fail marks the device as needing a full reopen, even if it still answers
// pings (e.g. it was reset and lost its configuration).
func (m *Manager) fail(err error) {
	m.mu.Lock()
	if m.failed == nil {
		m.failed = err
	}
	m.mu.Unlock()
	m.nudge()
}

// nudge asks Run to check the device soon (e.g. after a stream error).
func (m *Manager) nudge() {
	select {
	case m.check <- struct{}{}:
	default:
	}
}

type slot struct {
	fe       *dualhd.Frontend
	rf       int
	subs     map[*Sub]struct{}
	cancel   context.CancelFunc // non-nil while tuning or streaming
	stopping bool               // last viewer left; stream shutting down
	done     chan struct{}
	status   lgdt3306a.Status
	since    time.Time
	lastData time.Time
}

// Sub receives raw transport stream chunks for one RF channel. C is closed
// when the stream ends (viewer closed it, or the device went away).
type Sub struct {
	C       <-chan []byte
	c       chan []byte
	m       *Manager
	s       *slot
	Dropped int
}

// TunerInfo describes one tuner's state.
type TunerInfo struct {
	Index   int
	Online  bool
	RF      int // 0 when idle
	Viewers int
	Status  lgdt3306a.Status
	Since   time.Time
}

func (m *Manager) Status() []TunerInfo {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.board == nil {
		out := make([]TunerInfo, m.tuners)
		for i := range out {
			out[i].Index = i
		}
		return out
	}
	var out []TunerInfo
	for _, s := range m.slots {
		ti := TunerInfo{Index: s.fe.Index, Online: true, Viewers: len(s.subs)}
		if s.cancel != nil {
			ti.RF, ti.Status, ti.Since = s.rf, s.status, s.since
		}
		out = append(out, ti)
	}
	return out
}

// Subscribe returns a stream of the RF channel, tuning an idle tuner if no
// tuner is already on it.
func (m *Manager) Subscribe(rf int) (*Sub, error) {
	freq, err := dualhd.ChannelFreq(rf)
	if err != nil {
		return nil, err
	}
	m.mu.Lock()
	if m.board == nil {
		m.mu.Unlock()
		return nil, ErrNoDevice
	}
	var s *slot
	for _, c := range m.slots {
		if c.cancel != nil && !c.stopping && c.rf == rf {
			s = c
			break
		}
	}
	if s != nil {
		sub := m.addSub(s)
		m.mu.Unlock()
		return sub, nil
	}
	for _, c := range m.slots {
		if c.cancel == nil {
			s = c
			break
		}
	}
	if s == nil {
		m.mu.Unlock()
		return nil, ErrAllInUse
	}
	// Reserve the slot while tuning, outside the lock.
	ctx, cancel := context.WithCancel(context.Background())
	s.cancel, s.rf, s.done = cancel, rf, make(chan struct{})
	m.mu.Unlock()

	st, err := tune(s.fe, freq)
	if err == nil && ctx.Err() != nil {
		err = ErrNoDevice // detached while tuning
	}
	if err != nil {
		m.mu.Lock()
		cancel()
		for sub := range s.subs { // viewers who joined while we tuned
			delete(s.subs, sub)
			close(sub.c)
		}
		s.cancel = nil
		close(s.done)
		m.mu.Unlock()
		s.fe.Demod.Sleep()
		if !errors.Is(err, ErrNoLock) {
			m.nudge()
		}
		return nil, err
	}
	m.mu.Lock()
	s.status, s.since, s.lastData = st, time.Now(), time.Now()
	sub := m.addSub(s)
	m.mu.Unlock()

	log.Printf("tuner %d: RF %d locked (%v)", s.fe.Index, rf, st)
	go m.run(ctx, s)
	return sub, nil
}

func tune(fe *dualhd.Frontend, freq uint32) (lgdt3306a.Status, error) {
	if err := fe.TuneATSC(freq); err != nil {
		return lgdt3306a.Status{}, err
	}
	deadline := time.Now().Add(lockTimeout)
	for {
		st, err := fe.Status()
		if err != nil {
			return st, err
		}
		if st.Lock {
			return st, nil
		}
		if time.Now().After(deadline) {
			return st, fmt.Errorf("%w (%v)", ErrNoLock, st)
		}
	}
}

func (m *Manager) addSub(s *slot) *Sub {
	c := make(chan []byte, subscriberBuf)
	sub := &Sub{C: c, c: c, m: m, s: s}
	s.subs[sub] = struct{}{}
	return sub
}

// Close detaches the subscriber; the tuner is released when none remain.
func (sub *Sub) Close() {
	m := sub.m
	m.mu.Lock()
	defer m.mu.Unlock()
	s := sub.s
	if _, ok := s.subs[sub]; !ok {
		return
	}
	delete(s.subs, sub)
	close(sub.c)
	if len(s.subs) == 0 && s.cancel != nil {
		s.stopping = true
		s.cancel()
	}
}

// run streams from the frontend and fans chunks out to subscribers.
func (m *Manager) run(ctx context.Context, s *slot) {
	defer func() {
		m.mu.Lock()
		for sub := range s.subs { // stream ended with viewers attached
			delete(s.subs, sub)
			close(sub.c)
		}
		m.mu.Unlock()
		if ctx.Err() == nil || s.stopping { // device may still be there
			if err := s.fe.Demod.Sleep(); err != nil {
				log.Printf("tuner %d: sleep: %v", s.fe.Index, err)
			}
		}
		log.Printf("tuner %d: released RF %d", s.fe.Index, s.rf)
		m.mu.Lock()
		s.cancel, s.stopping = nil, false
		close(s.done)
		m.mu.Unlock()
	}()

	go m.monitor(ctx, s)
	err := s.fe.Stream(ctx, fanout{m, s})
	if err != nil && ctx.Err() == nil {
		log.Printf("tuner %d: stream: %v", s.fe.Index, err)
		m.nudge()
	}
}

// monitor watches for stalled streams and polls lock status, which also
// drives the demod's adaptive equaliser tweaks (as the Linux DVB frontend
// thread does).
func (m *Manager) monitor(ctx context.Context, s *slot) {
	t := time.NewTicker(time.Second)
	defer t.Stop()
	lastStatus := time.Now()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		m.mu.Lock()
		stalled := time.Since(s.lastData)
		m.mu.Unlock()
		if stalled > stallTimeout {
			log.Printf("tuner %d: no data from RF %d for %v", s.fe.Index, s.rf, stalled.Round(time.Second))
			m.fail(fmt.Errorf("tuner %d stream stalled", s.fe.Index))
			return
		}
		if time.Since(lastStatus) < statusEvery {
			continue
		}
		lastStatus = time.Now()
		st, err := s.fe.Status()
		if err != nil {
			if ctx.Err() == nil {
				log.Printf("tuner %d: status: %v", s.fe.Index, err)
				m.nudge()
			}
			continue
		}
		m.mu.Lock()
		was := s.status.Lock
		s.status = st
		m.mu.Unlock()
		if was != st.Lock {
			log.Printf("tuner %d: RF %d lock changed: %v", s.fe.Index, s.rf, st)
		}
	}
}

type fanout struct {
	m *Manager
	s *slot
}

func (f fanout) Write(p []byte) (int, error) {
	chunk := append([]byte(nil), p...)
	f.m.mu.Lock()
	f.s.lastData = time.Now()
	for sub := range f.s.subs {
		select {
		case sub.c <- chunk:
		default:
			sub.Dropped++
		}
	}
	f.m.mu.Unlock()
	return len(p), nil
}
