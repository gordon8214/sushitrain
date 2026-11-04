// Copyright (C) 2026 Gordon
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this file,
// You can obtain one at https://mozilla.org/MPL/2.0/.
package sushitrain

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/syncthing/syncthing/lib/config"
	"github.com/syncthing/syncthing/lib/fs"
	"github.com/syncthing/syncthing/lib/protocol"
)

func TestFinderPaths(t *testing.T) {
	for _, path := range []string{"../escape", "a/../b", "/absolute", "a//b", ".stfolder", ".stversions/v", ".syncthing.file.tmp"} {
		if finderPathAllowed(path) {
			t.Errorf("allowed %q", path)
		}
	}
	for _, path := range []string{"", "a/b.txt", "space %?#/café", `literal\name.txt`, "a/.stfolder", "a/.stignore", "a/.stversions/file"} {
		if runtime.GOOS == "windows" && path == `literal\name.txt` {
			if finderPathAllowed(path) {
				t.Error("accepted a Windows separator as a literal filename")
			}
			continue
		}
		if !finderPathAllowed(path) {
			t.Errorf("refused %q", path)
		}
	}
}

type finderTestMtimeMapping struct{ physical, virtual time.Time }

func (m *finderTestMtimeMapping) GetMtime(string, string) (time.Time, time.Time) {
	return m.physical, m.virtual
}
func (m *finderTestMtimeMapping) PutMtime(_ string, _ string, physical, virtual time.Time) error {
	m.physical, m.virtual = physical, virtual
	return nil
}
func (*finderTestMtimeMapping) DeleteMtime(string, string) error { return nil }

func TestFinderUsesVirtualModificationTimes(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "file")
	if err := os.WriteFile(path, []byte("current"), 0o600); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	mapping := &finderTestMtimeMapping{physical: info.ModTime(), virtual: info.ModTime().Add(time.Second)}
	filesystem := fs.NewFilesystem(fs.FilesystemTypeBasic, root, fs.NewMtimeOption(mapping, "root"))
	items := []finderItem{{"root", "file"}}
	fc := config.FolderConfiguration{}
	metadata := finderDiskMetadata(t.Context(), filesystem, items, "root", fc)
	current, ok := metadata["file"]
	if !ok || !current.ModTime().Equal(mapping.virtual) {
		t.Fatalf("virtual time lost: %+v", metadata)
	}
	current.Version = protocol.Vector{Counters: []protocol.Counter{{ID: 1, Value: 1}}}
	if got := finderFileBadge(current, current, true, true, metadata["file"], fc, "current", false, false); got != "current" {
		t.Fatalf("virtual timestamp: %q", got)
	}
	physical := current
	physical.ModifiedS, physical.ModifiedNs = info.ModTime().Unix(), int32(info.ModTime().Nanosecond())
	if got := finderFileBadge(current, current, true, true, physical, fc, "current", false, false); got != "waiting" {
		t.Fatalf("physical timestamp unexpectedly accepted: %q", got)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if got := finderDiskMetadata(ctx, filesystem, items, "root", fc); len(got) != 0 {
		t.Fatal("canceled metadata read returned items")
	}
}

func TestFinderFileStatus(t *testing.T) {
	path := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(path, []byte("original"), 0o600); err != nil {
		t.Fatal(err)
	}
	disk, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	current := protocol.FileInfo{Permissions: uint32(disk.Mode().Perm()), Name: "file", Size: disk.Size(), ModifiedS: disk.ModTime().Unix(), ModifiedNs: int32(disk.ModTime().Nanosecond()), Version: protocol.Vector{Counters: []protocol.Counter{{ID: 1, Value: 1}}}}
	for _, test := range []struct {
		name                string
		local               protocol.FileInfo
		global              protocol.FileInfo
		indexed             bool
		summary             string
		failed, downloading bool
		folderType          config.FolderType
		want                string
	}{
		{"current", current, current, true, "current", false, false, config.FolderTypeSendReceive, "current"},
		{"unindexed", current, current, false, "current", false, false, config.FolderTypeSendReceive, "waiting"},
		{"paused", current, current, true, "paused", false, false, config.FolderTypeSendReceive, "paused"},
		{"failed", current, current, true, "current", true, false, config.FolderTypeSendReceive, "attention"},
		{"downloading", current, current, true, "syncing", false, true, config.FolderTypeSendReceive, "syncing"},
		{"stopped", current, current, true, "", false, false, config.FolderTypeSendReceive, ""},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got := finderFileBadge(test.local, test.global, test.indexed, true, current, config.FolderConfiguration{Type: test.folderType}, test.summary, test.failed, test.downloading); got != test.want {
				t.Fatalf("got %q, want %q", got, test.want)
			}
		})
	}
	newer := current
	newer.Version = protocol.Vector{Counters: []protocol.Counter{{ID: 1, Value: 2}}}
	if got := finderFileBadge(current, newer, true, true, current, config.FolderConfiguration{Type: config.FolderTypeSendReceive}, "current", false, false); got != "waiting" {
		t.Fatal(got)
	}
	if got := finderFileBadge(current, newer, true, true, current, config.FolderConfiguration{Type: config.FolderTypeSendOnly}, "current", false, false); got != "attention" {
		t.Fatal(got)
	}
	changed := current
	changed.LocalFlags = protocol.FlagLocalReceiveOnly
	if got := finderFileBadge(changed, current, true, true, current, config.FolderConfiguration{Type: config.FolderTypeReceiveOnly}, "current", false, false); got != "attention" {
		t.Fatal(got)
	}
	ignored := current
	metadata := current
	metadata.ModifiedS++
	settings := config.FolderConfiguration{RawModTimeWindowS: 2}
	if got := finderFileBadge(current, current, true, true, metadata, settings, "current", false, false); got != "current" {
		t.Fatalf("timestamp window: %q", got)
	}
	metadata = current
	metadata.Permissions = 0o400
	if got := finderFileBadge(current, current, true, true, metadata, settings, "current", false, false); got != "waiting" {
		t.Fatalf("unscanned chmod: %q", got)
	}
	settings.IgnorePerms = true
	if got := finderFileBadge(current, current, true, true, metadata, settings, "current", false, false); got != "current" {
		t.Fatalf("ignored permissions: %q", got)
	}
	metadata = current
	metadata.Type = protocol.FileInfoTypeSymlink
	metadata.SymlinkTarget = []byte("elsewhere")
	if got := finderFileBadge(current, current, true, true, metadata, settings, "current", false, false); got != "waiting" {
		t.Fatalf("unscanned type change: %q", got)
	}
	link := metadata
	metadata.SymlinkTarget = []byte("changed")
	if got := finderFileBadge(link, link, true, true, metadata, settings, "current", false, false); got != "waiting" {
		t.Fatalf("unscanned symlink change: %q", got)
	}
	ignored.LocalFlags = protocol.FlagLocalIgnored
	if got := finderFileBadge(ignored, current, true, true, current, config.FolderConfiguration{Type: config.FolderTypeSendReceive}, "current", false, false); got != "" {
		t.Fatal(got)
	}
	if err := os.WriteFile(path, []byte("unscanned new contents"), 0o600); err != nil {
		t.Fatal(err)
	}
	disk, err = os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := finderFileBadge(current, current, true, true, protocol.FileInfo{Name: "file", Size: disk.Size(), ModifiedS: disk.ModTime().Unix(), ModifiedNs: int32(disk.ModTime().Nanosecond())}, config.FolderConfiguration{Type: config.FolderTypeSendReceive}, "current", false, false); got != "waiting" {
		t.Fatalf("unscanned edit was %q", got)
	}
}
