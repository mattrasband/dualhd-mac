// SPDX-License-Identifier: GPL-2.0-or-later
// Copyright (C) 2026 Matt Rasband

package tsfilter

// Health counts signs of damage in a transport stream: packets the
// demodulator flagged as uncorrectable (transport_error_indicator), and
// continuity counter gaps, which mean packets were lost.
type Health struct {
	Damaged int // packets with the transport error flag set
	Gaps    int // continuity counter jumps
	Resyncs int // times the stream lost 188-byte packet alignment

	cc map[uint16]byte // last continuity counter per PID, +1 so 0 is unset
}

// Check records one 188-byte packet.
func (h *Health) Check(pkt []byte) {
	if pkt[1]&0x80 != 0 {
		// The header itself may be wrong, so don't trust its PID or counter.
		h.Damaged++
		return
	}
	pid := uint16(pkt[1]&0x1f)<<8 | uint16(pkt[2])
	if pid == 0x1fff || pkt[3]&0x10 == 0 { // null packet, or no payload
		return
	}
	if h.cc == nil {
		h.cc = map[uint16]byte{}
	}
	cc := pkt[3] & 0x0f
	discontinuity := pkt[3]&0x20 != 0 && pkt[4] > 0 && pkt[5]&0x80 != 0
	if last, ok := h.cc[pid]; ok && !discontinuity {
		prev := last - 1
		if cc != (prev+1)&0x0f && cc != prev { // a repeat is allowed once
			h.Gaps++
		}
	}
	h.cc[pid] = cc + 1
}

// Any reports whether anything was counted.
func (h *Health) Any() bool { return h.Damaged+h.Gaps+h.Resyncs > 0 }

// Reset clears the counts but keeps the continuity state.
func (h *Health) Reset() { h.Damaged, h.Gaps, h.Resyncs = 0, 0, 0 }
