// Copyright (C) 2026 Gordon
// SPDX-License-Identifier: MPL-2.0
package files

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"t-shaped.nl/sushitrain/v2/previewcache"
	"t-shaped.nl/sushitrain/v2/remoteapi"
	"t-shaped.nl/sushitrain/v2/safecopy"
	"time"
)

type HistoryTarget struct {
	Folder, Path, ServiceID, DeviceID string
	Writable                          bool
}
type HistoryRestoreCapture struct {
	Entry                             Entry
	Revision, ServiceID, RemoteFolder string
	Created                           time.Time
}

func (s *Service) HistoryTarget(ctx context.Context, item int64) (HistoryTarget, error) {
	target, _, err := s.versionTarget(item)
	if err != nil {
		return HistoryTarget{}, err
	}
	local, err := s.ItemPath(item)
	if err != nil {
		return HistoryTarget{}, err
	}
	parts := strings.SplitN(filepath.ToSlash(local), "/", 2)
	if len(parts) != 2 {
		return HistoryTarget{}, errors.New("not a file in a selected folder")
	}
	// Correlation uses only the local index. An offline primary must not delay
	// listing independent paired sources.
	return HistoryTarget{Folder: target.folder.remoteID, Path: parts[1], ServiceID: target.folder.serviceID, Writable: target.writable && !target.pending}, nil
}
func (s *Service) CaptureHistoryRestore(ctx context.Context, item int64) (HistoryRestoreCapture, error) {
	target, client, err := s.versionTarget(item)
	if err != nil {
		return HistoryRestoreCapture{}, err
	}
	if target.pending {
		return HistoryRestoreCapture{}, ErrPendingChange
	}
	if !target.writable {
		return HistoryRestoreCapture{}, ErrReadOnly
	}
	list, err := client.Versions(ctx, target.gatewayID)
	if err != nil {
		return HistoryRestoreCapture{}, err
	}
	if list.Versioning == "" || list.Versioning == "none" || list.Versioning == "external" || list.Versioning == "customPath" {
		return HistoryRestoreCapture{}, errors.New("enable supported versioning on the destination gateway before replacing this file")
	}
	entry, ok, err := s.Item(item)
	if err != nil {
		return HistoryRestoreCapture{}, err
	}
	if !ok {
		return HistoryRestoreCapture{}, os.ErrNotExist
	}
	return HistoryRestoreCapture{Entry: entry, Revision: target.revision, ServiceID: target.folder.serviceID, RemoteFolder: target.folder.remoteID, Created: time.Now()}, nil
}
func (s *Service) RestoreHistoryCapture(ctx context.Context, c HistoryRestoreCapture, source string, modified time.Time) (Entry, error) {
	if time.Since(c.Created) > 10*time.Minute {
		return Entry{}, errors.New("the restore confirmation expired; refresh history")
	}
	target, _, err := s.versionTarget(c.Entry.Item)
	if err != nil {
		return Entry{}, err
	}
	if target.pending || target.revision != c.Revision || target.folder.serviceID != c.ServiceID || target.folder.remoteID != c.RemoteFolder {
		return Entry{}, ErrPendingChange
	}
	current, ok, err := s.Item(c.Entry.Item)
	if err != nil {
		return Entry{}, err
	}
	if !ok || current.Node != c.Entry.Node || !current.ModTime.Equal(c.Entry.ModTime) || current.Size != c.Entry.Size || current.ContentID != c.Entry.ContentID {
		return Entry{}, ErrPendingChange
	}
	f, err := safecopy.OpenRegular(source)
	if err != nil {
		return Entry{}, err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return Entry{}, err
	}
	return s.importSnapshot(ctx, c.Entry, 0, "", false, f, info.Size(), modified, "", &c)
}

func (s *Service) selectedHistoryContent(ctx context.Context, item int64, id, tag string) (io.ReadCloser, remoteapi.ItemVersion, error) {
	if tag == "" {
		return nil, remoteapi.ItemVersion{}, errors.New("refresh history before restoring this version")
	}
	target, client, err := s.versionTarget(item)
	if err != nil {
		return nil, remoteapi.ItemVersion{}, err
	}
	local, err := s.ItemPath(item)
	if err != nil {
		return nil, remoteapi.ItemVersion{}, err
	}
	parts := strings.SplitN(filepath.ToSlash(local), "/", 2)
	if len(parts) != 2 {
		return nil, remoteapi.ItemVersion{}, ErrInvalidName
	}
	list, err := client.PathVersions(ctx, target.folder.remoteID, parts[1])
	if err != nil {
		return nil, remoteapi.ItemVersion{}, err
	}
	for _, version := range list.Versions {
		if version.ID == id && version.EntityTag == tag {
			reader, err := client.HistoryContent(ctx, target.folder.remoteID, parts[1], version)
			return reader, version, err
		}
	}
	return nil, remoteapi.ItemVersion{}, errors.New("the selected archived version changed; refresh history")
}

