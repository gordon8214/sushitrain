// Copyright (C) 2026 Gordon
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this file,
// You can obtain one at https://mozilla.org/MPL/2.0/.

// Starts real engines, so these only run with the engineintegration tag.
//
//go:build engineintegration

package sushitrain

import (
	"crypto/tls"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/syncthing/syncthing/lib/config"
	"github.com/syncthing/syncthing/lib/model"
	"github.com/syncthing/syncthing/lib/protocol"
	"github.com/syncthing/syncthing/lib/tlsutil"
	"github.com/syncthing/syncthing/lib/versioner"
)

const engineActionTimeout = 20 * time.Second

func waitUntil(t *testing.T, what string, condition func() bool) {
	t.Helper()
	deadline := time.Now().Add(engineActionTimeout)
	for !condition() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting until %s", what)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func writeTestFile(t *testing.T, path string, contents string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
		t.Fatal(err)
	}
}

func scanTestFolder(t *testing.T, clt *Client) *Folder {
	t.Helper()
	folder := clt.FolderWithID("test")
	if folder == nil {
		t.Fatal("no folder")
	}
	waitUntil(t, "the folder runs", func() bool { return folder.State() == "idle" })
	if err := folder.Rescan(); err != nil {
		t.Fatal(err)
	}
	waitUntil(t, "the scan finished", func() bool { return folder.State() == "idle" })
	return folder
}

func TestEngineReportsWhyAFolderStopped(t *testing.T) {
	clt, _, dir := startTestEngine(t)
	folder := scanTestFolder(t, clt)
	if folder.State() != "idle" || folder.StateError() != "" {
		t.Fatalf("%q, %q", folder.State(), folder.StateError())
	}

	if err := os.RemoveAll(filepath.Join(dir, "folder", config.DefaultMarkerName)); err != nil {
		t.Fatal(err)
	}
	_ = folder.Rescan()
	waitUntil(t, "the folder stopped", func() bool { return folder.State() == "error" })
	if message := folder.StateError(); !strings.Contains(message, "marker") {
		t.Fatalf("the message is %q", message)
	}
}

func TestEngineReportsFailedItems(t *testing.T) {
	clt, _, dir := startTestEngine(t)
	unreadable := filepath.Join(dir, "folder", "unreadable.txt")
	writeTestFile(t, unreadable, "secret")
	if err := os.Chmod(unreadable, 0); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(unreadable, 0o600) })

	folder := scanTestFolder(t, clt)
	waitUntil(t, "the file is reported", func() bool { return folder.FailedItems().Count() == 1 })
	item := folder.FailedItems().ItemAt(0)
	if item.Path != "unreadable.txt" || !strings.Contains(item.Error, "permission denied") {
		t.Fatalf("got %+v", item)
	}

	if err := os.Chmod(unreadable, 0o600); err != nil {
		t.Fatal(err)
	}
	folder = scanTestFolder(t, clt)
	waitUntil(t, "the failure is gone", func() bool { return folder.FailedItems().Count() == 0 })

	if err := folder.SetPaused(true); err != nil {
		t.Fatal(err)
	}
	if count := folder.FailedItems().Count(); count != 0 {
		t.Fatalf("a paused folder has %d", count)
	}
}

func TestEngineRevertsLocalChanges(t *testing.T) {
	clt, _, dir := startTestEngineWith(t, func(cfg *config.Configuration) {
		cfg.Folders[0].Type = config.FolderTypeReceiveOnly
	})
	added := filepath.Join(dir, "folder", "added.txt")
	writeTestFile(t, added, "local")
	folder := scanTestFolder(t, clt)
	waitUntil(t, "the change is counted", func() bool { return folder.LocalChangedItems() == 1 })

	if err := folder.Override(); !errors.Is(err, errOverrideFolderType) {
		t.Fatalf("got %v", err)
	}
	if err := folder.Revert(); err != nil {
		t.Fatal(err)
	}
	waitUntil(t, "the change is gone", func() bool {
		_, err := os.Stat(added)
		return os.IsNotExist(err) && folder.LocalChangedItems() == 0
	})
}

