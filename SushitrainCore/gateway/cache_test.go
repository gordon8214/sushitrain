// Copyright (C) 2026 Gordon
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this file,
// You can obtain one at https://mozilla.org/MPL/2.0/.

package gateway

import (
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"testing"

	"github.com/syncthing/syncthing/lib/protocol"
)

func TestCacheDirHoldsTheSpoolAndThumbnails(t *testing.T) {
	home := t.TempDir()
	cache := filepath.Join(t.TempDir(), "RemoteFileService")
	// What a service that kept its caches in its home left there
	for _, name := range []string{"content", "thumbnails"} {
		if err := os.MkdirAll(filepath.Join(home, name), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(home, name, "old"), []byte("old"), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	s, err := New(Config{
		Home: home, CacheDir: cache, ListenAddress: "127.0.0.1:0",
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })

	if s.content.dir != filepath.Join(cache, "content") {
		t.Errorf("spool in %s", s.content.dir)
	}
	if s.thumbnailDir() != filepath.Join(cache, "thumbnails") {
		t.Errorf("thumbnails in %s", s.thumbnailDir())
	}
	for _, name := range []string{"content", "thumbnails"} {
		if _, err := os.Stat(filepath.Join(home, name)); !os.IsNotExist(err) {
			t.Errorf("%s left in the home directory: %v", name, err)
		}
	}
	if _, err := os.Stat(filepath.Join(home, "service.sqlite")); err != nil {
		t.Errorf("database not in the home directory: %v", err)
	}

	// The host may clear its caches while the service runs
	if err := os.RemoveAll(cache); err != nil {
		t.Fatal(err)
	}
	rep := &representation{m: s.content, spoolPath: filepath.Join(s.content.dir, "spool"),
		file: protocol.FileInfo{Size: 4}}
	if err := rep.open(); err != nil {
		t.Fatalf("spool after the cache was cleared: %v", err)
	}
	rep.spool.Close()
}

func TestCacheDirDefaultsToHome(t *testing.T) {
	home := t.TempDir()
	s, err := New(Config{Home: home, ListenAddress: "127.0.0.1:0",
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil))}, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	if s.content.dir != filepath.Join(home, "content") || s.thumbnailDir() != filepath.Join(home, "thumbnails") {
		t.Errorf("caches in %s and %s", s.content.dir, s.thumbnailDir())
	}
}
