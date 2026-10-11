// SPDX-License-Identifier: GPL-2.0-or-later
// Copyright (C) 2026 Matt Rasband

// Command scan searches US ATSC RF channels with both tuners and lists the
// virtual channels found, writing them to a JSON lineup file.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"os"
	"sort"
	"sync"
	"time"

	"github.com/mattrasband/dualhd-mac/dualhd"
	"github.com/mattrasband/dualhd-mac/lgdt3306a"
	"github.com/mattrasband/dualhd-mac/psip"
)

// Result is one RF channel's scan outcome.
type Result struct {
	RF       int            `json:"rf"`
	FreqHz   uint32         `json:"freq_hz"`
	RSSI     int8           `json:"rssi_dbm"`
	SNR      float64        `json:"snr_db"`
	Signal   bool           `json:"signal"`
	Lock     bool           `json:"lock"`
	TSID     uint16         `json:"tsid,omitempty"`
	Channels []psip.Channel `json:"channels,omitempty"`
	Note     string         `json:"note,omitempty"`
}

func main() {
	from := flag.Int("from", 2, "first RF channel")
	to := flag.Int("to", 36, "last RF channel")
	out := flag.String("o", "lineup.json", "output file")
	flag.Parse()

	b, err := dualhd.Open()
	if err != nil {
		log.Fatalf("open: %v", err)
	}
	defer b.Close()

	work := make(chan int)
	go func() {
		for rf := *from; rf <= *to; rf++ {
			work <- rf
		}
		close(work)
	}()

	start := time.Now()
	var (
		mu      sync.Mutex
		results []Result
		wg      sync.WaitGroup
	)
	for _, fe := range b.Tuner {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for rf := range work {
				r := scan(fe, rf)
				mu.Lock()
				results = append(results, r)
				mu.Unlock()
				if r.Lock || r.Signal {
					fmt.Printf("  RF %2d: %s\n", rf, summary(r))
				}
			}
		}()
	}
	wg.Wait()
	sort.Slice(results, func(i, j int) bool { return results[i].RF < results[j].RF })
	fmt.Printf("scanned RF %d-%d in %v\n\n", *from, *to, time.Since(start).Round(100*time.Millisecond))

	fmt.Println("RF  MHz   RSSI   SNR   status    channels")
	for _, r := range results {
		if !r.Signal && !r.Lock {
			continue
		}
		fmt.Printf("%-3d %-5d %-6s %-5s %-9s %s\n", r.RF, r.FreqHz/1e6, fmt.Sprintf("%ddBm", r.RSSI), fmt.Sprintf("%.1f", r.SNR), status(r), names(r))
	}

	f, err := os.Create(*out)
	if err != nil {
		log.Fatal(err)
	}
	defer f.Close()
	enc := json.NewEncoder(f)
	enc.SetIndent("", "  ")
	if err := enc.Encode(results); err != nil {
		log.Fatal(err)
	}
	fmt.Printf("\nwrote %s\n", *out)
}

func scan(fe *dualhd.Frontend, rf int) Result {
	freq, _ := dualhd.ChannelFreq(rf)
	r := Result{RF: rf, FreqHz: freq}
	if err := fe.TuneATSC(freq); err != nil {
		r.Note = err.Error()
		return r
	}
	r.RSSI, _ = fe.RSSI()
	var st lgdt3306a.Status
	var err error
	for i := 0; i < 3 && !st.Lock; i++ {
		if st, err = fe.Status(); err != nil {
			r.Note = err.Error()
			return r
		}
		if !st.Signal {
			break
		}
	}
	r.Signal, r.Lock, r.SNR = st.Signal, st.Lock, st.SNR
	if !st.Lock {
		return r
	}

	// Read PSIP for up to 3s.
	sc := psip.NewScanner()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	w := &packetWriter{fn: func(pkt []byte) {
		sc.Feed(pkt)
		if sc.Done() {
			cancel()
		}
	}}
	if err := fe.Stream(ctx, w); err != nil {
		r.Note = err.Error()
	}
	r.TSID = sc.TSID
	r.Channels = sc.Channels
	if !sc.Done() {
		if sc.HavePAT() {
			r.Note = fmt.Sprintf("no VCT; PAT lists %d programs", len(sc.Programs))
		} else {
			r.Note = "locked but no PSI"
		}
	}
	return r
}

type packetWriter struct {
	carry []byte
	fn    func([]byte)
}

func (p *packetWriter) Write(b []byte) (int, error) {
	data := append(p.carry, b...)
	i := 0
	for len(data)-i >= 188 {
		if data[i] != 0x47 {
			i++
			continue
		}
		p.fn(data[i : i+188])
		i += 188
	}
	p.carry = append(p.carry[:0], data[i:]...)
	return len(b), nil
}

func status(r Result) string {
	switch {
	case r.Lock:
		return "locked"
	case r.Signal:
		return "no-lock"
	}
	return "-"
}

func names(r Result) string {
	s := ""
	for _, c := range r.Channels {
		if c.Hidden {
			continue
		}
		s += fmt.Sprintf("%s %s  ", c.Number(), c.ShortName)
	}
	if r.Note != "" {
		s += "(" + r.Note + ")"
	}
	return s
}

func summary(r Result) string {
	return fmt.Sprintf("%s rssi=%ddBm snr=%.1fdB %s", status(r), r.RSSI, r.SNR, names(r))
}
