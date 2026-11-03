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
	"io"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"time"

	"github.com/google/uuid"
	"t-shaped.nl/sushitrain/v2/remoteapi"
	"t-shaped.nl/sushitrain/v2/remoteclient"
)

// Outbox states. queued, uploaded, submitted and pending are on their way; committed, conflict and canceled are done;
// failed, unknown and blocked wait for someone to export or discard them.
const (
	stateQueued    = "queued"
	stateUploaded  = "uploaded"
	stateSubmitted = "submitted"
	statePending   = "pending"
	stateCommitted = "committed"
	stateConflict  = "conflict"
	stateCanceled  = "canceled"
	stateFailed    = "failed"
	stateUnknown   = "unknown"
	stateBlocked   = "blocked"
)

const activeStates = `('queued', 'uploaded', 'submitted', 'pending')`

type outboxRow struct {
	id          int64
	opID        string
	folder      int64
	item        int64
	kind        string
	itemKind    string
	state       string
	baseRev     string
	baseContent string
	subtree     sql.NullInt64
	awaited     sql.NullInt64
	resends     int
	targetPar   sql.NullInt64
	targetName  string
	staged      sql.NullString
	digest      string
	size        int64
	modNs       sql.NullInt64
	uploadID    string
	payload     []byte
	serverOp    string
	generation  sql.NullInt64
	attempts    int
}

const outboxColumns = `id, op_id, folder, item, kind, item_kind, state, base_revision, base_content_id,
	subtree_generation, awaited_generation, resends, target_parent, target_name, staged, digest, size, mod_ns,
	upload_id, payload, server_op, authority_generation, attempts`

func scanOutbox(row interface{ Scan(...any) error }) (*outboxRow, error) {
	var o outboxRow
	err := row.Scan(&o.id, &o.opID, &o.folder, &o.item, &o.kind, &o.itemKind, &o.state, &o.baseRev, &o.baseContent,
		&o.subtree, &o.awaited, &o.resends, &o.targetPar, &o.targetName, &o.staged, &o.digest, &o.size, &o.modNs, &o.uploadID, &o.payload,
		&o.serverOp, &o.generation, &o.attempts)
	return &o, err
}

func (s *Service) wakeOutbox() {
	select {
	case s.outboxWake <- struct{}{}:
	default:
	}
}

// runOutbox sends the operations, one at a time per folder in their order
func (s *Service) runOutbox(ctx context.Context) {
	// Whether a gateway's database was reset or restored while the daemon didn't run must be known before anything
	// left submitted is sent again: it would be sent twice
	s.checkGateways(ctx)
	for ctx.Err() == nil {
		wait, err := s.outboxPass(ctx)
		if err != nil && ctx.Err() == nil {
			s.log.Warn("Could not send changes", "error", err)
			wait = 5 * time.Second
		}
		select {
		case <-ctx.Done():
			return
		case <-s.outboxWake:
		case <-time.After(wait):
		}
	}
}

// outboxPass handles the first unfinished operation of each folder whose time has come, and returns how long to wait
// before the next pass
func (s *Service) outboxPass(ctx context.Context) (time.Duration, error) {
	rows, err := s.store.db.Query(`SELECT ` + outboxColumns + ` FROM outbox AS o WHERE state IN ` + activeStates + `
		AND id = (SELECT MIN(id) FROM outbox WHERE folder = o.folder AND state IN ` + activeStates + `)`)
	if err != nil {
		return 0, err
	}
	var heads []*outboxRow
	for rows.Next() {
		o, err := scanOutbox(rows)
		if err != nil {
			rows.Close()
			return 0, err
		}
		heads = append(heads, o)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, err
	}
	wait := 10 * time.Minute
	worked := false
	for _, head := range heads {
		// Read again right before its step: settling may have merged a newer change into it, or a discard canceled
		// it, while the heads before it were stepped (an upload can take minutes)
		o, err := loadOutbox(s.store.db, head.id)
		if errors.Is(err, sql.ErrNoRows) {
			continue
		}
		if err != nil {
			return 0, err
		}
		if !slices.Contains([]string{stateQueued, stateUploaded, stateSubmitted, statePending}, o.state) {
			worked = true
			continue
		}
		if o, err = s.neededFirst(o); err != nil {
			return 0, err
		}
		var notBefore int64
		if err := s.store.db.QueryRow(`SELECT not_before FROM outbox WHERE id = ?`, o.id).Scan(&notBefore); err != nil {
			return 0, err
		}
		if now := nowMillis(); notBefore > now {
			wait = min(wait, time.Duration(notBefore-now)*time.Millisecond)
			continue
		}
		if err := s.step(ctx, o); err != nil && !errors.Is(err, errStepAgain) {
			if ctx.Err() != nil {
				return 0, nil
			}
			wait = min(wait, s.retryLater(o, err))
			continue
		}
		worked = true
	}
	if worked {
		return 0, nil
	}
	return wait, nil
}

// neededFirst returns the operation to step instead of o: the creation of the directory o creates or moves something
// into, when that directory isn't on the gateway and its creation was queued after o (removed on the gateway while
// something in it changed here, it is made again), and so on up. Otherwise o. Only a creation nothing before it
// waits for goes out of turn: the order of everything else is what keeps names and moves straight.
func (s *Service) neededFirst(o *outboxRow) (*outboxRow, error) {
	f, err := s.folderGateway(s.store.db, o.folder)
	if err != nil {
		return nil, err
	}
	for range 256 {
		if (o.kind != remoteapi.OpCreate && o.kind != remoteapi.OpMove) || !o.targetPar.Valid ||
			(o.state != stateQueued && o.state != stateUploaded) {
			return o, nil
		}
		if parentID, err := gatewayIDOf(s.store.db, o.targetPar.Int64, f); err != nil || parentID != "" {
			return o, err
		}
		creation, err := scanOutbox(s.store.db.QueryRow(`SELECT `+outboxColumns+` FROM outbox WHERE item = ?
			AND folder = ? AND kind = 'create' AND state IN `+activeStates+` ORDER BY id LIMIT 1`, o.targetPar.Int64,
			o.folder))
		if errors.Is(err, sql.ErrNoRows) {
			return o, nil
		}
		if err != nil {
			return nil, err
		}
		o = creation
	}
	return o, nil
}

