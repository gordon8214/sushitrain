// Copyright (C) 2026 Gordon
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this file,
// You can obtain one at https://mozilla.org/MPL/2.0/.

package gateway

import (
	"database/sql"
	"errors"
	"path"
	"strings"

	"github.com/syncthing/syncthing/lib/protocol"
)

// A row of the items table: the service's record of an item, with the identity it keeps through API moves
type itemRow struct {
	id         string
	folder     string
	path       string
	parent     sql.NullString
	kind       protocol.FileInfoType
	deleted    bool
	stSeq      int64
	revision   string
	contentID  string
	size       int64
	modNs      int64
	mode       uint32
	subtreeGen int64
	changeSeq  int64
	// For the tombstone of an item that moved to another folder: that folder
	movedTo string
}

const itemColumns = `item_id, folder_id, path, parent_item, kind, deleted, st_sequence, revision, content_id, size, mod_ns,
	mode, subtree_gen, change_seq`

func scanItem(scan func(dest ...any) error) (itemRow, error) {
	var r itemRow
	err := scan(&r.id, &r.folder, &r.path, &r.parent, &r.kind, &r.deleted, &r.stSeq, &r.revision, &r.contentID,
		&r.size, &r.modNs, &r.mode, &r.subtreeGen, &r.changeSeq)
	return r, err
}

func itemByID(q queryer, id string) (itemRow, bool, error) {
	r, err := scanItem(q.QueryRow(`SELECT `+itemColumns+` FROM items WHERE item_id = ?`, id).Scan)
	if errors.Is(err, sql.ErrNoRows) {
		return itemRow{}, false, nil
	}
	return r, err == nil, err
}

func liveItemByPath(q queryer, folder, p string) (itemRow, bool, error) {
	r, err := scanItem(q.QueryRow(`SELECT `+itemColumns+` FROM items WHERE folder_id = ? AND path = ? AND deleted = 0`,
		folder, p).Scan)
	if errors.Is(err, sql.ErrNoRows) {
		return itemRow{}, false, nil
	}
	return r, err == nil, err
}

func queryItems(q queryer, query string, args ...any) ([]itemRow, error) {
	rows, err := q.Query(query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var result []itemRow
	for rows.Next() {
		r, err := scanItem(rows.Scan)
		if err != nil {
			return nil, err
		}
		result = append(result, r)
	}
	return result, rows.Err()
}

// parentPath returns the folder-relative path of an item's directory ("" for the root)
func parentPath(p string) string {
	if p == "" {
		return ""
	}
	dir := path.Dir(p)
	if dir == "." {
		return ""
	}
	return dir
}

func baseName(p string) string {
	if p == "" {
		return ""
	}
	return path.Base(p)
}

// descendantRange returns bounds that select the paths below p with path > lower AND path < upper
func descendantRange(p string) (lower, upper string) {
	if p == "" {
		return "", "\U0010FFFF"
	}
	// '0' follows '/' in byte order
	return p + "/", p + "0"
}

func isDescendantPath(p, ancestor string) bool {
	return ancestor == "" && p != "" || strings.HasPrefix(p, ancestor+"/")
}

func kindName(kind protocol.FileInfoType) string {
	switch kind {
	case protocol.FileInfoTypeDirectory:
		return kindDirectory
	case protocol.FileInfoTypeSymlink:
		return kindSymlink
	default:
		return kindFile
	}
}

// IsSyncthingPrivatePath reports whether any component of a folder-relative path is a name Syncthing reserves: the
// folder marker, .stignore, .stversions, or a temporary or internal file. Names are compared without regard to case,
// as the folder's filesystem may (APFS, SMB shares).
func IsSyncthingPrivatePath(name, markerName string) bool {
	if markerName == "" {
		markerName = ".stfolder"
	}
	for component := range strings.SplitSeq(name, "/") {
		if strings.EqualFold(component, markerName) || strings.EqualFold(component, ".stignore") ||
			strings.EqualFold(component, ".stversions") || hasPrefixFold(component, ".syncthing.") ||
			hasPrefixFold(component, "~syncthing~") {
			return true
		}
	}
	return false
}

func hasPrefixFold(s, prefix string) bool {
	return len(s) >= len(prefix) && strings.EqualFold(s[:len(prefix)], prefix)
}

// toItem converts a row to its wire form, for a client whose write permission on the folder is canWrite
func (r itemRow) toItem(canWrite bool) Item {
	item := Item{
		ID:         r.id,
		FolderID:   r.folder,
		Name:       baseName(r.path),
		Path:       r.path,
		Kind:       kindName(r.kind),
		Size:       r.size,
		ModifiedNs: r.modNs,
		Mode:       r.mode,
		Writable:   canWrite,
		Revision:   r.revision,
		ContentID:  r.contentID,
		Deleted:    r.deleted,
	}
	if r.parent.Valid {
		parent := r.parent.String
		item.ParentID = &parent
	}
	if r.kind == protocol.FileInfoTypeDirectory {
		item.SubtreeGeneration = r.subtreeGen
	}
	return item
}

// rowFields fills the metadata of a row from a global index entry
func (r *itemRow) setFromFileInfo(fi protocol.FileInfo, ignorePerms bool) {
	r.kind = fi.Type
	r.stSeq = fi.Sequence
	r.revision = revisionOf(fi)
	r.contentID = contentIDOf(fi)
	r.size = 0
	if fi.Type == protocol.FileInfoTypeFile {
		r.size = fi.Size
	}
	r.modNs = fi.ModifiedS*1_000_000_000 + int64(fi.ModifiedNs)
	r.mode = 0
	if !ignorePerms && !fi.NoPermissions {
		r.mode = fi.Permissions & 0o7777
	}
}
