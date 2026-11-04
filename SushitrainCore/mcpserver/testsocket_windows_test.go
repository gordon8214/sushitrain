// Copyright (C) 2026 Gordon
// SPDX-License-Identifier: MPL-2.0
package mcpserver

import (
	"github.com/google/uuid"
	"t-shaped.nl/sushitrain/v2/winplatform"
	"testing"
)

func testSocket(t *testing.T, _ string) string {
	t.Helper()
	sid, err := winplatform.UserSID()
	if err != nil {
		t.Fatal(err)
	}
	return `\\.\pipe\Synctrain-` + sid + "-MCPTest-" + uuid.NewString()
}
