// SPDX-License-Identifier: GPL-2.0-or-later
// Copyright (C) 2026 Matt Rasband

// Package tsfilter reduces a multi-program transport stream to a single
// program, as an HDHomeRun does for /auto/v<channel> requests: a rewritten
// single-program PAT, the program's PMT, and the PIDs the PMT references.
package tsfilter

import (
	"encoding/binary"

	"github.com/mattrasband/dualhd-mac/psip"
)

const pidPAT = 0x0000

type Program struct {
	program uint16

	patAsm psip.SectionAssembler
	pmtAsm psip.SectionAssembler
	tsid   uint16
	pmtPID uint16 // 0 until found in the PAT
	pmtVer int    // -1 until a PMT is parsed
	keep   map[uint16]bool
	patCC  byte
	pat    []byte // rewritten PAT packet, rebuilt when the PAT changes
}

// New returns a filter for the given MPEG program_number.
func New(program uint16) *Program {
	return &Program{program: program, pmtVer: -1, keep: map[uint16]bool{}}
}

// Filter processes one 188-byte packet and appends whatever should be sent
// to out, returning the extended slice.
func (p *Program) Filter(pkt, out []byte) []byte {
	if len(pkt) != 188 || pkt[0] != 0x47 {
		return out
	}
	pid := uint16(pkt[1]&0x1f)<<8 | uint16(pkt[2])
	switch {
	case pid == pidPAT:
		for _, sec := range p.patAsm.Push(pkt) {
			p.parsePAT(sec)
		}
		// Emit our PAT at the source PAT's cadence (~10/s).
		if pkt[1]&0x40 != 0 && p.pat != nil {
			out = append(out, p.nextPAT()...)
		}
		return out
	case p.pmtPID != 0 && pid == p.pmtPID:
		for _, sec := range p.pmtAsm.Push(pkt) {
			p.parsePMT(sec)
		}
		if p.pmtVer >= 0 {
			out = append(out, pkt...)
		}
		return out
	case p.keep[pid]:
		return append(out, pkt...)
	}
	return out
}

func (p *Program) parsePAT(sec []byte) {
	if len(sec) < 12 || sec[0] != 0x00 || psip.CRC32(sec) != 0 {
		return
	}
	tsid := binary.BigEndian.Uint16(sec[3:])
	body := sec[8 : len(sec)-4]
	for i := 0; i+4 <= len(body); i += 4 {
		if binary.BigEndian.Uint16(body[i:]) != p.program {
			continue
		}
		pmt := binary.BigEndian.Uint16(body[i+2:]) & 0x1fff
		if pmt != p.pmtPID || tsid != p.tsid || p.pat == nil {
			if pmt != p.pmtPID {
				p.pmtAsm = psip.SectionAssembler{}
				p.pmtVer = -1
				p.keep = map[uint16]bool{}
			}
			p.pmtPID, p.tsid = pmt, tsid
			p.pat = buildPAT(tsid, p.program, pmt)
		}
		return
	}
}

func (p *Program) parsePMT(sec []byte) {
	if len(sec) < 16 || sec[0] != 0x02 || psip.CRC32(sec) != 0 {
		return
	}
	if binary.BigEndian.Uint16(sec[3:]) != p.program {
		return
	}
	ver := int(sec[5]>>1) & 0x1f
	if ver == p.pmtVer {
		return
	}
	keep := map[uint16]bool{}
	keep[binary.BigEndian.Uint16(sec[8:])&0x1fff] = true // PCR PID
	infoLen := int(binary.BigEndian.Uint16(sec[10:]) & 0x0fff)
	es := sec[12+infoLen : len(sec)-4]
	for len(es) >= 5 {
		keep[binary.BigEndian.Uint16(es[1:])&0x1fff] = true
		l := int(binary.BigEndian.Uint16(es[3:]) & 0x0fff)
		if 5+l > len(es) {
			break
		}
		es = es[5+l:]
	}
	delete(keep, 0x1fff)
	p.keep = keep
	p.pmtVer = ver
}

func (p *Program) nextPAT() []byte {
	p.pat[3] = 0x10 | p.patCC // payload only
	p.patCC = (p.patCC + 1) & 0x0f
	return p.pat
}

// buildPAT returns a TS packet carrying a single-program PAT.
func buildPAT(tsid, program, pmtPID uint16) []byte {
	sec := []byte{
		0x00,       // table_id
		0xb0, 0x0d, // section_syntax_indicator, length 13
		byte(tsid >> 8), byte(tsid),
		0xc1, // version 0, current
		0x00, 0x00,
		byte(program >> 8), byte(program),
		0xe0 | byte(pmtPID>>8), byte(pmtPID),
	}
	crc := psip.CRC32(sec)
	sec = binary.BigEndian.AppendUint32(sec, crc)

	pkt := make([]byte, 188)
	for i := range pkt {
		pkt[i] = 0xff
	}
	pkt[0], pkt[1], pkt[2], pkt[3] = 0x47, 0x40, 0x00, 0x10 // PUSI, PID 0
	pkt[4] = 0x00                                           // pointer_field
	copy(pkt[5:], sec)
	return pkt
}
