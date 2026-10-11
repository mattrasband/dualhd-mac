// SPDX-License-Identifier: GPL-2.0-or-later
// Copyright (C) 2026 Matt Rasband

package hdhr

import (
	"encoding/json"
	"fmt"
	"os"
	"sort"

	"github.com/mattrasband/dualhd-mac/psip"
)

// ScanResult is one RF channel from lineup.json (written by cmd/scan).
type ScanResult struct {
	RF       int            `json:"rf"`
	SNR      float64        `json:"snr_db"`
	Lock     bool           `json:"lock"`
	TSID     uint16         `json:"tsid"`
	Channels []psip.Channel `json:"channels"`
}

// Channel is one entry in the lineup served to Plex.
type Channel struct {
	Number  string // e.g. "4.1"
	Name    string
	RF      int
	Program uint16
	SNR     float64
	major   uint16
	minor   uint16
}

// LoadLineup reads a scan file and builds a de-duplicated lineup. A channel
// listed by several transports is taken from one that actually carries it
// (its channel_TSID matches the transport's), preferring the best SNR.
func LoadLineup(path string) ([]Channel, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var results []ScanResult
	if err := json.Unmarshal(data, &results); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}

	type cand struct {
		ch      Channel
		carried bool
	}
	best := map[string]cand{}
	for _, r := range results {
		if !r.Lock {
			continue
		}
		for _, c := range r.Channels {
			if c.Hidden || c.ServiceType != 0x02 || c.ProgramNumber == 0 {
				continue
			}
			n := cand{
				ch: Channel{
					Number: c.Number(), Name: c.ShortName, RF: r.RF,
					Program: c.ProgramNumber, SNR: r.SNR,
					major: c.Major, minor: c.Minor,
				},
				carried: c.TSID == r.TSID,
			}
			old, ok := best[n.ch.Number]
			switch {
			case !ok,
				n.carried && !old.carried,
				n.carried == old.carried && n.ch.SNR > old.ch.SNR:
				best[n.ch.Number] = n
			}
		}
	}

	var out []Channel
	for _, c := range best {
		if c.carried {
			out = append(out, c.ch)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].major != out[j].major {
			return out[i].major < out[j].major
		}
		return out[i].minor < out[j].minor
	})
	return out, nil
}
