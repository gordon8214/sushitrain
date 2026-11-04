// Copyright (C) 2026 Gordon
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this file,
// You can obtain one at https://mozilla.org/MPL/2.0/.
package sushitrain

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/syncthing/syncthing/lib/config"
)

func historyRecoveryFixture(t *testing.T) (*os.Root, string, historyJournal) {
	t.Helper()
	directory := t.TempDir()
	if err := os.WriteFile(filepath.Join(directory, "file.txt"), []byte("original"), 0o644); err != nil {
		t.Fatal(err)
	}
	staged := ".syncthing.history-new.tmp"
	if err := os.WriteFile(filepath.Join(directory, staged), []byte("download"), 0o644); err != nil {
		t.Fatal(err)
	}
	root, err := os.OpenRoot(directory)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { root.Close() })
	oldHash, err := historyFileHash(root, "file.txt")
	if err != nil {
		t.Fatal(err)
	}
	newHash, err := historyFileHash(root, staged)
	if err != nil {
		t.Fatal(err)
	}
	journal, record, err := beginHistoryJournal(context.Background(), root, "file.txt", staged, oldHash, newHash, filepath.Join(root.Name(), "pending"))
	if err != nil {
		t.Fatal(err)
	}
	return root, journal, record
}

func TestHistoryRecoveryAfterArchiveAndFailedPublish(t *testing.T) {
	root, journal, record := historyRecoveryFixture(t)
	if err := root.Rename(record.Target, "archived.txt"); err != nil {
		t.Fatal(err)
	}
	if err := root.Remove(record.Staged); err != nil {
		t.Fatal(err)
	} // Inject publication failure.
	if err := publishHistoryFile(root, record.Staged, record.Target, journal, record, filepath.Join(root.Name(), "pending")); err == nil {
		t.Fatal("publication unexpectedly succeeded")
	}
	contents, err := os.ReadFile(filepath.Join(root.Name(), record.Target))
	if err != nil || string(contents) != "original" {
		t.Fatalf("original was not recovered: %q %v", contents, err)
	}
}

func TestHistoryRecoveryBeforeEngineStartup(t *testing.T) {
	for _, phase := range []string{"prepared", "archived", "published"} {
		t.Run(phase, func(t *testing.T) {
			root, _, record := historyRecoveryFixture(t)
			if phase != "prepared" {
				if err := root.Rename(record.Target, "archived.txt"); err != nil {
					t.Fatal(err)
				}
			}
			if phase == "published" {
				if err := root.Rename(record.Staged, record.Target); err != nil {
					t.Fatal(err)
				}
			}
			folder := config.FolderConfiguration{ID: "docs", Path: root.Name(), FilesystemType: config.FilesystemTypeBasic}
			if err := recoverHistoryRestores(map[string]config.FolderConfiguration{"docs": folder}, filepath.Join(root.Name(), "pending")); err != nil {
				t.Fatal(err)
			}
			want := "original"
			if phase == "published" {
				want = "download"
			}
			contents, err := root.ReadFile(record.Target)
			if err != nil || string(contents) != want {
				t.Fatalf("recovered %q, want %q: %v", contents, want, err)
			}
			entries, err := root.ReadFile(record.Staged)
			if !os.IsNotExist(err) {
				t.Fatalf("staging left behind: %q %v", entries, err)
			}
		})
	}
}