// retryLater records a failure that left the operation as it was, and returns how long to wait before trying again:
// longer each time, so that one that keeps failing doesn't keep the outbox busy
func (s *Service) retryLater(o *outboxRow, err error) time.Duration {
	delay := min(time.Duration(1<<min(o.attempts, 8))*time.Second, 5*time.Minute)
	var e *remoteclient.Error
	switch {
	case remoteclient.KindOf(err) == remoteclient.KindUnauthenticated,
		remoteclient.KindOf(err) == remoteclient.KindUntrusted:
		delay = 5 * time.Minute
	case errors.As(err, &e) && e.RetryAfter > delay:
		delay = e.RetryAfter
	}
	s.log.Debug("Will send a change again", "operation", o.opID, "kind", o.kind, "error", err, "retry", delay)
	_ = s.store.tx(context.Background(), func(tx *sql.Tx) error {
		_, err := tx.Exec(`UPDATE outbox SET attempts = attempts + 1, not_before = ?, error = ?, updated_at = ?
			WHERE id = ?`, nowMillis()+delay.Milliseconds(), err.Error(), nowUnix(), o.id)
		return err
	})
	return delay
}

// folderGateway is what an operation needs of its folder
type folderGateway struct {
	serviceID   string
	remoteID    string
	rootItem    int64
	rootGateway sql.NullString
	generation  sql.NullInt64
}

func (s *Service) folderGateway(q queryer, folder int64) (folderGateway, error) {
	var f folderGateway
	err := q.QueryRow(`SELECT f.service_id, f.remote_id, f.root_item, f.root_gateway_id, g.authority_generation
		FROM folders AS f JOIN gateways AS g ON g.service_id = f.service_id WHERE f.id = ?`, folder).
		Scan(&f.serviceID, &f.remoteID, &f.rootItem, &f.rootGateway, &f.generation)
	return f, err
}

// step takes an operation one step further
func (s *Service) step(ctx context.Context, o *outboxRow) error {
	f, err := s.folderGateway(s.store.db, o.folder)
	if err != nil {
		return err
	}
	client, err := s.client(f.serviceID)
	if err != nil {
		return err
	}
	switch o.state {
	case stateQueued:
		return s.upload(ctx, o, f, client)
	case stateUploaded:
		return s.prepare(o, f)
	case stateSubmitted:
		if o.generation.Valid && f.generation.Valid && o.generation.Int64 != f.generation.Int64 {
			// The gateway's database was reset or restored since it was sent: whether it took effect can't be known,
			// and it is never sent again
			return s.setState(o.id, stateUnknown, "the gateway's database was reset or restored since it was sent")
		}
		if err := s.hook("outbox.submit"); err != nil {
			return err
		}
		result, err := client.SubmitOperation(ctx, o.payload)
		if err == nil {
			err = s.hook("outbox.sent")
		}
		if err != nil {
			if lost, err := s.uploadLost(ctx, client, o, err); lost || err != nil {
				return err
			}
			return s.refused(o, err)
		}
		return s.recordResult(o, f, result)
	case statePending:
		result, err := client.Operation(ctx, o.serverOp)
		if remoteclient.KindOf(err) == remoteclient.KindNotFound {
			return s.setState(o.id, stateUnknown, "the gateway doesn't know the operation any more")
		}
		var e *remoteclient.Error
		if errors.As(err, &e) && !e.Transient() && e.Kind != remoteclient.KindUnauthenticated &&
			e.Kind != remoteclient.KindUntrusted && e.Kind != remoteclient.KindProtocol {
			// Refused for good (access was taken away, say): how it ended can't be known any more
			return s.setState(o.id, stateUnknown, "the gateway can't be asked how it ended: "+e.Error())
		}
		if err != nil {
			return err
		}
		return s.recordResult(o, f, result)
	}
	return nil
}

// checkGateways checks the capabilities of every gateway that operations wait for. One out of reach is checked by its
// folders' loops before their next refresh; until then nothing is sent to it either.
func (s *Service) checkGateways(ctx context.Context) {
	rows, err := s.store.db.Query(`SELECT DISTINCT f.service_id FROM outbox AS o JOIN folders AS f ON f.id = o.folder
		WHERE o.state IN ('submitted', 'pending')`)
	if err != nil {
		return
	}
	var ids []string
	for rows.Next() {
		var id string
		if rows.Scan(&id) == nil {
			ids = append(ids, id)
		}
	}
	rows.Close()
	for _, id := range ids {
		g, err := s.gatewayRef(id)
		if err != nil {
			continue
		}
		client, err := s.client(id)
		if err != nil {
			continue
		}
		checkCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
		if err := s.checkCapabilities(checkCtx, g, client); err != nil {
			s.log.Debug("Could not check a gateway before sending", "gateway", id, "error", err)
		}
		cancel()
	}
}

// loadOutbox reads an operation as it is now
func loadOutbox(q queryer, id int64) (*outboxRow, error) {
	return scanOutbox(q.QueryRow(`SELECT `+outboxColumns+` FROM outbox WHERE id = ?`, id))
}

func (s *Service) setState(id int64, state, message string) error {
	err := s.store.tx(context.Background(), func(tx *sql.Tx) error {
		_, err := tx.Exec(`UPDATE outbox SET state = ?, error = ?, updated_at = ? WHERE id = ?`, state, message, nowUnix(),
			id)
		return err
	})
	if err == nil && (state == stateFailed || state == stateUnknown || state == stateBlocked) {
		s.log.Warn("A change can't be sent; export or discard it", "operation", id, "state", state, "reason", message)
		s.reviewNeeded()
	}
	return err
}

