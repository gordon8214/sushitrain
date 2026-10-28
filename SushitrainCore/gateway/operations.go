// Copyright (C) 2026 Gordon
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this file,
// You can obtain one at https://mozilla.org/MPL/2.0/.

package gateway

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"regexp"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
	"golang.org/x/text/unicode/norm"
)

// Operations change live files. Each is recorded (request, payload digest, journal) before anything on disk changes,
// applied at most once per client identity and operation ID, and reported committed only once its result is on disk,
// indexed by Syncthing, and in the change log.

const (
	// How long creating an operation waits for its result before answering 202
	operationSyncBudget = 10 * time.Second
	// Operation receipts are kept this long after they finish, for deduplicating replays
	receiptRetention = 30 * 24 * time.Hour
	retryInterval    = time.Minute
	maxNameBytes     = 255
	maxFolderIDBytes = 256
)

var revisionPattern = regexp.MustCompile(`^r1\.[A-Za-z0-9_-]{43}$`)

// Journal states (Docs/remote-api/invariants.md). Publishing is recorded before anything changes on disk, with the
// method, so that a run that stopped can tell afterwards whether the change happened.
const (
	journalIntent     = "intent"
	journalStaged     = "staged"
	journalPublishing = "publishing"
	journalPublished  = "published"
	journalIndexed    = "indexed"
	journalReceipted  = "receipted"
)

// How a change is put on disk
const (
	// Renamed into place (a new file, or a conflict copy)
	methodRename = "rename"
	// Swapped with the replaced file, which stays until the edit is indexed
	methodSwap = "swap"
	// The replaced file goes to the versioner or the Trash first
	methodArchive = "archive"
	methodMkdir   = "mkdir"
	methodMove    = "move"
	methodDelete  = "delete"
)

type journalData struct {
	// Folder-relative paths
	Temp      string `json:"temp,omitempty"`
	Target    string `json:"target,omitempty"`
	Displaced string `json:"displaced,omitempty"`
	// The block list hash the published file must have
	BlocksHash []byte `json:"blocksHash,omitempty"`
	// The staged file's identity, to recognize it after publication
	Device uint64 `json:"device,omitempty"`
	Inode  uint64 `json:"inode,omitempty"`
	// Set when the edit was kept as a conflict copy
	Conflict bool `json:"conflict,omitempty"`
	// The target before a conflict copy name was chosen, which further names are based on
	Original string `json:"original,omitempty"`
	// How the change is put on disk (method… constants). For moves, Device and Inode identify the moved entry.
	Method string `json:"method,omitempty"`
	// Failed attempts with server errors, which are retried a limited number of times
	Attempts int `json:"attempts,omitempty"`
}

type operationRecord struct {
	id         string
	clientID   string
	clientOp   string
	kind       string
	folder     string
	req        OperationRequest
	digest     []byte
	state      string
	journal    string
	data       journalData
	result     Operation
	httpStatus int
	generation int64
}

func (s *Service) loadOperation(q queryer, id string) (operationRecord, bool, error) {
	var op operationRecord
	var payload, data, result string
	err := q.QueryRow(`SELECT op_id, client_id, client_op, kind, folder_id, payload, payload_digest, state, journal,
		journal_data, result, http_status, generation FROM operations WHERE op_id = ?`, id).Scan(&op.id, &op.clientID,
		&op.clientOp, &op.kind, &op.folder, &payload, &op.digest, &op.state, &op.journal, &data, &result, &op.httpStatus,
		&op.generation)
	if errors.Is(err, sql.ErrNoRows) {
		return operationRecord{}, false, nil
	}
	if err != nil {
		return operationRecord{}, false, err
	}
	if err := json.Unmarshal([]byte(payload), &op.req); err != nil {
		return operationRecord{}, false, err
	}
	if data != "" {
		_ = json.Unmarshal([]byte(data), &op.data)
	}
	if result != "" {
		_ = json.Unmarshal([]byte(result), &op.result)
	}
	return op, true, nil
}

// saveJournal records an operation's progress durably before the next step
func (s *Service) saveJournal(op *operationRecord, state string) error {
	data, err := json.Marshal(op.data)
	if err != nil {
		return err
	}
	op.journal = state
	err = s.store.tx(context.Background(), func(tx *sql.Tx) error {
		_, err := tx.Exec(`UPDATE operations SET journal = ?, journal_data = ?, updated_at = ? WHERE op_id = ?`, state,
			string(data), s.store.now().Unix(), op.id)
		return err
	})
	if err == nil && s.journalHook != nil {
		err = s.journalHook(op.id, state)
	}
	return err
}

