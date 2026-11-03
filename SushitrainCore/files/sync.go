// Copyright (C) 2026 Gordon
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this file,
// You can obtain one at https://mozilla.org/MPL/2.0/.

package files

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"t-shaped.nl/sushitrain/v2/remoteapi"
	"t-shaped.nl/sushitrain/v2/remoteclient"
)

// errSyncPosition means another refresh of the folder moved its position meanwhile: the page is not applied
var errSyncPosition = errors.New("the folder's sync position changed meanwhile")

type syncState struct {
	cursor   string
	snapshot *snapshotRow
}

type snapshotRow struct {
	id         string
	startCur   string
	nextPage   string
	catchUpCur string
	mark       int64
}

func (s *Service) syncState(folder int64) (syncState, error) {
	var st syncState
	err := s.store.db.QueryRow(`SELECT cursor FROM cursors WHERE folder = ?`, folder).Scan(&st.cursor)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return st, err
	}
	var row snapshotRow
	var id, start, next, catchUp sql.NullString
	err = s.store.db.QueryRow(`SELECT snapshot_id, start_cursor, next_page, catch_up_cursor, mark FROM snapshots
		WHERE folder = ?`, folder).Scan(&id, &start, &next, &catchUp, &row.mark)
	switch {
	case errors.Is(err, sql.ErrNoRows):
	case err != nil:
		return st, err
	default:
		row.id, row.startCur, row.nextPage, row.catchUpCur = id.String, start.String, next.String, catchUp.String
		st.snapshot = &row
	}
	return st, nil
}

// refreshFolder brings a folder up to date with its gateway: a snapshot when it has no cursor (or one is in progress),
// then the changes after its cursor until the gateway has no more. With wait, the last request is a long poll.
func (s *Service) refreshFolder(ctx context.Context, f folderRef, client *remoteclient.Client, wait time.Duration) error {
	restarts := 0
	restart := func(err error) error {
		restarts++
		if restarts > 3 {
			return fmt.Errorf("the folder's position was lost %d times: %w", restarts, err)
		}
		return nil
	}
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		st, err := s.syncState(f.id)
		if err != nil {
			return err
		}
		if st.cursor == "" || st.snapshot != nil {
			err := s.runSnapshot(ctx, f, client, st.snapshot)
			switch {
			case errors.Is(err, remoteclient.ErrSnapshotExpired), errors.Is(err, remoteclient.ErrCursorExpired):
				// Start over; what is stored stays until a complete snapshot says it's gone
				if err := s.abandonSnapshot(f.id); err != nil {
					return err
				}
				if err := restart(err); err != nil {
					return err
				}
				continue
			case errors.Is(err, errSyncPosition):
				continue
			}
			if err != nil {
				return err
			}
			continue
		}
		page, err := client.Changes(ctx, f.remoteID, st.cursor, 0, wait)
		if errors.Is(err, remoteclient.ErrCursorExpired) {
			// The gateway can't serve this cursor (pruned, or another authority generation): take a fresh snapshot,
			// keeping everything stored until it is complete
			s.log.Info("Taking a fresh listing", "folder", f.dirName, "reason", err)
			if err := s.resetCursor(f.id); err != nil {
				return err
			}
			if err := restart(err); err != nil {
				return err
			}
			continue
		}
		if err != nil {
			return err
		}
		if page.More && page.Cursor == st.cursor {
			return remoteclient.ErrProtocolViolation
		}
		err = s.apply(ctx, func(p *projector) error {
			var stored string
			if err := p.tx.QueryRow(`SELECT cursor FROM cursors WHERE folder = ?`, f.id).Scan(&stored); err != nil ||
				stored != st.cursor {
				return errSyncPosition
			}
			for _, item := range page.Changes {
				if _, err := p.apply(item, f.id, 0, nil); err != nil {
					return err
				}
			}
			_, err := p.tx.Exec(`UPDATE cursors SET cursor = ? WHERE folder = ?`, page.Cursor, f.id)
			return err
		})
		if errors.Is(err, errSyncPosition) {
			continue
		}
		if err != nil || !page.More {
			return err
		}
		wait = 0
	}
}

