// Copyright (C) 2026 Gordon
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this file,
// You can obtain one at https://mozilla.org/MPL/2.0/.

package gateway

import (
	"cmp"
	"database/sql"
	"errors"
	"net/http"
	"slices"
	"time"

	"github.com/syncthing/syncthing/lib/protocol"
)

const snapshotLifetime = 24 * time.Hour

func (s *Service) handleCapabilities(w http.ResponseWriter, r *http.Request, p principal) error {
	if err := requireCredential(p); err != nil {
		return err
	}
	serviceID, err := s.store.serviceID()
	if err != nil {
		return err
	}
	generation, err := s.store.generation()
	if err != nil {
		return err
	}
	features := []string{featureLongPoll, featureDirectoryMoves, featureCrossFolderMoves}
	if s.cfg.Thumbnails != nil {
		features = append(features, s.cfg.Thumbnails.Features()...)
	}
	caps := Capabilities{
		APIVersion:          apiVersion,
		ServiceID:           serviceID,
		ServiceName:         s.cfg.ServiceName,
		SyncthingDeviceID:   s.backend.DeviceID().String(),
		AuthorityGeneration: generation,
		Features:            features,
		Limits: Limits{
			PageSize:            defaultPageSize,
			MaxPageSize:         maxPageSize,
			ManifestPageSize:    maxManifestPage,
			LongPollSeconds:     int(longPollMaximum / time.Second),
			LeaseSeconds:        int(leaseLifetime / time.Second),
			UploadExpirySeconds: int(uploadExpiry / time.Second),
			MaxUploadSize:       s.cfg.MaxUploadSize,
			MaxControlBodyBytes: maxControlBodyBytes,
		},
	}
	if s.UsesSelfSignedCertificate() {
		caps.Certificate = &CertificateInfo{SPKI: s.SPKI()}
	}
	return writeJSON(w, http.StatusOK, caps)
}

func (s *Service) handleFolders(w http.ResponseWriter, r *http.Request, p principal) error {
	if err := requireCredential(p); err != nil {
		return err
	}
	rows, err := s.store.db.Query(`SELECT e.folder_id FROM exports e JOIN permissions p ON p.folder_id = e.folder_id
		WHERE p.client_id = ? ORDER BY e.folder_id`, p.clientID)
	if err != nil {
		return err
	}
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return err
		}
		ids = append(ids, id)
	}
	rows.Close()

	response := FoldersResponse{Folders: []Folder{}}
	for _, id := range ids {
		access, err := s.access(p.clientID, id)
		if err != nil {
			continue
		}
		root, err := s.ensureFolderRow(r.Context(), id)
		if err != nil {
			return err
		}
		response.Folders = append(response.Folders, Folder{
			ID:          id,
			Label:       access.info.Label,
			RootItemID:  root,
			Permissions: FolderPermissions{Read: access.read, Write: access.write},
			Type:        access.info.Type,
			Selective:   access.info.Selective,
			State:       s.backend.FolderState(id),
		})
	}
	return writeJSON(w, http.StatusOK, response)
}

