// Copyright (C) 2026 Gordon
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this file,
// You can obtain one at https://mozilla.org/MPL/2.0/.

//go:build !windows

package sushitrain

import "syscall"

func addDescriptorLimits(infoJson map[string]any) {
	var rlimit syscall.Rlimit
	if err := syscall.Getrlimit(syscall.RLIMIT_NOFILE, &rlimit); err == nil {
		infoJson["currentFileDescriptorLimit"] = rlimit.Cur
		infoJson["maxFileDescriptorLimit"] = rlimit.Max
	}

}
