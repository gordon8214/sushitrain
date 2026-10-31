// Copyright (C) 2026 Gordon
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this file,
// You can obtain one at https://mozilla.org/MPL/2.0/.

package gateway

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"path/filepath"
	"slices"
	"sync"
	"time"

	"github.com/syncthing/syncthing/lib/config"
	"github.com/syncthing/syncthing/lib/events"
	"github.com/syncthing/syncthing/lib/fs"
	"github.com/syncthing/syncthing/lib/osutil"
	"github.com/syncthing/syncthing/lib/protocol"
	"github.com/syncthing/syncthing/lib/syncthing"
)

// FolderInfo describes a Syncthing folder as the service needs it
type FolderInfo struct {
	ID    string
	Label string
	// config.FolderType's text form: sendreceive, sendonly, receiveonly or receiveencrypted
	Type   string
	Paused bool
	// Whether this device holds only a selection of the folder
	Selective bool
	// Whether the folder is selective, and clients may change what this device doesn't hold (see ondemand.go)
	OnDemand bool
	// The folder's directory, when its filesystem is the native one ("basic" or a wrapper of it)
	NativePath     string
	FilesystemType string
	MarkerName     string
	ModTimeWindow  time.Duration
	IgnorePerms    bool
	// The type of the folder's versioning ("" without)
	Versioning string
	// Whether the versioning keeps versions somewhere other than the default directory in the folder, whose versions
	// the service doesn't serve
	CustomVersionsPath bool
}

// GlobalEntry is a global index entry without blocks, as iterated by name
type GlobalEntry struct {
	Name     string
	Sequence int64
	Type     protocol.FileInfoType
	Deleted  bool
	Invalid  bool
}

// RemoteFileBackend is what the service needs from the Syncthing engine of its host
type RemoteFileBackend interface {
	DeviceID() protocol.DeviceID
	Folder(id string) (FolderInfo, bool)
	FolderState(id string) string
	// IndexID and LocalSequence identify the state of this device's index of a folder, to detect a reset or restore
	IndexID(folder string) (protocol.IndexID, error)
	LocalSequence(folder string) (int64, error)
	// GlobalEntries calls fn for every global index entry of the folder
	GlobalEntries(folder string, fn func(GlobalEntry) error) error
	GlobalFile(folder, name string) (protocol.FileInfo, bool, error)
	LocalFile(folder, name string) (protocol.FileInfo, bool, error)
	// BlockAvailable reports whether a connected device can provide the block
	BlockAvailable(folder string, file protocol.FileInfo, index int) bool
	// FetchBlock gets a block from a connected device. The result is verified against the block's hash.
	FetchBlock(ctx context.Context, folder string, file protocol.FileInfo, index int) ([]byte, error)
	// RunInFolderLoop runs fn where it cannot race the folder's scanner and puller. It stops waiting for that when ctx
	// is done; fn's context also ends then.
	RunInFolderLoop(ctx context.Context, folder string, fn func(context.Context, syncthing.FolderHandle) error) error
	// FileVersions returns the versions of a file the folder's versioner keeps, newest first. It fails with
	// syncthing.ErrNoVersioner or syncthing.ErrVersionsNotSupported for folders whose versions cannot be listed, or are
	// not kept in the default versions directory (see versionsServed).
	FileVersions(folder, name string) ([]syncthing.FileVersion, error)
	// OpenFileVersion opens the version of a file archived at versionTime, or fails with
	// syncthing.ErrVersionNotFound. It doesn't wait for the folder loop.
	OpenFileVersion(folder, name string, versionTime time.Time) (fs.File, error)
	// KeepSelected selects paths of an on-demand folder, so that the engine goes on offering what the service wrote
	// there. It runs in RunInFolderLoop and doesn't wait for the folder. Paths selected already change nothing; it calls
	// record with the others, before it selects them.
	KeepSelected(folder string, paths []string, record func(added []string) error) error
	// DeselectKept takes paths out of the selection of an on-demand folder, and returns what its patterns ignore after
	// that, for names in the form a scan matches them with. What a parent selects stays selected. It runs in
	// RunInFolderLoop and doesn't wait for the folder.
	DeselectKept(folder string, paths []string) (func(name string) bool, error)
	// Peers returns the other devices the folder is shared with
	Peers(folder string) []protocol.DeviceID
	// PeerFile returns the index entry another device last announced for a file
	PeerFile(folder string, device protocol.DeviceID, name string) (protocol.FileInfo, bool, error)
	// IsConnected reports whether this device is connected to another one
	IsConnected(device protocol.DeviceID) bool
	// GlobalIgnores returns what excludes a path from an on-demand folder, whether it is selected or not
	GlobalIgnores(folder string) (func(name string) bool, error)
	// ScanPaths has the given paths scanned soon
	ScanPaths(folder string, paths []string)
	// Watch calls fn with a folder ID whenever that folder's index may have changed, until stop is called
	Watch(fn func(folder string)) (stop func())
}

var (
	errNoPeer        = errors.New("no connected device has the block")
	errBlockMismatch = errors.New("block does not match its hash")
)