// finish stores an operation's final result
func (s *Service) finish(op *operationRecord, result Operation, httpStatus int) error {
	encoded, err := json.Marshal(result)
	if err != nil {
		return err
	}
	data, err := json.Marshal(op.data)
	if err != nil {
		return err
	}
	op.state, op.result, op.httpStatus, op.journal = result.State, result, httpStatus, journalReceipted
	err = s.store.tx(context.Background(), func(tx *sql.Tx) error {
		if _, err := tx.Exec(`UPDATE operations SET state = ?, journal = ?, journal_data = ?, result = ?, http_status = ?,
			updated_at = ? WHERE op_id = ?`, result.State, journalReceipted, string(data), string(encoded), httpStatus,
			s.store.now().Unix(), op.id); err != nil {
			return err
		}
		// The upload has served its purpose either way
		if op.req.Upload != "" {
			_, err := tx.Exec(`DELETE FROM uploads WHERE upload_id = ? AND pinned_by = ?`, op.req.Upload, op.id)
			return err
		}
		return nil
	})
	if err == nil && op.req.Upload != "" {
		s.uploads.removeFile(op.req.Upload)
	}
	itemID := op.req.ItemID
	if result.Item != nil {
		itemID = result.Item.ID
	}
	s.audit(op.clientID, op.kind, op.id, itemID, result.State)
	return err
}

// normalizedRequest validates a request and returns its canonical form, whose digest identifies the payload
func normalizedRequest(req OperationRequest) (OperationRequest, []byte, error) {
	invalid := func(msg string) error { return newAPIError(http.StatusBadRequest, codeInvalidRequest, "%s", msg) }
	id, err := uuid.Parse(req.ID)
	if err != nil {
		return req, nil, invalid("id must be a UUID")
	}
	req.ID = id.String()
	if req.FolderID == "" {
		return req, nil, invalid("folderID is required")
	}
	if req.Name != "" {
		req.Name = norm.NFC.String(req.Name)
	}
	switch req.Kind {
	case opCreate:
		if req.ParentID == "" || req.Name == "" {
			return req, nil, invalid("create needs parentID and name")
		}
		switch req.ItemKind {
		case kindFile:
			if req.Upload == "" || req.UploadDigest == "" {
				return req, nil, invalid("creating a file needs upload and uploadDigest")
			}
		case kindDirectory:
			if req.Upload != "" {
				return req, nil, invalid("a directory has no upload")
			}
		default:
			return req, nil, invalid("itemKind must be file or directory")
		}
	case opReplace:
		if req.ItemID == "" || req.BaseRevision == "" || req.Upload == "" || req.UploadDigest == "" {
			return req, nil, invalid("replace needs itemID, baseRevision, upload and uploadDigest")
		}
		if req.OnConflict == "" {
			req.OnConflict = onConflictKeepBoth
		}
		if req.OnConflict != onConflictKeepBoth && req.OnConflict != onConflictReject {
			return req, nil, invalid("onConflict must be keepBoth or reject")
		}
	case opMove:
		if req.ItemID == "" || req.BaseRevision == "" || req.ParentID == "" || req.Name == "" {
			return req, nil, invalid("move needs itemID, baseRevision, parentID and name")
		}
	case opDelete:
		if req.ItemID == "" || req.BaseRevision == "" {
			return req, nil, invalid("delete needs itemID and baseRevision")
		}
	default:
		return req, nil, invalid("unknown kind")
	}
	if req.Name != "" {
		if err := validName(req.Name); err != nil {
			return req, nil, err
		}
	}
	if req.UploadDigest != "" {
		if _, ok := parseDigest(req.UploadDigest); !ok {
			return req, nil, invalid("uploadDigest must be a base64url SHA-256")
		}
	}
	// Receipts are kept for a long time: only well-formed references are stored
	for _, ref := range []struct{ value, prefix string }{{req.ItemID, prefixItem}, {req.ParentID, prefixItem},
		{req.Upload, prefixUpload}} {
		if ref.value != "" && !hasIDFormat(ref.value, ref.prefix) {
			return req, nil, invalid("malformed item or upload ID")
		}
	}
	if req.BaseRevision != "" && !revisionPattern.MatchString(req.BaseRevision) {
		return req, nil, invalid("malformed baseRevision")
	}
	if len(req.FolderID) > maxFolderIDBytes || len(req.DestinationFolderID) > maxFolderIDBytes {
		return req, nil, invalid("folder ID too long")
	}
	canonical, err := json.Marshal(req)
	if err != nil {
		return req, nil, err
	}
	sum := sha256.Sum256(canonical)
	return req, sum[:], nil
}

func validName(name string) error {
	if name == "." || name == ".." || strings.ContainsAny(name, "/\x00") || !utf8.ValidString(name) ||
		len(name) > maxNameBytes || strings.TrimSpace(name) == "" {
		return newAPIError(http.StatusBadRequest, codeInvalidRequest, "invalid name")
	}
	return nil
}

