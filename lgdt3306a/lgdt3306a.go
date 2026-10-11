// SPDX-License-Identifier: GPL-2.0-or-later
// Copyright (C) 2026 Matt Rasband
//
// Ported from the Linux kernel lgdt3306a driver
// (drivers/media/dvb-frontends/lgdt3306a.c):
//   Copyright (C) 2013 Fred Richter <frichter@hauppauge.com>

// Package lgdt3306a drives the LG LGDT3306A ATSC/QAM demodulator. Ported from
// the Linux kernel driver (drivers/media/dvb-frontends/lgdt3306a.c), VSB only,
// using the configuration of the Hauppauge WinTV-dualHD 01595.
package lgdt3306a

import (
	"fmt"
	"math"
	"time"

	"github.com/mattrasband/dualhd-mac/i2c"
)

// Config mirrors the subset of struct lgdt3306a_config this board needs.
type Config struct {
	VSBIFkHz    int  // 3250 on the dualHD
	XtalMHz     int  // 24 or 25
	SerialMPEG  bool // TS output mode
	TPClkRising bool
	TPValidHigh bool
}

// DualHD01595 is hauppauge_01595_lgdt3306a_config from em28xx-dvb.c.
var DualHD01595 = Config{
	VSBIFkHz:    3250,
	XtalMHz:     25,
	SerialMPEG:  true,
	TPClkRising: true,
	TPValidHigh: true,
}

type Demod struct {
	bus  i2c.Bus
	addr byte
	cfg  Config
}

func New(bus i2c.Bus, addr byte, cfg Config) *Demod {
	return &Demod{bus: bus, addr: addr, cfg: cfg}
}

func (d *Demod) ReadReg(reg uint16) (byte, error) {
	buf := make([]byte, 1)
	err := d.bus.Transfer(
		i2c.Msg{Addr: d.addr, Buf: []byte{byte(reg >> 8), byte(reg)}},
		i2c.Msg{Addr: d.addr, Read: true, Buf: buf})
	if err != nil {
		return 0, fmt.Errorf("lgdt3306a 0x%02x: read 0x%04x: %w", d.addr, reg, err)
	}
	return buf[0], nil
}

func (d *Demod) WriteReg(reg uint16, val byte) error {
	err := d.bus.Transfer(i2c.Msg{Addr: d.addr, Buf: []byte{byte(reg >> 8), byte(reg), val}})
	if err != nil {
		return fmt.Errorf("lgdt3306a 0x%02x: write 0x%04x: %w", d.addr, reg, err)
	}
	return nil
}

// regs batches read-modify-write sequences, stopping at the first error.
type regs struct {
	d   *Demod
	err error
}

func (r *regs) write(reg uint16, val byte) {
	if r.err == nil {
		r.err = r.d.WriteReg(reg, val)
	}
}

// update sets reg = (reg & keep) | set.
func (r *regs) update(reg uint16, keep, set byte) {
	if r.err != nil {
		return
	}
	v, err := r.d.ReadReg(reg)
	if err != nil {
		r.err = err
		return
	}
	r.err = r.d.WriteReg(reg, v&keep|set)
}

func (r *regs) bit(reg uint16, bit uint, on bool) {
	var set byte
	if on {
		set = 1 << bit
	}
	r.update(reg, ^byte(1<<bit), set)
}

func (r *regs) sleep(d time.Duration) {
	if r.err == nil {
		time.Sleep(d)
	}
}

// SetI2CGate opens (true) or closes the I2C repeater to the tuner behind this
// demodulator.
func (d *Demod) SetI2CGate(open bool) error {
	r := &regs{d: d}
	r.bit(0x0002, 7, !open) // NI2CRPTEN
	return r.err
}

func (r *regs) softReset() {
	r.bit(0x0000, 7, false)
	r.sleep(20 * time.Millisecond)
	r.bit(0x0000, 7, true)
}

func (r *regs) power(on bool) {
	if on {
		r.bit(0x0000, 7, true) // out of reset
		r.bit(0x0000, 0, true) // power up
	} else {
		r.bit(0x0000, 7, false) // into reset
		r.bit(0x0000, 0, false) // power down
	}
}

func (r *regs) mpegMode(serial bool) {
	r.bit(0x0071, 7, !serial) // TPSENB
	r.bit(0x0071, 6, false)   // TPSSOPBITEN: byte duration
	if serial {
		r.update(0x0070, 0xff, 0x10) // TPCLKSUPB
	} else {
		r.update(0x0070, ^byte(0x10), 0)
	}
}

func (r *regs) mpegPolarity(rising, validHigh bool) {
	var v byte
	if rising {
		v |= 0x04
	}
	if validHigh {
		v |= 0x02
	}
	r.update(0x0070, ^byte(0x06), v)
}