// runSnapshot takes a snapshot of the folder, or continues the one in progress, then applies the changes after its
// start and removes what it didn't show. Each page is stored with the position after it, and only if the position
// is still the one it was requested at.
func (s *Service) runSnapshot(ctx context.Context, f folderRef, client *remoteclient.Client, row *snapshotRow) error {
	if row == nil {
		var err error
		if row, err = s.beginSnapshot(f.id); err != nil {
			return err
		}
	}
	for row.catchUpCur == "" {
		page, err := client.Snapshot(ctx, f.remoteID, row.id, row.nextPage, 0)
		if err != nil {
			return err
		}
		if row.id != "" && page.Snapshot != row.id {
			return remoteclient.ErrProtocolViolation
		}
		next := *row
		next.id, next.nextPage = page.Snapshot, page.Next
		if row.id == "" {
			next.startCur = page.Cursor
		}
		if page.Next == "" {
			next.catchUpCur = next.startCur
		} else if page.Next == row.nextPage {
			return remoteclient.ErrProtocolViolation
		}
		err = s.apply(ctx, func(p *projector) error {
			if err := s.checkSnapshot(p.tx, f.id, row); err != nil {
				return err
			}
			for _, item := range page.Items {
				if _, err := p.apply(item, f.id, row.mark, nil); err != nil {
					return err
				}
			}
			_, err := p.tx.Exec(`UPDATE snapshots SET snapshot_id = ?, start_cursor = ?, next_page = ?,
				catch_up_cursor = ? WHERE folder = ?`, next.id, next.startCur, next.nextPage, nullable(next.catchUpCur),
				f.id)
			return err
		})
		if err != nil {
			return err
		}
		row = &next
	}
	// The pages may show states later than the snapshot's start; the changes since then complete it
	for {
		page, err := client.Changes(ctx, f.remoteID, row.catchUpCur, 0, 0)
		if err != nil {
			return err
		}
		if page.More && page.Cursor == row.catchUpCur {
			return remoteclient.ErrProtocolViolation
		}
		next := *row
		next.catchUpCur = page.Cursor
		err = s.apply(ctx, func(p *projector) error {
			if err := s.checkSnapshot(p.tx, f.id, row); err != nil {
				return err
			}
			for _, item := range page.Changes {
				if _, err := p.apply(item, f.id, 0, nil); err != nil {
					return err
				}
			}
			if page.More {
				_, err := p.tx.Exec(`UPDATE snapshots SET catch_up_cursor = ? WHERE folder = ?`, page.Cursor, f.id)
				return err
			}
			// Complete: what it didn't show is gone
			if err := p.finish(); err != nil {
				return err
			}
			if err := p.removeUnseen(f.id, row.mark); err != nil {
				return err
			}
			if _, err := p.tx.Exec(`DELETE FROM snapshots WHERE folder = ?`, f.id); err != nil {
				return err
			}
			_, err := p.tx.Exec(`INSERT INTO cursors (folder, cursor) VALUES (?, ?)
				ON CONFLICT (folder) DO UPDATE SET cursor = excluded.cursor`, f.id, page.Cursor)
			return err
		})
		if err != nil || !page.More {
			return err
		}
		row = &next
	}
}

func nullable(s string) sql.NullString {
	return sql.NullString{String: s, Valid: s != ""}
}

// checkSnapshot fails with errSyncPosition unless the folder's snapshot is still at row
func (s *Service) checkSnapshot(tx *sql.Tx, folder int64, row *snapshotRow) error {
	var id, next, catchUp sql.NullString
	var mark int64
	err := tx.QueryRow(`SELECT snapshot_id, next_page, catch_up_cursor, mark FROM snapshots WHERE folder = ?`, folder).
		Scan(&id, &next, &catchUp, &mark)
	if err != nil || id.String != row.id || next.String != row.nextPage || catchUp.String != row.catchUpCur ||
		mark != row.mark {
		return errSyncPosition
	}
	return nil
}

// beginSnapshot starts a snapshot of the folder, or returns the one another refresh started meanwhile
func (s *Service) beginSnapshot(folder int64) (*snapshotRow, error) {
	err := s.store.tx(context.Background(), func(tx *sql.Tx) error {
		mark, err := allocate(tx, metaNextMark)
		if err != nil {
			return err
		}
		_, err = tx.Exec(`INSERT INTO snapshots (folder, mark) VALUES (?, ?) ON CONFLICT (folder) DO NOTHING`, folder,
			mark)
		return err
	})
	if err != nil {
		return nil, err
	}
	st, err := s.syncState(folder)
	if err != nil {
		return nil, err
	}
	if st.snapshot == nil {
		return nil, errSyncPosition
	}
	return st.snapshot, nil
}

func (s *Service) abandonSnapshot(folder int64) error {
	return s.store.tx(context.Background(), func(tx *sql.Tx) error {
		_, err := tx.Exec(`DELETE FROM snapshots WHERE folder = ?`, folder)
		return err
	})
}

// resetCursor drops the folder's position, so that the next refresh takes a fresh snapshot. Items stay until it is
// complete.
func (s *Service) resetCursor(folder int64) error {
	return s.store.tx(context.Background(), func(tx *sql.Tx) error {
		if _, err := tx.Exec(`DELETE FROM snapshots WHERE folder = ?`, folder); err != nil {
			return err
		}
		_, err := tx.Exec(`DELETE FROM cursors WHERE folder = ?`, folder)
		return err
	})
}

// apply runs fn with a projector in one transaction, then tells the mount what changed
func (s *Service) apply(ctx context.Context, fn func(*projector) error) error {
	var changes Changes
	var resettle map[int64]bool
	err := s.store.tx(ctx, func(tx *sql.Tx) error {
		p := newProjector(tx)
		if err := fn(p); err != nil {
			return err
		}
		if err := p.finish(); err != nil {
			return err
		}
		changes = p.changes
		resettle = p.resettle
		return nil
	})
	if err != nil {
		return err
	}
	s.changed(changes)
	for folder := range resettle {
		s.settleLater(folder)
	}
	return nil
}

