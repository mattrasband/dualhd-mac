// SPDX-License-Identifier: GPL-2.0-or-later
// Copyright (C) 2026 Matt Rasband
//
// Ported from the Linux kernel em28xx driver (drivers/media/usb/em28xx):
//   Copyright (C) 2005 Ludovico Cavedon <cavedon@sssup.it>
//                      Markus Rechberger <mrechberger@gmail.com>
//                      Mauro Carvalho Chehab <mchehab@kernel.org>
//                      Sascha Sommer <saschasommer@freenet.de>
//   Copyright (C) 2012-2013 Frank Schäfer <fschaefer.oss@googlemail.com>

// Package em28xx is a userspace driver for the Empia EM28xx USB bridge as used
// on the Hauppauge WinTV-dualHD (2040:826d). Protocol details are ported from
// the Linux kernel driver (drivers/media/usb/em28xx).
package em28xx

import (
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/google/gousb"

	"github.com/mattrasband/dualhd-mac/i2c"
)

const (
	VendorHauppauge = 0x2040
	ProductDualHD   = 0x826d

	// USB_REQ_GET_STATUS is (ab)used by the bridge for register access.
	reqReg       = 0x00
	reqI2CStop   = 0x02
	reqI2CNoStop = 0x03

	RegI2CClk      = 0x06
	RegI2CStatus   = 0x05
	RegChipID      = 0x0a
	RegGPIOP0Ctrl  = 0x80
	RegTSEnable    = 0x5f
	RegTS1PktSize  = 0x5d
	RegTS2PktSize  = 0x5e
	i2cSecondary   = 0x04
	i2cWaitEnable  = 0x40
	i2cFreq400kHz  = 0x01
	ctrlTimeoutDur = time.Second
)

var ErrI2CNack = errors.New("i2c: no ack")

type Device struct {
	dev    *gousb.Device
	mu     sync.Mutex // serialises I2C transactions and read-modify-writes
	curBus int
}

// One libusb context is shared for the life of the process: re-initialising
// libusb after closing a context can fail, and gousb panics when it does.
var (
	usbCtxOnce sync.Once
	usbCtx     *gousb.Context
)

// Open finds and opens the first WinTV-dualHD on the system.
func Open() (*Device, error) {
	usbCtxOnce.Do(func() { usbCtx = gousb.NewContext() })
	dev, err := usbCtx.OpenDeviceWithVIDPID(VendorHauppauge, ProductDualHD)
	if err != nil {
		if dev != nil {
			dev.Close()
		}
		return nil, err
	}
	if dev == nil {
		return nil, fmt.Errorf("no device %04x:%04x found", VendorHauppauge, ProductDualHD)
	}
	dev.ControlTimeout = ctrlTimeoutDur
	return &Device{dev: dev, curBus: -1}, nil
}

func (d *Device) Close() error { return d.dev.Close() }

func (d *Device) USB() *gousb.Device { return d.dev }

const (
	rtIn  = gousb.ControlIn | gousb.ControlVendor | gousb.ControlDevice
	rtOut = gousb.ControlOut | gousb.ControlVendor | gousb.ControlDevice
)

func (d *Device) ReadReg(reg uint16) (byte, error) {
	buf := make([]byte, 1)
	n, err := d.dev.Control(rtIn, reqReg, 0, reg, buf)
	if err != nil {
		return 0, fmt.Errorf("read reg 0x%02x: %w", reg, err)
	}
	if n != 1 {
		return 0, fmt.Errorf("read reg 0x%02x: short read %d", reg, n)
	}
	return buf[0], nil
}

func (d *Device) WriteReg(reg uint16, val byte) error {
	_, err := d.dev.Control(rtOut, reqReg, 0, reg, []byte{val})
	if err != nil {
		return fmt.Errorf("write reg 0x%02x: %w", reg, err)
	}
	return nil
}

// WriteRegBits updates only the bits in mask. Safe for concurrent use.
func (d *Device) WriteRegBits(reg uint16, val, mask byte) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.writeRegBits(reg, val, mask)
}

func (d *Device) writeRegBits(reg uint16, val, mask byte) error {
	if mask != 0xff {
		old, err := d.ReadReg(reg)
		if err != nil {
			return err
		}
		val = (old &^ mask) | (val & mask)
	}
	return d.WriteReg(reg, val)
}

// RegSeq mirrors struct em28xx_reg_seq.
type RegSeq struct {
	Reg   uint16
	Val   byte
	Mask  byte
	Sleep time.Duration
}

func (d *Device) WriteSeq(seq []RegSeq) error {
	for _, s := range seq {
		if err := d.WriteRegBits(s.Reg, s.Val, s.Mask); err != nil {
			return err
		}
		time.Sleep(s.Sleep)
	}
	return nil
}

