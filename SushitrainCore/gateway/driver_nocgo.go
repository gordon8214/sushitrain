// Copyright (C) 2026 Gordon
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this file,
// You can obtain one at https://mozilla.org/MPL/2.0/.

//go:build !cgo

package gateway

import (
	_ "modernc.org/sqlite" // the driver Syncthing uses without cgo
)

const (
	driverName = "sqlite"
	// Full synchronization: an operation receipt must survive a power loss once reported
	driverOptions = "_pragma=foreign_keys(1)&_pragma=synchronous(2)&_pragma=journal_mode(WAL)&_pragma=busy_timeout(10000)&_txlock=immediate"
)