// checkCapabilities refreshes what the service knows of a gateway. A new authority generation (the gateway's database
// was reset or restored) means every folder of it takes a fresh snapshot.
func (s *Service) checkCapabilities(ctx context.Context, g gatewayRef, client *remoteclient.Client) error {
	caps, err := client.Capabilities(ctx)
	if err != nil {
		return err
	}
	if caps.ServiceID != g.serviceID {
		return fmt.Errorf("%w: the gateway at %s is another service now", remoteclient.ErrUntrusted, g.host)
	}
	if caps.APIVersion != remoteapi.APIVersion {
		return fmt.Errorf("the gateway speaks API version %d, not %d", caps.APIVersion, remoteapi.APIVersion)
	}
	pins := g.pins
	if len(pins) > 0 && caps.Certificate != nil && caps.Certificate.Next != nil && caps.Certificate.Next.SPKI != "" &&
		!containsString(pins, caps.Certificate.Next.SPKI) {
		// The gateway announced its next key: accept it too, so that the change doesn't need pairing again
		pins = []string{pins[0], caps.Certificate.Next.SPKI}
	}
	features := joinWords(caps.Features)
	err = s.store.tx(ctx, func(tx *sql.Tx) error {
		if g.generation != 0 && g.generation != caps.AuthorityGeneration {
			s.log.Warn("The gateway's database was reset or restored: taking fresh listings", "gateway", caps.ServiceName)
			if _, err := tx.Exec(`DELETE FROM cursors WHERE folder IN (SELECT id FROM folders WHERE service_id = ?)`,
				g.serviceID); err != nil {
				return err
			}
			if _, err := tx.Exec(`DELETE FROM snapshots WHERE folder IN (SELECT id FROM folders WHERE service_id = ?)`,
				g.serviceID); err != nil {
				return err
			}
			// Whether what was sent took effect can't be known: it is never sent again, and waits for review
			if _, err := tx.Exec(`UPDATE outbox SET state = 'unknown', error = 'the gateway''s database was reset or
				restored since it was sent', updated_at = ? WHERE state IN ('submitted', 'pending')
				AND folder IN (SELECT id FROM folders WHERE service_id = ?)`, nowUnix(), g.serviceID); err != nil {
				return err
			}
		}
		_, err := tx.Exec(`UPDATE gateways SET name = ?, authority_generation = ?, features = ?, pins = ?, state = ''
			WHERE service_id = ?`, caps.ServiceName, caps.AuthorityGeneration, features, joinWords(pins), g.serviceID)
		return err
	})
	if err != nil {
		return err
	}
	return s.refreshAccess(ctx, g.serviceID, client)
}

// refreshAccess records what each folder added from a gateway lets this machine do now, as the gateway can change a
// client's access at any time (or approve pairing again with other grants). The kernel drops the modes it has of
// the files of a folder whose access changed.
func (s *Service) refreshAccess(ctx context.Context, serviceID string, client *remoteclient.Client) error {
	remote, err := client.Folders(ctx)
	if err != nil {
		return err
	}
	access := map[string]remoteapi.FolderPermissions{}
	for _, f := range remote {
		access[f.ID] = f.Permissions
	}
	var changes Changes
	err = s.store.tx(ctx, func(tx *sql.Tx) error {
		changes = Changes{}
		rows, err := tx.Query(`SELECT id, remote_id, can_read, can_write FROM folders WHERE service_id = ?`, serviceID)
		if err != nil {
			return err
		}
		changed := map[int64]remoteapi.FolderPermissions{}
		for rows.Next() {
			var id int64
			var remoteID string
			var had remoteapi.FolderPermissions
			if err := rows.Scan(&id, &remoteID, &had.Read, &had.Write); err != nil {
				rows.Close()
				return err
			}
			if now := access[remoteID]; now != had {
				changed[id] = now
			}
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}
		for id, now := range changed {
			s.log.Info("The gateway changed this machine's access to a folder", "folder", id, "read", now.Read,
				"write", now.Write)
			if _, err := tx.Exec(`UPDATE folders SET can_read = ?, can_write = ? WHERE id = ?`, now.Read, now.Write,
				id); err != nil {
				return err
			}
			nodes, err := tx.Query(`SELECT node FROM items WHERE folder = ? AND node != 0`, id)
			if err != nil {
				return err
			}
			for nodes.Next() {
				var node uint64
				if err := nodes.Scan(&node); err != nil {
					nodes.Close()
					return err
				}
				changes.Attrs = append(changes.Attrs, node)
			}
			nodes.Close()
			if err := nodes.Err(); err != nil {
				return err
			}
		}
		return nil
	})
	if err == nil {
		s.changed(changes)
	}
	return err
}
