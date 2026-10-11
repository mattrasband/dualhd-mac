// SPDX-License-Identifier: GPL-2.0-or-later
// Copyright (C) 2026 Matt Rasband

// Command tune locks an ATSC channel on the WinTV-dualHD and records the
// transport stream to a file.
package main

import (
	"bufio"
	"context"
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"os/signal"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/mattrasband/dualhd-mac/dualhd"
	"github.com/mattrasband/dualhd-mac/lgdt3306a"
)

func main() {
	rfList := flag.String("rf", "35", "US ATSC RF channel, or two comma-separated to record on both tuners")
	tuner := flag.Int("tuner", 0, "tuner index (0 or 1) when recording one channel")
	dur := flag.Duration("d", 10*time.Second, "capture duration (0 to only tune)")
	out := flag.String("o", "rf%d.ts", "output file pattern (%d is the RF channel)")
	raw := flag.Bool("raw", false, "write raw bulk data without TS packet checks")
	flag.Parse()

	var rfs []int
	for _, f := range strings.Split(*rfList, ",") {
		n, err := strconv.Atoi(strings.TrimSpace(f))
		if err != nil {
			log.Fatalf("bad RF channel %q", f)
		}
		rfs = append(rfs, n)
	}
	if len(rfs) > 2 {
		log.Fatal("at most two channels")
	}

	start := time.Now()
	b, err := dualhd.Open()
	if err != nil {
		log.Fatalf("open: %v", err)
	}
	defer b.Close()
	fmt.Printf("board up in %v\n", time.Since(start).Round(time.Millisecond))

	type job struct {
		rf int
		fe *dualhd.Frontend
	}
	var jobs []job
	for i, rf := range rfs {
		idx := i
		if len(rfs) == 1 {
			idx = *tuner
		}
		fe := b.Tuner[idx]
		freq, err := dualhd.ChannelFreq(rf)
		if err != nil {
			log.Fatal(err)
		}
		fmt.Printf("tuner %d (%s): RF %d (%.3f MHz)\n", idx, fe.Tuner, rf, float64(freq)/1e6)
		if err := fe.TuneATSC(freq); err != nil {
			log.Fatalf("tune: %v", err)
		}
		rssi, _ := fe.RSSI()
		var st lgdt3306a.Status
		for i := 0; i < 5 && !st.Lock; i++ {
			if st, err = fe.Status(); err != nil {
				log.Fatalf("status: %v", err)
			}
		}
		fmt.Printf("  rssi=%ddBm %v\n", rssi, st)
		if !st.Lock {
			log.Fatalf("tuner %d: no lock", idx)
		}
		jobs = append(jobs, job{rf, fe})
	}
	if *dur == 0 {
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), *dur)
	defer cancel()
	ctx, stop := signal.NotifyContext(ctx, os.Interrupt)
	defer stop()

	fmt.Printf("capturing %v...\n", *dur)
	var wg sync.WaitGroup
	reports := make([]string, len(jobs))
	for i, j := range jobs {
		wg.Add(1)
		go func() {
			defer wg.Done()
			name := fmt.Sprintf(*out, j.rf)
			f, err := os.Create(name)
			if err != nil {
				log.Fatal(err)
			}
			defer f.Close()
			w := bufio.NewWriterSize(f, 1<<20)
			an := &analyzer{w: w, pids: map[uint16]int{}, cc: map[uint16]byte{}}
			var sink io.Writer = an
			if *raw {
				sink = w
			}
			t0 := time.Now()
			if err := j.fe.Stream(ctx, sink); err != nil {
				log.Fatalf("tuner %d stream: %v", j.fe.Index, err)
			}
			if err := w.Flush(); err != nil {
				log.Fatal(err)
			}
			reports[i] = fmt.Sprintf("tuner %d RF %d -> %s\n%s", j.fe.Index, j.rf, name, an.report(time.Since(t0)))
		}()
	}
	wg.Wait()
	for _, r := range reports {
		fmt.Print(r)
	}
}

// analyzer passes data through while checking TS packet integrity.
type analyzer struct {
	w        *bufio.Writer
	carry    []byte
	bytes    int
	packets  int
	syncLost int
	tei      int
	ccErrs   int
	pids     map[uint16]int
	cc       map[uint16]byte
}

func (a *analyzer) Write(p []byte) (int, error) {
	a.bytes += len(p)
	data := append(a.carry, p...)
	i := 0
	for len(data)-i >= 188 {
		if data[i] != 0x47 {
			a.syncLost++
			i++
			for i < len(data) && data[i] != 0x47 {
				i++
			}
			continue
		}
		pkt := data[i : i+188]
		a.packets++
		if pkt[1]&0x80 != 0 {
			a.tei++
		}
		pid := uint16(pkt[1]&0x1f)<<8 | uint16(pkt[2])
		a.pids[pid]++
		if pid != 0x1fff && pkt[3]&0x10 != 0 { // has payload
			cc := pkt[3] & 0x0f
			if last, ok := a.cc[pid]; ok && cc != (last+1)&0x0f && cc != last {
				a.ccErrs++
			}
			a.cc[pid] = cc
		}
		if _, err := a.w.Write(pkt); err != nil {
			return 0, err
		}
		i += 188
	}
	a.carry = append(a.carry[:0], data[i:]...)
	return len(p), nil
}

func (a *analyzer) report(d time.Duration) string {
	var sb strings.Builder
	fmt.Fprintf(&sb, "  received %d bytes in %v (%.2f Mbit/s)\n", a.bytes, d.Round(time.Millisecond), float64(a.bytes)*8/d.Seconds()/1e6)
	fmt.Fprintf(&sb, "  packets=%d sync_resyncs=%d transport_errors=%d continuity_errors=%d\n", a.packets, a.syncLost, a.tei, a.ccErrs)
	type pc struct {
		pid uint16
		n   int
	}
	var list []pc
	for p, n := range a.pids {
		list = append(list, pc{p, n})
	}
	sort.Slice(list, func(i, j int) bool { return list[i].n > list[j].n })
	sb.WriteString("  top PIDs:")
	for i, e := range list {
		if i == 10 {
			break
		}
		fmt.Fprintf(&sb, " 0x%04x(%d)", e.pid, e.n)
	}
	sb.WriteString("\n")
	return sb.String()
}