// needsUpload reports whether an operation sends content
func (o *outboxRow) needsUpload() bool {
	return o.staged.Valid && (o.kind == remoteapi.OpReplace ||
		(o.kind == remoteapi.OpCreate && o.itemKind == remoteapi.KindFile))
}

// upload sends the operation's content, recording the upload before any byte of it is sent
func (s *Service) upload(ctx context.Context, o *outboxRow, f folderGateway, client *remoteclient.Client) error {
	if o.needsUpload() {
		file, err := os.Open(filepath.Join(s.cfg.StateDir, o.staged.String))
		if err != nil {
			return s.setState(o.id, stateBlocked, "its content is missing: "+err.Error())
		}
		defer file.Close()
		digest, err := remoteapi.B64.DecodeString(o.digest)
		if err != nil || len(digest) != 32 {
			return s.setState(o.id, stateBlocked, "its content's digest is invalid")
		}
		upload := remoteapi.Upload{Upload: o.uploadID, Length: o.size}
		if o.uploadID == "" {
			var sum [32]byte
			copy(sum[:], digest)
			created, err := client.CreateUpload(ctx, f.remoteID, o.size, sum)
			if err != nil {
				return s.refused(o, err)
			}
			upload.Upload = created.Upload
			// Only for the content this upload sends: a newer change merged meanwhile has its own
			if err := s.guarded(ctx, `UPDATE outbox SET upload_id = ?, updated_at = ? WHERE id = ? AND state = 'queued'
				AND upload_id = '' AND staged IS ?`, upload.Upload, nowUnix(), o.id, o.staged); err != nil {
				return err
			}
			o.uploadID = upload.Upload
		}
		if err := client.SendUpload(ctx, upload, file, nil); err != nil {
			if remoteclient.KindOf(err) == remoteclient.KindNotFound {
				// Expired or removed: a new upload
				return s.guarded(ctx, `UPDATE outbox SET upload_id = '', updated_at = ? WHERE id = ? AND upload_id = ?`,
					nowUnix(), o.id, o.uploadID)
			}
			var e *remoteclient.Error
			if errors.As(err, &e) && e.Code == remoteapi.CodeUploadMismatch {
				return s.refused(o, err)
			}
			return err
		}
		if err := s.hook("outbox.uploaded"); err != nil {
			return err
		}
	}
	return s.guarded(ctx, `UPDATE outbox SET state = 'uploaded', attempts = 0, updated_at = ? WHERE id = ?
		AND state = 'queued' AND upload_id = ? AND staged IS ?`, nowUnix(), o.id, o.uploadID, o.staged)
}

// guarded runs an update of an operation that holds only if it is still as the step read it, and errStepAgain if
// it isn't: the next pass takes it as it is then
func (s *Service) guarded(ctx context.Context, query string, args ...any) error {
	return s.store.tx(ctx, func(tx *sql.Tx) error {
		res, err := tx.Exec(query, args...)
		if err != nil {
			return err
		}
		if n, err := res.RowsAffected(); err != nil {
			return err
		} else if n == 0 {
			return errStepAgain
		}
		return nil
	})
}

// gatewayIDOf is the gateway item an item shows, or "" while it has none (it was never sent, or its creation isn't
// committed)
func gatewayIDOf(q queryer, item int64, f folderGateway) (string, error) {
	if item == f.rootItem {
		return f.rootGateway.String, nil
	}
	var id string
	err := q.QueryRow(`SELECT gateway_id FROM remote_items WHERE item = ?`, item).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return id, err
}

// previousResult is what the operation before this one on the same item left of it on the gateway: its revision and
// content, and how it ended
func previousResult(q queryer, o *outboxRow) (revision, contentID, state string, err error) {
	err = q.QueryRow(`SELECT result_revision, result_content_id, state FROM outbox WHERE item = ? AND id < ?
		AND state != 'canceled' ORDER BY id DESC LIMIT 1`, o.item, o.id).Scan(&revision, &contentID, &state)
	if errors.Is(err, sql.ErrNoRows) {
		return "", "", "", nil
	}
	return
}

// errStepAgain means the operation changed (it became another one): the next pass takes it from its new state
var errStepAgain = errors.New("the operation changed")

// errFeedBehind means an operation waits for the change feed to show what the gateway reported
var errFeedBehind = errors.New("waiting for the change feed to catch up with the gateway")

// How many times an operation the gateway refused is sent again as a new intent: a directory's removal it found out of
// date, a creation or move whose directory went, one whose upload is gone or doesn't match
const maxResends = 5

// removalStillSeen checks a directory's removal the gateway found out of date, once the change feed has reached the
// subtree generation it reported: it may go again, with the feed's generation, only if the directory is where it was
// removed here and everything the gateway has below it is something removed here, with the content this client last
// had of it (so only this client's own changes came in between). Returns the generation to send, and whether it may.
func removalStillSeen(tx *sql.Tx, o *outboxRow, gatewayID string, f folderGateway) (int64, bool, error) {
	var generation sql.NullInt64
	var deleted bool
	var name string
	var parent sql.NullString
	if err := tx.QueryRow(`SELECT subtree_generation, deleted, name, parent_gateway_id FROM remote_items
		WHERE gateway_id = ?`, gatewayID).Scan(&generation, &deleted, &name, &parent); err != nil {
		return 0, false, err
	}
	if deleted {
		// Removed on the gateway meanwhile: the gateway answers that the removal holds
		return generation.Int64, true, nil
	}
	if generation.Int64 < o.awaited.Int64 {
		return 0, false, errFeedBehind
	}
	var itemName string
	var itemParent sql.NullInt64
	if err := tx.QueryRow(`SELECT name, parent FROM items WHERE id = ?`, o.item).Scan(&itemName,
		&itemParent); err != nil {
		return 0, false, err
	}
	var parentID string
	if itemParent.Valid {
		var err error
		if parentID, err = gatewayIDOf(tx, itemParent.Int64, f); err != nil {
			return 0, false, err
		}
	}
	if normalizedName(name) != itemName || parent.String != parentID {
		// Moved or renamed on the gateway
		return 0, false, nil
	}
	var unseen bool
	err := tx.QueryRow(`WITH RECURSIVE below(id) AS (SELECT ?1 UNION ALL
			SELECT r.gateway_id FROM remote_items AS r JOIN below ON r.parent_gateway_id = below.id WHERE r.deleted = 0)
		SELECT EXISTS (SELECT 1 FROM remote_items AS r LEFT JOIN items AS i ON i.id = r.item
			WHERE r.gateway_id IN (SELECT id FROM below) AND r.gateway_id != ?1 AND (i.id IS NULL OR i.deleted = 0
				OR (r.kind != ?2 AND IFNULL(r.content_id, '') != IFNULL(i.content_id, ''))))`, gatewayID,
		remoteapi.KindDirectory).Scan(&unseen)
	return generation.Int64, !unseen, err
}