// What a receive-encrypted folder deleted stays as records with the same flag, which are not changes anymore
func TestEngineDeletesUnexpectedItemsAndStopsCountingThem(t *testing.T) {
	clt, _, dir := startTestEngineWith(t, func(cfg *config.Configuration) {
		cfg.Folders[0].Type = config.FolderTypeReceiveEncrypted
	})
	stray := filepath.Join(dir, "folder", "stray.txt")
	writeTestFile(t, stray, "doesn't belong here")
	folder := scanTestFolder(t, clt)
	waitUntil(t, "the item is counted", func() bool { return folder.LocalChangedItems() == 1 })

	if err := folder.Revert(); err != nil {
		t.Fatal(err)
	}
	waitUntil(t, "the item is gone", func() bool {
		_, err := os.Stat(stray)
		return os.IsNotExist(err) && folder.LocalChangedItems() == 0
	})
}

func TestEngineOverridesASendOnlyFolder(t *testing.T) {
	clt, _, _ := startTestEngineWith(t, func(cfg *config.Configuration) {
		cfg.Folders[0].Type = config.FolderTypeSendOnly
	})
	folder := scanTestFolder(t, clt)
	if err := folder.Revert(); !errors.Is(err, errRevertFolderType) {
		t.Fatalf("got %v", err)
	}
	if err := folder.Override(); err != nil {
		t.Fatal(err)
	}
	if count := folder.OutOfSyncItems(); count != 0 {
		t.Fatalf("got %d", count)
	}
}

func TestEngineDoesNotPretendToRevertAPausedFolder(t *testing.T) {
	clt, _, _ := startTestEngineWith(t, func(cfg *config.Configuration) {
		cfg.Folders[0].Type = config.FolderTypeReceiveOnly
		cfg.Folders[0].Paused = true
	})
	folder := clt.FolderWithID("test")
	if err := folder.Revert(); !errors.Is(err, model.ErrFolderPaused) {
		t.Fatalf("got %v", err)
	}
}

// The first scan of a new folder comes right after it is added, and has to find the patterns there
func TestEngineNewFolderStartsWithTheDefaultIgnores(t *testing.T) {
	clt, _, _ := startTestEngine(t)
	if err := clt.SetDefaultIgnoreLines("*.tmp"); err != nil {
		t.Fatal(err)
	}
	root := filepath.Join(t.TempDir(), "new")
	writeTestFile(t, filepath.Join(root, "ignored.tmp"), "ignored")
	writeTestFile(t, filepath.Join(root, "kept.txt"), "kept")

	if err := clt.AddFolder("new", root, false); err != nil {
		t.Fatal(err)
	}
	folder := clt.FolderWithID("new")
	waitUntil(t, "the first scan finished", func() bool {
		info, _, err := clt.internals().GlobalFileInfo("new", "kept.txt")
		return err == nil && info.Name == "kept.txt" && folder.State() == "idle"
	})
	if _, ok, err := clt.internals().GlobalFileInfo("new", "ignored.tmp"); err != nil || ok {
		t.Fatalf("ignored.tmp was indexed: %v, %v", ok, err)
	}
	if contents, _ := os.ReadFile(filepath.Join(root, ignoreFileName)); string(contents) != "*.tmp\n" {
		t.Fatalf("got %q", contents)
	}
}

func TestEngineNewFolderKeepsItsOwnIgnores(t *testing.T) {
	clt, _, _ := startTestEngine(t)
	if err := clt.SetDefaultIgnoreLines("*.tmp"); err != nil {
		t.Fatal(err)
	}
	root := filepath.Join(t.TempDir(), "new")
	writeTestFile(t, filepath.Join(root, ignoreFileName), "*.own\n")
	if err := clt.AddFolder("new", root, false); err != nil {
		t.Fatal(err)
	}
	if contents, _ := os.ReadFile(filepath.Join(root, ignoreFileName)); string(contents) != "*.own\n" {
		t.Fatalf("got %q", contents)
	}
}

