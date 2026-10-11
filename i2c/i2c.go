// SPDX-License-Identifier: GPL-2.0-or-later
// Copyright (C) 2026 Matt Rasband

// Package i2c defines the minimal bus abstraction shared by the chip drivers.
package i2c

// Msg mirrors struct i2c_msg. Addr is the 7-bit address.
type Msg struct {
	Addr byte
	Read bool
	Buf  []byte
}

// Bus performs a combined transaction: every message except the last is sent
// without a stop condition (repeated start).
type Bus interface {
	Transfer(msgs ...Msg) error
}