func (s *Service) handleCreateOperation(w http.ResponseWriter, r *http.Request, p principal) error {
	if err := requireCredential(p); err != nil {
		return err
	}
	var req OperationRequest
	if err := decodeJSON(w, r, &req); err != nil {
		return err
	}
	req, digest, err := normalizedRequest(req)
	if err != nil {
		return err
	}

	// A replay returns what the first request got
	var existing string
	err = s.store.db.QueryRow(`SELECT op_id FROM operations WHERE client_id = ? AND client_op = ?`, p.clientID,
		req.ID).Scan(&existing)
	if err == nil {
		return s.replay(w, p, existing, digest)
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return err
	}

	access, err := s.access(p.clientID, req.FolderID)
	if err != nil {
		return err
	}
	if err := access.requireWrite(); err != nil {
		return err
	}
	if req.DestinationFolderID != "" && req.DestinationFolderID != req.FolderID {
		dest, err := s.access(p.clientID, req.DestinationFolderID)
		if err != nil {
			return err
		}
		if err := dest.requireWrite(); err != nil {
			return err
		}
	}

	generation, err := s.store.generation()
	if err != nil {
		return err
	}
	payload, err := json.Marshal(req)
	if err != nil {
		return err
	}
	op := operationRecord{
		id:         newID(prefixOperation),
		clientID:   p.clientID,
		clientOp:   req.ID,
		kind:       req.Kind,
		folder:     req.FolderID,
		req:        req,
		digest:     digest,
		state:      stateResultPending,
		journal:    journalIntent,
		generation: generation,
	}
	err = s.store.tx(r.Context(), func(tx *sql.Tx) error {
		// The same operation sent twice at the same moment (by the app and the extension) gets the replay answer
		existing = ""
		err := tx.QueryRow(`SELECT op_id FROM operations WHERE client_id = ? AND client_op = ?`, p.clientID,
			req.ID).Scan(&existing)
		if err == nil || !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		if req.Upload != "" {
			// The upload must be this client's, for this folder, complete, and not used by another operation
			u, ok, err := s.uploadByID(tx, req.Upload)
			if err != nil {
				return err
			}
			want, _ := parseDigest(req.UploadDigest)
			if !ok || u.clientID != p.clientID || u.folder != req.FolderID {
				return newAPIError(http.StatusNotFound, codeNotFound, "unknown upload")
			}
			if !u.complete || u.pinnedBy.Valid || !bytes.Equal(u.digest, want) {
				return newAPIError(http.StatusConflict, codeUploadMismatch,
					"the upload is incomplete, in use, or does not match uploadDigest")
			}
			if _, err := tx.Exec(`UPDATE uploads SET pinned_by = ? WHERE upload_id = ?`, op.id, req.Upload); err != nil {
				return err
			}
		}
		now := s.store.now().Unix()
		_, err = tx.Exec(`INSERT INTO operations (op_id, client_id, client_op, kind, folder_id, payload, payload_digest,
			state, journal, generation, created_at, updated_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`, op.id,
			op.clientID, op.clientOp, op.kind, op.folder, string(payload), digest, op.state, op.journal, generation, now,
			now)
		return err
	})
	if err != nil {
		return err
	}
	if existing != "" {
		return s.replay(w, p, existing, digest)
	}

	done := s.ops.start(op.id)
	select {
	case <-done:
	case <-time.After(operationSyncBudget):
	case <-r.Context().Done():
	}
	return s.respondOperation(w, op.id, http.StatusCreated)
}

// replay answers a repeated operation ID: the same result if the payload is the same
func (s *Service) replay(w http.ResponseWriter, p principal, opID string, digest []byte) error {
	op, ok, err := s.loadOperation(s.store.db, opID)
	if err != nil {
		return err
	}
	if !ok {
		return errNotFound
	}
	if err := s.mayReadReceipt(p, op); err != nil {
		return err
	}
	if !bytes.Equal(op.digest, digest) {
		return newAPIError(http.StatusUnprocessableEntity, codeOperationMismatch,
			"the operation ID was used with a different payload")
	}
	if op.state == stateResultPending {
		s.ops.start(op.id)
	}
	return s.respondOperation(w, opID, http.StatusOK)
}

// respondOperation writes an operation's current state: final results with finalStatus, failures found while
// handling the request with their own status, and unfinished work with 202
func (s *Service) respondOperation(w http.ResponseWriter, opID string, finalStatus int) error {
	op, ok, err := s.loadOperation(s.store.db, opID)
	if err != nil {
		return err
	}
	if !ok {
		return errNotFound
	}
	if op.state == stateResultPending {
		return writeJSON(w, http.StatusAccepted, s.pendingResult(op))
	}
	status := finalStatus
	if op.httpStatus >= 400 {
		status = op.httpStatus
	}
	if status >= 400 && op.result.Error != nil {
		return writeJSON(w, status, struct {
			Error     ErrorDetail `json:"error"`
			Operation Operation   `json:"operation"`
		}{*op.result.Error, op.result})
	}
	return writeJSON(w, status, op.result)
}