func (r *regs) mpegTristate(tristate bool) {
	if tristate {
		r.update(0x0070, ^byte(0xa8), 0) // TPOUTEN|TPCLKOUTEN|TPDATAOUTEN off
		r.bit(0x0003, 6, true)           // disable IFAGC pin
	} else {
		r.bit(0x0003, 6, false)
		r.update(0x0070, 0xff, 0xa8)
	}
}

func (r *regs) setVSB() {
	r.update(0x0002, 0xf7, 0x04) // SPECINVAUTO off, SPECINV on
	r.write(0x0008, 0x80)        // VSB mode
	r.update(0x0009, 0xe3, 0x0c) // STDOPDETTMODE=3 (6MHz)
	r.update(0x0009, 0xfc, 0x00) // STDOPDETCMODE=0
	r.update(0x000d, 0xbf, 0x00) // SAMPLING4XFEN=0

	// AICC (adjacent/co-channel interference) settings that "work well for
	// HVR-1955,1975".
	r.write(0x0024, 0x5a)
	r.write(0x002e, 0x5a)
	r.write(0x002f, 0x00)
	r.write(0x0030, 0x00)
	r.write(0x002b, 0x36)
	r.write(0x002c, 0x00)
	r.write(0x002d, 0x00)
	r.write(0x0028, 0x2a)
	r.write(0x0029, 0x00)
	r.write(0x002a, 0x00)
	r.write(0x0025, 0x06)
	r.write(0x0026, 0x00)
	r.write(0x0027, 0x00)

	r.update(0x001e, 0x0f, 0xa0)
	r.write(0x0022, 0x08)
	r.write(0x0023, 0xff)
	r.update(0x211f, 0xef, 0x00)
	r.write(0x2173, 0x01)
	r.update(0x1061, 0xf8, 0x04)
	r.update(0x103d, 0xcf, 0x00)
	r.write(0x2122, 0x40)
	r.update(0x2141, 0x3f, 0x00)
	r.update(0x2135, 0x0f, 0x70)
	r.update(0x0003, 0xf7, 0x00)
	r.update(0x001c, 0x7f, 0x00)
	r.update(0x2179, 0xf8, 0x00) // EQ step size
	r.update(0x217a, 0xf8, 0x00)
	r.softReset()
}

func (r *regs) setIF(khz int) {
	var nco1, nco2 byte
	switch khz {
	case 3500:
		nco1 = 0x38
	case 4000:
		nco1 = 0x40
	case 5000:
		nco1 = 0x50
	case 5380:
		nco1, nco2 = 0x56, 0x14
	default: // 3250
		nco1 = 0x34
	}
	r.write(0x0010, nco1)
	r.write(0x0011, nco2)
}

// Init is lgdt3306a_init: one-time setup, leaving the chip asleep.
func (d *Demod) Init() error {
	r := &regs{d: d}
	r.bit(0x0001, 0, true)  // SIMFASTENB: normal operation
	r.bit(0x0002, 3, false) // inversion auto off
	r.bit(0x0002, 2, true)  // inversion on
	r.bit(0x0004, 7, true)  // ADCSEL1V: 1Vpp
	r.bit(0x0004, 2, false) // ADC capture phase
	r.bit(0x0004, 3, false) // ADC clock: external
	r.bit(0x0005, 6, false) // PLLSETAUTO off
	r.update(0x0005, 0xc0, 0x25)
	r.write(0x0006, 0x64)
	switch d.cfg.XtalMHz {
	case 24:
		r.update(0x000d, 0xc0, 0x18)
	case 25:
		r.update(0x000d, 0xc0, 0x19)
	default:
		return fmt.Errorf("lgdt3306a: unsupported xtal %d MHz", d.cfg.XtalMHz)
	}
	r.write(0x0010, 0x34) // ADC centre frequency 3.25MHz
	r.write(0x0011, 0x00)
	r.write(0x0014, 0x00)        // gain error
	r.update(0x103c, 0x0f, 0x20) // VSB TR BW gear shift
	r.update(0x103d, 0xfc, 0x03) // low-temp timing offset (VSB)
	r.update(0x1036, 0xf0, 0x0c) // low-temp timing offset (QAM)
	r.update(0x211f, 0xef, 0x00) // don't use imaginary CIR
	r.update(0x2849, 0xef, 0x00) // enable no-signal detector
	r.setVSB()
	r.mpegMode(d.cfg.SerialMPEG)
	r.mpegTristate(true)
	r.mpegTristate(true) // lgdt3306a_sleep
	r.power(false)
	return r.err
}

// PowerUp takes the chip out of reset, ready for the tuner to be programmed.
func (d *Demod) PowerUp() error {
	r := &regs{d: d}
	r.power(true)
	return r.err
}

