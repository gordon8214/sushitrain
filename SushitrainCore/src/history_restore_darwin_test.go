// Copyright (C) 2026 Gordon
// SPDX-License-Identifier: MPL-2.0
//go:build engineintegration && darwin

package sushitrain

import (
	"context"
	"golang.org/x/sys/unix"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestEngineRestoreDownloadedRejectsMetadataOnlyEdit(t *testing.T) {
	clt, _, dir := startTestEngine(t)
	root := filepath.Join(dir, "folder")
	name := filepath.Join(root, "file.txt")
	writeTestFile(t, name, "current")
	folder := scanTestFolder(t, clt)
	source := filepath.Join(t.TempDir(), "download")
	writeTestFile(t, source, "history")
	token, err := folder.CaptureRestoreTarget("file.txt")
	if err != nil {
		t.Fatal(err)
	}
	if err := unix.Setxattr(name, "com.synctrain.history-test", []byte("edited metadata"), 0); err != nil {
		t.Fatal(err)
	}
	if err := folder.RestoreDownloadedVersion("file.txt", source, token, time.Now().UnixNano()); err == nil {
		t.Fatal("overwrote metadata-only edit")
	}
	if contents, _ := os.ReadFile(name); string(contents) != "current" {
		t.Fatal("lost current contents")
	}
	size, err := unix.Getxattr(name, "com.synctrain.history-test", nil)
	if err != nil || size == 0 {
		t.Fatalf("lost metadata: %v", err)
	}
}

func TestEngineRestoreSnapshotMetadataOnlyAndEmptyAttributes(t *testing.T) {
	clt, _, dir := startTestEngine(t)
	root := filepath.Join(dir, "folder")
	live := filepath.Join(root, "file.txt")
	writeTestFile(t, live, "same contents")
	folder := scanTestFolder(t, clt)
	if err := unix.Setxattr(live, "com.synctrain.extra", []byte("remove on restore"), 0); err != nil {
		t.Fatal(err)
	}
	source := filepath.Join(t.TempDir(), "snapshot")
	writeTestFile(t, source, "same contents")
	fork := []byte("historical fork")
	if err := unix.Setxattr(source, "com.apple.ResourceFork", fork, 0); err != nil {
		t.Fatal(err)
	}
	captured, err := folder.CaptureRestoreTarget("file.txt")
	if err != nil {
		t.Fatal(err)
	}
	if err := folder.RestoreSnapshotVersion("file.txt", source, captured, time.Now().UnixNano()); err != nil {
		t.Fatal(err)
	}
	actual := make([]byte, 128)
	n, err := unix.Getxattr(live, "com.apple.ResourceFork", actual)
	if err != nil || string(actual[:n]) != string(fork) {
		t.Fatalf("lost fork: %q %v", actual, err)
	}
	if _, err := unix.Getxattr(live, "com.synctrain.extra", nil); err == nil {
		t.Fatal("retained attribute absent in archive")
	}
	if err := unix.Removexattr(source, "com.apple.ResourceFork"); err != nil {
		t.Fatal(err)
	}
	captured, err = folder.CaptureRestoreTarget("file.txt")
	if err != nil {
		t.Fatal(err)
	}
	if err := folder.RestoreSnapshotVersion("file.txt", source, captured, time.Now().UnixNano()); err != nil {
		t.Fatal(err)
	}
	if _, err := unix.Getxattr(live, "com.apple.ResourceFork", nil); err == nil {
		t.Fatal("retained fork absent in empty archive")
	}
}

func TestHistoryRecoveryChecksMetadataAfterRestoringBackup(t *testing.T) {
	root, journal, record := historyRecoveryFixture(t)
	var err error
	record.MetadataHashFormat = 1
	record.OldMetadataHash, err = historyMetadataHash(filepath.Join(root.Name(), record.Target))
	if err != nil {
		t.Fatal(err)
	}
	record.NewMetadataHash, err = historyMetadataHash(filepath.Join(root.Name(), record.Staged))
	if err != nil {
		t.Fatal(err)
	}
	if err := root.Remove(record.Target); err != nil {
		t.Fatal(err)
	}
	if err := recoverHistoryJournal(root, journal, record, filepath.Join(root.Name(), "pending")); err != nil {
		t.Fatal(err)
	}
	data, err := root.ReadFile(record.Target)
	if err != nil || string(data) != "original" {
		t.Fatalf("recovered %q %v", data, err)
	}
}

func TestHistoryJournalRejectsMetadataOnlyEdit(t *testing.T) {
	root, _, existing := historyRecoveryFixture(t)
	oldMetadata, err := historyMetadataHash(filepath.Join(root.Name(), existing.Target))
	if err != nil {
		t.Fatal(err)
	}
	if err := unix.Setxattr(filepath.Join(root.Name(), existing.Target), "com.synctrain.edited", []byte("intervening edit"), 0); err != nil {
		t.Fatal(err)
	}
	_, _, err = beginHistoryJournal(context.Background(), root, existing.Target, existing.Staged, existing.OldHash, existing.NewHash, filepath.Join(root.Name(), "pending"), oldMetadata, oldMetadata)
	if err == nil {
		t.Fatal("accepted intervening metadata edit")
	}
	if _, err := unix.Getxattr(filepath.Join(root.Name(), existing.Target), "com.synctrain.edited", nil); err != nil {
		t.Fatal("lost intervening edit")
	}
}

func TestHistoryRecoverySupportsEarlierMetadataHashFormat(t *testing.T) {
	root, journal, record := historyRecoveryFixture(t)
	var err error
	record.OldMetadataHash, err = historyRecoveryMetadataHash(filepath.Join(root.Name(), record.Target), 0)
	if err != nil {
		t.Fatal(err)
	}
	record.NewMetadataHash, err = historyRecoveryMetadataHash(filepath.Join(root.Name(), record.Staged), 0)
	if err != nil {
		t.Fatal(err)
	}
	if err := root.Remove(record.Target); err != nil {
		t.Fatal(err)
	}
	if err := recoverHistoryJournal(root, journal, record, filepath.Join(root.Name(), "pending")); err != nil {
		t.Fatal(err)
	}
}
func TestEngineSnapshotExportPolicy(t *testing.T) {
	clt, _, dir := startTestEngine(t)
	root := filepath.Join(dir, "folder")
	live := filepath.Join(root, "file.txt")
	writeTestFile(t, live, "current")
	folder := scanTestFolder(t, clt)
	if err := folder.ValidateVersionExportDestination(live); err == nil {
		t.Fatal("accepted export over synchronized file")
	}
	if !folder.VersionExportRequiresExclusive(filepath.Join(root, "new.txt")) {
		t.Fatal("new synchronized file can be overwritten")
	}
	alias := filepath.Join(t.TempDir(), "alias")
	if err := os.Symlink(root, alias); err != nil {
		t.Fatal(err)
	}
	if !folder.VersionExportRequiresExclusive(filepath.Join(alias, "new.txt")) {
		t.Fatal("missed symlinked synchronized destination")
	}
	if folder.VersionExportRequiresExclusive(filepath.Join(t.TempDir(), "outside.txt")) {
		t.Fatal("treated ordinary destination as synchronized")
	}
}
