// Copyright (C) 2026 Gordon
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this file,
// You can obtain one at https://mozilla.org/MPL/2.0/.

//go:build fpintegration

// Switches a folder to on demand next to a real peer. Run with: go test -tags noassets,fpintegration -run OnDemand ./src
package sushitrain

import (
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/syncthing/syncthing/lib/config"
)

type onDemandDevice struct {
	client *Client
	root   string
	listen string
}

func newOnDemandDevice(t *testing.T) *onDemandDevice {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	listen := fmt.Sprintf("tcp://%s", l.Addr())
	l.Close()
	client, root := newIntegrationClient(t, func(cfg *config.Configuration) {
		cfg.Options.RawListenAddresses = []string{listen}
	})
	return &onDemandDevice{client: client, root: root, listen: listen}
}

func (d *onDemandDevice) write(t *testing.T, name, contents string) {
	t.Helper()
	full := filepath.Join(d.root, name)
	if err := os.MkdirAll(filepath.Dir(full), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(full, []byte(contents), 0o600); err != nil {
		t.Fatal(err)
	}
}

func (d *onDemandDevice) has(name string) bool {
	_, err := os.Stat(filepath.Join(d.root, name))
	return err == nil
}

func connectOnDemand(t *testing.T, a, b *onDemandDevice) {
	t.Helper()
	for _, pair := range [][2]*onDemandDevice{{a, b}, {b, a}} {
		self, peer := pair[0], pair[1]
		waiter, err := self.client.run.Load().config.Modify(func(c *config.Configuration) {
			c.SetDevice(config.DeviceConfiguration{DeviceID: peer.client.deviceID(), Name: "peer", Addresses: []string{peer.listen}})
			folder, _, _ := c.Folder("test")
			folder.Devices = append(folder.Devices, config.FolderDeviceConfiguration{DeviceID: peer.client.deviceID()})
			c.SetFolder(folder)
		})
		if err != nil {
			t.Fatal(err)
		}
		waiter.Wait()
	}
}

func waitForOnDemand(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(60 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func TestOnDemandSwitchKeepsWhatOnlyThisDeviceHas(t *testing.T) {
	mac, peer := newOnDemandDevice(t), newOnDemandDevice(t)
	// The peer has an entry for this file at the global version, but not the file
	if err := os.WriteFile(filepath.Join(peer.root, ".stignore"), []byte("not-on-the-peer.txt\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	mac.write(t, "shared.txt", "both have this")
	mac.write(t, "dir/shared too.txt", "both have this too")
	mac.write(t, "not-on-the-peer.txt", "only here")
	folder, peerFolder := mac.client.FolderWithID("test"), peer.client.FolderWithID("test")
	if err := folder.ScanPaths(List([]string{""})); err != nil {
		t.Fatal(err)
	}
	if err := peerFolder.ScanPaths(List([]string{""})); err != nil {
		t.Fatal(err)
	}
	connectOnDemand(t, mac, peer)
	waitForOnDemand(t, "the peer", func() bool {
		if !peer.has("shared.txt") || !peer.has("dir/shared too.txt") {
			return false
		}
		state, err := folder.State()
		return err == nil && state == "idle" && mac.client.internals().IsConnectedTo(peer.client.deviceID())
	})
	internals := peer.client.internals()
	waitForOnDemand(t, "the peer's entry for what it ignores", func() bool {
		_ = peerFolder.ScanPaths(List([]string{""}))
		file, ok, err := internals.LocalFileInfo("test", "not-on-the-peer.txt")
		if err != nil || !ok || !file.IsInvalid() {
			return false
		}
		// And that this device has heard of it: it is that entry that must not count as having the file
		sequence, err := internals.LocalSequence("test")
		if err != nil {
			return false
		}
		completion, err := mac.client.internals().Completion(peer.client.deviceID(), "test")
		return err == nil && completion.Sequence >= sequence
	})
	waitForOnDemand(t, "the peer's index on this device", func() bool {
		global, ok, err := mac.client.internals().GlobalFileInfo("test", "shared.txt")
		if err != nil || !ok || len(global.Blocks) == 0 {
			return false
		}
		available, err := mac.client.internals().BlockAvailability("test", global, global.Blocks[0])
		return err == nil && len(available) > 0
	})

	kept, err := folder.SwitchToOnDemand()
	if err != nil {
		t.Fatal(err)
	}
	if kept != 1 {
		t.Fatalf("kept %d files", kept)
	}
	if onDemand, err := folder.IsOnDemand(); err != nil || !onDemand {
		t.Fatalf("on demand: %v %v", onDemand, err)
	}
	if mac.has("shared.txt") || mac.has("dir/shared too.txt") {
		t.Fatal("files the peer has are still here")
	}
	if !mac.has("not-on-the-peer.txt") {
		t.Fatal("the only copy of a file is gone")
	}
	selected, err := folder.SelectedPaths(false)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(selected.data, []string{"not-on-the-peer.txt"}) {
		t.Fatalf("selected %q", selected.data)
	}
	// It stays offered: this device still has the only copy, and the index says so after a scan
	if err := folder.ScanPaths(List([]string{""})); err != nil {
		t.Fatal(err)
	}
	waitForOnDemand(t, "the scan", func() bool {
		file, ok, err := mac.client.internals().LocalFileInfo("test", "not-on-the-peer.txt")
		return err == nil && ok && !file.IsInvalid() && !file.Deleted
	})
	if !peer.has("shared.txt") {
		t.Fatal("the peer lost a file")
	}

	if err := folder.ClearSelection(false); !errors.Is(err, errOnDemandSelection) {
		t.Fatalf("clearing the selection: %v", err)
	}
	if err := folder.SetLocalFileExplicitlySelected("not-on-the-peer.txt", false); !errors.Is(err, errOnDemandSelection) {
		t.Fatalf("deselecting: %v", err)
	}
	if !mac.has("not-on-the-peer.txt") {
		t.Fatal("the only copy of a file is gone")
	}

	// Switching again changes nothing
	if kept, err := folder.SwitchToOnDemand(); err != nil || kept != 0 {
		t.Fatalf("switching again: %d %v", kept, err)
	}

	// Back to all files: the engine gets them back
	if err := folder.SetSelective(false); err != nil {
		t.Fatal(err)
	}
	waitForOnDemand(t, "the files back", func() bool {
		_ = folder.ScanPaths(List([]string{""}))
		return mac.has("shared.txt") && mac.has("dir/shared too.txt")
	})
}

func TestOnDemandSwitchRefusesPatternsItCannotKeep(t *testing.T) {
	mac, peer := newOnDemandDevice(t), newOnDemandDevice(t)
	if err := os.WriteFile(filepath.Join(mac.root, ".stignore"), []byte("node_modules\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	mac.write(t, "shared.txt", "both have this")
	mac.write(t, "node_modules/secret", "never leaves this device")
	folder := mac.client.FolderWithID("test")
	if err := folder.ScanPaths(List([]string{""})); err != nil {
		t.Fatal(err)
	}
	connectOnDemand(t, mac, peer)
	waitForOnDemand(t, "the peer", func() bool { return peer.has("shared.txt") })
	if _, err := folder.SwitchToOnDemand(); err == nil {
		t.Fatal("switched a folder whose ignore patterns would stop applying")
	}
	if onDemand, _ := folder.IsOnDemand(); onDemand {
		t.Fatal("the folder is on demand")
	}
	lines, _, err := mac.client.internals().Ignores("test")
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(lines, []string{"node_modules"}) {
		t.Fatalf("patterns %q", lines)
	}
}

// Files that came from the peer, as opposed to the ones above: the switch is about who holds them now
func TestOnDemandSwitchRemovesWhatCameFromThePeer(t *testing.T) {
	mac, peer := newOnDemandDevice(t), newOnDemandDevice(t)
	peer.write(t, "from the peer.txt", "came from the peer")
	peer.write(t, "dir/nested.txt", "nested")
	peerFolder, folder := peer.client.FolderWithID("test"), mac.client.FolderWithID("test")
	if err := peerFolder.ScanPaths(List([]string{""})); err != nil {
		t.Fatal(err)
	}
	connectOnDemand(t, mac, peer)
	waitForOnDemand(t, "the files here", func() bool {
		if !mac.has("from the peer.txt") || !mac.has("dir/nested.txt") {
			return false
		}
		state, err := folder.State()
		return err == nil && state == "idle" && mac.client.internals().IsConnectedTo(peer.client.deviceID())
	})
	kept, err := folder.SwitchToOnDemand()
	if err != nil {
		t.Fatal(err)
	}
	if kept != 0 || mac.has("from the peer.txt") || mac.has("dir/nested.txt") {
		t.Fatalf("kept %d, files here: %v %v", kept, mac.has("from the peer.txt"), mac.has("dir/nested.txt"))
	}
}

func TestOnDemandSwitchWaitsForUnsentDeletions(t *testing.T) {
	mac, peer, away := newOnDemandDevice(t), newOnDemandDevice(t), newOnDemandDevice(t)
	mac.write(t, "deleted.txt", "gone soon")
	mac.write(t, "kept.txt", "stays")
	folder := mac.client.FolderWithID("test")
	if err := folder.ScanPaths(List([]string{""})); err != nil {
		t.Fatal(err)
	}
	connectOnDemand(t, mac, peer)
	connectOnDemand(t, mac, away)
	waitForOnDemand(t, "both devices", func() bool { return peer.has("kept.txt") && away.has("kept.txt") })

	// One device is away when a file is deleted here: only this device can still tell it
	waiter, err := mac.client.run.Load().config.Modify(func(c *config.Configuration) {
		device, _, _ := c.Device(away.client.deviceID())
		device.Paused = true
		c.SetDevice(device)
	})
	if err != nil {
		t.Fatal(err)
	}
	waiter.Wait()
	waitForOnDemand(t, "the device away", func() bool { return !mac.client.internals().IsConnectedTo(away.client.deviceID()) })
	if err := os.Remove(filepath.Join(mac.root, "deleted.txt")); err != nil {
		t.Fatal(err)
	}
	if err := folder.ScanPaths(List([]string{""})); err != nil {
		t.Fatal(err)
	}
	waitForOnDemand(t, "the deletion on the peer", func() bool { return !peer.has("deleted.txt") })
	if _, err := folder.SwitchToOnDemand(); err == nil {
		t.Fatal("switched with a deletion that a device hasn't got")
	}
	if onDemand, _ := folder.IsOnDemand(); onDemand {
		t.Fatal("the folder is on demand")
	}
}

func TestOnDemandGlobalIgnoresReadLikeTheEngine(t *testing.T) {
	mac := newOnDemandDevice(t)
	if err := os.WriteFile(filepath.Join(mac.root, ".stignore"),
		[]byte("(?d).DS_Store\n(?d)node_modules\n!*.sync-conflict-*\n*\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	ignored, err := mac.client.FolderWithID("test").globalIgnores()
	if err != nil {
		t.Fatal(err)
	}
	for path, want := range map[string]bool{
		".DS_Store": true, "a/.DS_Store": true, "node_modules": true, "node_modules/x/y.js": true, "a/node_modules/z": true,
		"notes.txt": false, "a/notes.txt": false, "not_node_modules": false,
	} {
		if got := ignored(path); got != want {
			t.Errorf("%s: %v", path, got)
		}
	}
}

// What the service does with a real engine's selection: gateway/ondemand_test.go covers when it does it
func TestOnDemandKeepAndDeselect(t *testing.T) {
	mac := newOnDemandDevice(t)
	if err := os.WriteFile(filepath.Join(mac.root, ".stignore"),
		[]byte("(?d).DS_Store\n!*.sync-conflict-*\n!/selected\n*\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	folder := mac.client.FolderWithID("test")
	var recorded []string
	record := func(added []string) error {
		recorded = append(recorded, added...)
		return nil
	}
	if err := folder.keepSelected([]string{"selected/child", "written/file", "written/other"}, record); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(recorded, []string{"written/file", "written/other"}) {
		t.Fatalf("recorded %q", recorded)
	}
	// The line that was there isn't for the service to take away, and a record that failed selects nothing
	failed := errors.New("could not record")
	if err := folder.keepSelected([]string{"unrecorded"}, func([]string) error { return failed }); !errors.Is(err, failed) {
		t.Fatalf("failing to record: %v", err)
	}
	if onDemand, err := folder.IsOnDemand(); !onDemand || err != nil {
		t.Fatalf("on demand: %v %v", onDemand, err)
	}
	ignored, err := folder.deselectKept([]string{"written/file", "selected/child"})
	if err != nil {
		t.Fatal(err)
	}
	for path, want := range map[string]bool{
		"written/file": true, "written/other": false, "unrecorded": true, "selected/child": false,
		"written/x.sync-conflict-20260101-000000-ABCDEFG.txt": false, ".DS_Store": true,
	} {
		if got := ignored(path); got != want {
			t.Errorf("%s: %v", path, got)
		}
	}
	data, err := os.ReadFile(filepath.Join(mac.root, ".stignore"))
	if err != nil {
		t.Fatal(err)
	}
	if want := "(?d).DS_Store\n!*.sync-conflict-*\n!/selected\n!/written/other\n*\n"; string(data) != want {
		t.Fatalf("patterns %q", data)
	}
	// Nothing to take out: it only says what the patterns ignore
	if ignored, err := folder.deselectKept(nil); err != nil || ignored("written/other") {
		t.Fatalf("reading the patterns: %v", err)
	}
	if err := folder.SetSelective(false); err != nil {
		t.Fatal(err)
	}
	if _, err := folder.deselectKept([]string{"written/other"}); err == nil {
		t.Fatal("deselected in a folder that isn't on demand")
	}
}
