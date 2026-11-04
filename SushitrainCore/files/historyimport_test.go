// Copyright (C) 2026 Gordon
// SPDX-License-Identifier: MPL-2.0
package files

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/syncthing/syncthing/lib/config"
	"t-shaped.nl/sushitrain/v2/gateway/gatewaytest"
	"t-shaped.nl/sushitrain/v2/previewcache"
)

func TestSelectedHistoryRestoreAndCopy(t *testing.T) {
	e := gatewaytest.NewEngine(t, "docs", func(f *config.FolderConfiguration) {
		f.Versioning = config.VersioningConfiguration{Type: "trashcan", Params: map[string]string{"cleanoutDays": "0"}}
	})
	e.Write("doc.txt", "first")
	g := gatewaytest.NewGateway(t, e, "")
	g.Export(true)
	ts := newTestService(t, g, 0)
	ts.write(ts.mustLookup("docs/doc.txt"), "second", true)
	ts.settled()
	doc := ts.mustLookup("docs/doc.txt")
	list, err := ts.HistoryVersions(t.Context(), doc.Item)
	if err != nil || len(list.Versions) != 1 {
		t.Fatal(list, err)
	}
	selected := list.Versions[0]
	copy, err := ts.CopyHistoryVersion(t.Context(), doc.Item, selected.ID, selected.EntityTag)
	if err != nil {
		t.Fatal(err)
	}
	ts.settled()
	ts.gatewayHas(copy.Name, "first")
	archive := e.Path(".stversions/doc.txt")
	info, err := os.Stat(archive)
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(archive, []byte("different archived content"), 0600); err != nil {
		t.Fatal(err)
	}
	// Keep the timestamp-based version ID identical while its strong identity changes.
	if err = os.Chtimes(archive, info.ModTime(), info.ModTime()); err != nil {
		t.Fatal(err)
	}
	if _, err = ts.RestoreHistoryVersion(t.Context(), doc.Item, selected.ID, selected.EntityTag); err == nil {
		t.Fatal("restore accepted an archive changed after selection")
	}
	if _, err = ts.CopyHistoryVersion(t.Context(), doc.Item, selected.ID, selected.EntityTag); err == nil {
		t.Fatal("copy accepted an archive changed after selection")
	}
	if got := ts.readPath("docs/doc.txt"); got != "second" {
		t.Fatal("stale restore changed the destination", got)
	}
	list, err = ts.HistoryVersions(t.Context(), doc.Item)
	if err != nil || len(list.Versions) != 1 {
		t.Fatal(list, err)
	}
	fresh := list.Versions[0]
	if _, err = ts.RestoreHistoryVersion(t.Context(), doc.Item, fresh.ID, fresh.EntityTag); err != nil {
		t.Fatal(err)
	}
	ts.settled()
	ts.gatewayHas("doc.txt", "different archived content")
}

func TestPreviewRejectsLargerWorkingCopy(t *testing.T) {
	g := writableGateway(t, map[string]string{"document.txt": "small"})
	ts := newTestService(t, g, 0)
	entry := ts.mustLookup("docs/document.txt")
	writer, err := ts.OpenWriter(entry, false)
	if err != nil {
		t.Fatal(err)
	}
	defer writer.Close()
	if _, err = writer.WriteAt(t.Context(), []byte("a much larger working copy"), 0); err != nil {
		t.Fatal(err)
	}
	cache := t.TempDir()
	if _, err = ts.PreviewFile(t.Context(), entry.Item, entry.Node, 5, cache); err == nil {
		t.Fatal("preview admitted a working copy larger than its approved size")
	}
	if entries, err := os.ReadDir(cache); err != nil || len(entries) != 0 {
		t.Fatal("refused preview retained content", entries, err)
	}
}

func TestLegacyPreviewPreservesActiveLeases(t *testing.T) {
	e := gatewaytest.NewEngine(t, "docs", func(f *config.FolderConfiguration) {
		f.Versioning = config.VersioningConfiguration{Type: "trashcan", Params: map[string]string{"cleanoutDays": "0"}}
	})
	e.Write("doc.txt", "first")
	g := gatewaytest.NewGateway(t, e, "")
	g.Export(true)
	ts := newTestService(t, g, 0)
	ts.write(ts.mustLookup("docs/doc.txt"), "second", true)
	ts.settled()
	entry := ts.mustLookup("docs/doc.txt")
	versions, err := ts.Versions(t.Context(), entry.Item)
	if err != nil || len(versions.Versions) != 1 {
		t.Fatal(versions, err)
	}
	cache := t.TempDir()
	active := filepath.Join(cache, "active")
	if err = os.Mkdir(active, 0700); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(active, "open.pdf")
	if err = os.WriteFile(file, []byte("active preview"), 0600); err != nil {
		t.Fatal(err)
	}
	release, err := previewcache.Pin(active)
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	old := time.Now().Add(-48 * time.Hour)
	if err = os.Chtimes(active, old, old); err != nil {
		t.Fatal(err)
	}
	if _, err = ts.VersionToFile(context.Background(), entry.Item, versions.Versions[0].ID, cache); err != nil {
		t.Fatal(err)
	}
	if data, err := os.ReadFile(file); err != nil || string(data) != "active preview" {
		t.Fatal("legacy preview deleted an active leased copy", string(data), err)
	}
}