func TestEngineNewEncryptedFolderHasNoIgnores(t *testing.T) {
	clt, _, _ := startTestEngine(t)
	if err := clt.SetDefaultIgnoreLines("*.tmp"); err != nil {
		t.Fatal(err)
	}
	root := filepath.Join(t.TempDir(), "new")
	if err := clt.AddFolder("new", root, true); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(root, ignoreFileName)); !os.IsNotExist(err) {
		t.Fatalf("got %v", err)
	}
}

// versionedTestFolder has a folder keep versions, with one version of two files, one of which still exists
func versionedTestFolder(t *testing.T) (*Client, *Folder, string) {
	t.Helper()
	clt, _, dir := startTestEngineWith(t, func(cfg *config.Configuration) {
		cfg.Folders[0].Versioning = config.VersioningConfiguration{
			Type: VersioningTypeSimple, Params: map[string]string{"keep": "5", "cleanoutDays": "0"},
			CleanupIntervalS: 3600, FSType: config.FilesystemTypeBasic,
		}
	})
	root := filepath.Join(dir, "folder")
	stamp := time.Now().Add(-time.Hour).Format(versioner.TimeFormat)
	writeTestFile(t, filepath.Join(root, "kept.txt"), "current")
	writeTestFile(t, filepath.Join(root, ".stversions", "kept~"+stamp+".txt"), "old")
	writeTestFile(t, filepath.Join(root, ".stversions", "gone", "deleted~"+stamp+".txt"), "deleted")
	// Where the engine really puts these, as seen in a folder with platform history
	writeTestFile(t, filepath.Join(root, ".stversions", ".stversions", "platform", "0123.json.gz"), "not a file of the user's")
	return clt, scanTestFolder(t, clt), root
}

func TestEngineListsAndExportsVersions(t *testing.T) {
	_, folder, _ := versionedTestFolder(t)
	if !folder.CanBrowseVersions() {
		t.Fatal("the versions can't be browsed")
	}
	versions, err := folder.FileVersions("kept.txt")
	if err != nil {
		t.Fatal(err)
	}
	if versions.Count() != 1 || versions.ItemAt(0).Size != int64(len("old")) {
		t.Fatalf("got %d", versions.Count())
	}

	destination := filepath.Join(t.TempDir(), "exported.txt")
	if err := folder.ExportFileVersion("kept.txt", versions.ItemAt(0), destination); err != nil {
		t.Fatal(err)
	}
	if contents, _ := os.ReadFile(destination); string(contents) != "old" {
		t.Fatalf("got %q", contents)
	}
	if entries, _ := os.ReadDir(filepath.Dir(destination)); len(entries) != 1 {
		t.Fatalf("left behind: %v", entries)
	}
}

func TestEngineRestoresOverAnExistingFile(t *testing.T) {
	_, folder, root := versionedTestFolder(t)
	versions, err := folder.FileVersions("kept.txt")
	if err != nil {
		t.Fatal(err)
	}
	if err := folder.RestoreFileVersion("kept.txt", versions.ItemAt(0)); err != nil {
		t.Fatal(err)
	}
	if contents, _ := os.ReadFile(filepath.Join(root, "kept.txt")); string(contents) != "old" {
		t.Fatalf("got %q", contents)
	}
	// What was there isn't lost: it is a version now
	archived, err := folder.FileVersions("kept.txt")
	if err != nil {
		t.Fatal(err)
	}
	if archived.Count() != 1 || archived.ItemAt(0).Size != int64(len("current")) {
		t.Fatalf("got %d versions", archived.Count())
	}
}

