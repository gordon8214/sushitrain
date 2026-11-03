// Copyright (C) 2026 Gordon
// SPDX-License-Identifier: MPL-2.0

package gateway

import "t-shaped.nl/sushitrain/v2/winplatform"

func makePrivate(path string) error { return winplatform.PrivateDirectory(path) }
