// SPDX-License-Identifier: GPL-2.0-or-later
// Copyright (C) 2026 Matt Rasband

// Package hdhr emulates a SiliconDust HDHomeRun network tuner (HTTP API and
// UDP discovery) on top of the tuner manager, so Plex and other DVR software
// can use the WinTV-dualHD.
package hdhr

import (
	"encoding/json"
	"errors"
	"fmt"
	"hash/fnv"
	"log"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/mattrasband/dualhd-mac/tsfilter"
	"github.com/mattrasband/dualhd-mac/tuner"
)

type Server struct {
	DeviceID     uint32
	FriendlyName string
	TunerCount   int
	Lineup       []Channel
	Tuners       *tuner.Manager
	// Port is the HTTP port, used when building URLs for discovery replies.
	Port int

	byNumber map[string]Channel
}

// DeviceIDFromSerial derives a stable device ID with a valid HDHomeRun
// checksum (clients such as libhdhomerun reject IDs that fail it).
func DeviceIDFromSerial(serial string) uint32 {
	h := fnv.New32a()
	// The salt is the project's original name; changing it would change
	// every device ID and make Plex treat existing tuners as new devices.
	h.Write([]byte("hauppauge-mac:" + serial))
	id := h.Sum32() & 0xfffffff0
	return id | uint32(deviceIDChecksum(id))
}

var checksumLookup = [16]byte{0xa, 0x5, 0xf, 0x6, 0x7, 0xc, 0x1, 0xb, 0x9, 0x2, 0x8, 0xd, 0x4, 0x3, 0xe, 0x0}

// deviceIDChecksum returns the low nibble that makes id's checksum zero
// (hdhomerun_discover_validate_device_id).
func deviceIDChecksum(id uint32) byte {
	var c byte
	c ^= checksumLookup[id>>28&0xf]
	c ^= byte(id >> 24 & 0xf)
	c ^= checksumLookup[id>>20&0xf]
	c ^= byte(id >> 16 & 0xf)
	c ^= checksumLookup[id>>12&0xf]
	c ^= byte(id >> 8 & 0xf)
	c ^= checksumLookup[id>>4&0xf]
	return c
}

func (s *Server) Handler() http.Handler {
	s.byNumber = map[string]Channel{}
	for _, c := range s.Lineup {
		s.byNumber[c.Number] = c
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /discover.json", s.discover)
	mux.HandleFunc("GET /lineup_status.json", s.lineupStatus)
	mux.HandleFunc("GET /lineup.json", s.lineup)
	mux.HandleFunc("POST /lineup.post", func(w http.ResponseWriter, r *http.Request) {})
	mux.HandleFunc("GET /status.json", s.status)
	mux.HandleFunc("GET /auto/{ch}", s.stream)
	mux.HandleFunc("GET /{tuner}/{ch}", func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.PathValue("tuner"), "tuner") {
			http.NotFound(w, r)
			return
		}
		s.stream(w, r)
	})
	return logRequests(mux)
}

func (s *Server) deviceIDHex() string { return fmt.Sprintf("%08X", s.DeviceID) }

func baseURL(r *http.Request) string { return "http://" + r.Host }

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(v)
}

func (s *Server) discoverInfo(base string) map[string]any {
	return map[string]any{
		"FriendlyName":    s.FriendlyName,
		"Manufacturer":    "Silicondust",
		"ModelNumber":     "HDTC-2US",
		"FirmwareName":    "hdhomeruntc_atsc",
		"FirmwareVersion": "20200101",
		"DeviceID":        s.deviceIDHex(),
		"DeviceAuth":      "hauppaugemac",
		"TunerCount":      s.TunerCount,
		"BaseURL":         base,
		"LineupURL":       base + "/lineup.json",
	}
}

func (s *Server) discover(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, s.discoverInfo(baseURL(r)))
}

func (s *Server) lineupStatus(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, map[string]any{
		"ScanInProgress": 0,
		"ScanPossible":   1,
		"Source":         "Antenna",
		"SourceList":     []string{"Antenna"},
	})
}

func (s *Server) lineup(w http.ResponseWriter, r *http.Request) {
	type entry struct {
		GuideNumber string
		GuideName   string
		URL         string
	}
	out := []entry{}
	for _, c := range s.Lineup {
		out = append(out, entry{c.Number, c.Name, baseURL(r) + "/auto/v" + c.Number})
	}
	writeJSON(w, out)
}