func (s *Service) handleSnapshot(w http.ResponseWriter, r *http.Request, p principal) error {
	if err := requireCredential(p); err != nil {
		return err
	}
	folderID := r.PathValue("folderID")
	access, err := s.access(p.clientID, folderID)
	if err != nil {
		return err
	}
	limit, err := pageLimit(r)
	if err != nil {
		return err
	}
	if _, err := s.ensureFolderRow(r.Context(), folderID); err != nil {
		return err
	}

	query := r.URL.Query()
	snapshotID, pageID := query.Get("snapshot"), query.Get("page")
	var cursorSeq, generation int64
	var after string
	first := snapshotID == ""
	if first {
		if pageID != "" {
			return newAPIError(http.StatusBadRequest, codeInvalidRequest, "page requires snapshot")
		}
		snapshotID = newID(prefixSnapshot)
		err = s.store.tx(r.Context(), func(tx *sql.Tx) error {
			var err error
			if generation, err = metaInt(tx, metaGeneration); err != nil {
				return err
			}
			if cursorSeq, err = headSequence(tx); err != nil {
				return err
			}
			_, err = tx.Exec(`INSERT INTO snapshots (snapshot_id, client_id, folder_id, generation, cursor_seq, created_at)
				VALUES (?, ?, ?, ?, ?, ?)`, snapshotID, p.clientID, folderID, generation, cursorSeq, s.store.now().Unix())
			return err
		})
		if err != nil {
			return err
		}
	} else {
		var created int64
		var owner, folder string
		err := s.store.db.QueryRow(`SELECT client_id, folder_id, generation, cursor_seq, created_at FROM snapshots
			WHERE snapshot_id = ?`, snapshotID).Scan(&owner, &folder, &generation, &cursorSeq, &created)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		current, genErr := s.store.generation()
		if genErr != nil {
			return genErr
		}
		if errors.Is(err, sql.ErrNoRows) || owner != p.clientID || folder != folderID || generation != current ||
			s.store.now().Sub(time.Unix(created, 0)) > snapshotLifetime {
			return newAPIError(http.StatusGone, codeSnapshotExpired, "the snapshot has expired; start a new one")
		}
		if pageID != "" {
			err := s.store.db.QueryRow(`SELECT after_path FROM snapshot_pages WHERE page_id = ? AND snapshot_id = ?`,
				pageID, snapshotID).Scan(&after)
			if errors.Is(err, sql.ErrNoRows) {
				return newAPIError(http.StatusGone, codeSnapshotExpired, "unknown snapshot page")
			}
			if err != nil {
				return err
			}
		}
	}

	var rows []itemRow
	if first || pageID == "" {
		rows, err = queryItems(s.store.db, `SELECT `+itemColumns+` FROM items WHERE folder_id = ? AND deleted = 0
			ORDER BY path LIMIT ?`, folderID, limit+1)
	} else {
		rows, err = queryItems(s.store.db, `SELECT `+itemColumns+` FROM items WHERE folder_id = ? AND deleted = 0
			AND path > ? ORDER BY path LIMIT ?`, folderID, after, limit+1)
	}
	if err != nil {
		return err
	}
	page := SnapshotPage{
		Snapshot: snapshotID,
		Cursor:   cursor{generation: generation, sequence: cursorSeq}.String(),
		Items:    make([]Item, 0, min(len(rows), limit)),
	}
	if len(rows) > limit {
		rows = rows[:limit]
		page.Next = newID(prefixPage)
		err := s.store.tx(r.Context(), func(tx *sql.Tx) error {
			_, err := tx.Exec(`INSERT INTO snapshot_pages (page_id, snapshot_id, after_path) VALUES (?, ?, ?)`, page.Next,
				snapshotID, rows[len(rows)-1].path)
			return err
		})
		if err != nil {
			return err
		}
	}
	for _, row := range rows {
		page.Items = append(page.Items, row.toItem(access.write))
	}
	return writeJSON(w, http.StatusOK, page)
}

func (s *Service) handleChanges(w http.ResponseWriter, r *http.Request, p principal) error {
	if err := requireCredential(p); err != nil {
		return err
	}
	folderID := r.PathValue("folderID")
	access, err := s.access(p.clientID, folderID)
	if err != nil {
		return err
	}
	limit, err := pageLimit(r)
	if err != nil {
		return err
	}
	from, ok := parseCursor(r.URL.Query().Get("cursor"))
	if !ok {
		return newAPIError(http.StatusBadRequest, codeInvalidRequest, "invalid cursor")
	}
	if err := s.checkCursor(r, p, folderID, from); err != nil {
		return err
	}
	wait := waitDuration(r, longPollMaximum)
	deadline := time.Now().Add(wait)
	for {
		changed := s.notifier.wait(folderID)
		// The head is read before the rows: every sequence up to it is committed, so an empty page can skip to it
		// without passing over a change committed in between
		head, err := headSequence(s.store.db)
		if err != nil {
			return err
		}
		rows, err := s.changedItems(folderID, from.sequence, limit+1)
		if err != nil {
			return err
		}
		if len(rows) == 0 && time.Now().Before(deadline) {
			select {
			case <-changed:
				// Permissions may have changed while waiting
				if access, err = s.access(p.clientID, folderID); err != nil {
					return err
				}
				continue
			case <-time.After(time.Until(deadline)):
				continue
			case <-r.Context().Done():
				// The service is stopping (or the client left): answer with what there is rather than an empty body
			}
		}
		page := ChangesPage{Changes: make([]Item, 0, min(len(rows), limit))}
		if len(rows) > limit {
			rows = rows[:limit]
			page.More = true
		}
		next := from
		for _, row := range rows {
			item := row.toItem(access.write)
			item.MovedToFolderID = row.movedTo
			page.Changes = append(page.Changes, item)
			next.sequence = row.changeSeq
		}
		if len(rows) == 0 {
			next.sequence = max(next.sequence, head)
		}
		page.Cursor = next.String()
		return writeJSON(w, http.StatusOK, page)
	}
}