// prepare builds the request and stores it, exactly as it is sent and sent again, in the transaction that decides
// what it is: the gateway IDs it refers to, its base, and whether an edit from a base the gateway has moved on from
// becomes a conflict copy
func (s *Service) prepare(read *outboxRow, f folderGateway) error {
	var changes Changes
	err := s.store.tx(context.Background(), func(tx *sql.Tx) error {
		// The operation as it is in this transaction, which nothing changes meanwhile: what the step read before may
		// be older (a newer save merged into it, or a discard)
		o, err := loadOutbox(tx, read.id)
		if err != nil {
			return err
		}
		if o.state != stateUploaded || o.uploadID != read.uploadID || o.staged != read.staged {
			return errStepAgain
		}
		req := remoteapi.OperationRequest{ID: o.opID, Kind: o.kind, FolderID: f.remoteID}
		gatewayID, err := gatewayIDOf(tx, o.item, f)
		if err != nil {
			return err
		}
		baseRev, baseContent := o.baseRev, o.baseContent
		if baseRev == "" && o.kind != remoteapi.OpCreate {
			// Following an earlier change of the item: based on what that one committed, and on nothing else (the
			// gateway's latest revision may be another device's edit)
			revision, contentID, state, err := previousResult(tx, o)
			if err != nil {
				return err
			}
			switch state {
			case stateCommitted:
				baseRev, baseContent = revision, contentID
			case stateConflict:
				if o.kind == remoteapi.OpReplace {
					p := newProjector(tx)
					err := s.conflictCopy(tx, p, o)
					changes.merge(p.changes)
					return err
				}
				return blockTx(tx, o.id, "an earlier change of it was kept as a conflict copy")
			case stateFailed, stateUnknown, stateBlocked:
				return blockTx(tx, o.id, "an earlier change of it can't be sent")
			case "":
				var undone bool
				if err := tx.QueryRow(`SELECT EXISTS (SELECT 1 FROM outbox WHERE item = ? AND id < ?
					AND state = 'canceled' AND error != 'discarded')`, o.item, o.id).Scan(&undone); err != nil {
					return err
				}
				if undone {
					return blockTx(tx, o.id, "an earlier change of it was undone")
				}
			}
		}
		if o.kind != remoteapi.OpCreate && gatewayID == "" {
			switch o.kind {
			case remoteapi.OpDelete:
				// Gone from the gateway already
				_, err := tx.Exec(`UPDATE outbox SET state = 'committed', updated_at = ? WHERE id = ?`, nowUnix(), o.id)
				if err != nil {
					return err
				}
				p := newProjector(tx)
				err = s.resolveItem(tx, p, o.item)
				changes.merge(p.changes)
				return err
			case remoteapi.OpReplace:
				// It was removed on the gateway: the edit makes it again
				return s.convertToCreate(tx, o)
			default:
				return blockTx(tx, o.id, "it isn't on the gateway any more")
			}
		}
		switch o.kind {
		case remoteapi.OpCreate, remoteapi.OpMove:
			parentID, err := gatewayIDOf(tx, o.targetPar.Int64, f)
			if err != nil {
				return err
			}
			if parentID == "" {
				return blockTx(tx, o.id, "its directory isn't on the gateway")
			}
			req.ParentID, req.Name = parentID, o.targetName
		}
		switch o.kind {
		case remoteapi.OpCreate:
			req.ItemKind = o.itemKind
		case remoteapi.OpReplace:
			var current remoteRow
			err := tx.QueryRow(`SELECT revision, content_id FROM remote_items WHERE gateway_id = ?`, gatewayID).
				Scan(&current.revision, &current.contentID)
			if err != nil {
				return err
			}
			if current.contentID.String != baseContent {
				// Changed on the gateway since the change here was based on it: kept as a conflict copy
				p := newProjector(tx)
				err := s.conflictCopy(tx, p, o)
				changes.merge(p.changes)
				return err
			}
			// The same content under a newer revision (a move, a touch): based on that
			req.ItemID, req.BaseRevision, req.OnConflict = gatewayID, current.revision, remoteapi.OnConflictKeepBoth
		case remoteapi.OpMove, remoteapi.OpDelete:
			req.ItemID, req.BaseRevision = gatewayID, baseRev
			if o.kind == remoteapi.OpDelete && o.itemKind == "" {
				var kind string
				if err := tx.QueryRow(`SELECT kind FROM remote_items WHERE gateway_id = ?`, gatewayID).
					Scan(&kind); err != nil {
					return err
				}
				if kind == remoteapi.KindDirectory {
					// Not while a change of something that was below it can't be sent: that change (a move out of
					// it, say) would be lost with it
					var stuck bool
					if err := tx.QueryRow(`WITH RECURSIVE below(id) AS (SELECT ?1 UNION ALL
							SELECT r.gateway_id FROM remote_items AS r JOIN below ON r.parent_gateway_id = below.id)
						SELECT EXISTS (SELECT 1 FROM outbox AS o JOIN remote_items AS r ON r.item = o.item
							WHERE r.gateway_id IN (SELECT id FROM below) AND o.id < ?2
							AND o.state IN ('failed', 'unknown', 'blocked'))`, gatewayID, o.id).Scan(&stuck); err != nil {
						return err
					}
					if stuck {
						return blockTx(tx, o.id, "a change of something in it can't be sent: export or discard that first")
					}
					subtree := o.subtree
					if o.awaited.Valid {
						generation, seen, err := removalStillSeen(tx, o, gatewayID, f)
						if err != nil {
							return err
						}
						if !seen {
							s.log.Info("A removal was undone, as something in it changed on the gateway",
								"operation", o.opID)
							if _, err := tx.Exec(`UPDATE outbox SET state = 'canceled', updated_at = ? WHERE id = ?`,
								nowUnix(), o.id); err != nil {
								return err
							}
							p := newProjector(tx)
							err := s.undo(tx, p, o.item)
							changes.merge(p.changes)
							return err
						}
						subtree = sql.NullInt64{Int64: generation, Valid: true}
					}
					if !subtree.Valid {
						if err := tx.QueryRow(`SELECT subtree_generation FROM remote_items WHERE gateway_id = ?`,
							gatewayID).Scan(&subtree); err != nil {
							return err
						}
					}
					req.SubtreeGeneration = &subtree.Int64
				}
			}
			if req.BaseRevision == "" {
				if err := tx.QueryRow(`SELECT revision FROM remote_items WHERE gateway_id = ?`, gatewayID).
					Scan(&req.BaseRevision); err != nil {
					return err
				}
			}
		}
		if o.needsUpload() {
			req.Upload, req.UploadDigest = o.uploadID, o.digest
			if o.modNs.Valid {
				req.ModifiedNs = &o.modNs.Int64
			}
		}
		payload, err := remoteclient.EncodeOperation(req)
		if err != nil {
			return err
		}
		// The base recorded is the one sent, which the result is checked against
		if req.BaseRevision != "" {
			baseRev = req.BaseRevision
		}
		_, err = tx.Exec(`UPDATE outbox SET payload = ?, state = 'submitted', authority_generation = ?, attempts = 0,
			base_revision = ?, base_content_id = ?, updated_at = ? WHERE id = ? AND state = 'uploaded'`, payload,
			f.generation, baseRev, baseContent, nowUnix(), o.id)
		return err
	})
	s.changed(changes)
	return err
}

