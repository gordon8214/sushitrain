// Copyright (C) 2026 Gordon
// SPDX-License-Identifier: MPL-2.0

package gateway

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestWindowsHandleOperations(t *testing.T) {
	path := t.TempDir()
	d, err := openRoot(path)
	if err != nil {
		t.Fatal("root", err)
	}
	defer d.close()
	child, err := d.openDir("")
	if err != nil {
		t.Fatal("duplicate", err)
	}
	defer child.close()
	f, err := child.createExclusive("staged", 0644)
	if err != nil {
		t.Fatal("create", err)
	}
	if _, err = f.WriteString("saved"); err != nil {
		t.Fatal("write", err)
	}
	if err = f.Sync(); err != nil {
		t.Fatal("flush file", err)
	}
	if err = f.Close(); err != nil {
		t.Fatal("close", err)
	}
	before, err := child.lstat("staged")
	if err != nil {
		t.Fatal("stat", err)
	}
	if err = renameNoReplace(child, "staged", child, "published"); err != nil {
		t.Fatal("rename", err)
	}
	after, err := child.lstat("published")
	if err != nil || before.id != after.id {
		t.Fatal("identity", before, after, err)
	}
	if err = child.chtimes("published", time.Now()); err != nil {
		t.Fatal("time", err)
	}
	if err = child.sync(); err != nil {
		t.Fatal("durable directory", err)
	}
	if err = child.remove("published"); err != nil {
		t.Fatal("remove", err)
	}
	if _, err = os.Stat(filepath.Join(path, "published")); !os.IsNotExist(err) {
		t.Fatal("removed file", err)
	}
}

func TestWindowsExclusiveRenamePreservesTarget(t *testing.T) {
	path := t.TempDir()
	d, err := openRoot(path)
	if err != nil {
		t.Fatal(err)
	}
	defer d.close()
	for _, name := range []string{"source", "target"} {
		f, err := d.createExclusive(name, 0600)
		if err != nil {
			t.Fatal(err)
		}
		_, err = f.WriteString(name)
		f.Close()
		if err != nil {
			t.Fatal(err)
		}
	}
	if err := renameNoReplace(d, "source", d, "target"); !isExists(err) {
		t.Fatalf("expected existing destination, got %v", err)
	}
	for _, name := range []string{"source", "target"} {
		data, err := os.ReadFile(filepath.Join(path, name))
		if err != nil || string(data) != name {
			t.Fatalf("%s: %s %v", name, data, err)
		}
	}
}

func TestWindowsReplaceRecovery(t *testing.T) {
	path := t.TempDir()
	d, err := openRoot(path)
	if err != nil {
		t.Fatal(err)
	}
	defer d.close()
	for _, name := range []string{"incoming", "live"} {
		f, err := d.createExclusive(name, 0600)
		if err != nil {
			t.Fatal(err)
		}
		_, err = f.WriteString(name)
		f.Close()
		if err != nil {
			t.Fatal(err)
		}
	}
	before, err := d.lstat("incoming")
	if err != nil {
		t.Fatal(err)
	}
	if err := replaceWindows(d, "incoming", d, "live"); err != nil {
		t.Fatal(err)
	}
	// A crash here leaves the new file published and its predecessor at the recovery path.
	after, err := d.lstat("live")
	if err != nil || before.id != after.id {
		t.Fatal("published identity", before, after, err)
	}
	if err := recoverExchange(d, "incoming"); err != nil {
		t.Fatal(err)
	}
	for name, want := range map[string]string{"live": "incoming", "incoming": "live"} {
		data, err := os.ReadFile(filepath.Join(path, name))
		if err != nil || string(data) != want {
			t.Fatalf("%s: %s %v", name, data, err)
		}
	}
}

func TestWindowsPartialReplaceFailureRecovery(t *testing.T) {
	path := t.TempDir()
	d, err := openRoot(path)
	if err != nil {
		t.Fatal(err)
	}
	defer d.close()
	for name, data := range map[string]string{"incoming": "new", "live": "old"} {
		f, err := d.createExclusive(name, 0600)
		if err != nil {
			t.Fatal(err)
		}
		_, err = f.WriteString(data)
		f.Close()
		if err != nil {
			t.Fatal(err)
		}
	}
	st, err := d.lstat("incoming")
	if err != nil {
		t.Fatal(err)
	}
	// This is ReplaceFileW's documented ERROR_UNABLE_TO_MOVE_REPLACEMENT_2 state.
	if err := renameNoReplace(d, "live", d, "incoming.displaced"); err != nil {
		t.Fatal(err)
	}
	if err := repairExchange(d, "incoming", "live", st.id.dev, st.id.ino); err != nil {
		t.Fatal(err)
	}
	for name, want := range map[string]string{"incoming": "new", "live": "old"} {
		data, err := os.ReadFile(filepath.Join(path, name))
		if err != nil || string(data) != want {
			t.Fatalf("%s: %s %v", name, data, err)
		}
	}
}

func TestWindowsMetadataDetection(t *testing.T) {
	path := t.TempDir()
	file := filepath.Join(path, "file")
	if err := os.WriteFile(file, []byte("body"), 0600); err != nil {
		t.Fatal(err)
	}
	d, err := openRoot(path)
	if err != nil {
		t.Fatal(err)
	}
	defer d.close()
	if d.hasXattrs("file") {
		t.Fatal("plain file reports metadata")
	}
	if err := os.WriteFile(file+":metadata", []byte("retain me"), 0600); err != nil {
		t.Fatal(err)
	}
	if !d.hasXattrs("file") {
		t.Fatal("alternate stream was not detected")
	}
	if err := renameNoReplace(d, "file", d, "moved"); err != nil {
		t.Fatal(err)
	}
	if data, err := os.ReadFile(filepath.Join(path, "moved:metadata")); err != nil || string(data) != "retain me" {
		t.Fatalf("stream lost on rename: %q %v", data, err)
	}
	if !d.hasXattrs("missing") {
		t.Fatal("failed inspection must preserve metadata")
	}
}