func TestEngineListsAndRestoresDeletedFiles(t *testing.T) {
	_, folder, root := versionedTestFolder(t)
	deleted, err := folder.DeletedFiles()
	if err != nil {
		t.Fatal(err)
	}
	if deleted.Count() != 1 || deleted.ItemAt(0).Path != filepath.Join("gone", "deleted.txt") ||
		deleted.ItemAt(0).Versions != 1 {
		var paths []string
		for _, item := range deleted.items {
			paths = append(paths, item.Path)
		}
		t.Fatalf("got %v", paths)
	}

	failed, err := folder.RestoreLatestVersions(List([]string{deleted.ItemAt(0).Path, "never-existed.txt"}))
	if err != nil {
		t.Fatal(err)
	}
	if failed.Count() != 1 || failed.ItemAt(0).Path != "never-existed.txt" {
		t.Fatalf("got %d", failed.Count())
	}
	if contents, _ := os.ReadFile(filepath.Join(root, "gone", "deleted.txt")); string(contents) != "deleted" {
		t.Fatalf("got %q", contents)
	}
	if deleted, err = folder.DeletedFiles(); err != nil || deleted.Count() != 0 {
		t.Fatalf("got %v", err)
	}
}

// Replacing a file in a folder this way would keep no version of what was there, and go to its devices
func TestEngineDoesNotExportOverASynchronizedFile(t *testing.T) {
	_, folder, root := versionedTestFolder(t)
	versions, err := folder.FileVersions("kept.txt")
	if err != nil {
		t.Fatal(err)
	}
	if err := folder.ExportFileVersion("kept.txt", versions.ItemAt(0), filepath.Join(root, "kept.txt")); !errors.Is(err, errExportOverFile) {
		t.Fatalf("got %v", err)
	}
	if contents, _ := os.ReadFile(filepath.Join(root, "kept.txt")); string(contents) != "current" {
		t.Fatalf("got %q", contents)
	}
	// As a new file in a folder it is fine, and scanning doesn't see it before it is complete
	copyPath := filepath.Join(root, "copy.txt")
	if err := folder.ExportFileVersion("kept.txt", versions.ItemAt(0), copyPath); err != nil {
		t.Fatal(err)
	}
	if contents, _ := os.ReadFile(copyPath); string(contents) != "old" {
		t.Fatalf("got %q", contents)
	}
}

func TestEngineRefusesAFolderInAVersionsPath(t *testing.T) {
	clt, _, _ := startTestEngine(t)
	versions := filepath.Join(t.TempDir(), "versions")
	settings := clt.FolderWithID("test").VersioningSettings()
	settings.Type = VersioningTypeTrashcan
	settings.Path = versions
	if err := clt.FolderWithID("test").SetVersioningSettings(settings); err != nil {
		t.Fatal(err)
	}
	if err := clt.AddFolder("new", filepath.Join(versions, "new"), false); !errors.Is(err, ErrFolderInVersionsPath) {
		t.Fatalf("got %v", err)
	}
}

func TestEngineCantBrowseVersionsOfAFolderThatDoesNotRun(t *testing.T) {
	_, folder, _ := versionedTestFolder(t)
	if err := folder.SetPaused(true); err != nil {
		t.Fatal(err)
	}
	if folder.CanBrowseVersions() {
		t.Fatal("a paused folder has no versioner")
	}
	if _, err := folder.FileVersions("kept.txt"); !errors.Is(err, model.ErrFolderPaused) {
		t.Fatalf("got %v", err)
	}
}

// knockAsUnknownDevice connects like a device this engine doesn't know: it gets as far as the hello, which is what the
// engine records as a request to connect
func knockAsUnknownDevice(t *testing.T, address string) protocol.DeviceID {
	t.Helper()
	cert := knockCertificate(t)
	knockWith(t, address, cert)
	return protocol.NewDeviceID(cert.Certificate[0])
}

func knockCertificate(t *testing.T) tls.Certificate {
	t.Helper()
	cert, err := tlsutil.NewCertificateInMemory("syncthing", 1)
	if err != nil {
		t.Fatal(err)
	}
	return cert
}

func knockWith(t *testing.T, address string, cert tls.Certificate) {
	t.Helper()
	conn, err := tls.Dial("tcp", address, &tls.Config{
		Certificates: []tls.Certificate{cert}, InsecureSkipVerify: true, NextProtos: []string{"bep/1.0"}, //nolint:gosec
	})
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	hello := protocol.Hello{
		DeviceName: "Stranger", ClientName: "syncthing", ClientVersion: "v2.1.7", Timestamp: time.Now().UnixNano(),
	}
	_ = conn.SetDeadline(time.Now().Add(engineActionTimeout))
	_, _ = protocol.ExchangeHello(conn, hello)
}