// changedItems returns up to limit changes of a folder after a change sequence, in order: its items, and the
// tombstones of items that moved to another folder
func (s *Service) changedItems(folderID string, after int64, limit int) ([]itemRow, error) {
	rows, err := queryItems(s.store.db, `SELECT `+itemColumns+` FROM items WHERE folder_id = ? AND change_seq > ?
		ORDER BY change_seq LIMIT ?`, folderID, after, limit)
	if err != nil {
		return nil, err
	}
	result, err := s.store.db.Query(`SELECT item_id, path, parent_item, kind, to_folder, revision, change_seq
		FROM moved_out WHERE folder_id = ? AND change_seq > ? ORDER BY change_seq LIMIT ?`, folderID, after, limit)
	if err != nil {
		return nil, err
	}
	defer result.Close()
	var moved []itemRow
	for result.Next() {
		row := itemRow{folder: folderID, deleted: true}
		if err := result.Scan(&row.id, &row.path, &row.parent, &row.kind, &row.movedTo, &row.revision,
			&row.changeSeq); err != nil {
			return nil, err
		}
		moved = append(moved, row)
	}
	if err := result.Err(); err != nil {
		return nil, err
	}
	if len(moved) == 0 {
		return rows, nil
	}
	merged := append(rows, moved...)
	slices.SortFunc(merged, func(a, b itemRow) int { return cmp.Compare(a.changeSeq, b.changeSeq) })
	return merged[:min(len(merged), limit)], nil
}

// checkCursor rejects cursors a client cannot continue from: from another authority generation, older than the
// retained tombstones, or beyond the head of the change log, which means the service database was restored from an
// older state (the service then starts a new generation)
func (s *Service) checkCursor(r *http.Request, p principal, folderID string, c cursor) error {
	expired := newAPIError(http.StatusGone, codeCursorExpired, "the change cursor is no longer valid; take a new snapshot")
	generation, err := s.store.generation()
	if err != nil {
		return err
	}
	if c.generation != generation {
		return expired
	}
	head, err := headSequence(s.store.db)
	if err != nil {
		return err
	}
	if c.sequence > head {
		// A restored database is only running since the last start, so one new generation per start is enough. Any
		// client can present such a cursor, and must not be able to expire everyone's cursors at will.
		if !s.restoreDetected.CompareAndSwap(false, true) {
			return expired
		}
		err := s.store.tx(r.Context(), func(tx *sql.Tx) error {
			current, err := metaInt(tx, metaGeneration)
			if err == nil && current == generation {
				_, err = bumpGeneration(tx)
				s.log.Warn("A client presented a change cursor beyond the change log; the service database was restored",
					"folder", folderID, "client", p.clientID)
			}
			return err
		})
		if err != nil {
			return err
		}
		return expired
	}
	var prunedBelow int64
	if err := s.store.db.QueryRow(`SELECT pruned_below FROM folders WHERE folder_id = ?`, folderID).Scan(&prunedBelow); err != nil &&
		!errors.Is(err, sql.ErrNoRows) {
		return err
	}
	if c.sequence < prunedBelow {
		return expired
	}
	return nil
}

func (s *Service) handleItem(w http.ResponseWriter, r *http.Request, p principal) error {
	if err := requireCredential(p); err != nil {
		return err
	}
	row, access, err := s.itemForClient(p.clientID, r.PathValue("itemID"))
	if err != nil {
		return err
	}
	item := row.toItem(access.write)
	if !row.deleted && row.path != "" {
		item.Availability = s.availability(row)
	}
	return writeJSON(w, http.StatusOK, item)
}

// itemForClient looks up an item and checks the client's access to its folder
func (s *Service) itemForClient(clientID, itemID string) (itemRow, folderAccess, error) {
	if !hasIDFormat(itemID, prefixItem) {
		return itemRow{}, folderAccess{}, errNotFound
	}
	row, ok, err := itemByID(s.store.db, itemID)
	if err != nil {
		return itemRow{}, folderAccess{}, err
	}
	if !ok {
		return itemRow{}, folderAccess{}, errNotFound
	}
	access, err := s.access(clientID, row.folder)
	if err != nil {
		if errors.Is(err, errForbidden) {
			// Don't reveal items of folders the client cannot see
			return itemRow{}, folderAccess{}, errNotFound
		}
		return itemRow{}, folderAccess{}, err
	}
	return row, access, nil
}

// availability reports where the gateway can read an item's current content from
func (s *Service) availability(row itemRow) string {
	if row.kind != protocol.FileInfoTypeFile {
		return availabilityLocal
	}
	global, ok, err := s.backend.GlobalFile(row.folder, row.path)
	if err != nil || !ok {
		return availabilityUnavailable
	}
	local, ok, err := s.backend.LocalFile(row.folder, row.path)
	if err == nil && ok && !local.Deleted && local.Version.Equal(global.Version) {
		return availabilityLocal
	}
	if len(global.Blocks) == 0 || s.backend.BlockAvailable(row.folder, global, 0) {
		return availabilityPeers
	}
	return availabilityUnavailable
}