// StartVSB is the remainder of lgdt3306a_set_parameters, run after the tuner
// has been set.
func (d *Demod) StartVSB() error {
	r := &regs{d: d}
	r.setVSB()
	r.setIF(d.cfg.VSBIFkHz)
	r.mpegMode(d.cfg.SerialMPEG)
	r.mpegPolarity(d.cfg.TPClkRising, d.cfg.TPValidHigh)
	r.mpegTristate(false)
	r.softReset()
	return r.err
}

// Sleep tristates the TS bus and powers the chip down.
func (d *Demod) Sleep() error {
	r := &regs{d: d}
	r.mpegTristate(true)
	r.power(false)
	return r.err
}

// Status summarises lock state, as reported by lgdt3306a_read_status.
type Status struct {
	Signal bool    // never-lock detector reports a carrier
	Sync   bool    // frame sync
	Lock   bool    // sync + SNR >= 15dB + packet errors < 0xff
	SNR    float64 // dB
	Errors byte    // packet error counter
}

func (s Status) String() string {
	return fmt.Sprintf("signal=%v sync=%v lock=%v snr=%.1fdB errors=%d", s.Signal, s.Sync, s.Lock, s.SNR, s.Errors)
}

func (d *Demod) snr() (float64, error) {
	var b [4]byte
	for i, reg := range []uint16{0x00ec, 0x00ed, 0x00e8, 0x00e9} {
		v, err := d.ReadReg(reg)
		if err != nil {
			return 0, err
		}
		b[i] = v
	}
	mse := float64(uint16(b[0])<<8 | uint16(b[1]))
	pwr := float64(uint16(b[2])<<8 | uint16(b[3]))
	if mse == 0 {
		return 0, nil
	}
	return 10*math.Log10(pwr/mse) + 10*math.Log10(10000) - 30, nil
}

// ReadStatus ports lgdt3306a_read_status for VSB, including the adaptive
// equaliser tweaks it makes while polling. Takes up to ~1s without a signal.
func (d *Demod) ReadStatus() (Status, error) {
	var st Status
	for i := 0; i < 5; i++ {
		time.Sleep(30 * time.Millisecond)
		v, err := d.ReadReg(0x0080)
		if err != nil {
			return st, err
		}
		if v&0x03 == 0x02 { // LG3306_NL_LOCK
			st.Signal = true
			break
		}
	}
	if !st.Signal {
		return st, nil
	}
	for cnt := 0; cnt < 10; cnt++ {
		sync := false
		for i := 0; i < 2 && !sync; i++ {
			time.Sleep(30 * time.Millisecond)
			v, err := d.ReadReg(0x00a6)
			if err != nil {
				return st, err
			}
			sync = v&0x80 != 0
		}
		if !sync {
			return st, nil
		}
		st.Sync = true
		time.Sleep(20 * time.Millisecond)
		if err := d.preMonitoring(); err != nil {
			return st, err
		}
		var err error
		if st.Errors, err = d.ReadReg(0x00fa); err != nil {
			return st, err
		}
		if st.SNR, err = d.snr(); err != nil {
			return st, err
		}
		if st.SNR >= 15 && st.Errors < 0xff {
			st.Lock = true
			return st, d.monitorVSB()
		}
	}
	return st, nil
}

func (d *Demod) preMonitoring() error {
	v, err := d.ReadReg(0x2199)
	if err != nil {
		return err
	}
	r := &regs{d: d}
	if v&0x40 == 0 { // dynamic ghost
		r.update(0x2135, 0x0f, 0xa0)
		r.update(0x2141, 0x3f, 0x80)
		r.write(0x2122, 0x70)
	} else { // weak ghost or static channel
		r.update(0x2135, 0x0f, 0x70)
		r.update(0x2141, 0x3f, 0x40)
		r.write(0x2122, 0x40)
	}
	return r.err
}

func (d *Demod) monitorVSB() error {
	var v [5]byte
	for i, reg := range []uint16{0x21a1, 0x2185, 0x2191, 0x2180, 0x2181} {
		b, err := d.ReadReg(reg)
		if err != nil {
			return err
		}
		v[i] = b
	}
	snrRef := v[0] & 0x3f
	maxPowerMan := v[1]
	nCombDet := v[2] >> 7
	fbDlyCir := uint16(v[3]&0x03)<<8 | uint16(v[4])

	r := &regs{d: d}
	if snrRef > 18 && maxPowerMan > 0x68 && nCombDet == 1 && (fbDlyCir == 0x3ff || fbDlyCir < 0x6c) {
		r.update(0x1061, 0xf8, 0x00) // clean channel: final bandwidth 0
	} else {
		r.update(0x1061, 0xf8, 0x04)
	}
	if nCombDet == 0 {
		r.update(0x0024, 0x0f, 0x50) // notch filter on
	} else {
		r.update(0x0024, 0x0f, 0x00)
	}
	r.update(0x103d, 0xcf, 0x20)
	return r.err
}
