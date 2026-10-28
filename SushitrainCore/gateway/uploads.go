// Copyright (C) 2026 Gordon
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this file,
// You can obtain one at https://mozilla.org/MPL/2.0/.

package gateway

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

// Staged uploads (tus 1.0 with the creation, checksum, expiration and termination extensions). An upload only stages
// bytes; an operation that references it publishes them.

const (
	tusVersion       = "1.0.0"
	uploadExpiry     = 7 * 24 * time.Hour
	statusChecksum   = 460
	uploadCopyBuffer = 1 << 20
)

type uploadManager struct {
	s     *Service
	dir   string
	locks sync.Map
}

func newUploadManager(s *Service, dir string) (*uploadManager, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	return &uploadManager{s: s, dir: dir}, nil
}

func (m *uploadManager) path(id string) string {
	return filepath.Join(m.dir, id)
}

func (m *uploadManager) removeFile(id string) {
	_ = os.Remove(m.path(id))
	m.locks.Delete(id)
}

func (m *uploadManager) lock(id string) *sync.Mutex {
	lock, _ := m.locks.LoadOrStore(id, &sync.Mutex{})
	return lock.(*sync.Mutex)
}

// expire removes uploads without activity for the expiry period, unless an operation references them
func (m *uploadManager) expire(ctx context.Context) {
	var expired []string
	err := m.s.store.tx(ctx, func(tx *sql.Tx) error {
		cutoff := m.s.store.now().Add(-uploadExpiry).Unix()
		rows, err := tx.Query(`SELECT upload_id FROM uploads WHERE touched_at < ? AND pinned_by IS NULL`, cutoff)
		if err != nil {
			return err
		}
		for rows.Next() {
			var id string
			if err := rows.Scan(&id); err != nil {
				rows.Close()
				return err
			}
			expired = append(expired, id)
		}
		rows.Close()
		_, err = tx.Exec(`DELETE FROM uploads WHERE touched_at < ? AND pinned_by IS NULL`, cutoff)
		return err
	})
	if err != nil {
		m.s.log.Warn("Could not expire uploads", "error", err)
		return
	}
	for _, id := range expired {
		m.removeFile(id)
	}
	// Files without a row are left from an interrupted creation or removal
	entries, err := os.ReadDir(m.dir)
	if err != nil {
		return
	}
	for _, entry := range entries {
		var one int
		if m.s.store.db.QueryRow(`SELECT 1 FROM uploads WHERE upload_id = ?`, entry.Name()).Scan(&one) != nil {
			if info, err := entry.Info(); err == nil && time.Since(info.ModTime()) > time.Hour {
				_ = os.Remove(filepath.Join(m.dir, entry.Name()))
			}
		}
	}
}

type uploadRow struct {
	id, clientID, folder string
	length, received     int64
	digest               []byte
	complete             bool
	pinnedBy             sql.NullString
	touched              int64
}

func (s *Service) uploadByID(q queryer, id string) (uploadRow, bool, error) {
	var u uploadRow
	err := q.QueryRow(`SELECT upload_id, client_id, folder_id, length, received, digest, complete, pinned_by, touched_at
		FROM uploads WHERE upload_id = ?`, id).Scan(&u.id, &u.clientID, &u.folder, &u.length, &u.received, &u.digest,
		&u.complete, &u.pinnedBy, &u.touched)
	if errors.Is(err, sql.ErrNoRows) {
		return uploadRow{}, false, nil
	}
	return u, err == nil, err
}

func setTusHeaders(w http.ResponseWriter) {
	w.Header().Set("Tus-Resumable", tusVersion)
}

func (s *Service) handleUploadOptions(w http.ResponseWriter, r *http.Request) error {
	setTusHeaders(w)
	h := w.Header()
	h.Set("Tus-Version", tusVersion)
	h.Set("Tus-Extension", "creation,checksum,expiration,termination")
	h.Set("Tus-Max-Size", strconv.FormatInt(min(s.cfg.MaxUploadSize, s.cfg.StagingQuota), 10))
	h.Set("Tus-Checksum-Algorithm", "sha256")
	w.WriteHeader(http.StatusNoContent)
	return nil
}

