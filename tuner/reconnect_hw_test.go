// SPDX-License-Identifier: GPL-2.0-or-later
// Copyright (C) 2026 Matt Rasband

package tuner

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/mattrasband/dualhd-mac/dualhd"
	"github.com/mattrasband/dualhd-mac/em28xx"
)

// TestReconnectHardware needs the real tuner (and the hdhr service stopped):
//
//	HAUPPAUGE_HW=1 go test -v -run Reconnect ./tuner
func TestReconnectHardware(t *testing.T) {
	if os.Getenv("HAUPPAUGE_HW") == "" {
		t.Skip("set HAUPPAUGE_HW=1 to run against the real device")
	}
	// Each case breaks the device in a way it can fail in practice, while
	// the USB handle stays valid (e.g. a reset in place on sleep/wake).
	cases := []struct {
		name string
		regs [][2]byte
	}{
		// Bridge lost all configuration: caught by the health-check ping.
		{"config-lost", [][2]byte{{em28xx.RegTSEnable, 0x00}, {em28xx.RegI2CClk, 0x00}}},
		// TS output silently stops: caught by the stall watchdog.
		{"silent-stall", [][2]byte{{em28xx.RegTSEnable, 0x00}}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) { testReconnect(t, tc.regs) })
	}
}

func testReconnect(t *testing.T, breakRegs [][2]byte) {
	m := NewManager(dualhd.Open)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	connects := make(chan struct{}, 4)
	m.OnConnect = func(*dualhd.Board) { connects <- struct{}{} }
	go m.Run(ctx)

	waitConnect := func() {
		select {
		case <-connects:
		case <-time.After(20 * time.Second):
			t.Fatal("device did not connect")
		}
		for i := 0; i < 50 && m.Status()[0].Online == false; i++ {
			time.Sleep(20 * time.Millisecond)
		}
	}
	stream := func(d time.Duration) (*Sub, int) {
		var sub *Sub
		var err error
		for i := 0; i < 10; i++ { // board may still be attaching
			if sub, err = m.Subscribe(35); err == nil {
				break
			}
			time.Sleep(200 * time.Millisecond)
		}
		if err != nil {
			t.Fatalf("subscribe: %v", err)
		}
		n := 0
		deadline := time.After(d)
		for {
			select {
			case c, ok := <-sub.C:
				if !ok {
					return sub, n
				}
				n += len(c)
			case <-deadline:
				return sub, n
			}
		}
	}

	waitConnect()
	sub, n := stream(2 * time.Second)
	t.Logf("before reset: %d bytes in 2s", n)
	if n == 0 {
		t.Fatal("no data before reset")
	}

	m.mu.Lock()
	b := m.board
	m.mu.Unlock()
	t0 := time.Now()
	for _, r := range breakRegs {
		if err := b.Bridge().WriteReg(uint16(r[0]), r[1]); err != nil {
			t.Fatal(err)
		}
	}
	// The viewer's stream must end rather than hang.
	giveUp := time.After(20 * time.Second)
	for ended := false; !ended; {
		select {
		case _, ok := <-sub.C:
			ended = !ok
		case <-giveUp:
			t.Fatal("stream did not end after reset")
		}
	}
	t.Logf("stream ended %v after reset", time.Since(t0).Round(time.Millisecond))
	sub.Close()

	waitConnect()
	t.Logf("reconnected %v after reset", time.Since(t0).Round(time.Millisecond))
	_, n = stream(2 * time.Second)
	t.Logf("after reconnect: %d bytes in 2s", n)
	if n == 0 {
		t.Fatal("no data after reconnect")
	}
}
