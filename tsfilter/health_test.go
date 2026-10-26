// SPDX-License-Identifier: GPL-2.0-or-later
// Copyright (C) 2026 Matt Rasband

package tsfilter

import "testing"

func packet(pid uint16, cc byte, tei bool) []byte {
	p := make([]byte, 188)
	p[0], p[1], p[2], p[3] = 0x47, byte(pid>>8), byte(pid), 0x10|cc
	if tei {
		p[1] |= 0x80
	}
	return p
}

func TestHealth(t *testing.T) {
	var h Health
	for _, p := range [][]byte{
		packet(0x31, 14, false),
		packet(0x31, 15, false),
		packet(0x31, 0, false),   // wraps
		packet(0x31, 0, false),   // repeat: allowed
		packet(0x44, 3, false),   // another PID
		packet(0x31, 3, false),   // gap (1 and 2 missing)
		packet(0x31, 9, true),    // damaged
		packet(0x1fff, 7, false), // null packets are ignored
		packet(0x31, 4, false),
	} {
		h.Check(p)
	}
	if h.Damaged != 1 || h.Gaps != 1 {
		t.Errorf("damaged=%d gaps=%d, want 1 and 1", h.Damaged, h.Gaps)
	}
}
