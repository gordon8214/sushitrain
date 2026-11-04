// Copyright (C) 2026 Gordon
// SPDX-License-Identifier: MPL-2.0
package sushitrain

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"reflect"
	"strconv"
	"strings"
	"t-shaped.nl/sushitrain/v2/safecopy"
	"t-shaped.nl/sushitrain/v2/versionsnapshot"
	"time"
)

type snapshotVersion struct {
	ID             string `json:"id"`
	VersionTime    string `json:"versionTime"`
	ModifiedNs     int64  `json:"modifiedNs"`
	Size           int64  `json:"size"`
	MetadataStatus string `json:"metadataStatus"`
	SnapshotETag   string `json:"snapshotETag,omitempty"`
}

func (fld *Folder) snapshotTag(name string, stamp time.Time) (string, error) {
	internals := fld.client.internals()
	if internals == nil {
		return "", ErrStillLoading
	}
	file, pd, err := internals.OpenFileVersionSnapshot(fld.FolderID, name, stamp)
	if err != nil {
		return "", err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return "", err
	}
	if pd.Darwin == nil {
		return "", versionsnapshot.ErrIncomplete
	}
	if err = versionsnapshot.ValidAttributes(pd.Darwin.Xattrs); err != nil {
		return "", err
	}
	return versionsnapshot.Tag(fmt.Sprintf("%s:%d:%d", historyFileIdentityFromSnapshot(info.Sys()), info.Size(), info.ModTime().UnixNano()), pd)
}

// The filesystem identity is included without exposing native paths in listings.
func historyFileIdentityFromSnapshot(value any) string {
	item := reflect.Indirect(reflect.ValueOf(value))
	if !item.IsValid() || item.Kind() != reflect.Struct {
		return ""
	}
	result := ""
	for _, name := range []string{"Dev", "Ino", "Ctim", "Ctimespec", "CreationTime", "LastWriteTime"} {
		field := item.FieldByName(name)
		if field.IsValid() && field.CanInterface() {
			result += fmt.Sprintf("%s=%v;", name, field.Interface())
		}
	}
	return result
}
func (fld *Folder) VersionSnapshots(name string) (string, error) {
	versions, err := fld.internalVersions(name)
	if err != nil {
		return "", err
	}
	result := []snapshotVersion{}
	for _, v := range versions {
		tag, err := fld.snapshotTag(name, v.VersionTime)
		status := "complete"
		if err != nil {
			status = "missing"
		}
		result = append(result, snapshotVersion{ID: "ver_" + strconv.FormatInt(v.VersionTime.Unix(), 10), VersionTime: v.VersionTime.UTC().Format(time.RFC3339), ModifiedNs: v.ModTime.UnixNano(), Size: v.Size, MetadataStatus: status, SnapshotETag: tag})
	}
	data, err := json.Marshal(result)
	return string(data), err
}
func (fld *Folder) ExportVersionSnapshot(name, id, expected, destination string) error {
	return fld.ExportVersionSnapshotContext(context.Background(), name, id, expected, destination)
}
func (fld *Folder) ExportVersionSnapshotContext(parent context.Context, name, id, expected, destination string) error {
	stamp, err := snapshotTime(id)
	if err != nil {
		return err
	}
	tag, err := fld.snapshotTag(name, stamp)
	if err != nil {
		return err
	}
	if expected == "" || tag != expected {
		return errVersionNotListed
	}
	file, pd, err := fld.client.internals().OpenFileVersionSnapshot(fld.FolderID, name, stamp)
	if err != nil {
		return err
	}
	defer file.Close()
	// Validate the actual opened content and metadata, not another path lookup.
	info, err := file.Stat()
	if err != nil {
		return err
	}
	tag, err = versionsnapshot.Tag(fmt.Sprintf("%s:%d:%d", historyFileIdentityFromSnapshot(info.Sys()), info.Size(), info.ModTime().UnixNano()), pd)
	if err != nil || tag != expected {
		return errVersionNotListed
	}
	out, err := safecopy.Create(destination)
	if err != nil {
		return err
	}
	defer out.Close()
	ctx, cancel := context.WithTimeout(parent, 2*time.Minute)
	defer cancel()
	if err = versionsnapshot.Write(ctx, out.File, file, pd, fld.FolderID, name, id); err != nil {
		return err
	}
	return out.Commit()
}
func snapshotTime(id string) (time.Time, error) {
	if !strings.HasPrefix(id, "ver_") {
		return time.Time{}, errors.New("invalid archive ID")
	}
	stamp, err := strconv.ParseInt(strings.TrimPrefix(id, "ver_"), 10, 64)
	return time.Unix(stamp, 0), err
}

// MaterializeVersionSnapshot validates identity, bytes, and every archived attribute.
func MaterializeVersionSnapshot(source, destination, folder, path, id string) error {
	file, err := os.Open(source)
	if err != nil {
		return err
	}
	defer file.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	_, err = versionsnapshot.Extract(ctx, file, destination, folder, path, id)
	return err
}
func (fld *Folder) RestoreSnapshotVersion(name, source, expected string, modifiedNs int64) error {
	return fld.restoreDownloadedVersion(name, source, expected, modifiedNs, true)
}

// VersionExportRequiresExclusive uses the same native containment as the export
// guard, including symlinked roots. An unavailable configuration fails closed.
func (fld *Folder) VersionExportRequiresExclusive(destination string) bool {
	cfg := fld.client.cfg()
	if cfg == nil {
		return true
	}
	return isInSynchronizedFolder(destination, cfg.Folders())
}
