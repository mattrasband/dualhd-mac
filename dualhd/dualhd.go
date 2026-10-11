// SPDX-License-Identifier: GPL-2.0-or-later
// Copyright (C) 2026 Matt Rasband
//
// Board assembly ported from the Linux kernel em28xx driver
// (drivers/media/usb/em28xx/em28xx-dvb.c, em28xx-cards.c):
//   (c) 2008-2011 Mauro Carvalho Chehab <mchehab@kernel.org>
//   (c) 2008 Devin Heitmueller <devin.heitmueller@gmail.com>
//   (c) 2008 Aidan Thornton <makosoft@googlemail.com>
//   Copyright (C) 2005 Ludovico Cavedon <cavedon@sssup.it>
//   Copyright (C) 2012 Frank Schäfer <fschaefer.oss@googlemail.com>

// Package dualhd assembles the Hauppauge WinTV-dualHD 01595 (2040:826d): an
// EM28274 bridge feeding two LGDT3306A demodulator + Si2157 tuner pairs.
// Mirrors em28174_dvb_init_hauppauge_wintv_dualhd_01595 in em28xx-dvb.c.
package dualhd

import (
	"context"
	"errors"
	"fmt"
	"io"

	"github.com/google/gousb"

	"github.com/mattrasband/dualhd-mac/em28xx"
	"github.com/mattrasband/dualhd-mac/lgdt3306a"
	"github.com/mattrasband/dualhd-mac/si2157"
)

const (
	i2cBus = 1

	tsPacket = 188
	// Bulk transfers of 188*256 bytes, matching the bridge's max TS size
	// register setting of 0xff packets.
	transferSize = tsPacket * 256
	numTransfers = 8
)

type tsPort struct {
	demodAddr, tunerAddr byte
	endpoint             int
	pktSizeReg           uint16
	captureMask, allMask byte
}

var ports = [2]tsPort{
	{0x59, 0x60, 0x84, em28xx.RegTS1PktSize, 0x01, 0x07},
	{0x0e, 0x62, 0x85, em28xx.RegTS2PktSize, 0x10, 0x70},
}

type Board struct {
	dev   *em28xx.Device
	cfg   *gousb.Config
	intf  *gousb.Interface // interface 0 alt 0: bulk ep4 (TS1) and ep5 (TS2)
	Tuner [2]*Frontend
}

// Open initialises the bridge and both frontends.
func Open() (*Board, error) {
	dev, err := em28xx.Open()
	if err != nil {
		return nil, err
	}
	b := &Board{dev: dev}
	if err := b.init(); err != nil {
		b.Close()
		return nil, err
	}
	return b, nil
}

func (b *Board) init() error {
	id, err := b.dev.ReadReg(em28xx.RegChipID)
	if err != nil {
		return err
	}
	if id != 113 {
		return fmt.Errorf("unexpected bridge chip id %d", id)
	}
	if err := b.dev.WriteSeq(em28xx.DualHDDVBSeq); err != nil {
		return err
	}
	if err := b.dev.WriteSeq(em28xx.DualTSBulkSeq); err != nil {
		return err
	}
	if err := b.dev.InitI2C(); err != nil {
		return err
	}
	cfgNum, err := b.dev.USB().ActiveConfigNum()
	if err != nil {
		return err
	}
	if b.cfg, err = b.dev.USB().Config(cfgNum); err != nil {
		return fmt.Errorf("usb config %d: %w", cfgNum, err)
	}
	if b.intf, err = b.cfg.Interface(0, 0); err != nil {
		return fmt.Errorf("claim interface 0: %w", err)
	}
	bus := b.dev.Bus(i2cBus)
	for i, p := range ports {
		fe := &Frontend{
			b:     b,
			port:  p,
			Index: i,
			Demod: lgdt3306a.New(bus, p.demodAddr, lgdt3306a.DualHD01595),
			Tuner: si2157.New(bus, p.tunerAddr, si2157.DualHD01595),
		}
		if err := fe.init(); err != nil {
			return fmt.Errorf("tuner %d: %w", i, err)
		}
		b.Tuner[i] = fe
	}
	return nil
}

func (b *Board) Close() error {
	if b.intf != nil {
		b.intf.Close()
	}
	if b.cfg != nil {
		b.cfg.Close()
	}
	return b.dev.Close()
}

