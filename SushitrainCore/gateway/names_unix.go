// Copyright (C) 2026 Gordon
// SPDX-License-Identifier: MPL-2.0

//go:build !windows

package gateway

func platformNameValid(string) bool { return true }