func (s *Service) pendingResult(op operationRecord) Operation {
	return Operation{ID: op.clientOp, Operation: op.id, Kind: op.kind, State: stateResultPending,
		AuthorityGeneration: op.generation}
}

func (s *Service) handleOperation(w http.ResponseWriter, r *http.Request, p principal) error {
	if err := requireCredential(p); err != nil {
		return err
	}
	id := r.PathValue("operationID")
	if !hasIDFormat(id, prefixOperation) {
		return errNotFound
	}
	op, ok, err := s.loadOperation(s.store.db, id)
	if err != nil {
		return err
	}
	if !ok || op.clientID != p.clientID {
		return errNotFound
	}
	if err := s.mayReadReceipt(p, op); err != nil {
		return err
	}
	if op.state == stateResultPending {
		return writeJSON(w, http.StatusOK, s.pendingResult(op))
	}
	return writeJSON(w, http.StatusOK, op.result)
}

// mayReadReceipt checks that the client may still see the folder of an operation, whose result describes its items.
// A folder that is only unavailable for now (paused) doesn't hide the operation.
func (s *Service) mayReadReceipt(p principal, op operationRecord) error {
	if _, err := s.access(p.clientID, op.folder); err != nil && !errors.Is(err, errNotFound) {
		return err
	}
	return nil
}

// operationRunner runs each operation in at most one goroutine at a time
type operationRunner struct {
	s       *Service
	mutex   sync.Mutex
	running map[string]chan struct{}
	wg      sync.WaitGroup
}

func newOperationRunner(s *Service) *operationRunner {
	return &operationRunner{s: s, running: make(map[string]chan struct{})}
}

// start runs an operation unless it already runs, and returns a channel closed when that run ends
func (o *operationRunner) start(id string) <-chan struct{} {
	o.mutex.Lock()
	defer o.mutex.Unlock()
	if done, ok := o.running[id]; ok {
		return done
	}
	if o.s.ctx.Err() != nil {
		// Stopping: the operation continues at the next start
		done := make(chan struct{})
		close(done)
		return done
	}
	done := make(chan struct{})
	o.running[id] = done
	o.wg.Add(1)
	go func() {
		defer o.wg.Done()
		defer func() {
			o.mutex.Lock()
			delete(o.running, id)
			o.mutex.Unlock()
			close(done)
		}()
		o.s.runOperation(o.s.ctx, id)
	}()
	return done
}

func (o *operationRunner) wait() {
	o.wg.Wait()
}

// recover resumes operations interrupted by a stop or crash, from the journal state they reached
func (o *operationRunner) recover(ctx context.Context) error {
	ids, err := o.s.pendingOperationIDs("")
	if err != nil {
		return err
	}
	for _, id := range ids {
		o.start(id)
	}
	go func() {
		ticker := time.NewTicker(retryInterval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				o.s.retryPendingOperations("")
				o.s.pruneReceipts(ctx)
			}
		}
	}()
	return nil
}

func (s *Service) pendingOperationIDs(folder string) ([]string, error) {
	query := `SELECT op_id FROM operations WHERE state = 'pending' ORDER BY created_at`
	args := []any{}
	if folder != "" {
		query = `SELECT op_id FROM operations WHERE state = 'pending' AND folder_id = ? ORDER BY created_at`
		args = append(args, folder)
	}
	rows, err := s.store.db.Query(query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

// retryPendingOperations restarts waiting operations, such as replacements waiting for this device to have their
// base version
func (s *Service) retryPendingOperations(folder string) {
	ids, err := s.pendingOperationIDs(folder)
	if err != nil {
		return
	}
	for _, id := range ids {
		s.ops.start(id)
	}
}

func (s *Service) pruneReceipts(ctx context.Context) {
	cutoff := s.store.now().Add(-receiptRetention).Unix()
	err := s.store.tx(ctx, func(tx *sql.Tx) error {
		// Kept while the client is paired, and for the retention period after it was revoked
		_, err := tx.Exec(`DELETE FROM operations WHERE state != 'pending' AND updated_at < ? AND client_id IN
			(SELECT client_id FROM clients WHERE revoked_at IS NOT NULL AND revoked_at < ?)`, cutoff, cutoff)
		return err
	})
	if err != nil {
		s.log.Warn("Could not prune operation receipts", "error", err)
	}
}
