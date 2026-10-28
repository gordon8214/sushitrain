// Copyright (C) 2026 Gordon
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this file,
// You can obtain one at https://mozilla.org/MPL/2.0/.

package main

import (
	"fmt"
	"log/slog"
	"os"
	"strings"

	"github.com/syncthing/syncthing/lib/syncthing"
)

// The engine installs its own log handler as slog's default when its packages load: it feeds the web GUI's log and
// error notices, and applies a level per package. The gateway logs through it (it never replaces the default).

// configureLogging applies STLOGLEVEL (the level of all packages) and STTRACE (levels of single packages, such as
// "model,protocol:WARN") as stock Syncthing does
func configureLogging() error {
	level := slog.LevelInfo
	if v := strings.TrimSpace(os.Getenv("STLOGLEVEL")); v != "" {
		if err := level.UnmarshalText([]byte(v)); err != nil {
			return fmt.Errorf("STLOGLEVEL: %w", err)
		}
	}
	syncthing.SetLogLevels(level, os.Getenv("STTRACE"))
	return nil
}