// verifyBlock checks a block's contents against its hash. Blocks are never trusted unverified, whether read from
// disk or received from a peer.
func verifyBlock(data []byte, block protocol.BlockInfo) error {
	if len(data) != block.Size {
		return fmt.Errorf("%w: %d bytes, expected %d", errBlockMismatch, len(data), block.Size)
	}
	sum := sha256.Sum256(data)
	if !bytes.Equal(sum[:], block.Hash) {
		return errBlockMismatch
	}
	return nil
}

// EngineBackend implements RemoteFileBackend on a running Syncthing app
type EngineBackend struct {
	App    *syncthing.App
	Config config.Wrapper
	Events events.Logger
	ID     protocol.DeviceID
	// Optional: fetches a block from peers (for instance with the host's peer selection). Results are verified.
	Fetch func(ctx context.Context, folder string, file protocol.FileInfo, index int) ([]byte, error)
	// Optional: whether the folder holds only a selection of its files on this device
	IsSelective func(folderID string) bool
	// Optional, for hosts with on-demand folders: all four, or none
	IsOnDemand        func(folderID string) bool
	Select            func(folderID string, paths []string, record func(added []string) error) error
	Deselect          func(folderID string, paths []string) (func(name string) bool, error)
	LoadGlobalIgnores func(folderID string) (func(name string) bool, error)

	scanMutex sync.Mutex
	scans     map[string]map[string]struct{}
}

func (b *EngineBackend) DeviceID() protocol.DeviceID {
	return b.ID
}

func (b *EngineBackend) Folder(id string) (FolderInfo, bool) {
	fc, ok := b.Config.Folder(id)
	if !ok {
		return FolderInfo{}, false
	}
	info := FolderInfo{
		ID:             fc.ID,
		Label:          fc.Label,
		Type:           fc.Type.String(),
		Paused:         fc.Paused,
		FilesystemType: string(fc.FilesystemType),
		MarkerName:     fc.MarkerName,
		ModTimeWindow:  fc.ModTimeWindow(),
		IgnorePerms:    fc.IgnorePerms,
		Versioning:     fc.Versioning.Type,
	}
	info.CustomVersionsPath = info.Versioning != "" && !versionsServed(fc)
	if info.Label == "" {
		info.Label = fc.ID
	}
	if isNativeFilesystemType(info.FilesystemType) {
		if path, err := filepath.Abs(fc.Path); err == nil {
			info.NativePath = path
		}
	}
	if b.IsSelective != nil {
		info.Selective = b.IsSelective(fc.ID)
	}
	if info.Selective && b.IsOnDemand != nil {
		info.OnDemand = b.IsOnDemand(fc.ID)
	}
	return info, true
}

// isNativeFilesystemType reports whether a folder of this filesystem type is a plain directory on disk
func isNativeFilesystemType(fsType string) bool {
	return fsType == string(config.FilesystemTypeBasic) || fsType == "basic-trash"
}

func (b *EngineBackend) FolderState(id string) string {
	state, _, err := b.App.Internals.FolderState(id)
	if err != nil {
		return "error"
	}
	return state
}

func (b *EngineBackend) IndexID(folder string) (protocol.IndexID, error) {
	return b.App.Internals.LocalIndexID(folder)
}

func (b *EngineBackend) LocalSequence(folder string) (int64, error) {
	return b.App.Internals.LocalSequence(folder)
}

func (b *EngineBackend) GlobalEntries(folder string, fn func(GlobalEntry) error) error {
	entries, errFn := b.App.Internals.AllGlobalFiles(folder)
	for meta := range entries {
		entry := GlobalEntry{
			// Syncthing returns names in the platform's form (NFD on macOS); item paths use its normalized form,
			// which is also the order entries come in
			Name:     osutil.NormalizedFilename(meta.Name),
			Sequence: meta.Sequence,
			Type:     meta.Type,
			Deleted:  meta.Deleted,
			Invalid:  meta.LocalFlags&protocol.LocalInvalidFlags != 0,
		}
		if err := fn(entry); err != nil {
			return err
		}
	}
	return errFn()
}

func (b *EngineBackend) GlobalFile(folder, name string) (protocol.FileInfo, bool, error) {
	return b.App.Internals.GlobalFileInfo(folder, name)
}

func (b *EngineBackend) LocalFile(folder, name string) (protocol.FileInfo, bool, error) {
	return b.App.Internals.LocalFileInfo(folder, name)
}

func (b *EngineBackend) BlockAvailable(folder string, file protocol.FileInfo, index int) bool {
	availables, err := b.App.Internals.BlockAvailability(folder, file, file.Blocks[index])
	if err != nil {
		return false
	}
	for _, available := range availables {
		if b.App.Internals.IsConnectedTo(available.ID) {
			return true
		}
	}
	return false
}

