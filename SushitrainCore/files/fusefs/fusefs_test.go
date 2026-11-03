// Copyright (C) 2026 Gordon
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this file,
// You can obtain one at https://mozilla.org/MPL/2.0/.

//go:build linux

package fusefs

import (
	"bytes"
	"context"
	"crypto/rand"
	"errors"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"golang.org/x/sys/unix"
	"t-shaped.nl/sushitrain/v2/files"
	"t-shaped.nl/sushitrain/v2/gateway/gatewaytest"
)

type mounted struct {
	t       *testing.T
	service *files.Service
	g       *gatewaytest.Gateway
	folder  files.Folder
	dir     string
}

// mountGateway mounts a new service with the gateway's folder added
func mountGateway(t *testing.T, g *gatewaytest.Gateway) *mounted {
	t.Helper()
	if _, err := os.Stat("/dev/fuse"); err != nil {
		t.Skip("no /dev/fuse")
	}
	if _, err := exec.LookPath("fusermount3"); err != nil {
		t.Skip("no fusermount3")
	}
	dir := t.TempDir()
	cfg := testConfig(dir)
	logger := cfg.Logger
	s, err := files.Open(cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	if err := s.Start(); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	done := make(chan error, 1)
	var gw files.Gateway
	go func() {
		var err error
		gw, err = s.Pair(ctx, g.PairingLink(), nil)
		done <- err
	}()
	g.ApproveNext(map[string]string{g.Engine.FolderID: "write"})
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	folder, err := s.AddFolder(ctx, gw.ServiceID, g.Engine.FolderID)
	if err != nil {
		t.Fatal(err)
	}
	m := &mounted{t: t, service: s, g: g, folder: folder, dir: filepath.Join(dir, "mnt")}
	m.refresh()
	mount, err := MountService(m.dir, s, Options{Logger: logger})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := mount.Unmount(); err != nil {
			t.Errorf("unmounting: %v", err)
			_ = exec.Command("fusermount3", "-uz", m.dir).Run()
		}
	})
	return m
}

func (m *mounted) refresh() {
	m.t.Helper()
	m.g.Sync()
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	if err := m.service.Refresh(ctx, m.folder.ID); err != nil {
		m.t.Fatal(err)
	}
}

func (m *mounted) path(name string) string {
	return filepath.Join(m.dir, name)
}

func xattr(t *testing.T, path, name string) string {
	t.Helper()
	buf := make([]byte, 64)
	n, err := unix.Getxattr(path, name, buf)
	if err != nil {
		t.Fatalf("getxattr %s %s: %v", path, name, err)
	}
	return string(buf[:n])
}

func TestMount(t *testing.T) {
	big := make([]byte, 3<<20)
	_, _ = rand.Read(big)
	e := gatewaytest.NewEngine(t, "docs", nil)
	e.Write("readme.txt", "hello")
	e.Write("sub/a.txt", "a")
	e.Write("big.bin", string(big))
	g := gatewaytest.NewGateway(t, e, "")
	g.Export(true)
	m := mountGateway(t, g)

	entries, err := os.ReadDir(m.dir)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, entry := range entries {
		names = append(names, entry.Name())
	}
	if !slices.Equal(names, []string{".trackerignore", "docs"}) {
		t.Fatalf("the mount's root has %v", names)
	}
	if data, err := os.ReadFile(m.path("docs/readme.txt")); err != nil || string(data) != "hello" {
		t.Fatalf("readme: %q, %v", data, err)
	}
	info, err := os.Stat(m.path("docs/big.bin"))
	if err != nil || info.Size() != int64(len(big)) || info.Mode().Perm() != 0o644 {
		t.Fatalf("big.bin: %v, %v", info, err)
	}
	if got := xattr(t, m.path("docs/big.bin"), xattrState); got != "remote" {
		t.Fatalf("big.bin before reading is %s", got)
	}

	// A read far into a file fetches what it needs
	f, err := os.Open(m.path("docs/big.bin"))
	if err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 4096)
	off := int64(len(big) - 10000)
	if n, err := f.ReadAt(buf, off); err != nil || n != len(buf) || !bytes.Equal(buf, big[off:off+4096]) {
		t.Fatalf("read at %d: %d, %v", off, n, err)
	}
	f.Close()
	if data, err := os.ReadFile(m.path("docs/big.bin")); err != nil || !bytes.Equal(data, big) {
		t.Fatalf("the whole file: %v", err)
	}


	// Changes on the gateway show without remounting, past what the kernel cached
	if _, err := os.Stat(m.path("docs/sub/b.txt")); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("b.txt before it exists: %v", err)
	}
	old, err := os.Open(m.path("docs/readme.txt"))
	if err != nil {
		t.Fatal(err)
	}
	defer old.Close()
	e.Write("readme.txt", "changed on the gateway")
	e.Write("sub/b.txt", "b")
	m.refresh()
	if data, err := os.ReadFile(m.path("docs/readme.txt")); err != nil || string(data) != "changed on the gateway" {
		t.Fatalf("readme after a change: %q, %v", data, err)
	}
	if data, err := os.ReadFile(m.path("docs/sub/b.txt")); err != nil || string(data) != "b" {
		t.Fatalf("a new file: %q, %v", data, err)
	}
	// The handle opened before the change still reads what it opened
	got := make([]byte, 5)
	if _, err := old.ReadAt(got, 0); err != nil || string(got) != "hello" {
		t.Fatalf("the old handle reads %q, %v", got, err)
	}

	// Keep on This Device, by extended attribute
	if err := unix.Setxattr(m.path("docs/sub"), xattrKeep, []byte("1"), 0); err != nil {
		t.Fatal(err)
	}
	gatewaytest.WaitFor(t, 30*time.Second, "sub to be kept", func() bool {
		return xattr(t, m.path("docs/sub/a.txt"), xattrState) == "kept"
	})
	if got := xattr(t, m.path("docs/sub"), xattrKeep); got != "1" {
		t.Fatalf("keep on sub is %s", got)
	}

	// A folder removed from the mount goes from it
	if _, _, err := m.service.RemoveFolder(m.folder.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(m.path("docs")); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("the removed folder: %v", err)
	}
}