type Frontend struct {
	b     *Board
	port  tsPort
	Index int
	Demod *lgdt3306a.Demod
	Tuner *si2157.Tuner
}

// withGate runs fn with the demod's I2C repeater to the tuner open.
func (f *Frontend) withGate(fn func() error) error {
	if err := f.Demod.SetI2CGate(true); err != nil {
		return err
	}
	err := fn()
	return errors.Join(err, f.Demod.SetI2CGate(false))
}

func (f *Frontend) init() error {
	if err := f.Demod.Init(); err != nil {
		return err
	}
	return f.withGate(f.Tuner.Init)
}

// ChannelFreq returns the centre frequency in Hz of a US ATSC RF channel.
func ChannelFreq(rf int) (uint32, error) {
	var mhz int
	switch {
	case rf >= 2 && rf <= 4:
		mhz = 57 + (rf-2)*6
	case rf >= 5 && rf <= 6:
		mhz = 79 + (rf-5)*6
	case rf >= 7 && rf <= 13:
		mhz = 177 + (rf-7)*6
	case rf >= 14 && rf <= 36:
		mhz = 473 + (rf-14)*6
	default:
		return 0, fmt.Errorf("RF channel %d out of range (2-36)", rf)
	}
	return uint32(mhz) * 1_000_000, nil
}

// TuneATSC tunes an 8VSB channel; follows lgdt3306a_set_parameters.
func (f *Frontend) TuneATSC(freqHz uint32) error {
	if err := f.Demod.PowerUp(); err != nil {
		return err
	}
	if err := f.withGate(func() error { return f.Tuner.TuneATSC(freqHz) }); err != nil {
		return err
	}
	return f.Demod.StartVSB()
}

func (f *Frontend) Status() (lgdt3306a.Status, error) { return f.Demod.ReadStatus() }

func (f *Frontend) RSSI() (int8, error) {
	var v int8
	err := f.withGate(func() (err error) {
		v, err = f.Tuner.RSSI()
		return err
	})
	return v, err
}

func (f *Frontend) captureEnable(on bool) error {
	var v byte
	if on {
		v = f.port.captureMask
	}
	return f.b.dev.WriteRegBits(em28xx.RegTSEnable, v, f.port.allMask)
}

// Stream copies the transport stream to w until ctx is cancelled.
func (f *Frontend) Stream(ctx context.Context, w io.Writer) error {
	in, err := f.b.intf.InEndpoint(f.port.endpoint & 0x0f)
	if err != nil {
		return err
	}

	if err := f.b.dev.WriteReg(f.port.pktSizeReg, 0xff); err != nil {
		return err
	}
	if err := f.captureEnable(true); err != nil {
		return err
	}
	defer f.captureEnable(false)

	s, err := in.NewStream(transferSize, numTransfers)
	if err != nil {
		return err
	}
	defer s.Close()

	buf := make([]byte, transferSize)
	for {
		n, err := s.ReadContext(ctx, buf)
		if n > 0 {
			if _, werr := w.Write(buf[:n]); werr != nil {
				return werr
			}
		}
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return err
		}
	}
}

// Serial returns the USB serial number, stable per physical device.
func (b *Board) Serial() string {
	s, _ := b.dev.USB().SerialNumber()
	return s
}

// Ping checks the bridge still responds, by reading its chip ID.
func (b *Board) Ping() error {
	id, err := b.dev.ReadReg(em28xx.RegChipID)
	if err != nil {
		return err
	}
	if id != 113 {
		return fmt.Errorf("unexpected bridge chip id %d (device reset?)", id)
	}
	// The I2C clock register is set during init; if it has lost that
	// setting the device was reset and needs initialising again.
	clk, err := b.dev.ReadReg(em28xx.RegI2CClk)
	if err != nil {
		return err
	}
	if clk&^em28xx.I2CBusSelect != em28xx.I2CClkConfig {
		return fmt.Errorf("bridge lost its configuration (reg 0x06 = 0x%02x)", clk)
	}
	return nil
}

// Bridge exposes the EM28xx bridge for diagnostics.
func (b *Board) Bridge() *em28xx.Device { return b.dev }
