// SPDX-License-Identifier: GPL-2.0-or-later
// Copyright (C) 2026 Matt Rasband

package hdhr

import (
	"encoding/binary"
	"net"
	"testing"
)

func TestDeviceIDChecksum(t *testing.T) {
	// Real HDHomeRun IDs have a zero checksum over all eight nibbles.
	for _, serial := range []string{"0012345678", "", "abc"} {
		id := DeviceIDFromSerial(serial)
		if got := deviceIDChecksum(id) ^ byte(id&0xf); got != 0 {
			t.Errorf("serial %q: id %08X fails checksum", serial, id)
		}
	}
	if DeviceIDFromSerial("x") != DeviceIDFromSerial("x") {
		t.Error("not stable")
	}
}

func TestDiscoveryRoundTrip(t *testing.T) {
	s := &Server{DeviceID: DeviceIDFromSerial("t"), TunerCount: 2, Port: 5004}
	u32 := func(v uint32) []byte { return binary.BigEndian.AppendUint32(nil, v) }
	req := buildPacket(typeDiscoverReq, []tlv{
		{tagDeviceType, u32(deviceTypeTuner)},
		{tagDeviceID, u32(deviceIDWildcard)},
	})
	rpy, err := s.discoverReply(req, net.IPv4(127, 0, 0, 1))
	if err != nil || rpy == nil {
		t.Fatalf("no reply: %v", err)
	}
	typ, tlvs, err := parsePacket(rpy)
	if err != nil || typ != typeDiscoverRpy {
		t.Fatalf("bad reply %x: %v", typ, err)
	}
	got := map[byte]string{}
	for _, v := range tlvs {
		got[v.tag] = string(v.val)
	}
	if got[tagBaseURL] != "http://127.0.0.1:5004" {
		t.Errorf("base url %q", got[tagBaseURL])
	}
	if binary.BigEndian.Uint32([]byte(got[tagDeviceID])) != s.DeviceID {
		t.Error("device id mismatch")
	}

	// A request for another device ID gets no reply.
	req = buildPacket(typeDiscoverReq, []tlv{{tagDeviceID, u32(s.DeviceID ^ 0x100)}})
	if rpy, _ := s.discoverReply(req, net.IPv4(127, 0, 0, 1)); rpy != nil {
		t.Error("replied to other device id")
	}
}
