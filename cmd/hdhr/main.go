// SPDX-License-Identifier: GPL-2.0-or-later
// Copyright (C) 2026 Matt Rasband

// Command hdhr serves the WinTV-dualHD as an emulated HDHomeRun network tuner
// for Plex. Run cmd/scan first to produce the lineup file.
//
// The server stays up while the tuner is unplugged and reconnects to it when
// it returns.
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"strconv"
	"strings"

	"github.com/mattrasband/dualhd-mac/dualhd"
	"github.com/mattrasband/dualhd-mac/hdhr"
	"github.com/mattrasband/dualhd-mac/tuner"
)

func main() {
	addr := flag.String("addr", ":5004", "HTTP listen address")
	lineupPath := flag.String("lineup", "lineup.json", "scan results from cmd/scan")
	idPath := flag.String("device-id-file", "device_id", "where the HDHomeRun device ID is remembered")
	name := flag.String("name", "WinTV-dualHD (Mac)", "friendly name")
	discovery := flag.Bool("discovery", true, "answer HDHomeRun UDP discovery on port 65001")
	flag.Parse()

	lineup, err := hdhr.LoadLineup(*lineupPath)
	if err != nil {
		log.Fatalf("lineup: %v (run ./bin/scan first)", err)
	}
	_, portStr, err := net.SplitHostPort(*addr)
	if err != nil {
		log.Fatal(err)
	}
	port, _ := strconv.Atoi(portStr)

	mgr := tuner.NewManager(dualhd.Open)

	// The device ID comes from the USB serial number. It is remembered so
	// the server can start, with the same identity, while unplugged.
	id, haveID := loadID(*idPath)
	first := make(chan uint32, 1)
	mgr.OnConnect = func(b *dualhd.Board) {
		got := hdhr.DeviceIDFromSerial(b.Serial())
		if haveID && got != id {
			log.Printf("note: connected tuner (serial %s) differs from remembered device %08X; keeping %08X", b.Serial(), id, id)
		}
		if !haveID {
			saveID(*idPath, got)
		}
		select {
		case first <- got:
		default:
		}
	}
	go mgr.Run(context.Background())
	if !haveID {
		log.Printf("waiting for the tuner to be connected for the first time...")
		id = <-first
	}

	srv := &hdhr.Server{
		DeviceID:     id,
		FriendlyName: *name,
		TunerCount:   2,
		Lineup:       lineup,
		Tuners:       mgr,
		Port:         port,
	}
	log.Printf("device %08X, %d tuners, %d channels", srv.DeviceID, srv.TunerCount, len(lineup))
	if *discovery {
		go func() {
			if err := srv.ServeDiscovery(); err != nil {
				log.Printf("discovery disabled: %v", err)
			}
		}()
	}
	fmt.Printf("listening on %s\n", *addr)
	log.Fatal(http.ListenAndServe(*addr, srv.Handler()))
}

func loadID(path string) (uint32, bool) {
	b, err := os.ReadFile(path)
	if err != nil {
		return 0, false
	}
	v, err := strconv.ParseUint(strings.TrimSpace(string(b)), 16, 32)
	if err != nil {
		return 0, false
	}
	return uint32(v), true
}

func saveID(path string, id uint32) {
	if err := os.WriteFile(path, []byte(fmt.Sprintf("%08X\n", id)), 0o644); err != nil {
		log.Printf("saving device id: %v", err)
	}
}
