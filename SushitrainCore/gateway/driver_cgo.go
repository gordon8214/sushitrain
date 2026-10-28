// Copyright (C) 2026 Gordon
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this file,
// You can obtain one at https://mozilla.org/MPL/2.0/.

//go:build cgo

package gateway

import (
	_ "github.com/mattn/go-sqlite3" // the driver Syncthing uses with cgo
)

const (
	driverName = "sqlite3"
	// Full synchronization: an operation receipt must survive a power loss once reported
	driverOptions = "_fk=true&_sync=2&_journal_mode=WAL&_busy_timeout=10000&_txlock=immediate"
)