func blockTx(tx *sql.Tx, id int64, message string) error {
	_, err := tx.Exec(`UPDATE outbox SET state = 'blocked', error = ?, updated_at = ? WHERE id = ?`, message, nowUnix(),
		id)
	return err
}

// convertToCreate makes an edit of an item that is gone from the gateway create it again where it is here
func (s *Service) convertToCreate(tx *sql.Tx, o *outboxRow) error {
	var parent sql.NullInt64
	var name string
	if err := tx.QueryRow(`SELECT parent, name FROM items WHERE id = ?`, o.item).Scan(&parent, &name); err != nil {
		return err
	}
	if _, err := tx.Exec(`UPDATE remote_items SET item = NULL, placed = 1 WHERE item = ?`, o.item); err != nil {
		return err
	}
	_, err := tx.Exec(`UPDATE outbox SET kind = 'create', item_kind = 'file', op_id = ?, target_parent = ?,
		target_name = ?, payload = NULL, state = 'uploaded', updated_at = ? WHERE id = ?`, uuid.NewString(), parent, name,
		nowUnix(), o.id)
	return err
}

// conflictCopy keeps an edit made from a base the gateway has moved on from as a new file next to the item, named as
// Syncthing names conflict copies; the item shows the gateway's version again
func (s *Service) conflictCopy(tx *sql.Tx, p *projector, o *outboxRow) error {
	var folder int64
	var parent sql.NullInt64
	var name string
	if err := tx.QueryRow(`SELECT folder, parent, name FROM items WHERE id = ?`, o.item).
		Scan(&folder, &parent, &name); err != nil {
		return err
	}
	clientID, err := metaValue(tx, metaClientID)
	if err != nil {
		return err
	}
	copyName := conflictName(name, clientID, time.Now())
	node, err := allocate(tx, metaNextNode)
	if err != nil {
		return err
	}
	res, err := tx.Exec(`INSERT INTO items (folder, parent, name, kind, size, mod_ns, node, local_content)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)`, folder, parent, copyName, kindFile, o.size, o.modNs.Int64, node, o.staged)
	if err != nil {
		return err
	}
	copyID, err := res.LastInsertId()
	if err != nil {
		return err
	}
	parentNode, err := p.nodeOf(parent)
	if err != nil {
		return err
	}
	p.changes.entry(parentNode, copyName, 0)
	if _, err := tx.Exec(`UPDATE outbox SET kind = 'create', item_kind = 'file', item = ?, op_id = ?,
		target_parent = ?, target_name = ?, payload = NULL, state = 'uploaded', updated_at = ? WHERE id = ?`, copyID,
		uuid.NewString(), parent, copyName, nowUnix(), o.id); err != nil {
		return err
	}
	if _, err := tx.Exec(`UPDATE items SET local_content = NULL WHERE id = ? AND local_content = ?`, o.item,
		o.staged); err != nil {
		return err
	}
	s.log.Info("An edit made from an older version is kept as a conflict copy", "name", copyName)
	return s.resolveItem(tx, p, o.item)
}

