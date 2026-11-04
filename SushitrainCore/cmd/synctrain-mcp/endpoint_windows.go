// Copyright (C) 2026 Gordon
// SPDX-License-Identifier: MPL-2.0
package main

import (
	"context"
	"os"
	"t-shaped.nl/sushitrain/v2/winplatform"
)

func defaultEndpoint(context.Context) (string, error) {
	if p := os.Getenv("SYNCTRAIN_MCP_SOCKET"); p != "" {
		return p, nil
	}
	sid, err := winplatform.UserSID()
	return `\\.\pipe\Synctrain-` + sid, err
}
