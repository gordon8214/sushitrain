// Copyright (C) 2026 Gordon
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this file,
// You can obtain one at https://mozilla.org/MPL/2.0/.

package gateway

// Previous versions of files, as the folder's Syncthing versioner keeps them. Versions are found by the file's current
// path and identified by when they were archived (to the second), which is unique per path.

import (
	"crypto/sha256"
	"errors"
	"fmt"
	"github.com/syncthing/syncthing/lib/fs"
	"io"
	iofs "io/fs"
	"net/http"
	"reflect"
	"strconv"
	"strings"
	"time"

	"github.com/syncthing/syncthing/lib/model"
	"github.com/syncthing/syncthing/lib/protocol"
	"github.com/syncthing/syncthing/lib/syncthing"
	"t-shaped.nl/sushitrain/v2/remoteapi"
)

const (
	prefixVersion = remoteapi.PrefixVersion
	// ItemVersionList.Versioning of folders without versioning, and of folders whose versions are kept in a custom path,
	// which the service doesn't serve
	versioningNone       = remoteapi.VersioningNone
	versioningCustomPath = remoteapi.VersioningCustomPath
)

var errVersionNotFound = newAPIError(http.StatusNotFound, codeVersionNotFound, "the version does not exist")

// versionID identifies the version archived at t
func versionID(t time.Time) string {
	return prefixVersion + strconv.FormatInt(t.Unix(), 10)
}

// parseVersionID returns the time of the version an ID identifies. Times before 1970 (a trashcan entry whose time could
// not be set when it was archived) have negative IDs.
func parseVersionID(id string) (time.Time, bool) {
	number, ok := strings.CutPrefix(id, prefixVersion)
	digits := strings.TrimPrefix(number, "-")
	if !ok || digits == "" || len(digits) > 19 || strings.Trim(digits, "0123456789") != "" {
		return time.Time{}, false
	}
	seconds, err := strconv.ParseInt(number, 10, 64)
	if err != nil {
		return time.Time{}, false
	}
	return time.Unix(seconds, 0), true
}

// versionsError maps an error of the engine's version lookup to what clients get
func versionsError(err error) error {
	var pathErr *iofs.PathError
	switch {
	case errors.Is(err, syncthing.ErrVersionNotFound), errors.Is(err, syncthing.ErrNoVersioner),
		errors.Is(err, syncthing.ErrVersionsNotSupported):
		// Versioning may have been turned off since the version was listed
		return errVersionNotFound
	case errors.Is(err, model.ErrFolderMissing):
		return errNotFound
	case errors.Is(err, model.ErrFolderPaused), errors.Is(err, model.ErrFolderNotRunning):
		return newAPIError(http.StatusServiceUnavailable, codeContentUnavailable, "the folder is not running").
			withRetryAfter(30)
	case errors.As(err, &pathErr):
		// Logged: without the path, which names the file
		return fmt.Errorf("version %s: %w", pathErr.Op, pathErr.Err)
	}
	return err
}

// fileVersions returns the versions of a file, none for folders whose versions cannot be listed
func (s *Service) fileVersions(folder, name string) ([]syncthing.FileVersion, error) {
	versions, err := s.backend.FileVersions(folder, name)
	if errors.Is(err, syncthing.ErrNoVersioner) || errors.Is(err, syncthing.ErrVersionsNotSupported) {
		return nil, nil
	}
	if err != nil {
		return nil, versionsError(err)
	}
	return versions, nil
}

// findVersion returns the version of a file archived at t
func (s *Service) findVersion(folder, name string, t time.Time) (syncthing.FileVersion, error) {
	versions, err := s.fileVersions(folder, name)
	if err != nil {
		return syncthing.FileVersion{}, err
	}
	for _, version := range versions {
		if version.VersionTime.Unix() == t.Unix() {
			return version, nil
		}
	}
	return syncthing.FileVersion{}, errVersionNotFound
}

// versionedFile returns the live file whose versions a request is about
func (s *Service) versionedFile(r *http.Request, p principal) (itemRow, folderAccess, error) {
	if err := requireCredential(p); err != nil {
		return itemRow{}, folderAccess{}, err
	}
	row, access, err := s.itemForClient(p.clientID, r.PathValue("itemID"))
	if err != nil {
		return itemRow{}, folderAccess{}, err
	}
	if row.deleted {
		return itemRow{}, folderAccess{}, errNotFound
	}
	if row.kind != protocol.FileInfoTypeFile {
		return itemRow{}, folderAccess{}, newAPIError(http.StatusConflict, codeInvalidRequest, "only files have versions")
	}
	return row, access, nil
}

func (s *Service) handleVersions(w http.ResponseWriter, r *http.Request, p principal) error {
	row, access, err := s.versionedFile(r, p)
	if err != nil {
		return err
	}
	list := ItemVersionList{ItemID: row.id, Revision: row.revision, Versioning: access.info.Versioning,
		Versions: []ItemVersion{}}
	switch {
	case list.Versioning == "":
		list.Versioning = versioningNone
	case access.info.CustomVersionsPath:
		list.Versioning = versioningCustomPath
	}
	versions, err := s.fileVersions(row.folder, row.path)
	if err != nil {
		return err
	}
	for _, version := range versions {
		list.Versions = append(list.Versions, ItemVersion{
			ID:          versionID(version.VersionTime),
			VersionTime: version.VersionTime.UTC().Format(time.RFC3339),
			ModifiedNs:  version.ModTime.UnixNano(),
			Size:        version.Size,
		})
	}
	return writeJSON(w, http.StatusOK, list)
}

