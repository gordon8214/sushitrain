// Copyright (C) 2026 Gordon
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this file,
// You can obtain one at https://mozilla.org/MPL/2.0/.
package sushitrain

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"time"

	"github.com/syncthing/syncthing/lib/config"
	"github.com/syncthing/syncthing/lib/fs"
	"github.com/syncthing/syncthing/lib/osutil"
	"github.com/syncthing/syncthing/lib/syncthing"
	"github.com/syncthing/syncthing/lib/versioner"
)

// ClientDeviceID identifies this folder's engine when deduplicating history sources.
func (fld *Folder) ClientDeviceID() string { return fld.client.DeviceID() }

var errRestoreTargetChanged = errors.New("the destination changed; refresh before restoring")

type historyTarget struct {
	Root     string
	Exists   bool
	Hash     string
	Modified int64
	Size     int64
	Version  string
	Mode     uint32
	Identity string
}

func (fld *Folder) CanRestoreDownloadedVersion() bool {
	f := fld.folderConfiguration()
	return f != nil && !f.Paused && f.Type != config.FolderTypeReceiveEncrypted && isNativeFilesystem(f.FilesystemType)
}

func (fld *Folder) withHistoryTarget(name string, fn func(context.Context, syncthing.FolderHandle, string) error) error {
	if name == "" || !finderPathAllowed(name) || !fld.CanRestoreDownloadedVersion() {
		return errRestoreTargetChanged
	}
	internals := fld.client.internals()
	if internals == nil {
		return ErrStillLoading
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	return internals.RunInFolderLoop(ctx, fld.FolderID, func(ctx context.Context, h syncthing.FolderHandle) error {
		// The admitted handle may belong to a newly configured root. Never use a pre-admission path.
		root, err := historyHandleRoot(h)
		if err != nil {
			return err
		}
		if h.IsIgnored(name) {
			return errRestoreTargetChanged
		}
		if err := osutil.TraversesSymlink(h.Filesystem(), filepath.Dir(name)); err != nil {
			return err
		}
		return fn(ctx, h, root)
	})
}

func historyHandleRoot(h syncthing.FolderHandle) (string, error) {
	filesystem := h.Filesystem()
	if filesystem.Type() != fs.FilesystemTypeBasic {
		return "", errRestoreTargetChanged
	}
	return filesystem.URI(), nil
}

// Native identity and change time detect inode replacement and metadata-only edits; access time is deliberately excluded.
func historyFileIdentity(info os.FileInfo) string {
	value := reflect.Indirect(reflect.ValueOf(info.Sys()))
	if !value.IsValid() || value.Kind() != reflect.Struct {
		return ""
	}
	identity := ""
	for _, name := range []string{"Dev", "Ino", "Ctim", "Ctimespec", "CreationTime", "LastWriteTime"} {
		if field := value.FieldByName(name); field.IsValid() && field.CanInterface() {
			identity += fmt.Sprintf("%s=%v;", name, field.Interface())
		}
	}
	return identity
}

type historyContextReader struct {
	ctx    context.Context
	reader io.Reader
}

func (r historyContextReader) Read(p []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	return r.reader.Read(p)
}

func captureHistoryTarget(ctx context.Context, h syncthing.FolderHandle, root, name string) (historyTarget, error) {
	target := historyTarget{Root: root}
	if current, ok, err := h.CurrentFile(name); err != nil {
		return target, err
	} else if ok {
		target.Version = current.Version.String()
	}
	info, err := h.Filesystem().Lstat(name)
	if os.IsNotExist(err) {
		return target, nil
	}
	if err != nil {
		return target, err
	}
	if !info.IsRegular() {
		return target, errRestoreTargetChanged
	}
	before, err := os.Lstat(filepath.Join(root, name))
	if err != nil || !before.Mode().IsRegular() {
		return target, errRestoreTargetChanged
	}
	file, err := h.Filesystem().Open(name)
	if err != nil {
		return target, err
	}
	defer file.Close()
	hash := sha256.New()
	if _, err := io.Copy(hash, historyContextReader{ctx, file}); err != nil {
		return target, err
	}
	after, err := os.Lstat(filepath.Join(root, name))
	if err != nil || !os.SameFile(before, after) || historyFileIdentity(before) != historyFileIdentity(after) ||
		before.Size() != after.Size() || before.ModTime() != after.ModTime() || before.Mode() != after.Mode() {
		return target, errRestoreTargetChanged
	}
	target.Identity = historyFileIdentity(after)
	target.Mode = uint32(after.Mode()) & 0o777
	target.Exists, target.Hash, target.Modified, target.Size = true, hex.EncodeToString(hash.Sum(nil)), after.ModTime().UnixNano(), after.Size()
	return target, nil
}

// CaptureRestoreTarget snapshots both indexed state and actual bytes, including edits not yet scanned.
func (fld *Folder) CaptureRestoreTarget(name string) (string, error) {
	var result []byte
	err := fld.withHistoryTarget(name, func(ctx context.Context, h syncthing.FolderHandle, root string) error {
		target, err := captureHistoryTarget(ctx, h, root, name)
		if err != nil {
			return err
		}
		result, err = json.Marshal(target)
		return err
	})
	return string(result), err
}

// RestoreDownloadedVersion publishes a completed download while the folder's scanner and puller are held.
func (fld *Folder) RestoreDownloadedVersion(name, source, expected string, modifiedNs int64) error {
	var captured historyTarget
	if err := json.Unmarshal([]byte(expected), &captured); err != nil {
		return err
	}
	return fld.withHistoryTarget(name, func(ctx context.Context, h syncthing.FolderHandle, rootPath string) error {
		current, err := captureHistoryTarget(ctx, h, rootPath, name)
		if err != nil {
			return err
		}
		if captured != current {
			return errRestoreTargetChanged
		}
		input, err := os.Open(source)
		if err != nil {
			return err
		}
		defer input.Close()
		root, err := os.OpenRoot(rootPath)
		if err != nil {
			return err
		}
		defer root.Close()
		temp := filepath.Join(filepath.Dir(name), ".syncthing.history-"+rand.Text()+".tmp")
		output, err := root.OpenFile(temp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
		if err != nil {
			return err
		}
		defer root.Remove(temp)
		hash := sha256.New()
		_, copyErr := io.Copy(io.MultiWriter(output, hash), historyContextReader{ctx, input})
		syncErr := output.Sync()
		closeErr := output.Close()
		if err := errors.Join(copyErr, syncErr, closeErr, ctx.Err()); err != nil {
			return err
		}
		mode := os.FileMode(0o644)
		if captured.Exists {
			mode = os.FileMode(captured.Mode)
		}
		if err := root.Chmod(temp, mode); err != nil {
			return err
		}
		if err := root.Chtimes(temp, time.Unix(0, modifiedNs), time.Unix(0, modifiedNs)); err != nil {
			return err
		}
		// Check again after staging, in case another application edited the file while it was copied.
		latest, err := captureHistoryTarget(ctx, h, rootPath, name)
		if err != nil {
			return err
		}
		if latest != captured {
			return errRestoreTargetChanged
		}
		if captured.Exists && captured.Hash == hex.EncodeToString(hash.Sum(nil)) {
			return nil
		}
		journal := ""
		var record historyJournal
		archive := h.Archive
		hasVersioner := h.HasVersioner()
		if cfg := fld.folderConfiguration(); cfg == nil || cfg.Filesystem().URI() != rootPath {
			return errRestoreTargetChanged
		} else if !hasVersioner && cfg.Versioning.Type != "" {
			// Send Only runners have no versioner handle, even when destination versioning is configured.
			configured, err := versioner.New(*cfg)
			if err != nil {
				return err
			}
			archive, hasVersioner = configured.Archive, true
			if platformVersioner, ok := versioner.WithPlatformData(configured); ok {
				archive = func(path string) error {
					current, exists, err := h.CurrentFile(path)
					if err != nil {
						return err
					}
					if !exists || current.Deleted {
						return errors.New("cannot archive without indexed platform metadata")
					}
					return platformVersioner.ArchiveWithPlatformData(path, current.Platform, false)
				}
			}
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		pendingDirectory := filepath.Join(fld.client.CurrentConfigDirectory(), "history-restores")
		if current.Exists && hasVersioner {
			journal, record, err = beginHistoryJournal(ctx, root, name, temp, captured.Hash, hex.EncodeToString(hash.Sum(nil)), pendingDirectory)
			if err != nil {
				return err
			}
			if err := ctx.Err(); err != nil {
				return errors.Join(err, recoverHistoryJournal(root, journal, record, pendingDirectory))
			}
			if err := archive(name); err != nil {
				return errors.Join(err, recoverHistoryJournal(root, journal, record, pendingDirectory))
			}
		}
		if err := ctx.Err(); err != nil {
			if journal != "" {
				return errors.Join(err, recoverHistoryJournal(root, journal, record, pendingDirectory))
			}
			return err
		}
		if err := publishHistoryFile(root, temp, name, journal, record, pendingDirectory); err != nil {
			return err
		}
		return h.Scan(ctx, []string{name})
	})
}

// ValidateVersionExportDestination prevents Save As from bypassing guarded restoration of a synchronized file.
func (fld *Folder) ValidateVersionExportDestination(destination string) error {
	cfg := fld.client.cfg()
	if cfg == nil {
		return ErrStillLoading
	}
	if _, err := os.Lstat(destination); err == nil && isInSynchronizedFolder(destination, cfg.Folders()) {
		return errExportOverFile
	}
	return nil
}
