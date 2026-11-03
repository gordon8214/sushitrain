// Copyright (C) 2026 Gordon
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this file,
// You can obtain one at https://mozilla.org/MPL/2.0/.

package files

import (
	"context"
	"database/sql"
	"time"
)

// How long the rows of removed items and of finished operations are kept. Nothing here asks about them later (the
// kernel is told of a removal at once, unlike Finder, which is why the Mac keeps them for 30 days), except an item
// that comes back soon, which then keeps its row.
const pruneAfter = 7 * 24 * time.Hour

// runPruning prunes the index at start and then once a day
func (s *Service) runPruning(ctx context.Context) {
	for {
		if err := s.prune(time.Now().Add(-pruneAfter)); err != nil && ctx.Err() == nil {
			s.log.Warn("Could not prune the index", "error", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(24 * time.Hour):
		}
	}
}

// prune removes what is left of items removed before cutoff that nothing refers to any more, the gateway's rows of
// removed items that no item shows, and operations finished before cutoff that no unresolved one after them builds
// its base on
func (s *Service) prune(cutoff time.Time) error {
	before := cutoff.Unix()
	return s.store.tx(context.Background(), func(tx *sql.Tx) error {
		// A directory's row goes after those of everything that was in it
		for range 64 {
			res, err := tx.Exec(`DELETE FROM items AS i WHERE i.deleted = 1 AND i.deleted_at < ? AND NOT `+overlaid+`
				AND NOT EXISTS (SELECT 1 FROM work AS w WHERE w.item = i.id)
				AND NOT EXISTS (SELECT 1 FROM items AS c WHERE c.parent = i.id)
				AND NOT EXISTS (SELECT 1 FROM remote_items AS r WHERE r.item = i.id AND r.deleted = 0)`, before)
			if err != nil {
				return err
			}
			if n, err := res.RowsAffected(); err != nil || n == 0 {
				break
			}
		}
		if _, err := tx.Exec(`DELETE FROM remote_items WHERE deleted = 1
			AND (item IS NULL OR NOT EXISTS (SELECT 1 FROM items WHERE id = remote_items.item))`); err != nil {
			return err
		}
		_, err := tx.Exec(`DELETE FROM outbox AS o WHERE o.state IN ('committed', 'conflict', 'canceled')
			AND o.updated_at < ? AND NOT EXISTS (SELECT 1 FROM outbox AS later WHERE later.item = o.item
				AND later.id > o.id AND later.state IN `+unresolvedStates+`)`, before)
		return err
	})
}
