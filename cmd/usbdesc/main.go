// SPDX-License-Identifier: GPL-2.0-or-later
// Copyright (C) 2026 Matt Rasband

// Command usbdesc prints the WinTV-dualHD's USB interface and endpoint layout.
package main

import (
	"fmt"
	"log"

	"github.com/mattrasband/dualhd-mac/em28xx"
)

func main() {
	d, err := em28xx.Open()
	if err != nil {
		log.Fatal(err)
	}
	defer d.Close()
	for _, c := range d.USB().Desc.Configs {
		fmt.Printf("config %d\n", c.Number)
		for _, i := range c.Interfaces {
			for _, a := range i.AltSettings {
				fmt.Printf("  interface %d alt %d class %v\n", i.Number, a.Alternate, a.Class)
				for _, e := range a.Endpoints {
					fmt.Printf("    %v %v maxpkt=%d\n", e.Address, e.TransferType, e.MaxPacketSize)
				}
			}
		}
	}
}
