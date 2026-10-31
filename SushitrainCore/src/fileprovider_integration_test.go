// Copyright (C) 2026 Gordon
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this file,
// You can obtain one at https://mozilla.org/MPL/2.0/.

//go:build fpintegration

// Verifies the assumptions the file provider change feed makes about Syncthing's database against a real (local only)
// Syncthing instance. Run with: go test -tags noassets,fpintegration -run Integration ./...
package sushitrain

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/syncthing/syncthing/lib/config"
	"github.com/syncthing/syncthing/lib/events"
	"github.com/syncthing/syncthing/lib/protocol"
	"github.com/syncthing/syncthing/lib/svcutil"
	"github.com/syncthing/syncthing/lib/syncthing"
)

func newIntegrationClient(t *testing.T, modify ...func(*config.Configuration)) (*Client, string) {
	t.Helper()
	dir := t.TempDir()
	cert, err := syncthing.GenerateCertificate(filepath.Join(dir, "cert.pem"), filepath.Join(dir, "key.pem"))
	if err != nil {
		t.Fatal(err)
	}
	devID := protocol.NewDeviceID(cert.Certificate[0])

	folderPath := filepath.Join(dir, "folder")
	if err := os.MkdirAll(folderPath, 0o700); err != nil {
		t.Fatal(err)
	}

	cfg := config.New(devID)
	cfg.GUI.Enabled = false
	cfg.Options.GlobalAnnEnabled = false
	cfg.Options.LocalAnnEnabled = false
	cfg.Options.RelaysEnabled = false
	cfg.Options.NATEnabled = false
	cfg.Options.CREnabled = false
	cfg.Options.URAccepted = -1
	cfg.Options.RawListenAddresses = []string{"tcp://127.0.0.1:0"}
	folder := cfg.Defaults.Folder.Copy()
	folder.ID = "test"
	folder.Path = folderPath
	folder.FilesystemType = config.FilesystemTypeBasic
	folder.RescanIntervalS = 0
	folder.FSWatcherEnabled = false
	folder.Devices = []config.FolderDeviceConfiguration{{DeviceID: devID}}
	cfg.Folders = []config.FolderConfiguration{folder}
	for _, change := range modify {
		change(&cfg)
	}

	ctx, cancel := context.WithCancel(context.Background())
	evLogger := events.NewLogger()
	go evLogger.Serve(ctx)
	wrapper := config.Wrap(filepath.Join(dir, "config.xml"), cfg, devID, evLogger)
	// The configuration is saved when serving ends, which must happen before the temporary directory is removed
	served := make(chan struct{})
	go func() {
		defer close(served)
		_ = wrapper.Serve(ctx)
	}()

	sdb, err := syncthing.OpenDatabase(filepath.Join(dir, "index"), time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	app, err := syncthing.New(wrapper, sdb, evLogger, cert, syncthing.Options{})
	if err != nil {
		t.Fatal(err)
	}
	if err := app.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		app.Stop(svcutil.ExitSuccess)
		app.Wait()
		cancel()
		<-served
	})

	clt := (&Client{filesPath: dir, cert: &cert}).withTestRun(wrapper, app, evLogger)
	return clt, folderPath
}

func scanAndCollect(t *testing.T, folder *Folder, cursor int64) ([]fileProviderRecord, *FileProviderPassResult) {
	t.Helper()
	if err := folder.ScanPaths(List([]string{""})); err != nil {
		t.Fatal(err)
	}
	delegate := &collectingRecordDelegate{}
	result, err := folder.GlobalChanges(cursor, delegate)
	if err != nil {
		t.Fatal(err)
	}
	return delegate.records(), result
}

func recordFor(records []fileProviderRecord, path string) *fileProviderRecord {
	for i := range records {
		if records[i].Path == path {
			return &records[i]
		}
	}
	return nil
}

