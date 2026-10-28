// Copyright (C) 2026 Gordon
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this file,
// You can obtain one at https://mozilla.org/MPL/2.0/.

package gateway

import (
	"context"
	"net"
	"os"
	"path/filepath"
	"testing"
)

func TestAdminListenSocket(t *testing.T) {
	// Short, as Unix socket paths are limited in length
	dir, err := os.MkdirTemp("/tmp", "gwadm")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	path := filepath.Join(dir, "admin.sock")

	// Something that isn't a socket is not replaced
	if err := os.WriteFile(path, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := NewAdminServer().Listen(ctx, path, true); err == nil {
		t.Fatal("replaced a regular file")
	}
	os.Remove(path)

	first := NewAdminServer()
	if err := first.Listen(ctx, path, true); err != nil {
		t.Fatal(err)
	}
	info, err := os.Lstat(path)
	if err != nil || info.Mode().Type() != os.ModeSocket || info.Mode().Perm() != 0o660 {
		t.Fatalf("socket %v, %v", info, err)
	}
	conn, err := net.Dial("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	conn.Close()
	// A socket that is served is not taken over
	if err := NewAdminServer().Listen(ctx, path, true); err == nil {
		t.Fatal("took over a live socket")
	}
	first.Close()
	if _, err := os.Lstat(path); !os.IsNotExist(err) {
		t.Fatalf("socket left behind: %v", err)
	}
	entries, _ := os.ReadDir(dir)
	if len(entries) != 0 {
		t.Fatalf("left %v", entries)
	}

	// Owner only, unless the group may use it; a directory others could change entries of is refused
	owner := NewAdminServer()
	if err := owner.Listen(ctx, path, false); err != nil {
		t.Fatal(err)
	}
	if info, err := os.Lstat(path); err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("owner-only socket %v, %v", info, err)
	}
	owner.Close()
	if err := os.Chmod(dir, 0o770); err != nil {
		t.Fatal(err)
	}
	if err := NewAdminServer().Listen(ctx, path, false); err == nil {
		t.Fatal("listened in a directory its group can write, without group access")
	}
	if err := os.Chmod(dir, 0o777); err != nil {
		t.Fatal(err)
	}
	if err := NewAdminServer().Listen(ctx, path, true); err == nil {
		t.Fatal("listened in a directory everyone can write")
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		t.Fatal(err)
	}
}