func TestHistoryRecoveryPreservesInterveningEdits(t *testing.T) {
	root, journal, record := historyRecoveryFixture(t)
	if err := os.WriteFile(filepath.Join(root.Name(), record.Target), []byte("changed by another app"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := recoverHistoryJournal(root, journal, record, filepath.Join(root.Name(), "pending")); err == nil {
		t.Fatal("unrecognized edit was accepted")
	}
	contents, _ := root.ReadFile(record.Target)
	if string(contents) != "changed by another app" {
		t.Fatalf("lost edit: %q", contents)
	}
	if _, err := root.Lstat(journal); err != nil {
		t.Fatalf("recovery journal lost: %v", err)
	}
}

func TestHistoryRecoveryRequiresPendingRootEvenAfterReconfiguration(t *testing.T) {
	root, journal, record := historyRecoveryFixture(t)
	pendingDirectory := t.TempDir()
	pendingRoot, err := os.OpenRoot(pendingDirectory)
	if err != nil {
		t.Fatal(err)
	}
	defer pendingRoot.Close()
	pending := historyPending{Root: root.Name(), Journal: journal, Record: record}
	if err := writeHistoryJSON(pendingRoot, historyPendingName(root.Name(), journal), pending); err != nil {
		t.Fatal(err)
	}
	if err := root.Rename(record.Target, "archived.txt"); err != nil {
		t.Fatal(err)
	}
	root.Close()
	moved := root.Name() + "-offline"
	if err := os.Rename(root.Name(), moved); err != nil {
		t.Fatal(err)
	}
	defer os.Rename(moved, root.Name())
	if err := recoverHistoryRestores(nil, pendingDirectory); err == nil {
		t.Fatal("started without pending volume")
	}
	if err := os.Rename(moved, root.Name()); err != nil {
		t.Fatal(err)
	}
	if err := recoverHistoryRestores(nil, pendingDirectory); err != nil {
		t.Fatal(err)
	}
	contents, err := os.ReadFile(filepath.Join(root.Name(), record.Target))
	if err != nil || string(contents) != "original" {
		t.Fatalf("lost original: %q %v", contents, err)
	}
	entries, _ := os.ReadDir(pendingDirectory)
	if len(entries) != 0 {
		t.Fatal("pending marker left after durable recovery")
	}
}

func TestHistoryRecoveryIgnoresInterruptedDraft(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, ".syncthing.history-draft-interrupted.tmp"), []byte("{partial"), 0o600); err != nil {
		t.Fatal(err)
	}
	folder := config.FolderConfiguration{ID: "docs", Path: dir, FilesystemType: config.FilesystemTypeBasic}
	if err := recoverHistoryRestores(map[string]config.FolderConfiguration{"docs": folder}, t.TempDir()); err != nil {
		t.Fatal(err)
	}
}

func TestHistoryRecoveryRetriesCleanupAndStaleMarker(t *testing.T) {
	root, journal, record := historyRecoveryFixture(t)
	if err := root.Remove(record.Staged); err != nil {
		t.Fatal(err)
	}
	if err := root.Mkdir(record.Staged, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root.Name(), record.Staged, "busy"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	pending := filepath.Join(root.Name(), "pending")
	if err := recoverHistoryJournal(root, journal, record, pending); err == nil {
		t.Fatal("ignored cleanup failure")
	}
	if _, err := root.Lstat(journal); err != nil {
		t.Fatal("lost retry record")
	}
	if err := root.Remove(filepath.Join(record.Staged, "busy")); err != nil {
		t.Fatal(err)
	}
	if err := recoverHistoryJournal(root, journal, record, pending); err != nil {
		t.Fatal(err)
	}
	// A crash after removing the root journal but before removing the external marker is safe to reconcile again.
	pendingRoot, err := os.OpenRoot(pending)
	if err != nil {
		t.Fatal(err)
	}
	defer pendingRoot.Close()
	if err := writeHistoryJSON(pendingRoot, historyPendingName(root.Name(), journal), historyPending{root.Name(), journal, record}); err != nil {
		t.Fatal(err)
	}
	if err := recoverHistoryRestores(nil, pending); err != nil {
		t.Fatal(err)
	}
}

func TestHistoryCopyHonorsCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := io.Copy(io.Discard, historyContextReader{ctx, strings.NewReader("contents")}); !errors.Is(err, context.Canceled) {
		t.Fatalf("copy ignored cancellation: %v", err)
	}
	root, _, record := historyRecoveryFixture(t)
	if _, err := historyFileHashContext(ctx, root, record.Target); !errors.Is(err, context.Canceled) {
		t.Fatalf("hash ignored cancellation: %v", err)
	}
}
