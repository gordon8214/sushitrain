// Copyright (C) 2026 Gordon
// SPDX-License-Identifier: MPL-2.0
package sushitrain

import (
	"context"
	"errors"
	"github.com/syncthing/syncthing/lib/fs"
	"github.com/syncthing/syncthing/lib/syncthing"
	"os"
	"t-shaped.nl/sushitrain/v2/safecopy"
	"t-shaped.nl/sushitrain/v2/versionsnapshot"
)

// RestoreFileVersionChecked stages the exact selected archive before archiving
// current content. This also prevents a same-second simple/staggered tag from
// overwriting the selected archive before its bytes are consumed.
func (fld *Folder) RestoreFileVersionChecked(ctx context.Context, name string, version *FileVersion, expected string) error {
	if expected == "" || !fld.CanBrowseVersions() {
		return errVersionNotListed
	}
	return fld.withHistoryTargetContext(ctx, name, func(ctx context.Context, h syncthing.FolderHandle, root string) error {
		captured, err := captureHistoryTarget(ctx, h, root, name)
		if err != nil {
			return err
		}
		cfg := fld.folderConfiguration()
		if cfg == nil {
			return errFolderConfigNotFound
		}
		complete := cfg.Versioning.Type == VersioningTypeStaggered && cfg.Versioning.Params[versioningParamPlatformData] == "true"
		var input fs.File
		var applyMetadata func(string) error
		if complete {
			file, metadata, err := fld.client.internals().OpenFileVersionSnapshot(fld.FolderID, name, version.versionTime)
			if err != nil {
				return err
			}
			input = file
			if metadata.Darwin == nil {
				input.Close()
				return versionsnapshot.ErrIncomplete
			}
			if err := versionsnapshot.ValidAttributes(metadata.Darwin.Xattrs); err != nil {
				input.Close()
				return err
			}
			applyMetadata = func(destination string) error {
				return versionsnapshot.ApplyAttributes(destination, metadata.Darwin.Xattrs)
			}
		} else {
			input, err = fld.client.internals().OpenFileVersion(fld.FolderID, name, version.versionTime)
			if err != nil {
				return err
			}
			applyMetadata, err = fld.archiveAttributes(input)
			if err != nil {
				input.Close()
				return err
			}
		}
		defer input.Close()
		info, err := input.Stat()
		if err != nil {
			return err
		}
		if !info.IsRegular() || archiveRevision(info) != expected {
			return errVersionNotListed
		}
		mode := os.FileMode(info.Mode()).Perm()
		finish := func() error {
			current, err := input.Stat()
			if err != nil {
				return err
			}
			if archiveRevision(current) != expected {
				return errors.New("the selected archive changed while staging")
			}
			return input.Close()
		}
		return fld.restoreHistoryInput(ctx, h, root, name, input, captured, version.modTime.UnixNano(), complete, &mode, applyMetadata, finish)
	})
}

// DesktopExportVersionChecked validates the retained input, never a separately
// opened pathname. A stale selection cannot export a replacement archive.
func (f *Folder) DesktopExportVersionChecked(ctx context.Context, name string, v *FileVersion, destination, expected string) error {
	internals := f.client.internals()
	if internals == nil {
		return ErrStillLoading
	}
	input, err := internals.OpenFileVersion(f.FolderID, name, v.versionTime)
	if err != nil {
		return err
	}
	defer input.Close()
	info, err := input.Stat()
	if err != nil {
		return err
	}
	revision := archiveRevision(info)
	if !info.IsRegular() || expected != "" && revision != expected {
		return errVersionNotListed
	}
	dest, err := safecopy.Create(destination)
	if err != nil {
		return err
	}
	defer dest.Close()
	if _, err = safecopy.Copy(ctx, dest.File, input); err != nil {
		return err
	}
	current, err := input.Stat()
	if err != nil {
		return err
	}
	if archiveRevision(current) != revision {
		return errors.New("the selected archive changed while exporting")
	}
	return dest.Commit()
}
