// SPDX-License-Identifier: GPL-2.0-or-later
// Copyright (C) 2026 Matt Rasband

package tsfilter

import (
	"os"
	"testing"
)

// TestCapture filters a real capture when one is present (see cmd/tune).
func TestCapture(t *testing.T) {
	in, err := os.ReadFile("../rf35.ts")
	if err != nil {
		t.Skip("no capture:", err)
	}
	f := New(1)
	var out []byte
	var h Health
	for i := 0; i+188 <= len(in); i += 188 {
		n := len(out)
		out = f.Filter(in[i:i+188], out)
		if len(out) > n {
			h.Check(out[n:])
		}
	}
	t.Logf("damaged=%d gaps=%d", h.Damaged, h.Gaps)
	if len(out) == 0 || len(out)%188 != 0 {
		t.Fatalf("bad output length %d", len(out))
	}
	t.Logf("kept %d of %d bytes, pids %v", len(out), len(in), f.keep)
	if dst := os.Getenv("TSFILTER_OUT"); dst != "" {
		os.WriteFile(dst, out, 0o644)
	}
}
