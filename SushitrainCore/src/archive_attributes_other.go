// Copyright (C) 2026 Gordon
// SPDX-License-Identifier: MPL-2.0
//go:build !darwin

package sushitrain

import "github.com/syncthing/syncthing/lib/fs"

func (fld *Folder) archiveAttributes(file fs.File) (func(string) error, error) { return nil, nil }