// refused handles an operation the gateway didn't take. Errors worth trying again are returned.
func (s *Service) refused(o *outboxRow, err error) error {
	var e *remoteclient.Error
	if !errors.As(err, &e) || e.Transient() || e.Kind == remoteclient.KindUnauthenticated ||
		e.Kind == remoteclient.KindUntrusted || e.Kind == remoteclient.KindProtocol || e.Kind == remoteclient.KindCanceled {
		return err
	}
	var changes Changes
	defer func() { s.changed(changes) }()
	return s.store.tx(context.Background(), func(tx *sql.Tx) error {
		p := newProjector(tx)
		defer func() { changes = p.changes }()
		switch {
		case e.Kind == remoteclient.KindNameConflict && (o.kind == remoteapi.OpCreate || o.kind == remoteapi.OpMove):
			// The name was taken on the gateway meanwhile: the item here takes a conflict copy's name instead
			return s.renameToConflict(tx, p, o)
		case e.Kind == remoteclient.KindRevisionMismatch && o.kind == remoteapi.OpReplace:
			return s.conflictCopy(tx, p, o)
		case e.Kind == remoteclient.KindRevisionMismatch && o.kind == remoteapi.OpDelete && e.Item != nil &&
			e.Item.Kind == remoteapi.KindDirectory && o.resends < maxResends:
			// Something changed below the directory since it was removed here, which may be only what this client
			// sent meanwhile (a save in it that was on its way): once the change feed has reached what the gateway
			// reported, it is checked and sent again, as a new intent (prepare)
			_, err := tx.Exec(`UPDATE outbox SET awaited_generation = ?, subtree_generation = NULL,
				resends = resends + 1, op_id = ?, payload = NULL, state = 'uploaded', not_before = ?, error = ?,
				updated_at = ? WHERE id = ?`, e.Item.SubtreeGeneration, uuid.NewString(), nowMillis()+1000, e.Error(),
				nowUnix(), o.id)
			return err
		case e.Kind == remoteclient.KindRevisionMismatch, e.Code == remoteapi.CodeNotEmpty,
			e.Code == remoteapi.CodeIgnoredPath && o.kind == remoteapi.OpMove:
			// Moving or removing an item that changed on the gateway is refused, which leaves it intact: so is the
			// change here. So is a move to a name the gateway ignores, which would take it out of the folder.
			s.log.Info("A move or removal was undone, as the item changed on the gateway", "operation", o.opID)
			if _, err := tx.Exec(`UPDATE outbox SET state = 'canceled', error = ?, updated_at = ? WHERE id = ?`,
				e.Error(), nowUnix(), o.id); err != nil {
				return err
			}
			return s.undo(tx, p, o.item)
		case e.Kind == remoteclient.KindNotFound && o.kind == remoteapi.OpDelete:
			if _, err := tx.Exec(`UPDATE outbox SET state = 'committed', updated_at = ? WHERE id = ?`, nowUnix(),
				o.id); err != nil {
				return err
			}
			return s.resolveItem(tx, p, o.item)
		case e.Kind == remoteclient.KindNotFound && o.kind == remoteapi.OpReplace:
			return s.convertToCreate(tx, o)
		case e.Code == remoteapi.CodeIgnoredPath && o.kind == remoteapi.OpCreate:
			// The gateway's ignore patterns exclude it: it stays here only, with everything made in it (a build
			// directory, say), whose creations can't be sent either
			if _, err := tx.Exec(`UPDATE outbox SET state = 'canceled', error = ?, updated_at = ? WHERE id = ?`,
				e.Error(), nowUnix(), o.id); err != nil {
				return err
			}
			if _, err := tx.Exec(`WITH RECURSIVE sub(id) AS (SELECT ?1 UNION ALL
					SELECT i.id FROM items AS i JOIN sub ON i.parent = sub.id WHERE i.deleted = 0)
				UPDATE outbox SET state = 'canceled', error = 'in a directory the gateway ignores', updated_at = ?2
				WHERE item IN (SELECT id FROM sub) AND item != ?1 AND state IN ('queued', 'uploaded', 'blocked')`,
				o.item, nowUnix()); err != nil {
				return err
			}
			_, err := tx.Exec(`WITH RECURSIVE sub(id) AS (SELECT ?1 UNION ALL
					SELECT i.id FROM items AS i JOIN sub ON i.parent = sub.id WHERE i.deleted = 0)
				UPDATE items SET local = local | ?2 WHERE id IN (SELECT id FROM sub)`, o.item, localOnly|localCreated)
			return err
		case e.Kind == remoteclient.KindNotFound && (o.kind == remoteapi.OpCreate || o.kind == remoteapi.OpMove) &&
			o.resends < maxResends:
			// Its directory, or the move's destination, went from the gateway: sent again once the change feed has
			// shown that, when a directory made again here for it goes first (neededFirst)
			return resend(tx, o, e)
		case e.Code == remoteapi.CodeUploadMismatch && o.resends < maxResends:
			// The upload is used, incomplete, or not what was staged: upload again
			return resend(tx, o, e)
		}
		_, err := tx.Exec(`UPDATE outbox SET state = 'failed', error = ?, updated_at = ? WHERE id = ?`, e.Error(),
			nowUnix(), o.id)
		if err == nil {
			s.log.Warn("The gateway refused a change; export or discard it", "operation", o.opID, "error", e)
			s.reviewNeeded()
		}
		return err
	})
}

// resend sends an operation again as a new intent (the gateway keeps its answer to this one), after a while longer each
// time, from a new upload if it sends content (the gateway removes an upload once an operation used it)
func resend(tx *sql.Tx, o *outboxRow, reason error) error {
	state := stateUploaded
	if o.needsUpload() {
		state = stateQueued
	}
	delay := time.Duration(1<<(o.resends+1)) * time.Second
	_, err := tx.Exec(`UPDATE outbox SET upload_id = '', payload = NULL, op_id = ?, state = ?, resends = resends + 1,
		not_before = ?, error = ?, updated_at = ? WHERE id = ?`, uuid.NewString(), state,
		nowMillis()+delay.Milliseconds(), reason.Error(), nowUnix(), o.id)
	return err
}

