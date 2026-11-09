// Copyright (C) 2026 Gordon
// SPDX-License-Identifier: MPL-2.0
package files

import (
	"cmp"
	"context"
	"errors"
	"math/bits"
	"slices"
)

// TransferProgress exposes resumable download progress without authentication or
// internal staging paths. Uploads remain visible through the durable edit queue.
type TransferProgress struct {
	ID   string `json:"id"`
	Item int64  `json:"item"`
	// Slash-separated below the mount's root, starting with the folder (empty when the item is gone)
	Path       string `json:"path"`
	TotalBytes int64  `json:"totalBytes"`
	// An estimate from the share of blocks verified, as blocks differ in size
	VerifiedBytes  int64 `json:"verifiedBytes"`
	VerifiedBlocks int   `json:"verifiedBlocks"`
	// 0 until the gateway's manifest of the content is in
	TotalBlocks int `json:"totalBlocks"`
	// "downloading" while it runs, "failed" when it stopped on an error, and "paused" when nothing reads it now (it
	// resumes from its verified blocks when something does)
	State string `json:"state"`
	// Why it failed, or why a running download waits to try again
	Error string `json:"error,omitempty"`
}

const (
	TransferDownloading = "downloading"
	TransferFailed      = "failed"
	TransferPaused      = "paused"
)

// liveTransfer is what a download in memory has done, which is ahead of its record
type liveTransfer struct {
	item, size      int64
	verified, total int
	running         bool
	err, retryErr   error
}

func (c *cache) liveTransfers() map[string]liveTransfer {
	c.mutex.Lock()
	defer c.mutex.Unlock()
	out := make(map[string]liveTransfer, len(c.transfers))
	for id, t := range c.transfers {
		t.mutex.Lock()
		out[id] = liveTransfer{item: t.item, size: t.size, verified: t.count, total: len(t.blocks), running: t.running,
			err: t.err, retryErr: t.retryErr}
		t.mutex.Unlock()
	}
	return out
}

// transferPaths gives each record's item its path below the mount's root in one query
const transferPaths = `WITH RECURSIVE up(content_id, parent, path) AS (
		SELECT t.content_id, i.parent, i.name FROM transfers t JOIN items i ON i.id = t.item AND i.deleted = 0
		UNION ALL
		SELECT up.content_id, i.parent, i.name || '/' || up.path FROM up JOIN items i ON i.id = up.parent
	)`

// Transfers lists the downloads in the order they started, then those without a record yet. Records of content no
// item has any more are left out: nothing would ever resume them.
func (s *Service) Transfers(ctx context.Context) ([]TransferProgress, error) {
	live := map[string]liveTransfer{}
	if s.cache != nil {
		live = s.cache.liveTransfers()
	}
	rows, err := s.store.db.QueryContext(ctx, transferPaths+`
		SELECT t.content_id, t.item, t.size, t.block_count, t.verified,
			COALESCE((SELECT path FROM up WHERE up.content_id = t.content_id AND up.parent IS NULL), '')
		FROM transfers t
		WHERE EXISTS (SELECT 1 FROM items WHERE content_id = t.content_id AND deleted = 0)
		ORDER BY t.rowid LIMIT 10001`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []TransferProgress{}
	for rows.Next() {
		if len(out) == 10000 {
			return nil, errors.New("too many transfers; wait for transfers to settle")
		}
		var transfer TransferProgress
		var bitmap []byte
		if err := rows.Scan(&transfer.ID, &transfer.Item, &transfer.TotalBytes, &transfer.TotalBlocks, &bitmap,
			&transfer.Path); err != nil {
			return nil, err
		}
		for _, value := range bitmap {
			transfer.VerifiedBlocks += bits.OnesCount8(value)
		}
		transfer.State = TransferPaused
		if t, ok := live[transfer.ID]; ok {
			delete(live, transfer.ID)
			if t.total > 0 {
				transfer.VerifiedBlocks, transfer.TotalBlocks = t.verified, t.total
			}
			if t.item != transfer.Item {
				// The download moved to another item with the same content
				transfer.Item, transfer.Path = t.item, s.pathOrEmpty(t.item)
			}
			transfer.State, transfer.Error = liveState(t)
		}
		out = append(out, transfer.finish())
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	// Downloads that have no record yet, as their manifest isn't in
	var pending []TransferProgress
	for id, t := range live {
		if !t.running && t.err == nil {
			continue
		}
		transfer := TransferProgress{ID: id, Item: t.item, Path: s.pathOrEmpty(t.item), TotalBytes: t.size,
			VerifiedBlocks: t.verified, TotalBlocks: t.total}
		transfer.State, transfer.Error = liveState(t)
		pending = append(pending, transfer.finish())
	}
	slices.SortFunc(pending, func(a, b TransferProgress) int { return cmp.Compare(a.ID, b.ID) })
	return append(out, pending...), nil
}

// pathOrEmpty is the item's path, or empty when an item on the way is gone
func (s *Service) pathOrEmpty(item int64) string {
	path, err := s.itemPath(item)
	if err != nil {
		return ""
	}
	return path
}

func liveState(t liveTransfer) (string, string) {
	switch {
	case t.err != nil:
		return TransferFailed, t.err.Error()
	case t.running && t.retryErr != nil:
		return TransferDownloading, t.retryErr.Error()
	case t.running:
		return TransferDownloading, ""
	default:
		return TransferPaused, ""
	}
}

func (t TransferProgress) finish() TransferProgress {
	t.VerifiedBlocks = min(max(t.VerifiedBlocks, 0), t.TotalBlocks)
	if t.TotalBlocks > 0 {
		t.VerifiedBytes = min(t.TotalBytes, int64(float64(t.TotalBytes)*float64(t.VerifiedBlocks)/float64(t.TotalBlocks)))
	}
	return t
}