// DualHDDVBSeq is hauppauge_dualhd_dvb from em28xx-cards.c.
var DualHDDVBSeq = []RegSeq{
	{RegGPIOP0Ctrl, 0xff, 0xff, 0},
	{0x0d, 0xff, 0xff, 200 * time.Millisecond},
	{0x50, 0x04, 0xff, 300 * time.Millisecond},
	{RegGPIOP0Ctrl, 0xbf, 0xff, 100 * time.Millisecond}, // demod 1 reset
	{RegGPIOP0Ctrl, 0xff, 0xff, 100 * time.Millisecond},
	{RegGPIOP0Ctrl, 0xdf, 0xff, 100 * time.Millisecond}, // demod 2 reset
	{RegGPIOP0Ctrl, 0xff, 0xff, 100 * time.Millisecond},
	{RegTSEnable, 0x00, 0xff, 50 * time.Millisecond}, // disable TS filters
	{RegTS1PktSize, 0x05, 0xff, 50 * time.Millisecond},
	{RegTS2PktSize, 0x05, 0xff, 50 * time.Millisecond},
}

// DualTSBulkSeq enables the second TS input with both ep4 and ep5 in bulk
// mode (em28xx_usb_probe, "Configure hardware to support TS2").
var DualTSBulkSeq = []RegSeq{
	{0x0b, 0x96, 0xff, 100 * time.Millisecond},
	{0x0b, 0x80, 0xff, 100 * time.Millisecond},
}

// I2CClkConfig is the I2C clock register value for the 01595 board, and
// I2CBusSelect the bit within it that selects the secondary bus.
const (
	I2CClkConfig = i2cWaitEnable | i2cFreq400kHz
	I2CBusSelect = i2cSecondary
)

// InitI2C sets the bus clock as configured for the 01595 board.
func (d *Device) InitI2C() error {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.curBus = -1
	return d.WriteReg(RegI2CClk, I2CClkConfig)
}

func (d *Device) selectBus(bus int) error {
	if bus == d.curBus {
		return nil
	}
	var v byte
	if bus == 1 {
		v = i2cSecondary
	}
	if err := d.writeRegBits(RegI2CClk, v, i2cSecondary); err != nil {
		return err
	}
	d.curBus = bus
	return nil
}

func (d *Device) i2cStatus() error {
	deadline := time.Now().Add(100 * time.Millisecond)
	for {
		st, err := d.ReadReg(RegI2CStatus)
		if err != nil {
			return err
		}
		switch {
		case st == 0:
			return nil
		case st == 0x10:
			return ErrI2CNack
		case time.Now().After(deadline):
			return fmt.Errorf("i2c: status 0x%02x", st)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// I2CTransfer performs a combined transaction on the given bus; every message
// except the last is sent without a stop condition (repeated start).
func (d *Device) I2CTransfer(bus int, msgs ...i2c.Msg) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if err := d.selectBus(bus); err != nil {
		return err
	}
	for i, m := range msgs {
		addr := uint16(m.Addr) << 1
		if len(m.Buf) == 0 || len(m.Buf) > 64 {
			return fmt.Errorf("i2c: unsupported length %d", len(m.Buf))
		}
		if m.Read {
			n, err := d.dev.Control(rtIn, reqI2CStop, 0, addr, m.Buf)
			if err != nil {
				return fmt.Errorf("i2c read 0x%02x: %w", m.Addr, err)
			}
			if err := d.i2cStatus(); err != nil {
				return fmt.Errorf("i2c read 0x%02x: %w", m.Addr, err)
			}
			if n != len(m.Buf) {
				return fmt.Errorf("i2c read 0x%02x: short read %d", m.Addr, n)
			}
		} else {
			req := byte(reqI2CStop)
			if i < len(msgs)-1 {
				req = reqI2CNoStop
			}
			if _, err := d.dev.Control(rtOut, req, 0, addr, m.Buf); err != nil {
				return fmt.Errorf("i2c write 0x%02x: %w", m.Addr, err)
			}
			if err := d.i2cStatus(); err != nil {
				return fmt.Errorf("i2c write 0x%02x: %w", m.Addr, err)
			}
		}
	}
	return nil
}

// Probe reports whether a device acks at addr.
func (d *Device) Probe(bus int, addr byte) bool {
	return d.I2CTransfer(bus, i2c.Msg{Addr: addr, Read: true, Buf: make([]byte, 1)}) == nil
}

// Bus returns an i2c.Bus bound to one of the bridge's I2C buses.
func (d *Device) Bus(n int) i2c.Bus { return bus{d, n} }

type bus struct {
	d *Device
	n int
}

func (b bus) Transfer(msgs ...i2c.Msg) error { return b.d.I2CTransfer(b.n, msgs...) }
