// Copyright (C) 2026 Gordon
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this file,
// You can obtain one at https://mozilla.org/MPL/2.0/.

package main

import (
	"io"
	"os"
	"strings"

	"github.com/vitrun/qart/qr"
)

// printQR draws a QR code in a terminal with half-block characters, two modules per character cell vertically,
// with the quiet zone scanners need around it
func printQR(w io.Writer, text string) error {
	code, err := qr.Encode(text, qr.M)
	if err != nil {
		return err
	}
	const quiet = 2
	size := code.Size + 2*quiet
	black := func(x, y int) bool { return code.Black(x-quiet, y-quiet) }
	var b strings.Builder
	for y := 0; y < size; y += 2 {
		for x := 0; x < size; x++ {
			top, bottom := black(x, y), y+1 < size && black(x, y+1)
			// Light modules are drawn, so the code reads the same on dark and light terminals
			switch {
			case !top && !bottom:
				b.WriteRune('█')
			case !top:
				b.WriteRune('▀')
			case !bottom:
				b.WriteRune('▄')
			default:
				b.WriteRune(' ')
			}
		}
		b.WriteByte('\n')
	}
	_, err = io.WriteString(w, b.String())
	return err
}

// writeQRPNG writes the code as a PNG image that only the owner can read: it holds the pairing secret
func writeQRPNG(path, text string) error {
	code, err := qr.Encode(text, qr.M)
	if err != nil {
		return err
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	// An existing file keeps its mode when it is opened: restrict it before writing the secret
	if err := f.Chmod(0o600); err != nil {
		f.Close()
		return err
	}
	if _, err := f.Write(code.PNG()); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}
