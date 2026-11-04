// Copyright (C) 2026 Gordon
// SPDX-License-Identifier: MPL-2.0
package sushitrain

import (
	"os"
	"path/filepath"
	"testing"
)

func TestDesktopImportPublishesHeldContent(t *testing.T) {
	dir := t.TempDir()
	root, err := os.OpenRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	output, err := root.OpenFile("temporary", os.O_CREATE|os.O_EXCL|os.O_RDWR, 0600)
	if err != nil {
		t.Fatal(err)
	}
	defer output.Close()
	if _, err = output.WriteString("selected input"); err != nil {
		t.Fatal(err)
	}
	if err = root.Rename("temporary", "held-original"); err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(dir, "temporary"), []byte("another writer"), 0600); err != nil {
		t.Fatal(err)
	}
	if err = publishDesktopImport(root, output, "imported"); err != nil {
		t.Fatal(err)
	}
	if data, err := os.ReadFile(filepath.Join(dir, "imported")); err != nil || string(data) != "selected input" {
		t.Fatal("import published a substituted temporary file", string(data), err)
	}
	if data, err := os.ReadFile(filepath.Join(dir, "temporary")); err != nil || string(data) != "another writer" {
		t.Fatal("import changed another writer's content", string(data), err)
	}
}
