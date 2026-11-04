// Copyright (C) 2026 Gordon
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this file,
// You can obtain one at https://mozilla.org/MPL/2.0/.
package gateway

import (
	"errors"
	"github.com/syncthing/syncthing/lib/syncthing"
	"io/fs"
	"net/http"
	"runtime"
	"strings"
	"t-shaped.nl/sushitrain/v2/remoteapi"
	"time"
)

// PathVersionList identifies history independently of a gateway's private item IDs.
type PathVersionList = remoteapi.PathVersionList

func (s *Service) historyPath(r *http.Request, p principal) (string, folderAccess, error) {
	if err := requireCredential(p); err != nil {
		return "", folderAccess{}, err
	}
	access, err := s.access(p.clientID, r.PathValue("folderID"))
	if err != nil {
		return "", folderAccess{}, errNotFound
	}
	values := r.URL.Query()["path"]
	if len(values) != 1 || !fs.ValidPath(values[0]) || values[0] == "." || strings.ContainsRune(values[0], 0) || runtime.GOOS == "windows" && strings.Contains(values[0], "\\") {
		return "", folderAccess{}, newAPIError(http.StatusBadRequest, codeInvalidRequest, "a folder-relative file path is required")
	}
	name := values[0]
	if IsSyncthingPrivatePath(name, access.info.MarkerName) {
		return "", folderAccess{}, errNotFound
	}
	root, err := openRootReadOnly(access.info.NativePath)
	if err != nil {
		return "", folderAccess{}, errNotFound
	}
	defer root.close()
	parent, err := root.openDirReadOnly(parentPath(name))
	if err != nil {
		// An archived file can outlive its containing directory. A symlink is never followed.
		if errors.Is(err, fs.ErrNotExist) {
			return name, access, nil
		}
		return "", folderAccess{}, errNotFound
	}
	defer parent.close()
	st, err := parent.lstat(baseName(name))
	if err != nil || st.exists && st.kind != fsKindRegular {
		return "", folderAccess{}, errNotFound
	}
	return name, access, nil
}

func (s *Service) handlePathVersions(w http.ResponseWriter, r *http.Request, p principal) error {
	name, access, err := s.historyPath(r, p)
	if err != nil {
		return err
	}
	list := PathVersionList{FolderID: access.info.ID, Path: name, Versioning: access.info.Versioning, Versions: []ItemVersion{}}
	if list.Versioning == "" {
		list.Versioning = versioningNone
	}
	if access.info.CustomVersionsPath {
		list.Versioning = versioningCustomPath
	}
	versions, err := s.fileVersions(access.info.ID, name)
	if err != nil {
		return err
	}
	for _, version := range versions {
		file, err := s.backend.OpenFileVersion(access.info.ID, name, version.VersionTime)
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) || errors.Is(err, syncthing.ErrVersionNotFound) {
				continue
			}
			return versionsError(err)
		}
		info, statErr := file.Stat()
		file.Close()
		if statErr != nil {
			return versionsError(statErr)
		}
		snapshotTag, metadataStatus := s.versionSnapshotStatus(access.info.ID, name, version.VersionTime)
		list.Versions = append(list.Versions, ItemVersion{SnapshotETag: snapshotTag, MetadataStatus: metadataStatus, ID: versionID(version.VersionTime), EntityTag: versionContentETag(version.VersionTime, info),
			VersionTime: version.VersionTime.UTC().Format(time.RFC3339), ModifiedNs: info.ModTime().UnixNano(), Size: info.Size()})
	}
	return writeJSON(w, http.StatusOK, list)
}

func (s *Service) handlePathVersionContent(w http.ResponseWriter, r *http.Request, p principal) error {
	name, access, err := s.historyPath(r, p)
	if err != nil {
		return err
	}
	t, ok := parseVersionID(r.PathValue("versionID"))
	if !ok {
		return errVersionNotFound
	}
	return s.serveVersionContent(w, r, p, access.info.ID, name, "", t)
}