// RestoreHistoryVersion binds both the selected archive and the destination at commit.
func (s *Service) RestoreHistoryVersion(ctx context.Context, item int64, id, tag string) (Entry, error) {
	capture, err := s.CaptureHistoryRestore(ctx, item)
	if err != nil {
		return Entry{}, err
	}
	reader, version, err := s.selectedHistoryContent(ctx, item, id, tag)
	if err != nil {
		return Entry{}, err
	}
	defer reader.Close()
	return s.importSnapshot(ctx, capture.Entry, 0, "", false, reader, version.Size, time.Unix(0, version.ModifiedNs), "", &capture)
}

// CopyHistoryVersion stages the selected bytes as a new file without replacing a sibling.
func (s *Service) CopyHistoryVersion(ctx context.Context, item int64, id, tag string) (Entry, error) {
	entry, ok, err := s.Item(item)
	if err != nil {
		return Entry{}, err
	}
	if !ok {
		return Entry{}, os.ErrNotExist
	}
	if !entry.Writable {
		return Entry{}, ErrReadOnly
	}
	var parent int64
	if err = s.store.db.QueryRow(`SELECT parent FROM items WHERE id = ? AND deleted = 0`, item).Scan(&parent); err != nil {
		return Entry{}, err
	}
	siblings, err := s.Children(parent)
	if err != nil {
		return Entry{}, err
	}
	taken := map[string]bool{}
	for _, sibling := range siblings {
		taken[sibling.Name] = true
	}
	reader, version, err := s.selectedHistoryContent(ctx, item, id, tag)
	if err != nil {
		return Entry{}, err
	}
	defer reader.Close()
	modified := time.Unix(0, version.ModifiedNs)
	return s.CreateSnapshot(ctx, parent, versionCopyName(entry.Name, modified, taken), false, reader, version.Size, modified)
}

// HistoryVersions uses the path-based API so selected archives carry strong tags.
func (s *Service) HistoryVersions(ctx context.Context, item int64) (Versions, error) {
	target, client, err := s.versionTarget(item)
	if err != nil {
		return Versions{}, err
	}
	local, err := s.ItemPath(item)
	if err != nil {
		return Versions{}, err
	}
	parts := strings.SplitN(filepath.ToSlash(local), "/", 2)
	if len(parts) != 2 {
		return Versions{}, ErrInvalidName
	}
	list, err := client.PathVersions(ctx, target.folder.remoteID, parts[1])
	if err != nil {
		return Versions{}, err
	}
	result := Versions{Versioning: list.Versioning, Writable: target.writable && !target.pending, Versions: []Version{}}
	for _, v := range list.Versions {
		archived, _ := time.Parse(time.RFC3339, v.VersionTime)
		result.Versions = append(result.Versions, Version{ID: v.ID, EntityTag: v.EntityTag, Archived: archived, ModTime: time.Unix(0, v.ModifiedNs), Size: v.Size})
	}
	return result, nil
}
func (s *Service) ExportHistoryVersion(ctx context.Context, item int64, id, tag, dest string, forbidden ...string) error {
	if tag == "" {
		return errors.New("refresh history before saving this version")
	}
	target, client, err := s.versionTarget(item)
	if err != nil {
		return err
	}
	local, err := s.ItemPath(item)
	if err != nil {
		return err
	}
	parts := strings.SplitN(filepath.ToSlash(local), "/", 2)
	if len(parts) != 2 {
		return ErrInvalidName
	}
	list, err := client.PathVersions(ctx, target.folder.remoteID, parts[1])
	if err != nil {
		return err
	}
	for _, v := range list.Versions {
		if v.ID == id && v.EntityTag == tag {
			reader, err := client.HistoryContent(ctx, target.folder.remoteID, parts[1], v)
			if err != nil {
				return err
			}
			defer reader.Close()
			output, err := safecopy.Create(dest, forbidden...)
			if err != nil {
				return err
			}
			defer output.Close()
			n, err := safecopy.Copy(ctx, output.File, io.LimitReader(reader, v.Size+1))
			if err != nil {
				return err
			}
			if n != v.Size {
				return io.ErrUnexpectedEOF
			}
			return output.Commit()
		}
	}
	return errors.New("the selected archived version changed; refresh history")
}

func (s *Service) PreviewHistoryVersion(ctx context.Context, item int64, id, tag, cache string) (string, error) {
	s.previewMutex.Lock()
	defer s.previewMutex.Unlock()
	list, err := s.HistoryVersions(ctx, item)
	if err != nil {
		return "", err
	}
	for _, v := range list.Versions {
		if v.ID == id && v.EntityTag == tag {
			if err = os.MkdirAll(cache, 0700); err != nil {
				return "", err
			}
			release, err := previewcache.Reserve(ctx, cache, v.Size)
			if err != nil {
				return "", err
			}
			defer release()
			dir, err := os.MkdirTemp(cache, "version-")
			if err != nil {
				return "", err
			}
			entry, ok, err := s.Item(item)
			if err != nil || !ok {
				os.RemoveAll(dir)
				return "", os.ErrNotExist
			}
			dest := filepath.Join(dir, entry.Name)
			if err = s.ExportHistoryVersion(ctx, item, id, tag, dest); err != nil {
				os.RemoveAll(dir)
				return "", err
			}
			return dest, nil
		}
	}
	return "", errors.New("the selected archive changed; refresh history")
}
