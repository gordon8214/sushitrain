// Copyright (C) 2026 Gordon
// SPDX-License-Identifier: MPL-2.0
package history

import (
	"golang.org/x/sys/windows"
	"path/filepath"
	"strings"
	"t-shaped.nl/sushitrain/v2/winplatform"
	"testing"
)

func TestHistoryStateHasPrivateWindowsACL(t *testing.T) {
	base := t.TempDir()
	state := filepath.Join(base, "history")
	s, err := Open(state, filepath.Join(base, "cache"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	sd, err := windows.GetNamedSecurityInfo(state, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION)
	if err != nil {
		t.Fatal(err)
	}
	acl, _, err := sd.DACL()
	if err != nil {
		t.Fatal(err)
	}
	sid, err := winplatform.UserSID()
	if err != nil {
		t.Fatal(err)
	}
	text := sd.String()
	if acl.AceCount != 2 || !strings.Contains(text, "D:P") || !strings.Contains(text, sid) || !strings.Contains(text, ";;;SY)") {
		t.Fatalf("history ACL grants unexpected access: %s", text)
	}
}
