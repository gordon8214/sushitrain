// Copyright (C) 2026 Gordon
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this file,
// You can obtain one at https://mozilla.org/MPL/2.0/.

//go:build !windows

package sushitrain

import (
	"path"
	"syscall"
)

// onSameVolume reports whether to (or its nearest existing parent) is on the volume of from, so that a rename can move
// one to the other
func onSameVolume(from string, to string) bool {
	var source syscall.Stat_t
	if err := syscall.Stat(from, &source); err != nil {
		return false
	}
	for dir := path.Dir(to); ; dir = path.Dir(dir) {
		var target syscall.Stat_t
		if err := syscall.Stat(dir, &target); err == nil {
			return source.Dev == target.Dev
		}
		if dir == "/" {
			return false
		}
	}
}
