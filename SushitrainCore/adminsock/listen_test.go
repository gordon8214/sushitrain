// Copyright (C) 2026 Gordon
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this file,
// You can obtain one at https://mozilla.org/MPL/2.0/.

package adminsock

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"os"
	"path/filepath"
	"testing"
)

func TestListenSocket(t *testing.T) {
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
	if err := NewServer().Listen(ctx, path, true); err == nil {
		t.Fatal("replaced a regular file")
	}
	os.Remove(path)

	first := NewServer()
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
	if err := NewServer().Listen(ctx, path, true); err == nil {
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
	owner := NewServer()
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
	if err := NewServer().Listen(ctx, path, false); err == nil {
		t.Fatal("listened in a directory its group can write, without group access")
	}
	if err := os.Chmod(dir, 0o777); err != nil {
		t.Fatal(err)
	}
	if err := NewServer().Listen(ctx, path, true); err == nil {
		t.Fatal("listened in a directory everyone can write")
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		t.Fatal(err)
	}
}

func TestCall(t *testing.T) {
	dir, err := os.MkdirTemp("/tmp", "gwadm")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	path := filepath.Join(dir, "admin.sock")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	server := NewServer()
	server.Register("echo", func(ctx context.Context, params json.RawMessage) (any, error) {
		var p struct{ Text string }
		if err := json.Unmarshal(params, &p); err != nil {
			return nil, err
		}
		if p.Text == "" {
			return nil, errors.New("nothing to echo")
		}
		return map[string]string{"text": p.Text}, nil
	})
	if err := server.Listen(ctx, path, false); err != nil {
		t.Fatal(err)
	}
	defer server.Close()
	var out struct{ Text string }
	if err := Call(path, "echo", map[string]string{"text": "hi"}, &out); err != nil || out.Text != "hi" {
		t.Fatalf("echo: %v, %+v", err, out)
	}
	if err := Call(path, "echo", map[string]string{}, nil); err == nil || err.Error() != "nothing to echo" {
		t.Fatalf("method error came back as %v", err)
	}
	if err := Call(path, "missing", nil, nil); err == nil {
		t.Fatal("an unknown method succeeded")
	}
}
