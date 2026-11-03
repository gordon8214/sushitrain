// Copyright (C) 2026 Gordon
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this file,
// You can obtain one at https://mozilla.org/MPL/2.0/.

package files

import (
	"database/sql"
	"errors"
	"fmt"
	"io/fs"
	"strings"
	"time"
)

// trackerIgnoreName is a file at the mount's root that keeps GNOME's LocalSearch from indexing the mount, which would
// read, and so download, every file in it (it is in LocalSearch's default ignored-directories-with-content)
const trackerIgnoreName = ".trackerignore"

// Entry is an item as the mount shows it
type Entry struct {
	Item   int64
	Node   uint64
	Folder int64
	Name   string
	// kindFile, kindDir or kindRoot (a folder's directory at the mount's root)
	Kind      int
	Size      int64
	ModTime   time.Time
	ContentID string
	// Whether this client may change the folder
	Writable bool
	// Kept on this device on purpose (Keep on This Device), here or above
	Keep bool
}

// IsDir reports whether the entry is a directory (a folder's root included)
func (e Entry) IsDir() bool {
	return e.Kind == kindDir || e.Kind == kindRoot
}

const entrySelect = `SELECT i.id, i.node, i.folder, i.name, i.kind, i.size, i.mod_ns, i.content_id, f.can_write, i.keep
	FROM items AS i JOIN folders AS f ON f.id = i.folder`

func scanEntry(row interface{ Scan(...any) error }) (Entry, error) {
	var e Entry
	var modNs int64
	var contentID sql.NullString
	if err := row.Scan(&e.Item, &e.Node, &e.Folder, &e.Name, &e.Kind, &e.Size, &modNs, &contentID, &e.Writable,
		&e.Keep); err != nil {
		return Entry{}, err
	}
	e.ModTime, e.ContentID = time.Unix(0, modNs), contentID.String
	return e, nil
}

// Lookup returns the entry named name in the directory parent (0: the mount's root, whose entries are the folders)
func (s *Service) Lookup(parent int64, name string) (Entry, bool, error) {
	name = normalizedName(name)
	var e Entry
	var err error
	if parent == 0 {
		e, err = scanEntry(s.store.db.QueryRow(entrySelect+` WHERE i.parent IS NULL AND i.name = ? AND i.deleted = 0`,
			name))
	} else {
		e, err = scanEntry(s.store.db.QueryRow(entrySelect+` WHERE i.parent = ? AND i.name = ? AND i.deleted = 0`,
			parent, name))
	}
	if errors.Is(err, sql.ErrNoRows) {
		return Entry{}, false, nil
	}
	if err != nil {
		return Entry{}, false, err
	}
	return s.withKeep(e)
}

// Children lists a directory (0: the mount's root)
func (s *Service) Children(parent int64) ([]Entry, error) {
	var rows *sql.Rows
	var err error
	if parent == 0 {
		rows, err = s.store.db.Query(entrySelect + ` WHERE i.parent IS NULL AND i.deleted = 0 ORDER BY i.name`)
	} else {
		rows, err = s.store.db.Query(entrySelect+` WHERE i.parent = ? AND i.deleted = 0 ORDER BY i.name`, parent)
	}
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var result []Entry
	for rows.Next() {
		e, err := scanEntry(rows)
		if err != nil {
			return nil, err
		}
		result = append(result, e)
	}
	if err := rows.Err(); err != nil || parent == 0 {
		return result, err
	}
	// Everything here is kept when the directory is
	keep, err := s.effectiveKeep(parent)
	if err != nil {
		return nil, err
	}
	for i := range result {
		result[i].Keep = result[i].Keep || keep
	}
	return result, nil
}

// Item returns an item's entry as it is now
func (s *Service) Item(id int64) (Entry, bool, error) {
	e, err := scanEntry(s.store.db.QueryRow(entrySelect+` WHERE i.id = ? AND i.deleted = 0`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return Entry{}, false, nil
	}
	if err != nil {
		return Entry{}, false, err
	}
	return s.withKeep(e)
}

func (s *Service) withKeep(e Entry) (Entry, bool, error) {
	if !e.Keep {
		keep, err := s.effectiveKeep(e.Item)
		if err != nil {
			return Entry{}, false, err
		}
		e.Keep = keep
	}
	return e, true, nil
}

// effectiveKeep reports whether an item is kept on this device, itself or through a directory above it
func (s *Service) effectiveKeep(item int64) (bool, error) {
	var keep bool
	err := s.store.db.QueryRow(`WITH RECURSIVE up(id, parent, keep) AS (
			SELECT id, parent, keep FROM items WHERE id = ?
			UNION ALL SELECT i.id, i.parent, i.keep FROM items AS i JOIN up ON i.id = up.parent)
		SELECT COALESCE(MAX(keep), 0) FROM up`, item).Scan(&keep)
	return keep, err
}

// Resolve returns the entry at a slash-separated path below the mount's root, such as "Documents/report.odt"
func (s *Service) Resolve(path string) (Entry, error) {
	var e Entry
	var parent int64
	found := false
	for _, name := range strings.Split(strings.Trim(path, "/"), "/") {
		if name == "" || name == "." {
			continue
		}
		var ok bool
		var err error
		if e, ok, err = s.Lookup(parent, name); err != nil {
			return Entry{}, err
		} else if !ok {
			return Entry{}, fmt.Errorf("%s: %w", path, fs.ErrNotExist)
		}
		parent, found = e.Item, true
	}
	if !found {
		return Entry{}, fmt.Errorf("%s is the mount itself, not an item in it", path)
	}
	return e, nil
}
