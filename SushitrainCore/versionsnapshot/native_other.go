// Copyright (C) 2026 Gordon
// SPDX-License-Identifier: MPL-2.0
//go:build !darwin

package versionsnapshot

import (
	"errors"
	"github.com/syncthing/syncthing/lib/protocol"
	"time"
)

func timeFromNs(ns int64) time.Time { return time.Unix(0, ns) }
func ApplyAttributes(_ string, _ []protocol.Xattr) error {
	return errors.New("Mac snapshots must be materialized on macOS")
}
