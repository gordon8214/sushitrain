// Copyright (C) 2026 Gordon
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this file,
// You can obtain one at https://mozilla.org/MPL/2.0/.

//go:build engineintegration

package sushitrain

import (
	"fmt"
	"net"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/syncthing/syncthing/lib/config"
	"github.com/syncthing/syncthing/lib/protocol"
)

const entryTestTimeout = 60 * time.Second

func waitForEntries(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(entryTestTimeout)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// writeFiles creates files in the folder of an engine that startTestEngine started in dir
func writeFiles(t *testing.T, dir string, names ...string) {
	t.Helper()
	for _, name := range names {
		path := filepath.Join(dir, "folder", filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte("contents of "+name), 0o600); err != nil {
			t.Fatal(err)
		}
	}
}

// scanOnceRunning scans the folder, which fails until the engine has started it
func scanOnceRunning(t *testing.T, clt *Client) {
	t.Helper()
	waitForEntries(t, "the folder to run", func() bool { return clt.scanFolderSubdirs("test", nil) == nil })
}

// Removing a file takes the parent directories that leaves empty with it. Nothing else goes. The index hears of the file
// and what held it at once; a rescan of what is above that tells it the rest.
func TestEngineRemovesFileAndEmptiedParents(t *testing.T) {
	clt, _, dir := startTestEngine(t)
	folder := clt.FolderWithID("test")
	root := filepath.Join(dir, "folder")
	writeFiles(t, dir, "nested/sub/only.txt", "kept/one.txt", "kept/other.txt", "gone.txt")
	scanOnceRunning(t, clt)
	remove := func(path string) error {
		t.Helper()
		entry, err := folder.GetFileInformation(path)
		if err != nil || entry == nil || entry.IsDeleted() {
			t.Fatalf("%s: %v, %v", path, entry, err)
		}
		return entry.Remove()
	}
	wantDeleted := func(path string) {
		t.Helper()
		if _, err := os.Lstat(filepath.Join(root, filepath.FromSlash(path))); !os.IsNotExist(err) {
			t.Fatalf("%s is still there: %v", path, err)
		}
		waitForEntries(t, path+" to be indexed as deleted", func() bool {
			entry, err := folder.GetFileInformation(path)
			return err == nil && entry != nil && entry.IsDeleted()
		})
	}

	if err := remove("nested/sub/only.txt"); err != nil {
		t.Fatal(err)
	}
	wantDeleted("nested/sub/only.txt")
	wantDeleted("nested/sub")
	if err := clt.scanFolderSubdirs("test", nil); err != nil {
		t.Fatal(err)
	}
	wantDeleted("nested")

	if err := remove("kept/one.txt"); err != nil {
		t.Fatal(err)
	}
	wantDeleted("kept/one.txt")

	// A directory only goes when it is empty, and what it holds stays
	if err := remove("kept"); err == nil {
		t.Fatal("removed a directory that isn't empty")
	}
	if _, err := os.Lstat(filepath.Join(root, "kept", "other.txt")); err != nil {
		t.Fatal(err)
	}

	// What is already gone from disk still reaches the index
	if err := os.Remove(filepath.Join(root, "gone.txt")); err != nil {
		t.Fatal(err)
	}
	if err := remove("gone.txt"); err != nil {
		t.Fatal(err)
	}
	wantDeleted("gone.txt")
}

// shareWith adds a device to the configuration and shares the folder with it
func shareWith(cfg *config.Configuration, device protocol.DeviceID, address string) {
	cfg.SetDevice(config.DeviceConfiguration{DeviceID: device, Addresses: []string{address}, RawNumConnections: 1})
	folder, _, _ := cfg.Folder("test")
	folder.Devices = append(folder.Devices, config.FolderDeviceConfiguration{DeviceID: device})
	cfg.SetFolder(folder)
}

func listenAddress(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	return fmt.Sprintf("tcp://127.0.0.1:%d", l.Addr().(*net.TCPAddr).Port)
}

// A device holds a directory without what operating systems leave in it, which devices tend to ignore, but not without
// any other file
func TestEnginePeersWithFullCopyLeaveOutSystemFiles(t *testing.T) {
	address, peerAddress := listenAddress(t), listenAddress(t)
	peer, _, _ := startTestEngineWith(t, func(cfg *config.Configuration) {
		cfg.Options.RawListenAddresses = []string{peerAddress}
	})
	clt, _, dir := startTestEngineWith(t, func(cfg *config.Configuration) {
		cfg.Options.RawListenAddresses = []string{address}
		shareWith(cfg, peer.deviceID(), peerAddress)
	})
	waitForEntries(t, "the peer's folder to run", func() bool {
		return peer.FolderWithID("test").SetIgnoreLines(List([]string{".DS_Store", "*.draft"})) == nil
	})
	err := peer.changeConfiguration(func(cfg *config.Configuration) { shareWith(cfg, clt.deviceID(), address) })
	if err != nil {
		t.Fatal(err)
	}

	writeFiles(t, dir, "full/notes.txt", "full/.DS_Store", "partial/notes.txt", "partial/notes.draft")
	scanOnceRunning(t, clt)

	folder := clt.FolderWithID("test")
	holds := func(path string) bool {
		t.Helper()
		entry, err := folder.GetFileInformation(path)
		if err != nil {
			t.Fatal(err)
		}
		if entry == nil {
			return false
		}
		peers, err := entry.PeersWithFullCopy()
		if err != nil {
			t.Fatal(err)
		}
		return slices.Contains(peers.data, peer.DeviceID())
	}
	waitForEntries(t, "the peer to have the files it doesn't ignore", func() bool {
		return holds("full/notes.txt") && holds("partial/notes.txt")
	})
	if !holds("full") {
		t.Error("a directory the peer holds, apart from what systems left in it")
	}
	if holds("partial") {
		t.Error("a directory the peer doesn't hold all of")
	}
}
