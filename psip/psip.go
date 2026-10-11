// SPDX-License-Identifier: GPL-2.0-or-later
// Copyright (C) 2026 Matt Rasband

// Package psip extracts ATSC channel information (PAT and the Virtual Channel
// Table, A/65) from an MPEG transport stream.
package psip

import (
	"encoding/binary"
	"fmt"
	"strings"
	"unicode/utf16"
)

const (
	PIDPAT  = 0x0000
	PIDPSIP = 0x1ffb // ATSC base PID: MGT, TVCT, STT, ...

	tableTVCT = 0xc8
	tableCVCT = 0xc9
)

// Channel is one virtual channel from the VCT.
type Channel struct {
	Major, Minor  uint16
	ShortName     string
	ProgramNumber uint16
	SourceID      uint16
	ServiceType   byte // 0x02 = ATSC digital TV, 0x03 = audio, 0x04 = data
	Hidden        bool
	TSID          uint16 // channel_TSID: the RF transport this channel lives on
}

func (c Channel) Number() string { return fmt.Sprintf("%d.%d", c.Major, c.Minor) }

// Scanner consumes TS packets and collects PAT and VCT contents.
type Scanner struct {
	asm      map[uint16]*SectionAssembler
	TSID     uint16
	Programs map[uint16]uint16 // program_number -> PMT PID
	Channels []Channel
	havePAT  bool
	haveVCT  bool
	vctSecs  map[byte]bool
	lastVCT  byte
}

func NewScanner() *Scanner {
	return &Scanner{
		asm:      map[uint16]*SectionAssembler{PIDPAT: {}, PIDPSIP: {}},
		Programs: map[uint16]uint16{},
		vctSecs:  map[byte]bool{},
	}
}

// Done reports whether both the PAT and a complete VCT have been seen.
func (s *Scanner) Done() bool { return s.havePAT && s.haveVCT }

// HavePAT reports whether a PAT was seen.
func (s *Scanner) HavePAT() bool { return s.havePAT }

// Feed processes one 188-byte TS packet.
func (s *Scanner) Feed(pkt []byte) {
	if len(pkt) != 188 || pkt[0] != 0x47 || pkt[1]&0x80 != 0 {
		return
	}
	pid := uint16(pkt[1]&0x1f)<<8 | uint16(pkt[2])
	a := s.asm[pid]
	if a == nil {
		return
	}
	for _, sec := range a.Push(pkt) {
		s.section(pid, sec)
	}
}

func (s *Scanner) section(pid uint16, sec []byte) {
	if len(sec) < 12 || CRC32(sec) != 0 {
		return
	}
	tableID := sec[0]
	switch {
	case pid == PIDPAT && tableID == 0x00:
		s.parsePAT(sec)
	case pid == PIDPSIP && (tableID == tableTVCT || tableID == tableCVCT):
		s.parseVCT(sec)
	}
}

func (s *Scanner) parsePAT(sec []byte) {
	s.TSID = binary.BigEndian.Uint16(sec[3:])
	body := sec[8 : len(sec)-4]
	for i := 0; i+4 <= len(body); i += 4 {
		prog := binary.BigEndian.Uint16(body[i:])
		pid := binary.BigEndian.Uint16(body[i+2:]) & 0x1fff
		if prog != 0 {
			s.Programs[prog] = pid
		}
	}
	s.havePAT = true
}

func (s *Scanner) parseVCT(sec []byte) {
	secNum, lastSec := sec[6], sec[7]
	if s.vctSecs[secNum] {
		return
	}
	s.vctSecs[secNum] = true
	s.lastVCT = lastSec

	n := int(sec[9])
	p := sec[10 : len(sec)-4]
	for i := 0; i < n && len(p) >= 32; i++ {
		name := make([]uint16, 7)
		for j := range name {
			name[j] = binary.BigEndian.Uint16(p[j*2:])
		}
		for len(name) > 0 && name[len(name)-1] == 0 {
			name = name[:len(name)-1]
		}
		b := binary.BigEndian.Uint32(p[14:])
		ch := Channel{
			ShortName:     strings.TrimSpace(string(utf16.Decode(name))),
			Major:         uint16(b>>18) & 0x3ff,
			Minor:         uint16(b>>8) & 0x3ff,
			TSID:          binary.BigEndian.Uint16(p[22:]),
			ProgramNumber: binary.BigEndian.Uint16(p[24:]),
			Hidden:        p[26]&0x10 != 0,
			ServiceType:   p[27] & 0x3f,
			SourceID:      binary.BigEndian.Uint16(p[28:]),
		}
		s.Channels = append(s.Channels, ch)
		descLen := int(binary.BigEndian.Uint16(p[30:]) & 0x3ff)
		if 32+descLen > len(p) {
			break
		}
		p = p[32+descLen:]
	}
	if len(s.vctSecs) == int(s.lastVCT)+1 {
		s.haveVCT = true
	}
}

// SectionAssembler reassembles PSI sections from TS packet payloads of one
// PID. The zero value is ready to use.
type SectionAssembler struct {
	buf    []byte
	active bool
}

// Push consumes one TS packet and returns any sections it completed.
func (a *SectionAssembler) Push(pkt []byte) [][]byte {
	pusi := pkt[1]&0x40 != 0
	afc := pkt[3] >> 4 & 0x3
	if afc&0x1 == 0 {
		return nil // no payload
	}
	payload := pkt[4:]
	if afc == 0x3 {
		if int(payload[0])+1 > len(payload) {
			return nil
		}
		payload = payload[1+int(payload[0]):]
	}
	var out [][]byte
	if pusi {
		if len(payload) == 0 {
			return nil
		}
		ptr := int(payload[0])
		payload = payload[1:]
		if ptr > len(payload) {
			a.active = false
			return nil
		}
		if a.active {
			a.buf = append(a.buf, payload[:ptr]...)
			out = append(out, a.drain()...)
		}
		a.buf = append(a.buf[:0], payload[ptr:]...)
		a.active = true
	} else if a.active {
		a.buf = append(a.buf, payload...)
	}
	if a.active {
		out = append(out, a.drain()...)
	}
	return out
}

// drain pops all complete sections from the buffer.
func (a *SectionAssembler) drain() [][]byte {
	var out [][]byte
	for len(a.buf) >= 3 && a.buf[0] != 0xff {
		l := 3 + int(binary.BigEndian.Uint16(a.buf[1:])&0x0fff)
		if len(a.buf) < l {
			return out
		}
		out = append(out, append([]byte(nil), a.buf[:l]...))
		a.buf = a.buf[l:]
	}
	if len(a.buf) > 0 && a.buf[0] == 0xff { // stuffing: rest of packet unused
		a.buf = a.buf[:0]
		a.active = false
	}
	return out
}

// CRC32 is the MPEG-2 CRC; it returns 0 over a section including its CRC.
func CRC32(b []byte) uint32 {
	crc := uint32(0xffffffff)
	for _, v := range b {
		crc ^= uint32(v) << 24
		for i := 0; i < 8; i++ {
			if crc&0x80000000 != 0 {
				crc = crc<<1 ^ 0x04c11db7
			} else {
				crc <<= 1
			}
		}
	}
	return crc
}