// parseTusMetadata parses Upload-Metadata: comma-separated "key base64-value" pairs
func parseTusMetadata(header string) map[string]string {
	result := make(map[string]string)
	for pair := range strings.SplitSeq(header, ",") {
		key, value, _ := strings.Cut(strings.TrimSpace(pair), " ")
		if key == "" {
			continue
		}
		decoded, err := base64.StdEncoding.DecodeString(strings.TrimSpace(value))
		if err != nil {
			continue
		}
		result[key] = string(decoded)
	}
	return result
}

// parseDigest accepts a SHA-256 digest as raw bytes or as base64 or base64url text
func parseDigest(value string) ([]byte, bool) {
	if len(value) == sha256.Size {
		return []byte(value), true
	}
	for _, enc := range []*base64.Encoding{base64.RawURLEncoding, base64.URLEncoding, base64.StdEncoding,
		base64.RawStdEncoding} {
		if d, err := enc.DecodeString(value); err == nil && len(d) == sha256.Size {
			return d, true
		}
	}
	return nil, false
}

func (s *Service) handleCreateUpload(w http.ResponseWriter, r *http.Request, p principal) error {
	setTusHeaders(w)
	if err := requireCredential(p); err != nil {
		return err
	}
	if r.Header.Get("Tus-Resumable") != tusVersion {
		w.Header().Set("Tus-Version", tusVersion)
		return newAPIError(http.StatusPreconditionFailed, codeInvalidRequest, "unsupported tus version")
	}
	length, err := strconv.ParseInt(r.Header.Get("Upload-Length"), 10, 64)
	if err != nil || length < 0 {
		return newAPIError(http.StatusBadRequest, codeInvalidRequest, "Upload-Length is required")
	}
	if length > s.cfg.MaxUploadSize {
		return newAPIError(http.StatusRequestEntityTooLarge, codeInvalidRequest, "upload too large")
	}
	meta := parseTusMetadata(r.Header.Get("Upload-Metadata"))
	digest, ok := parseDigest(meta["digest"])
	if !ok || meta["folder"] == "" {
		return newAPIError(http.StatusBadRequest, codeInvalidRequest, "Upload-Metadata needs folder and digest")
	}
	access, err := s.access(p.clientID, meta["folder"])
	if err != nil {
		return err
	}
	if err := access.requireWrite(); err != nil {
		return err
	}
	// An empty upload is complete when created: no PATCH follows
	empty := sha256.Sum256(nil)
	if length == 0 && !equalBytes(digest, empty[:]) {
		return newAPIError(http.StatusBadRequest, codeUploadMismatch, "an empty upload must have the digest of no bytes")
	}

	id := newID(prefixUpload)
	now := s.store.now()
	token, err := deriveToken(s.store, prefixUploadTok, id)
	if err != nil {
		return err
	}
	err = s.store.tx(r.Context(), func(tx *sql.Tx) error {
		var staged int64
		if err := tx.QueryRow(`SELECT COALESCE(SUM(length), 0) FROM uploads WHERE client_id = ?`, p.clientID).Scan(
			&staged); err != nil {
			return err
		}
		if staged+length > s.cfg.StagingQuota {
			return newAPIError(http.StatusInsufficientStorage, codeInsufficientStorage, "the staging quota is used up")
		}
		_, err := tx.Exec(`INSERT INTO uploads (upload_id, token_hash, client_id, folder_id, length, digest, complete,
			created_at, touched_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`, id, secretHash(token), p.clientID, meta["folder"],
			length, digest, length == 0, now.Unix(), now.Unix())
		return err
	})
	if err != nil {
		return err
	}
	f, err := os.OpenFile(s.uploads.path(id), os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	f.Close()
	expires := now.Add(uploadExpiry)
	h := w.Header()
	h.Set("Location", "/v1/uploads/"+id)
	h.Set("Upload-Expires", expires.UTC().Format(http.TimeFormat))
	h.Set("Upload-Offset", "0")
	h.Set(headerUploadToken, token)
	s.audit(p.clientID, "upload", "", "", "created")
	return writeJSON(w, http.StatusCreated, Upload{Upload: id, Token: token, Offset: 0, Length: length,
		Expires: expires.UTC().Format(time.RFC3339)})
}

// uploadForRequest returns the upload a request is for, if the client may use it
func (s *Service) uploadForRequest(r *http.Request, p principal) (uploadRow, error) {
	id := r.PathValue("uploadID")
	if p.kind == principalUpload && p.resource != id || p.kind != principalUpload && p.kind != principalCredential {
		return uploadRow{}, errUnauthenticated
	}
	if !hasIDFormat(id, prefixUpload) {
		return uploadRow{}, errNotFound
	}
	u, ok, err := s.uploadByID(s.store.db, id)
	if err != nil {
		return uploadRow{}, err
	}
	if !ok || u.clientID != p.clientID ||
		!u.pinnedBy.Valid && s.store.now().Sub(time.Unix(u.touched, 0)) > uploadExpiry {
		return uploadRow{}, errNotFound
	}
	return u, nil
}

func (s *Service) handleUploadHead(w http.ResponseWriter, r *http.Request, p principal) error {
	setTusHeaders(w)
	u, err := s.uploadForRequest(r, p)
	if err != nil {
		return err
	}
	h := w.Header()
	h.Set("Upload-Offset", strconv.FormatInt(u.received, 10))
	h.Set("Upload-Length", strconv.FormatInt(u.length, 10))
	h.Set("Upload-Expires", time.Unix(u.touched, 0).Add(uploadExpiry).UTC().Format(http.TimeFormat))
	w.WriteHeader(http.StatusOK)
	return nil
}

func (s *Service) handleUploadPatch(w http.ResponseWriter, r *http.Request, p principal) error {
	setTusHeaders(w)
	if !strings.HasPrefix(r.Header.Get("Content-Type"), "application/offset+octet-stream") {
		return newAPIError(http.StatusUnsupportedMediaType, codeInvalidRequest, "expected application/offset+octet-stream")
	}
	offset, err := strconv.ParseInt(r.Header.Get("Upload-Offset"), 10, 64)
	if err != nil || offset < 0 {
		return newAPIError(http.StatusBadRequest, codeInvalidRequest, "Upload-Offset is required")
	}
	var expected []byte
	if checksum := r.Header.Get("Upload-Checksum"); checksum != "" {
		algorithm, value, _ := strings.Cut(checksum, " ")
		sum, err := base64.StdEncoding.DecodeString(strings.TrimSpace(value))
		if algorithm != "sha256" || err != nil || len(sum) != sha256.Size {
			return newAPIError(http.StatusBadRequest, codeInvalidRequest, "unsupported Upload-Checksum")
		}
		expected = sum
	}

	// Requests for one upload are serialized: an app and an extension can both send one after a claim expired. The
	// one that comes second asks for the offset again.
	if _, err := s.uploadForRequest(r, p); err != nil {
		return err
	}
	lock := s.uploads.lock(r.PathValue("uploadID"))
	if !lock.TryLock() {
		return newAPIError(http.StatusConflict, codeInvalidRequest, "another request is writing this upload")
	}
	defer lock.Unlock()

	u, err := s.uploadForRequest(r, p)
	if err != nil {
		return err
	}
	// The client must still be allowed to write the folder the upload is for
	if access, err := s.access(p.clientID, u.folder); err != nil {
		return err
	} else if err := access.requireWrite(); err != nil {
		return err
	}
	if u.pinnedBy.Valid || u.complete {
		return newAPIError(http.StatusConflict, codeInvalidRequest, "the upload is complete")
	}
	if offset != u.received {
		w.Header().Set("Upload-Offset", strconv.FormatInt(u.received, 10))
		return newAPIError(http.StatusConflict, codeInvalidRequest, "Upload-Offset does not match")
	}
	f, err := os.OpenFile(s.uploads.path(u.id), os.O_WRONLY, 0)
	if err != nil {
		return err
	}
	defer f.Close()

	remaining := u.length - u.received
	hasher := sha256.New()
	body := io.LimitReader(r.Body, remaining+1)
	buf := make([]byte, uploadCopyBuffer)
	written := int64(0)
	var copyErr error
	for {
		n, err := body.Read(buf)
		if n > 0 {
			if written+int64(n) > remaining {
				copyErr = newAPIError(http.StatusRequestEntityTooLarge, codeInvalidRequest, "more bytes than the upload's length")
				break
			}
			if _, err := f.WriteAt(buf[:n], u.received+written); err != nil {
				// The service's own failure, unlike a client that went away: this request's bytes are dropped
				if errors.Is(err, syscall.ENOSPC) {
					copyErr = newAPIError(http.StatusInsufficientStorage, codeInsufficientStorage, "no space for the upload")
				} else {
					copyErr = newAPIError(http.StatusInternalServerError, codeInternal, "could not store the upload")
					s.log.Warn("Could not write an upload", "error", err)
				}
				break
			}
			hasher.Write(buf[:n])
			written += int64(n)
		}
		if err == io.EOF {
			break
		}
		if err != nil {
			// The client went away. Without a checksum, keep what arrived.
			copyErr = err
			break
		}
	}

	var ae *apiError
	keep := copyErr == nil || expected == nil && !errors.As(copyErr, &ae)
	if expected != nil && copyErr == nil && !equalBytes(hasher.Sum(nil), expected) {
		keep = false
		copyErr = newAPIError(statusChecksum, codeInvalidRequest, "checksum mismatch")
	}
	if !keep {
		_ = f.Truncate(u.received)
		return copyErr
	}
	if err := f.Sync(); err != nil {
		return err
	}
	newOffset := u.received + written
	complete := newOffset == u.length
	if complete {
		ok, err := s.uploads.verifyDigest(u)
		if err != nil {
			return err
		}
		if !ok {
			// Nothing can make this upload match its digest: start over
			_ = f.Truncate(0)
			newOffset, complete = 0, false
			_, _ = s.store.db.Exec(`UPDATE uploads SET received = 0, touched_at = ? WHERE upload_id = ?`,
				s.store.now().Unix(), u.id)
			return newAPIError(statusChecksum, codeUploadMismatch, "the upload does not match its digest; it was reset")
		}
	}
	err = s.store.tx(context.Background(), func(tx *sql.Tx) error {
		_, err := tx.Exec(`UPDATE uploads SET received = ?, complete = ?, touched_at = ? WHERE upload_id = ?`, newOffset,
			complete, s.store.now().Unix(), u.id)
		return err
	})
	if err != nil {
		return err
	}
	if copyErr != nil {
		// The request broke off; the client resumes from the offset HEAD reports
		return nil
	}
	w.Header().Set("Upload-Offset", strconv.FormatInt(newOffset, 10))
	w.Header().Set("Upload-Expires", s.store.now().Add(uploadExpiry).UTC().Format(http.TimeFormat))
	w.WriteHeader(http.StatusNoContent)
	return nil
}

// verifyDigest checks a complete upload against the SHA-256 it was created with
func (m *uploadManager) verifyDigest(u uploadRow) (bool, error) {
	f, err := os.Open(m.path(u.id))
	if err != nil {
		return false, err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.CopyBuffer(h, f, make([]byte, uploadCopyBuffer)); err != nil {
		return false, err
	}
	return equalBytes(h.Sum(nil), u.digest), nil
}

func (s *Service) handleUploadDelete(w http.ResponseWriter, r *http.Request, p principal) error {
	setTusHeaders(w)
	if _, err := s.uploadForRequest(r, p); err != nil {
		return err
	}
	lock := s.uploads.lock(r.PathValue("uploadID"))
	if !lock.TryLock() {
		return newAPIError(http.StatusConflict, codeInvalidRequest, "another request is writing this upload")
	}
	defer lock.Unlock()
	u, err := s.uploadForRequest(r, p)
	if err != nil {
		return err
	}
	if u.pinnedBy.Valid {
		return newAPIError(http.StatusConflict, codeInvalidRequest, "an operation still uses the upload")
	}
	removed := false
	err = s.store.tx(r.Context(), func(tx *sql.Tx) error {
		res, err := tx.Exec(`DELETE FROM uploads WHERE upload_id = ? AND pinned_by IS NULL`, u.id)
		if err != nil {
			return err
		}
		n, err := res.RowsAffected()
		removed = n > 0
		return err
	})
	if err != nil {
		return err
	}
	if !removed {
		// An operation took it in the meantime
		return newAPIError(http.StatusConflict, codeInvalidRequest, "an operation still uses the upload")
	}
	s.uploads.removeFile(u.id)
	w.WriteHeader(http.StatusNoContent)
	return nil
}

func equalBytes(a, b []byte) bool {
	return len(a) == len(b) && string(a) == string(b)
}