func (s *Server) status(w http.ResponseWriter, r *http.Request) {
	type tunerStatus struct {
		Resource         string
		VctNumber        string `json:",omitempty"`
		VctName          string `json:",omitempty"`
		Frequency        int    `json:",omitempty"`
		SignalQualityPct int    `json:",omitempty"`
		SymbolQualityPct int    `json:",omitempty"`
		SNRdB            string `json:",omitempty"`
		Viewers          int
	}
	var out []tunerStatus
	for _, t := range s.Tuners.Status() {
		ts := tunerStatus{Resource: fmt.Sprintf("tuner%d", t.Index), Viewers: t.Viewers}
		if !t.Online {
			ts.VctName = "device not connected"
		} else if t.RF != 0 {
			ts.VctNumber = fmt.Sprintf("RF %d", t.RF)
			ts.SNRdB = fmt.Sprintf("%.1f", t.Status.SNR)
			ts.SignalQualityPct = min(100, int(t.Status.SNR*100/30))
			if t.Status.Lock {
				ts.SymbolQualityPct = 100
			}
		}
		out = append(out, ts)
	}
	writeJSON(w, out)
}

func (s *Server) stream(w http.ResponseWriter, r *http.Request) {
	num, ok := strings.CutPrefix(r.PathValue("ch"), "v")
	ch, found := s.byNumber[num]
	if !ok || !found {
		http.Error(w, "unknown channel", http.StatusNotFound)
		return
	}
	sub, err := s.Tuners.Subscribe(ch.RF)
	if err != nil {
		code := "807 No Video Data"
		switch {
		case errors.Is(err, tuner.ErrAllInUse):
			code = "805 All Tuners In Use"
		case errors.Is(err, tuner.ErrNoDevice):
			code = "806 Tune Failed"
		}
		log.Printf("%s %s: %v", ch.Number, ch.Name, err)
		w.Header().Set("X-HDHomeRun-Error", code)
		http.Error(w, code, http.StatusServiceUnavailable)
		return
	}
	defer sub.Close()
	log.Printf("streaming %s %s (RF %d program %d) to %s", ch.Number, ch.Name, ch.RF, ch.Program, r.RemoteAddr)
	start := time.Now()

	w.Header().Set("Content-Type", "video/mpeg")
	w.Header().Set("Cache-Control", "no-cache")
	w.WriteHeader(http.StatusOK)
	rc := http.NewResponseController(w)

	f := tsfilter.New(ch.Program)
	var carry, out []byte
	var sent int64
	// Damage is logged at most every 10s, so a glitch a player reports
	// can be matched to what the antenna delivered.
	var health tsfilter.Health
	lastHealth := time.Now().Add(-10 * time.Second)
	defer func() {
		if health.Any() {
			log.Printf("%s: damaged signal: %d uncorrectable packets, %d continuity gaps, %d resyncs",
				ch.Number, health.Damaged, health.Gaps, health.Resyncs)
		}
		log.Printf("stopped %s for %s after %v (%.1f MB, %d chunks dropped)",
			ch.Number, r.RemoteAddr, time.Since(start).Round(time.Second), float64(sent)/1e6, sub.Dropped)
	}()
	for {
		var chunk []byte
		select {
		case <-r.Context().Done():
			return
		case c, ok := <-sub.C:
			if !ok {
				return
			}
			chunk = c
		}
		data := append(carry, chunk...)
		out = out[:0]
		i, synced := 0, true
		for len(data)-i >= 188 {
			if data[i] != 0x47 {
				if synced {
					health.Resyncs++
				}
				synced = false
				i++
				continue
			}
			synced = true
			n := len(out)
			out = f.Filter(data[i:i+188], out)
			if len(out) > n {
				health.Check(out[n:])
			}
			i += 188
		}
		carry = append(carry[:0], data[i:]...)
		if health.Any() && time.Since(lastHealth) >= 10*time.Second {
			log.Printf("%s: damaged signal: %d uncorrectable packets, %d continuity gaps, %d resyncs",
				ch.Number, health.Damaged, health.Gaps, health.Resyncs)
			health.Reset()
			lastHealth = time.Now()
		}
		if len(out) == 0 {
			continue
		}
		if _, err := w.Write(out); err != nil {
			return
		}
		sent += int64(len(out))
		rc.Flush()
	}
}

func logRequests(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.URL.Path, "/auto/") && !strings.HasPrefix(r.URL.Path, "/tuner") {
			log.Printf("%s %s %s", r.RemoteAddr, r.Method, r.URL.Path)
		}
		h.ServeHTTP(w, r)
	})
}

// localIPFor returns this host's address on the route to remote.
func localIPFor(remote net.IP) net.IP {
	c, err := net.DialUDP("udp4", nil, &net.UDPAddr{IP: remote, Port: 9})
	if err != nil {
		return nil
	}
	defer c.Close()
	return c.LocalAddr().(*net.UDPAddr).IP
}
