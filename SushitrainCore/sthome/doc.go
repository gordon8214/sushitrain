// Copyright (C) 2026 Gordon
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this file,
// You can obtain one at https://mozilla.org/MPL/2.0/.

// Package sthome reads and checks a Syncthing home (its identity, configuration and database) for the app and for
// synctrain-gateway. It never replaces an identity or a configuration that exists but can't be used; it only creates
// them when they are missing, and migrates a configuration from an older Syncthing after archiving it.
package sthome
