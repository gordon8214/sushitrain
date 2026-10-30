// Copyright (C) 2026 Gordon
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this file,
// You can obtain one at https://mozilla.org/MPL/2.0/.

//go:build cgo

package sthome

import (
	_ "github.com/mattn/go-sqlite3" // the driver Syncthing uses with cgo
)

// The database/sql driver Syncthing uses in this build
const sqliteDriver = "sqlite3"