// handleVersionContent serves a version's content from the gateway's disk. Unlike current content, it is not served
// through leases and manifests: clients never install these downloads as verified current content; restoration stages them as new edits.
func (s *Service) handleVersionContent(w http.ResponseWriter, r *http.Request, p principal) error {
	row, _, err := s.versionedFile(r, p)
	if err != nil {
		return err
	}
	t, ok := parseVersionID(r.PathValue("versionID"))
	if !ok {
		return errVersionNotFound
	}
	return s.serveVersionContent(w, r, p, row.folder, row.path, row.id, t)
}

func (s *Service) serveVersionContent(w http.ResponseWriter, r *http.Request, p principal, folder, name, auditID string, t time.Time) error {
	fd, err := s.backend.OpenFileVersion(folder, name, t)
	if err != nil {
		return versionsError(err)
	}
	defer fd.Close()
	st, err := fd.Stat()
	if err != nil {
		return versionsError(err)
	}
	size := st.Size()

	// Tied to the file's modification time too, as a newer version archived in the same second replaces it
	etag := versionContentETag(t, st)
	if expected := r.Header.Get("If-Match"); expected != "" && expected != etag {
		return newAPIError(http.StatusPreconditionFailed, codeRevisionMismatch, "the archived version changed")
	}
	start, end, status, err := contentRange(w, r, etag, size)
	if err != nil {
		return err
	}
	if r.Method == http.MethodGet && start == 0 {
		s.audit(p.clientID, "version", folder, auditID, "ok")
	}
	w.WriteHeader(status)
	if r.Method == http.MethodHead || end == start {
		return nil
	}
	if _, err := io.CopyBuffer(w, io.NewSectionReader(fd, start, end-start), make([]byte, min(contentCopyBuffer, end-start))); err != nil {
		// The status is sent: end the transfer early
		panic(http.ErrAbortHandler)
	}
	return nil
}

// versionSource resolves the version an operation writes, when the operation is accepted: versions are found by path,
// which the source item may change before the operation runs
func (s *Service) versionSource(req OperationRequest) (journalData, error) {
	sourceID := req.SourceItemID
	if sourceID == "" {
		sourceID = req.ItemID
	}
	row, ok, err := itemByID(s.store.db, sourceID)
	if err != nil {
		return journalData{}, err
	}
	// Items of other folders are not revealed
	if !ok || row.deleted || row.folder != req.FolderID {
		return journalData{}, newAPIError(http.StatusNotFound, codeNotFound, "unknown source item")
	}
	if row.kind != protocol.FileInfoTypeFile {
		return journalData{}, newAPIError(http.StatusConflict, codeInvalidRequest, "only files have versions")
	}
	t, _ := parseVersionID(req.SourceVersion)
	version, err := s.findVersion(row.folder, row.path, t)
	if err != nil {
		return journalData{}, err
	}
	return journalData{
		SourcePath:    row.path,
		SourceVersion: t.Unix(),
		SourceSize:    version.Size,
		SourceModNs:   version.ModTime.UnixNano(),
	}, nil
}

// openVersionSource opens the version an operation writes, checking that it is still the one recorded when the
// operation was accepted (a version archived later in the same second replaces it), and records its modification time
// to the nanosecond
func (s *Service) openVersionSource(op *operationRecord) (io.ReadCloser, int64, error) {
	fd, err := s.backend.OpenFileVersion(op.folder, op.data.SourcePath, time.Unix(op.data.SourceVersion, 0))
	if err != nil {
		return nil, 0, versionsError(err)
	}
	st, err := fd.Stat()
	if err != nil {
		fd.Close()
		return nil, 0, versionsError(err)
	}
	if st.Size() != op.data.SourceSize || st.ModTime().Unix() != time.Unix(0, op.data.SourceModNs).Unix() {
		fd.Close()
		return nil, 0, errVersionNotFound
	}
	op.data.SourceModNs = st.ModTime().UnixNano()
	return fd, st.Size(), nil
}

// Includes stable filesystem identity and change time, excluding access time (which reads may update).
// Sys differs across supported platforms; absent fields simply contribute nothing.
func versionContentETag(t time.Time, info fs.FileInfo) string {
	identity := ""
	value := reflect.ValueOf(info.Sys())
	if value.IsValid() && value.Kind() == reflect.Pointer && !value.IsNil() {
		value = value.Elem()
	}
	if value.IsValid() && value.Kind() == reflect.Struct {
		for _, name := range []string{"Dev", "Ino", "Ctim", "Ctimespec", "CreationTime", "LastWriteTime"} {
			field := value.FieldByName(name)
			if field.IsValid() && field.CanInterface() {
				identity += fmt.Sprintf("%s=%v;", name, field.Interface())
			}
		}
	}
	digest := sha256.Sum256([]byte(identity))
	return fmt.Sprintf(`"%s.%d.%d.%x"`, versionID(t), info.Size(), info.ModTime().UnixNano(), digest)
}
