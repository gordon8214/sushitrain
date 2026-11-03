// Copyright (C) 2026 Gordon
// SPDX-License-Identifier: MPL-2.0
package files

import (
	"context"
	"database/sql"
	"errors"
)

// Thumbnail asks the gateway for a bounded preview without materializing the
// file. Pending local changes never reuse a thumbnail of the remote base.
func (s *Service) Thumbnail(ctx context.Context, item int64, size int) ([]byte, error) {
	if size < 16 || size > 1024 {
		return nil, ErrInvalidName
	}
	var remote, revision, service string
	err := s.store.db.QueryRow(`SELECT r.gateway_id,r.revision,f.service_id FROM remote_items r JOIN items i ON i.id=r.item JOIN folders f ON f.id=i.folder WHERE r.item=? AND i.deleted=0 AND i.local=0 AND i.local_content IS NULL`, item).Scan(&remote, &revision, &service)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrContentUnavailable
	}
	if err != nil {
		return nil, err
	}
	client, err := s.client(service)
	if err != nil {
		return nil, err
	}
	return client.Thumbnail(ctx, remote, revision, size)
}
