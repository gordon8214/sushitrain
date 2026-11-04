// Copyright (C) 2026 Gordon
// SPDX-License-Identifier: MPL-2.0
package safecopy

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestPublishNeverReplacesAnotherFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "export")
	dest, err := Create(path)
	if err != nil {
		t.Fatal(err)
	}
	defer dest.Close()
	if _, err = dest.File.WriteString("downloaded version"); err != nil {
		t.Fatal(err)
	}
	if _, err = os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("partial export became visible", err)
	}
	if err = os.WriteFile(path, []byte("another writer"), 0600); err != nil {
		t.Fatal(err)
	}
	if err = dest.Commit(); !errors.Is(err, os.ErrExist) {
		t.Fatal("commit did not refuse another writer", err)
	}
	data, _ := os.ReadFile(path)
	if string(data) != "another writer" {
		t.Fatal("existing content replaced")
	}
	if _, err = Create(path); !errors.Is(err, os.ErrExist) {
		t.Fatal("existing export accepted", err)
	}
}
func TestHeldParentCannotBeRedirected(t *testing.T) {
	dir := t.TempDir()
	parent := filepath.Join(dir, "destination")
	moved := filepath.Join(dir, "held")
	os.Mkdir(parent, 0700)
	dest, err := Create(filepath.Join(parent, "export"))
	if err != nil {
		t.Fatal(err)
	}
	defer dest.Close()
	dest.File.WriteString("safe")
	if err = os.Rename(parent, moved); err != nil {
		t.Fatal(err)
	}
	os.Mkdir(parent, 0700)
	if err = dest.Commit(); err != nil {
		t.Fatal(err)
	}
	if _, err = os.Stat(filepath.Join(parent, "export")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("export followed replacement parent", err)
	}
	data, err := os.ReadFile(filepath.Join(moved, "export"))
	if err != nil || string(data) != "safe" {
		t.Fatal(string(data), err)
	}
}
func TestRejectSymlinkIntoMountedFolders(t *testing.T) {
	dir := t.TempDir()
	mount := filepath.Join(dir, "mount")
	alias := filepath.Join(dir, "alias")
	os.Mkdir(mount, 0700)
	if err := os.Symlink(mount, alias); err != nil {
		t.Fatal(err)
	}
	if d, err := Create(filepath.Join(alias, "export"), mount); err == nil {
		d.Close()
		t.Fatal("export followed alias into mounted folders")
	}
}
func TestCanceledCopyDoesNotPublish(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "export")
	dest, err := Create(path)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err = Copy(ctx, dest.File, bytes.NewBufferString("content")); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	dest.Close()
	entries, err := os.ReadDir(dir)
	if err != nil || len(entries) != 0 {
		t.Fatal("canceled export left content", entries, err)
	}
}