// uploadLost handles a submission of an operation with an upload that the gateway answered with 404 without storing
// it (no operation in the answer): if the upload is gone (it expired while the daemon didn't run, say), and not the
// item, the operation goes again from a new upload of its staged copy, as the Swift client does. Reports whether it
// handled the answer; an error means whether the upload is gone can't be told now, and the submission is tried
// again later (the gateway stored nothing of it).
func (s *Service) uploadLost(ctx context.Context, client *remoteclient.Client, o *outboxRow, err error) (bool,
	error) {
	var e *remoteclient.Error
	if !errors.As(err, &e) || e.Kind != remoteclient.KindNotFound || e.Operation != nil || !o.needsUpload() ||
		o.uploadID == "" {
		return false, nil
	}
	if _, err := client.UploadOffset(ctx, o.uploadID, ""); remoteclient.KindOf(err) != remoteclient.KindNotFound {
		// Still there, so something else isn't; or it can't be told now
		return err != nil, err
	}
	if o.resends >= maxResends {
		return true, s.setState(o.id, stateFailed, "the gateway lost its upload again and again")
	}
	s.log.Info("The gateway no longer has a change's upload: uploading it again", "operation", o.opID)
	return true, s.guarded(ctx, `UPDATE outbox SET upload_id = '', payload = NULL, op_id = ?, state = 'queued',
		resends = resends + 1, updated_at = ? WHERE id = ? AND state = 'submitted' AND op_id = ?`, uuid.NewString(),
		nowUnix(), o.id, o.opID)
}

// renameToConflict gives an item whose name was taken on the gateway a conflict copy's name, and sends its creation
// or move again under that name (a new intent: a new operation ID)
func (s *Service) renameToConflict(tx *sql.Tx, p *projector, o *outboxRow) error {
	var parent sql.NullInt64
	var name string
	var node int64
	if err := tx.QueryRow(`SELECT parent, name, node FROM items WHERE id = ?`, o.item).Scan(&parent, &name,
		&node); err != nil {
		return err
	}
	clientID, err := metaValue(tx, metaClientID)
	if err != nil {
		return err
	}
	newName := conflictName(o.targetName, clientID, time.Now())
	if _, err := tx.Exec(`UPDATE items SET name = ? WHERE id = ?`, newName, o.item); err != nil {
		return err
	}
	parentNode, err := p.nodeOf(parent)
	if err != nil {
		return err
	}
	p.changes.entry(parentNode, name, uint64(node))
	p.changes.entry(parentNode, newName, 0)
	_, err = tx.Exec(`UPDATE outbox SET target_name = ?, op_id = ?, payload = NULL, state = 'uploaded', updated_at = ?
		WHERE id = ?`, newName, uuid.NewString(), nowUnix(), o.id)
	return err
}

// recordResult stores what the gateway answered
func (s *Service) recordResult(o *outboxRow, f folderGateway, result remoteapi.Operation) error {
	switch result.State {
	case remoteapi.OperationPending:
		return s.store.tx(context.Background(), func(tx *sql.Tx) error {
			_, err := tx.Exec(`UPDATE outbox SET state = 'pending', server_op = ?, not_before = ?, updated_at = ?
				WHERE id = ? AND state IN ('submitted', 'pending')`, result.Operation, nowMillis()+1000, nowUnix(), o.id)
			return err
		})
	case remoteapi.OperationFailed:
		e := &remoteclient.Error{Kind: remoteclient.KindOther, Message: "the operation failed"}
		if result.Error != nil {
			e.Code, e.Message, e.Item = result.Error.Code, result.Error.Message, result.Error.Item
			switch result.Error.Code {
			case remoteapi.CodeRevisionMismatch:
				e.Kind = remoteclient.KindRevisionMismatch
			case remoteapi.CodeNameConflict:
				e.Kind = remoteclient.KindNameConflict
			case remoteapi.CodeNotFound:
				e.Kind = remoteclient.KindNotFound
			}
			e.Status = http.StatusConflict
		}
		return s.refused(o, e)
	case remoteapi.OperationCommitted, remoteapi.OperationConflict:
	default:
		return fmt.Errorf("unexpected operation state %q", result.State)
	}

	// The content sent becomes the cache's copy of what the gateway committed
	contentID := result.ContentID
	if contentID == "" && result.Item != nil && result.State == remoteapi.OperationCommitted {
		contentID = result.Item.ContentID
	}
	if result.State == remoteapi.OperationConflict && result.ConflictItem != nil {
		contentID = result.ConflictItem.ContentID
	}
	if o.staged.Valid && contentID != "" {
		if err := s.cache.adopt(filepath.Join(s.cfg.StateDir, o.staged.String), contentID, o.size); err != nil {
			s.log.Warn("Could not keep sent content in the cache", "error", err)
		}
	}
	var changes Changes
	err := s.store.tx(context.Background(), func(tx *sql.Tx) error {
		p := newProjector(tx)
		defer func() { changes = p.changes }()
		var revision, resultContent string
		if result.Item != nil {
			item := *result.Item
			revision, resultContent = item.Revision, item.ContentID
			switch {
			case o.kind == remoteapi.OpCreate && result.State == remoteapi.OperationCommitted:
				if err := p.mapItem(item, o.folder, o.item); err != nil {
					return err
				}
			default:
				// Stored unless the change feed stored something newer already
				known := []string{o.baseRev}
				if _, err := p.apply(item, o.folder, 0, known); err != nil {
					return err
				}
			}
		}
		if result.State == remoteapi.OperationConflict && result.ConflictItem != nil {
			// The gateway kept both: the item is the gateway's, and the copy with this edit comes in the change feed
			if _, err := p.apply(*result.ConflictItem, o.folder, 0, []string{}); err != nil {
				return err
			}
		}
		if o.staged.Valid && contentID != "" && result.State == remoteapi.OperationCommitted {
			if _, err := tx.Exec(`UPDATE items SET content_id = ? WHERE id = ?`, contentID, o.item); err != nil {
				return err
			}
		}
		if o.staged.Valid {
			if _, err := tx.Exec(`UPDATE items SET local_content = NULL WHERE id = ? AND local_content = ?`, o.item,
				o.staged); err != nil {
				return err
			}
		}
		state := stateCommitted
		if result.State == remoteapi.OperationConflict {
			state = stateConflict
		}
		if _, err := tx.Exec(`UPDATE outbox SET state = ?, result_revision = ?, result_content_id = ?, server_op = ?,
			error = '', updated_at = ? WHERE id = ?`, state, revision, resultContent, result.Operation, nowUnix(),
			o.id); err != nil {
			return err
		}
		return s.resolveItem(tx, p, o.item)
	})
	if err != nil {
		return err
	}
	s.changed(changes)
	s.collectStagedSoon()
	return nil
}

