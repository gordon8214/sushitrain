// Copyright (C) 2026 Gordon
// SPDX-License-Identifier: MPL-2.0
package sushitrain

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestNativeTrashRetainsSourceDirectory(t *testing.T) {
	data := t.TempDir()
	t.Setenv("XDG_DATA_HOME", data)
	parent := filepath.Join(t.TempDir(), "source")
	os.Mkdir(parent, 0700)
	os.WriteFile(filepath.Join(parent, "report.txt"), []byte("selected"), 0600)
	root, err := os.OpenRoot(parent)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	moved := parent + "-moved"
	if err = os.Rename(parent, moved); err != nil {
		t.Fatal(err)
	}
	os.Mkdir(parent, 0700)
	os.WriteFile(filepath.Join(parent, "report.txt"), []byte("replacement"), 0600)
	if err = trashFromRoot(context.Background(), root, "report.txt"); err != nil {
		t.Fatal(err)
	}
	bytes, err := os.ReadFile(filepath.Join(parent, "report.txt"))
	if err != nil || string(bytes) != "replacement" {
		t.Fatal("Trash followed replacement root", err)
	}
	list, err := os.ReadDir(filepath.Join(data, "Trash", "files"))
	if err != nil || len(list) != 1 {
		t.Fatal(list, err)
	}
	bytes, err = os.ReadFile(filepath.Join(data, "Trash", "files", list[0].Name()))
	if err != nil || string(bytes) != "selected" {
		t.Fatal(string(bytes), err)
	}
	bytes, err = os.ReadFile(filepath.Join(data, "Trash", "info", list[0].Name()+".trashinfo"))
	if err != nil || !strings.Contains(string(bytes), "Path="+moved+"/report.txt") {
		t.Fatal(string(bytes), err)
	}
}
func TestNativeTrashRejectsSymlinkRoot(t *testing.T) {
	parent := t.TempDir()
	outside := t.TempDir()
	os.Symlink(outside, filepath.Join(parent, "Trash"))
	if root, err := privateTrash(filepath.Join(parent, "Trash")); err == nil {
		root.Close()
		t.Fatal("Trash followed root symlink")
	}
	list, err := os.ReadDir(outside)
	if err != nil || len(list) != 0 {
		t.Fatal("Trash wrote through symlink", err)
	}
}

func TestNativeTrashFindsMountRoot(t *testing.T) {
	parent, err := os.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer parent.Close()
	root, err := mountRoot(parent)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	identity, err := mountIdentity(parent)
	if err != nil {
		t.Fatal(err)
	}
	topIdentity, err := mountIdentity(root)
	if err != nil || identity != topIdentity {
		t.Fatal("Trash left source mount", identity, topIdentity, err)
	}
}
