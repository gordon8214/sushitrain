// Copyright (C) 2026 Gordon
// SPDX-License-Identifier: MPL-2.0
package safecopy

import (
	"golang.org/x/sys/unix"
	"os"
	"path/filepath"
	"testing"
)

func TestExportCannotPublishReplacedTemporaryName(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "export")
	dest, err := Create(path)
	if err != nil {
		t.Fatal(err)
	}
	defer dest.Close()
	dest.File.WriteString("selected content")
	if err = os.Remove(filepath.Join(dir, dest.temporary)); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(dir, dest.temporary), []byte("replacement"), 0600)
	err = dest.Commit()
	if err == nil {
		bytes, e := os.ReadFile(path)
		if e != nil || string(bytes) != "selected content" {
			t.Fatal("published another inode", string(bytes), e)
		}
	}
	bytes, e := os.ReadFile(filepath.Join(dir, dest.temporary))
	if e != nil || string(bytes) != "replacement" {
		t.Fatal("removed another writer's file", e)
	}
}
func TestRegularSourceRejectsFIFOWithoutBlocking(t *testing.T) {
	path := filepath.Join(t.TempDir(), "fifo")
	if err := unix.Mkfifo(path, 0600); err != nil {
		t.Fatal(err)
	}
	if file, err := OpenRegular(path); err == nil {
		file.Close()
		t.Fatal("FIFO accepted")
	}
}