// resolveItem shows the gateway's state of an item once nothing of it is pending here any more: a removed item's
// tombstone goes, and the row of another follows the gateway's metadata again
func (s *Service) resolveItem(tx *sql.Tx, p *projector, item int64) error {
	overlaid, err := p.hasOverlay(item)
	if err != nil || overlaid {
		return err
	}
	var deleted bool
	var gatewayID sql.NullString
	err = tx.QueryRow(`SELECT i.deleted, r.gateway_id FROM items AS i LEFT JOIN remote_items AS r ON r.item = i.id
		WHERE i.id = ?`, item).Scan(&deleted, &gatewayID)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	if deleted {
		// Its removal is committed: the tombstone and those of everything below it that went with it
		if _, err := tx.Exec(`WITH RECURSIVE sub(id) AS (SELECT ?1 UNION ALL
				SELECT i.id FROM items AS i JOIN sub ON i.parent = sub.id WHERE i.deleted = 1)
			UPDATE remote_items SET item = NULL WHERE item IN (SELECT id FROM sub)`, item); err != nil {
			return err
		}
		_, err := tx.Exec(`WITH RECURSIVE sub(id) AS (SELECT ?1 UNION ALL
				SELECT i.id FROM items AS i JOIN sub ON i.parent = sub.id WHERE i.deleted = 1)
			DELETE FROM items WHERE id IN (SELECT id FROM sub) AND deleted = 1`, item)
		return err
	}
	if gatewayID.Valid {
		_, err := p.place(gatewayID.String, true)
		return err
	}
	return nil
}

// undo takes back a change the gateway refused, which left the item as it is there: a removed item (and what went
// with it) comes back, a moved one goes back to where it is on the gateway
func (s *Service) undo(tx *sql.Tx, p *projector, item int64) error {
	rows, err := tx.Query(`WITH RECURSIVE sub(id) AS (SELECT ?1 UNION ALL
			SELECT i.id FROM items AS i JOIN sub ON i.parent = sub.id WHERE i.deleted = 1 AND i.local & ?2 != 0)
		SELECT r.gateway_id FROM sub JOIN remote_items AS r ON r.item = sub.id`, item, localFolded)
	if err != nil {
		return err
	}
	var gatewayIDs []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return err
		}
		gatewayIDs = append(gatewayIDs, id)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	if _, err := tx.Exec(`WITH RECURSIVE sub(id) AS (SELECT ?1 UNION ALL
			SELECT i.id FROM items AS i JOIN sub ON i.parent = sub.id WHERE i.deleted = 1 AND i.local & ?2 != 0)
		UPDATE items SET local = local & ~?3 WHERE id IN (SELECT id FROM sub)`, item, localFolded,
		localMoved|localDeleted|localFolded); err != nil {
		return err
	}
	for _, id := range gatewayIDs {
		if _, err := p.place(id, true); err != nil {
			return err
		}
	}
	return p.finish()
}

// adopt moves content that was sent to the gateway into the cache under the content ID the gateway committed it as
func (c *cache) adopt(path, contentID string, size int64) error {
	if cached, err := c.cached(contentID); err != nil || cached {
		return err
	}
	tmp := c.contentPath(contentID) + ".adopt"
	// One left by a crash would stop the clone
	os.Remove(tmp)
	f, _, err := cloneFile(path, tmp)
	if err != nil {
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		os.Remove(tmp)
		return err
	}
	f.Close()
	if err := os.Rename(tmp, c.contentPath(contentID)); err != nil {
		os.Remove(tmp)
		return err
	}
	return c.s.store.tx(context.Background(), func(tx *sql.Tx) error {
		_, err := tx.Exec(`INSERT INTO content (content_id, size, last_used) VALUES (?, ?, ?)
			ON CONFLICT (content_id) DO NOTHING`, contentID, size, nowUnix())
		return err
	})
}

// collectStaged removes staged content nothing refers to any more
func (s *Service) collectStaged() error {
	s.settleMutex.Lock()
	defer s.settleMutex.Unlock()
	rows, err := s.store.db.Query(`SELECT staged FROM outbox WHERE staged IS NOT NULL AND state IN ` +
		unresolvedStates + ` UNION SELECT local_content FROM items WHERE local_content IS NOT NULL`)
	if err != nil {
		return err
	}
	used := map[string]bool{}
	for rows.Next() {
		var rel string
		if err := rows.Scan(&rel); err != nil {
			rows.Close()
			return err
		}
		used[filepath.Base(rel)] = true
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	entries, err := os.ReadDir(s.stagedDir())
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return err
	}
	for _, entry := range entries {
		if !used[entry.Name()] {
			_ = os.Remove(filepath.Join(s.stagedDir(), entry.Name()))
		}
	}
	return nil
}

func (s *Service) collectStagedSoon() {
	go func() {
		if err := s.collectStaged(); err != nil {
			s.log.Debug("Could not collect staged content", "error", err)
		}
	}()
}

// exportContent copies an operation's or a working copy's content to w
func exportContent(path string, w io.Writer) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = io.Copy(w, f)
	return err
}
