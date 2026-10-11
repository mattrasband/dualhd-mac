// SPDX-License-Identifier: GPL-2.0-or-later
// Copyright (C) 2026 Matt Rasband

package hdhr

import (
	"encoding/binary"
	"fmt"
	"hash/crc32"
	"log"
	"net"
)

// HDHomeRun discovery protocol (libhdhomerun hdhomerun_pkt.h).
const (
	DiscoverPort = 65001

	typeDiscoverReq = 0x0002
	typeDiscoverRpy = 0x0003

	tagDeviceType = 0x01
	tagDeviceID   = 0x02
	tagTunerCount = 0x10
	tagLineupURL  = 0x27
	tagBaseURL    = 0x2a
	tagDeviceAuth = 0x2b

	deviceTypeTuner    = 0x00000001
	deviceTypeWildcard = 0xffffffff
	deviceIDWildcard   = 0xffffffff
)

// ServeDiscovery answers UDP discovery broadcasts until the socket fails.
func (s *Server) ServeDiscovery() error {
	conn, err := net.ListenUDP("udp4", &net.UDPAddr{Port: DiscoverPort})
	if err != nil {
		return err
	}
	defer conn.Close()
	buf := make([]byte, 1500)
	for {
		n, from, err := conn.ReadFromUDP(buf)
		if err != nil {
			return err
		}
		reply, err := s.discoverReply(buf[:n], from.IP)
		if err != nil {
			log.Printf("discovery from %v: %v", from, err)
			continue
		}
		if reply != nil {
			if _, err := conn.WriteToUDP(reply, from); err != nil {
				log.Printf("discovery reply to %v: %v", from, err)
			}
		}
	}
}

type tlv struct {
	tag byte
	val []byte
}

func parsePacket(b []byte) (uint16, []tlv, error) {
	if len(b) < 8 {
		return 0, nil, fmt.Errorf("short packet")
	}
	typ := binary.BigEndian.Uint16(b)
	l := int(binary.BigEndian.Uint16(b[2:]))
	if 4+l+4 != len(b) {
		return 0, nil, fmt.Errorf("bad length")
	}
	if crc32.ChecksumIEEE(b[:4+l]) != binary.LittleEndian.Uint32(b[4+l:]) {
		return 0, nil, fmt.Errorf("bad crc")
	}
	var tlvs []tlv
	p := b[4 : 4+l]
	for len(p) >= 2 {
		tag := p[0]
		vl := int(p[1])
		hdr := 2
		if vl&0x80 != 0 {
			if len(p) < 3 {
				break
			}
			vl = vl&0x7f | int(p[2])<<7
			hdr = 3
		}
		if hdr+vl > len(p) {
			break
		}
		tlvs = append(tlvs, tlv{tag, p[hdr : hdr+vl]})
		p = p[hdr+vl:]
	}
	return typ, tlvs, nil
}

func buildPacket(typ uint16, tlvs []tlv) []byte {
	var payload []byte
	for _, t := range tlvs {
		payload = append(payload, t.tag)
		if len(t.val) < 128 {
			payload = append(payload, byte(len(t.val)))
		} else {
			payload = append(payload, byte(len(t.val))|0x80, byte(len(t.val)>>7))
		}
		payload = append(payload, t.val...)
	}
	pkt := binary.BigEndian.AppendUint16(nil, typ)
	pkt = binary.BigEndian.AppendUint16(pkt, uint16(len(payload)))
	pkt = append(pkt, payload...)
	return binary.LittleEndian.AppendUint32(pkt, crc32.ChecksumIEEE(pkt))
}

func (s *Server) discoverReply(req []byte, from net.IP) ([]byte, error) {
	typ, tlvs, err := parsePacket(req)
	if err != nil {
		return nil, err
	}
	if typ != typeDiscoverReq {
		return nil, nil
	}
	for _, t := range tlvs {
		if len(t.val) != 4 {
			continue
		}
		v := binary.BigEndian.Uint32(t.val)
		switch t.tag {
		case tagDeviceType:
			if v != deviceTypeTuner && v != deviceTypeWildcard {
				return nil, nil
			}
		case tagDeviceID:
			if v != deviceIDWildcard && v != s.DeviceID {
				return nil, nil
			}
		}
	}
	ip := localIPFor(from)
	if ip == nil {
		return nil, fmt.Errorf("no route")
	}
	base := fmt.Sprintf("http://%s:%d", ip, s.Port)
	u32 := func(v uint32) []byte { return binary.BigEndian.AppendUint32(nil, v) }
	return buildPacket(typeDiscoverRpy, []tlv{
		{tagDeviceType, u32(deviceTypeTuner)},
		{tagDeviceID, u32(s.DeviceID)},
		{tagTunerCount, []byte{byte(s.TunerCount)}},
		{tagBaseURL, []byte(base)},
		{tagLineupURL, []byte(base + "/lineup.json")},
		{tagDeviceAuth, []byte("hauppaugemac")},
	}), nil
}
