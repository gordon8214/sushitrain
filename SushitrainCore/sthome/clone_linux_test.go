// Copyright (C) 2026 Gordon
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this file,
// You can obtain one at https://mozilla.org/MPL/2.0/.

package sthome

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// Run with TMPDIR on btrfs or XFS to clone; elsewhere a failed clone must leave nothing for the copy to trip over
func TestCloneFile(t *testing.T) {
	dir := t.TempDir()
	from, to := filepath.Join(dir, "from"), filepath.Join(dir, "to")
	if err := os.WriteFile(from, []byte("the database"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := cloneFile(from, to); err != nil {
		if _, statErr := os.Stat(to); !errors.Is(statErr, os.ErrNotExist) {
			t.Fatalf("a clone that failed (%v) left a file", err)
		}
		t.Skipf("no reflinks in %s: %v", dir, err)
	}
	if data, err := os.ReadFile(to); err != nil || string(data) != "the database" {
		t.Fatalf("%q %v", data, err)
	}
	if err := cloneFile(from, to); err == nil {
		t.Fatal("cloned over an existing file")
	}
}
