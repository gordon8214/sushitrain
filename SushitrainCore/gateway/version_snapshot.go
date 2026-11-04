// Copyright (C) 2026 Gordon
// SPDX-License-Identifier: MPL-2.0
package gateway

import (
	"errors"
	"github.com/syncthing/syncthing/lib/fs"
	"github.com/syncthing/syncthing/lib/protocol"
	"github.com/syncthing/syncthing/lib/versioner"
	"net/http"
	"t-shaped.nl/sushitrain/v2/versionsnapshot"
	"time"
)

type snapshotBackend interface {
	OpenFileVersionSnapshot(string, string, time.Time) (fs.File, protocol.PlatformData, error)
}

func (b *EngineBackend) OpenFileVersionSnapshot(folder, name string, stamp time.Time) (fs.File, protocol.PlatformData, error) {
	if fc, ok := b.Config.Folder(folder); ok && !versionsServed(fc) {
		return nil, protocol.PlatformData{}, versioner.ErrVersionMetadataMissing
	}
	return b.App.Internals.OpenFileVersionSnapshot(folder, name, stamp)
}
func (s *Service) versionSnapshotStatus(folder, name string, stamp time.Time) (string, string) {
	b, ok := s.backend.(snapshotBackend)
	if !ok {
		return "", "unsupported"
	}
	file, pd, err := b.OpenFileVersionSnapshot(folder, name, stamp)
	if errors.Is(err, versioner.ErrVersionMetadataMissing) {
		return "", "missing"
	}
	if err != nil {
		return "", "unavailable"
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return "", "unavailable"
	}
	if pd.Darwin == nil {
		return "", "missing"
	}
	if err := versionsnapshot.ValidAttributes(pd.Darwin.Xattrs); err != nil {
		return "", "corrupt"
	}
	tag, err := versionsnapshot.Tag(versionContentETag(stamp, info), pd)
	if err != nil {
		return "", "corrupt"
	}
	return tag, "complete"
}
func (s *Service) handleVersionSnapshot(w http.ResponseWriter, r *http.Request, p principal) error {
	name, access, err := s.historyPath(r, p)
	if err != nil {
		return err
	}
	stamp, ok := parseVersionID(r.PathValue("versionID"))
	if !ok {
		return errVersionNotFound
	}
	b, ok := s.backend.(snapshotBackend)
	if !ok {
		return newAPIError(http.StatusConflict, codeInvalidRequest, "snapshot export is unsupported")
	}
	file, pd, err := b.OpenFileVersionSnapshot(access.info.ID, name, stamp)
	if err != nil {
		return newAPIError(http.StatusConflict, codeInvalidRequest, "complete archived Mac metadata is unavailable")
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return err
	}
	tag, err := versionsnapshot.Tag(versionContentETag(stamp, info), pd)
	if err != nil {
		return err
	}
	if r.Header.Get("If-Match") != tag {
		return newAPIError(http.StatusPreconditionFailed, codeRevisionMismatch, "the archived snapshot changed")
	}
	if pd.Darwin == nil {
		return versionsnapshot.ErrIncomplete
	}
	if err := versionsnapshot.ValidAttributes(pd.Darwin.Xattrs); err != nil {
		return err
	}
	w.Header().Set("ETag", tag)
	w.Header().Set("Content-Type", "application/x-tar")
	w.WriteHeader(http.StatusOK)
	if err := versionsnapshot.Write(r.Context(), w, file, pd, access.info.ID, name, versionID(stamp)); err != nil {
		panic(http.ErrAbortHandler)
	}
	s.audit(p.clientID, "version-snapshot", access.info.ID, "", "ok")
	return nil
}