func TestIntegrationGlobalSequences(t *testing.T) {
	clt, root := newIntegrationClient(t)
	folder := clt.FolderWithID("test")

	write := func(name string, contents string) {
		t.Helper()
		full := filepath.Join(root, name)
		if err := os.MkdirAll(filepath.Dir(full), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(contents), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	write("a.txt", "one")
	write("dir/b.txt", "two")
	records, first := scanAndCollect(t, folder, 0)
	a := recordFor(records, "a.txt")
	if a == nil || recordFor(records, "dir") == nil || recordFor(records, "dir/b.txt") == nil {
		t.Fatalf("initial scan missing entries: %+v", records)
	}
	if a.BlocksHash == "" || !a.Full || a.Size != 3 {
		t.Fatalf("unexpected record %+v", a)
	}

	// Nothing changed: no records above the cursor
	records, unchanged := scanAndCollect(t, folder, first.HighestSequence)
	if len(records) != 0 || unchanged.HighestSequence != first.HighestSequence {
		t.Fatalf("unchanged folder produced records: %+v", records)
	}

	// Modifying a file gives it a higher sequence and a new blocks hash
	write("a.txt", "changed")
	records, second := scanAndCollect(t, folder, first.HighestSequence)
	modified := recordFor(records, "a.txt")
	if modified == nil || modified.Sequence <= first.HighestSequence || modified.BlocksHash == a.BlocksHash {
		t.Fatalf("modification not reported: %+v", records)
	}
	if recordFor(records, "dir/b.txt") != nil {
		t.Fatal("unchanged file reported")
	}

	// Deleting a file leaves a tombstone with a higher sequence
	if err := os.Remove(filepath.Join(root, "dir", "b.txt")); err != nil {
		t.Fatal(err)
	}
	records, third := scanAndCollect(t, folder, second.HighestSequence)
	deleted := recordFor(records, "dir/b.txt")
	if deleted == nil || !deleted.Deleted || deleted.Sequence <= second.HighestSequence {
		t.Fatalf("deletion not reported as tombstone: %+v", records)
	}
	if third.SequenceReset {
		t.Fatal("unexpected sequence reset")
	}

	// The snapshot contains everything, including the tombstone
	delegate := &collectingRecordDelegate{}
	if _, err := folder.GlobalSnapshot(delegate); err != nil {
		t.Fatal(err)
	}
	snapshot := delegate.records()
	if recordFor(snapshot, "a.txt") == nil || recordFor(snapshot, "dir/b.txt") == nil || !recordFor(snapshot, "dir/b.txt").Deleted {
		t.Fatalf("unexpected snapshot %+v", snapshot)
	}
}

func TestIntegrationSelectionChangesWaitForSelectionLock(t *testing.T) {
	clt, root := newIntegrationClient(t)
	folder := clt.FolderWithID("test")
	if err := os.WriteFile(filepath.Join(root, "a.txt"), []byte("one"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := clt.internals().SetIgnores("test", []string{"*"}); err != nil {
		t.Fatal(err)
	}
	if err := folder.ScanPaths(List([]string{""})); err != nil {
		t.Fatal(err)
	}
	// Changing the ignore patterns starts another scan
	for deadline := time.Now().Add(10 * time.Second); ; {
		if state, err := folder.State(); err == nil && state == "idle" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("folder does not become idle")
		}
		time.Sleep(10 * time.Millisecond)
	}

	// Deselecting decides what to delete from the new selection, so it holds the lock until it has deleted
	unlock := lockSelection("test")
	done := make(chan error, 1)
	go func() {
		for {
			// The folder may still be scanning after the change of its ignore patterns
			err := folder.SetLocalFileExplicitlySelected("a.txt", false)
			if err != nil && strings.Contains(err.Error(), "not idle or syncing") {
				time.Sleep(10 * time.Millisecond)
				continue
			}
			done <- err
			return
		}
	}()
	select {
	case err := <-done:
		unlock()
		t.Fatalf("selection changed while the selection was locked: %v", err)
	case <-time.After(100 * time.Millisecond):
	}
	unlock()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("selection lock was not released")
	}
}
