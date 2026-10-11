// SPDX-License-Identifier: GPL-2.0-or-later
// Copyright (C) 2026 Matt Rasband
//
// Ported from the Linux kernel si2157 driver (drivers/media/tuners/si2157.c):
//   Copyright (C) 2014 Antti Palosaari <crope@iki.fi>

// Package si2157 drives the Silicon Labs Si2157 silicon tuner. Ported from the
// Linux kernel driver (drivers/media/tuners/si2157.c), digital (ATSC) only.
// The tuner runs from its ROM firmware; no patch is downloaded.
package si2157

import (
	"errors"
	"fmt"
	"time"

	"github.com/mattrasband/dualhd-mac/i2c"
)

type Config struct {
	IFPort    byte // 1 on the dualHD
	Inversion bool // true on the dualHD
}

var DualHD01595 = Config{IFPort: 1, Inversion: true}

// ATSCIFHz is the IF the tuner outputs for ATSC; the demod must match it.
const ATSCIFHz = 3250000

type Tuner struct {
	bus  i2c.Bus
	addr byte
	cfg  Config
	ifHz uint32

	PartID   byte
	ROMID    byte
	Firmware string
}

func New(bus i2c.Bus, addr byte, cfg Config) *Tuner {
	return &Tuner{bus: bus, addr: addr, cfg: cfg, ifHz: 5000000}
}

var (
	errTimeout = errors.New("si2157: command timeout")
	errCmd     = errors.New("si2157: command error")
)

// cmd is si2157_cmd_execute: write args, then poll until CTS (bit 7) is set.
func (t *Tuner) cmd(args []byte, rlen int) ([]byte, error) {
	if len(args) > 0 {
		if err := t.bus.Transfer(i2c.Msg{Addr: t.addr, Buf: args}); err != nil {
			return nil, fmt.Errorf("si2157 cmd 0x%02x: %w", args[0], err)
		}
	}
	if rlen == 0 {
		return nil, nil
	}
	resp := make([]byte, rlen)
	deadline := time.Now().Add(80 * time.Millisecond)
	for {
		if err := t.bus.Transfer(i2c.Msg{Addr: t.addr, Read: true, Buf: resp}); err != nil {
			return nil, fmt.Errorf("si2157 cmd status: %w", err)
		}
		if resp[0]&0x80 != 0 {
			break
		}
		if time.Now().After(deadline) {
			return resp, errTimeout
		}
	}
	if resp[0]&0x40 != 0 {
		return resp, errCmd
	}
	return resp, nil
}

func (t *Tuner) setProperty(prop uint16, val uint16) error {
	_, err := t.cmd([]byte{0x14, 0x00, byte(prop), byte(prop >> 8), byte(val), byte(val >> 8)}, 4)
	if err != nil {
		return fmt.Errorf("set property 0x%04x: %w", prop, err)
	}
	return nil
}

// Init is si2157_init: power up and boot the ROM firmware. Safe to call when
// already running (it detects the warm state).
func (t *Tuner) Init() error {
	// Xtal trim property read: succeeds with a small value if already running.
	if r, err := t.cmd([]byte{0x15, 0x00, 0x02, 0x04}, 4); err == nil {
		if trim := uint16(r[2]) | uint16(r[3])<<8; trim < 16 {
			return t.readInfo()
		}
	}
	t.ifHz = 0

	// Power up (Si2157 variant).
	if _, err := t.cmd([]byte{0xc0, 0x00, 0x0c, 0x00, 0x00, 0x01, 0x01, 0x01, 0x01, 0x01, 0x01, 0x02, 0x00, 0x00, 0x01}, 1); err != nil {
		return fmt.Errorf("si2157 power up: %w", err)
	}

	// Part info is only answered before the firmware boots.
	r, err := t.cmd([]byte{0x02}, 13)
	if err != nil {
		return fmt.Errorf("si2157 part info: %w", err)
	}
	t.PartID, t.ROMID = r[2], r[12]

	// Boot firmware from ROM.
	if _, err := t.cmd([]byte{0x01, 0x01}, 1); err != nil {
		return fmt.Errorf("si2157 boot: %w", err)
	}
	if err := t.readInfo(); err != nil {
		return err
	}

	// Enable tuner status flags.
	for _, p := range []uint16{0x0501, 0x0601, 0x0701} {
		if err := t.setProperty(p, 1); err != nil {
			return fmt.Errorf("si2157 status flags: %w", err)
		}
	}
	return nil
}

func (t *Tuner) readInfo() error {
	r, err := t.cmd([]byte{0x11}, 10)
	if err != nil {
		return fmt.Errorf("si2157 firmware version: %w", err)
	}
	t.Firmware = fmt.Sprintf("%c.%c.%d", r[6], r[7], r[8])
	return nil
}

func (t *Tuner) String() string {
	if t.PartID == 0 { // warm start: part info unavailable
		return fmt.Sprintf("Si21xx firmware %s", t.Firmware)
	}
	return fmt.Sprintf("Si21%d ROM 0x%02x firmware %s", t.PartID, t.ROMID, t.Firmware)
}

// TuneATSC is si2157_set_params for an 8VSB channel centred on freqHz.
func (t *Tuner) TuneATSC(freqHz uint32) error {
	const (
		deliveryATSC = 0x00
		bw6MHz       = 6
	)
	var inv uint16
	if t.cfg.Inversion {
		inv = 1
	}
	// DTV_MODE: delivery system | bandwidth, inversion.
	if err := t.setProperty(0x0703, uint16(deliveryATSC|bw6MHz)|inv<<8); err != nil {
		return err
	}
	// DTV_AGC_SOURCE / IF port.
	if err := t.setProperty(0x0702, uint16(t.cfg.IFPort)); err != nil {
		return err
	}
	if t.ifHz != ATSCIFHz {
		if err := t.setProperty(0x0706, ATSCIFHz/1000); err != nil {
			return err
		}
		t.ifHz = ATSCIFHz
	}
	_, err := t.cmd([]byte{0x41, 0x00, 0x00, 0x00,
		byte(freqHz), byte(freqHz >> 8), byte(freqHz >> 16), byte(freqHz >> 24)}, 1)
	if err != nil {
		return fmt.Errorf("si2157 tune %d: %w", freqHz, err)
	}
	return t.tuneWait()
}

func (t *Tuner) tuneWait() error {
	st := make([]byte, 1)
	deadline := time.Now().Add(40 * time.Millisecond)
	for {
		if err := t.bus.Transfer(i2c.Msg{Addr: t.addr, Read: true, Buf: st}); err != nil {
			return err
		}
		if st[0]&0x81 == 0x81 || time.Now().After(deadline) {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if st[0]&0xc0 != 0x80 {
		return fmt.Errorf("si2157 tune wait: status 0x%02x", st[0])
	}
	return nil
}

// RSSI returns the received signal strength in dBm (TUNER_STATUS command).
func (t *Tuner) RSSI() (int8, error) {
	r, err := t.cmd([]byte{0x42, 0x00}, 12)
	if err != nil {
		return 0, err
	}
	return int8(r[3]), nil
}
