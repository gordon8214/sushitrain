// Copyright (C) 2026 Gordon. SPDX-License-Identifier: MPL-2.0
package fusefs

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"t-shaped.nl/sushitrain/v2/gateway/gatewaytest"
)

func TestKDEThumbnailWorkerDoesNotBlockFileCopies(t *testing.T) {
	for _, tc := range []struct {
		args      []string
		thumbnail bool
	}{
		{[]string{"kioworker", "/usr/lib64/qt6/plugins/kf6/kio/thumbnail.so", "thumbnail", "local:/a", "local:/b"}, true},
		{[]string{"kioworker", "/usr/lib64/qt6/plugins/kf6/kio/file.so", "file", "local:/a", "local:/b"}, false},
		{[]string{"kioworker", "file", "/home/gordon/thumbnail.jpg"}, false},
		{[]string{"kioworker"}, false},
	} {
		if got := thumbnailWorker(tc.args); got != tc.thumbnail {
			t.Errorf("%v: %v", tc.args, got)
		}
	}
}

func TestBalooCommandCannotHydrateRemoteContent(t *testing.T) {
	e := gatewaytest.NewEngine(t, "docs", nil)
	e.Write("remote.txt", "remote content")
	g := gatewaytest.NewGateway(t, e, "")
	g.Export(true)
	m := mountGateway(t, g)
	cat, err := exec.LookPath("cat")
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(cat)
	if err != nil {
		t.Fatal(err)
	}
	indexer := filepath.Join(t.TempDir(), "balooctl6")
	if err := os.WriteFile(indexer, data, 0700); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	if out, err := exec.CommandContext(ctx, indexer, m.path("docs/remote.txt")).CombinedOutput(); err == nil {
		t.Fatalf("indexer fetched remote content: %s", out)
	} else if exit := new(exec.ExitError); !errors.As(err, &exit) || exit.ExitCode() != 1 {
		t.Fatalf("indexer did not run to its read error: %s, %v", out, err)
	}
	storage, err := m.service.Storage()
	if err != nil || storage.DownloadedBytes != 0 {
		t.Fatalf("indexing hydrated content: %+v, %v", storage, err)
	}
	// An ordinary file operation still reads the same file successfully.
	if out, err := exec.CommandContext(ctx, cat, m.path("docs/remote.txt")).Output(); err != nil || string(out) != "remote content" {
		t.Fatalf("ordinary file operation: %q, %v", out, err)
	}
}
