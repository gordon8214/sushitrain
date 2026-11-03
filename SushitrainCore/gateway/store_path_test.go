// Copyright (C) 2026 Gordon
// SPDX-License-Identifier: MPL-2.0

package gateway

import (
	"os"
	"path/filepath"
	"testing"
)

func TestRelativeStorePath(t *testing.T) {
	t.Chdir(t.TempDir())
	if err := os.Mkdir("state", 0700); err != nil {
		t.Fatal(err)
	}
	s, err := openStore(filepath.Join("state", "service.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.db.Close()
	if _, err := os.Stat(filepath.Join("state", "service.sqlite")); err != nil {
		t.Fatalf("database was not created in the requested relative home: %v", err)
	}
}
