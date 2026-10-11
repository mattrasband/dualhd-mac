// SPDX-License-Identifier: GPL-2.0-or-later
// Copyright (C) 2026 Matt Rasband

// Command probe brings up the WinTV-dualHD bridge and checks that the
// demodulators and tuners respond on I2C.
package main

import (
	"fmt"
	"log"

	"github.com/mattrasband/dualhd-mac/em28xx"
	"github.com/mattrasband/dualhd-mac/i2c"
)

const busDefault = 1

func main() {
	d, err := em28xx.Open()
	if err != nil {
		log.Fatalf("open: %v", err)
	}
	defer d.Close()

	cfg, _ := d.USB().ActiveConfigNum()
	fmt.Printf("opened %s (config %d)\n", d.USB(), cfg)

	id, err := d.ReadReg(em28xx.RegChipID)
	if err != nil {
		log.Fatalf("chip id: %v", err)
	}
	fmt.Printf("bridge chip id: %d (0x%02x)\n", id, id)

	if err := d.WriteSeq(em28xx.DualHDDVBSeq); err != nil {
		log.Fatalf("gpio sequence: %v", err)
	}
	if err := d.InitI2C(); err != nil {
		log.Fatalf("i2c init: %v", err)
	}

	for bus := 0; bus < 2; bus++ {
		fmt.Printf("i2c bus %d:", bus)
		for a := byte(0x08); a < 0x78; a++ {
			if d.Probe(bus, a) {
				fmt.Printf(" 0x%02x", a)
			}
		}
		fmt.Println()
	}

	for _, addr := range []byte{0x59, 0x0e} {
		fmt.Printf("lgdt3306a @0x%02x:", addr)
		for _, reg := range []uint16{0x0000, 0x0001, 0x0002} {
			buf := make([]byte, 1)
			err := d.I2CTransfer(busDefault,
				i2c.Msg{Addr: addr, Buf: []byte{byte(reg >> 8), byte(reg)}},
				i2c.Msg{Addr: addr, Read: true, Buf: buf})
			if err != nil {
				fmt.Printf(" reg%04x=ERR(%v)", reg, err)
				continue
			}
			fmt.Printf(" reg%04x=0x%02x", reg, buf[0])
		}
		fmt.Println("  (expect masks: r0&0x74=0x74, r1&0xf6=0xc6, r2&0x73=0x03)")
	}
}
