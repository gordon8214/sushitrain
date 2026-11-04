// Copyright (C) 2026 Gordon
// SPDX-License-Identifier: MPL-2.0
package history

import "t-shaped.nl/sushitrain/v2/winplatform"

func secureState(path string) error { return winplatform.PrivateDirectory(path) }
