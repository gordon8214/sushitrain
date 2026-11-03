// Copyright (C) 2026 Gordon
// SPDX-License-Identifier: MPL-2.0

package sushitrain

import (
	"os"
	"path/filepath"
	"t-shaped.nl/sushitrain/v2/winplatform"
)

func onSameVolume(from, to string) bool {
	f, err := os.Open(from)
	if err != nil {
		return false
	}
	defer f.Close()
	source, err := winplatform.Information(f)
	if err != nil {
		return false
	}
	for dir := filepath.Dir(to); ; dir = filepath.Dir(dir) {
		if target, err := os.Open(dir); err == nil {
			info, err := winplatform.Information(target)
			target.Close()
			return err == nil && source.VolumeSerialNumber == info.VolumeSerialNumber
		}
		if filepath.Dir(dir) == dir {
			return false
		}
	}
}
