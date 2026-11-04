// Copyright (C) 2026 Gordon
// SPDX-License-Identifier: MPL-2.0
package files

import (
	"context"
	"errors"
	"math/bits"
)

// TransferProgress exposes resumable download progress without authentication or
// internal staging paths. Uploads remain visible through the durable edit queue.
type TransferProgress struct {
	ID             string `json:"id"`
	Item           int64  `json:"item"`
	TotalBytes     int64  `json:"totalBytes"`
	VerifiedBlocks int    `json:"verifiedBlocks"`
	TotalBlocks    int    `json:"totalBlocks"`
}

func (s *Service) Transfers(ctx context.Context) ([]TransferProgress, error) {
	rows, err := s.store.db.QueryContext(ctx, `SELECT content_id, item, size, block_count, verified FROM transfers ORDER BY updated_at LIMIT 10001`)
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
		if err := rows.Scan(&transfer.ID, &transfer.Item, &transfer.TotalBytes, &transfer.TotalBlocks, &bitmap); err != nil {
			return nil, err
		}
		for _, value := range bitmap {
			transfer.VerifiedBlocks += bits.OnesCount8(value)
		}
		transfer.VerifiedBlocks = min(transfer.VerifiedBlocks, transfer.TotalBlocks)
		out = append(out, transfer)
	}
	return out, rows.Err()
}