func freeLocalAddress(t *testing.T) string {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	return listener.Addr().String()
}

func TestEngineHandlesARequestToConnect(t *testing.T) {
	address := freeLocalAddress(t)
	clt, recorder, _ := startTestEngineWith(t, func(cfg *config.Configuration) {
		cfg.Options.RawListenAddresses = []string{fmt.Sprintf("tcp://%s", address)}
	})
	waitUntil(t, "the engine takes the connection", func() bool {
		if conn, err := net.DialTimeout("tcp", address, time.Second); err == nil {
			_ = conn.Close()
			return true
		}
		return false
	})
	stranger := knockAsUnknownDevice(t, address)
	waitUntil(t, "the request shows", func() bool {
		pending, err := clt.PendingDevices()
		return err == nil && pending.Count() == 1 && recorder.pendingDeviceChanges() > 0
	})
	pending, _ := clt.PendingDevices()
	request := pending.ItemAt(0)
	if request.DeviceID != stranger.String() || request.Name != "Stranger" || request.Address == "" ||
		request.Time().UnixMilliseconds() == 0 {
		t.Fatalf("got %+v", request)
	}

	// Dismissing it forgets it, until the device tries again
	if err := clt.DismissPendingDevice(stranger.String()); err != nil {
		t.Fatal(err)
	}
	if pending, _ = clt.PendingDevices(); pending.Count() != 0 {
		t.Fatalf("got %d", pending.Count())
	}
	knockAsUnknownDevice(t, address)
	waitUntil(t, "another device shows", func() bool {
		pending, err := clt.PendingDevices()
		return err == nil && pending.Count() == 1
	})

	// Ignoring one keeps what is known of it, and ends the request
	pending, _ = clt.PendingDevices()
	ignoredID := pending.ItemAt(0).DeviceID
	if err := clt.IgnoreDevice(ignoredID); err != nil {
		t.Fatal(err)
	}
	if err := clt.IgnoreDevice(ignoredID); err != nil {
		t.Fatal(err)
	}
	waitUntil(t, "the request ended", func() bool {
		pending, err := clt.PendingDevices()
		return err == nil && pending.Count() == 0
	})
	ignored := clt.IgnoredDevices()
	if ignored.Count() != 1 || ignored.ItemAt(0).DeviceID != ignoredID || ignored.ItemAt(0).Name != "Stranger" {
		t.Fatalf("got %d", ignored.Count())
	}

	// An ignored device doesn't come back as a request
	strangerCert := knockCertificate(t)
	if err := clt.IgnoreDevice(protocol.NewDeviceID(strangerCert.Certificate[0]).String()); err != nil {
		t.Fatal(err)
	}
	knockWith(t, address, strangerCert)
	if pending, err := clt.PendingDevices(); err != nil || pending.Count() != 0 {
		t.Fatalf("got %+v, %v", pending, err)
	}
	if err := clt.UnignoreDevice(protocol.NewDeviceID(strangerCert.Certificate[0]).String()); err != nil {
		t.Fatal(err)
	}

	if err := clt.UnignoreDevice(ignoredID); err != nil {
		t.Fatal(err)
	}
	if clt.IgnoredDevices().Count() != 0 {
		t.Fatal("still ignored")
	}

	// Adding a device takes the name it gave
	knockAsUnknownDevice(t, address)
	waitUntil(t, "a device shows", func() bool {
		pending, err := clt.PendingDevices()
		return err == nil && pending.Count() == 1
	})
	pending, _ = clt.PendingDevices()
	addedID := pending.ItemAt(0).DeviceID
	if err := clt.AddPeer(addedID); err != nil {
		t.Fatal(err)
	}
	added := clt.PeerWithID(addedID)
	if added == nil || added.Name() != "Stranger" {
		t.Fatalf("got %+v", added)
	}
}