func (b *EngineBackend) FetchBlock(ctx context.Context, folder string, file protocol.FileInfo, index int) ([]byte, error) {
	block := file.Blocks[index]
	if b.Fetch != nil {
		data, err := b.Fetch(ctx, folder, file, index)
		if err != nil {
			return nil, err
		}
		return data, verifyBlock(data, block)
	}
	availables, err := b.App.Internals.BlockAvailability(folder, file, block)
	if err != nil {
		return nil, err
	}
	lastErr := errNoPeer
	for _, available := range availables {
		if !b.App.Internals.IsConnectedTo(available.ID) {
			continue
		}
		data, err := b.App.Internals.DownloadBlock(ctx, available.ID, folder, file.Name, index, block,
			available.FromTemporary)
		if err == nil {
			if err = verifyBlock(data, block); err == nil {
				return data, nil
			}
		}
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		lastErr = err
	}
	return nil, lastErr
}

func (b *EngineBackend) RunInFolderLoop(ctx context.Context, folder string,
	fn func(context.Context, syncthing.FolderHandle) error,
) error {
	return b.App.Internals.RunInFolderLoop(ctx, folder, fn)
}

// versionsServed reports whether the service serves a folder's versions: only from the default versions directory in
// the folder. A custom path may be shared with another folder or point into one, whose files a client without access
// to that folder would then see.
func versionsServed(fc config.FolderConfiguration) bool {
	return fc.Versioning.FSPath == ""
}

func (b *EngineBackend) FileVersions(folder, name string) ([]syncthing.FileVersion, error) {
	if fc, ok := b.Config.Folder(folder); ok && !versionsServed(fc) {
		return nil, syncthing.ErrVersionsNotSupported
	}
	return b.App.Internals.FileVersions(folder, name)
}

func (b *EngineBackend) OpenFileVersion(folder, name string, versionTime time.Time) (fs.File, error) {
	if fc, ok := b.Config.Folder(folder); ok && !versionsServed(fc) {
		return nil, syncthing.ErrVersionsNotSupported
	}
	return b.App.Internals.OpenFileVersion(folder, name, versionTime)
}

func (b *EngineBackend) KeepSelected(folder string, paths []string, record func(added []string) error) error {
	if b.Select == nil {
		return errors.New("this host cannot select paths")
	}
	return b.Select(folder, paths, record)
}

func (b *EngineBackend) DeselectKept(folder string, paths []string) (func(name string) bool, error) {
	if b.Deselect == nil {
		return nil, errors.New("this host cannot deselect paths")
	}
	return b.Deselect(folder, paths)
}

func (b *EngineBackend) Peers(folder string) []protocol.DeviceID {
	fc, ok := b.Config.Folder(folder)
	if !ok {
		return nil
	}
	return slices.DeleteFunc(fc.DeviceIDs(), func(id protocol.DeviceID) bool { return id == b.ID })
}

func (b *EngineBackend) PeerFile(folder string, device protocol.DeviceID, name string) (protocol.FileInfo, bool, error) {
	return b.App.Internals.DeviceFileInfo(folder, device, name)
}

func (b *EngineBackend) IsConnected(device protocol.DeviceID) bool {
	return b.App.Internals.IsConnectedTo(device)
}

func (b *EngineBackend) GlobalIgnores(folder string) (func(name string) bool, error) {
	if b.LoadGlobalIgnores == nil {
		return nil, errors.New("this host cannot check ignore patterns")
	}
	return b.LoadGlobalIgnores(folder)
}

// ScanPaths queues the paths and scans them in the background, coalescing requests that arrive while a scan of the
// folder waits
func (b *EngineBackend) ScanPaths(folder string, paths []string) {
	b.scanMutex.Lock()
	if b.scans == nil {
		b.scans = make(map[string]map[string]struct{})
	}
	pending, running := b.scans[folder]
	if !running {
		pending = make(map[string]struct{})
		b.scans[folder] = pending
	}
	for _, p := range paths {
		pending[p] = struct{}{}
	}
	b.scanMutex.Unlock()
	if running {
		return
	}
	go func() {
		for {
			b.scanMutex.Lock()
			batch := b.scans[folder]
			if len(batch) == 0 {
				delete(b.scans, folder)
				b.scanMutex.Unlock()
				return
			}
			b.scans[folder] = make(map[string]struct{})
			b.scanMutex.Unlock()
			list := make([]string, 0, len(batch))
			for p := range batch {
				list = append(list, p)
			}
			_ = b.App.Internals.ScanFolderSubdirs(folder, list)
		}
	}()
}

func (b *EngineBackend) Watch(fn func(folder string)) func() {
	sub := b.Events.Subscribe(events.LocalIndexUpdated | events.RemoteIndexUpdated | events.FolderPaused |
		events.FolderResumed)
	done := make(chan struct{})
	go func() {
		for {
			select {
			case <-done:
				return
			case ev, ok := <-sub.C():
				if !ok {
					return
				}
				if data, ok := ev.Data.(map[string]any); ok {
					if folder, ok := data["folder"].(string); ok {
						fn(folder)
					}
				} else if data, ok := ev.Data.(map[string]string); ok {
					if folder, ok := data["id"]; ok {
						fn(folder)
					}
				}
			}
		}
	}()
	var once sync.Once
	return func() {
		once.Do(func() {
			close(done)
			sub.Unsubscribe()
		})
	}
}
