// Copyright (C) 2026 Gordon
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this file,
// You can obtain one at https://mozilla.org/MPL/2.0/.

package gateway

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
)

// Pairing: the host starts a session, which yields a single-use secret shown as a QR code. A client submits the
// secret with its stable identity and name; the host approves the request (granting folders) or denies it; the
// client then collects its credential once. Credentials are created only when collected, so none is stored.

const (
	pairingSessionLifetime = 10 * time.Minute
	pairingRequestLifetime = 10 * time.Minute
	// Wrong secrets tolerated from one address per period. Secrets are long enough that guessing is hopeless; the
	// limit only slows a misbehaving client, without letting one stop everyone else from pairing.
	pairingMaxWrongSecrets = 10
	pairingWrongPeriod     = 10 * time.Minute
	pairingLimiterSize     = 1024
)

// Pairing request states in the database
const (
	pairingStatePending   = "pending"
	pairingStateApproved  = "approved"
	pairingStateDenied    = "denied"
	pairingStateDelivered = "delivered"
)

type pairingLimiter struct {
	mutex sync.Mutex
	wrong map[string]wrongSecrets
}

type wrongSecrets struct {
	count int
	since time.Time
}

// blocked reports whether an address sent too many wrong secrets recently
func (l *pairingLimiter) blocked(addr string, now time.Time) bool {
	l.mutex.Lock()
	defer l.mutex.Unlock()
	w, ok := l.wrong[addr]
	return ok && w.count >= pairingMaxWrongSecrets && now.Sub(w.since) < pairingWrongPeriod
}

func (l *pairingLimiter) recordWrong(addr string, now time.Time) {
	l.mutex.Lock()
	defer l.mutex.Unlock()
	if l.wrong == nil {
		l.wrong = make(map[string]wrongSecrets)
	}
	if len(l.wrong) >= pairingLimiterSize {
		for a, w := range l.wrong {
			if now.Sub(w.since) >= pairingWrongPeriod {
				delete(l.wrong, a)
			}
		}
	}
	w := l.wrong[addr]
	if now.Sub(w.since) >= pairingWrongPeriod {
		w = wrongSecrets{since: now}
	}
	w.count++
	if len(l.wrong) < pairingLimiterSize || w.count > 1 {
		l.wrong[addr] = w
	}
}

// remoteHost is the address a request came from, without its port
func remoteHost(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

// PairingSession is a started pairing, for showing to the person approving it
type PairingSession struct {
	SessionID string    `json:"sessionID"`
	Secret    string    `json:"secret"`
	URL       string    `json:"url"`
	Expires   time.Time `json:"expires"`
}

// StartPairing creates a pairing session. host and port are where clients reach the service; spki is the pinned
// public key hash for self-signed certificates ("" when the certificate is trusted normally).
func (s *Service) StartPairing(host string, port int, spki string) (PairingSession, error) {
	serviceID, err := s.store.serviceID()
	if err != nil {
		return PairingSession{}, err
	}
	secret := newSecret()
	session := PairingSession{
		SessionID: newID("pss_"),
		Secret:    secret,
		Expires:   s.store.now().Add(pairingSessionLifetime),
	}
	err = s.store.tx(context.Background(), func(tx *sql.Tx) error {
		_, err := tx.Exec(`INSERT INTO pairing_sessions (session_id, secret_hash, expires_at) VALUES (?, ?, ?)`,
			session.SessionID, secretHash(secret), session.Expires.Unix())
		return err
	})
	if err != nil {
		return PairingSession{}, err
	}
	query := url.Values{"service": {serviceID}, "secret": {secret}}
	if spki != "" {
		query.Set("spki", spki)
	}
	u := url.URL{Scheme: "synctrain+pair", Host: net.JoinHostPort(host, strconv.Itoa(port)), Path: "/",
		RawQuery: query.Encode()}
	session.URL = u.String()
	return session, nil
}

// PendingPairing is a client waiting for approval
type PendingPairing struct {
	RequestID  string    `json:"requestID"`
	SessionID  string    `json:"sessionID"`
	ClientID   string    `json:"clientID"`
	ClientName string    `json:"clientName"`
	Platform   string    `json:"platform"`
	Requested  time.Time `json:"requested"`
	// The name of the paired client this request would replace (pairing again with the same identity), if any
	Replaces string `json:"replaces,omitempty"`
}

func (s *Service) PendingPairings() ([]PendingPairing, error) {
	rows, err := s.store.db.Query(`SELECT r.request_id, r.session_id, r.client_id, r.client_name, r.platform,
		r.created_at, COALESCE(c.name, '') FROM pairing_requests r LEFT JOIN clients c ON c.client_id = r.client_id
		WHERE r.state = ? AND r.expires_at > ? ORDER BY r.created_at`, pairingStatePending, s.store.now().Unix())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var result []PendingPairing
	for rows.Next() {
		var p PendingPairing
		var created int64
		if err := rows.Scan(&p.RequestID, &p.SessionID, &p.ClientID, &p.ClientName, &p.Platform, &created,
			&p.Replaces); err != nil {
			return nil, err
		}
		p.Requested = time.Unix(created, 0)
		result = append(result, p)
	}
	return result, rows.Err()
}

// ApprovePairing approves a request, granting the given folders ("read" or "write" per folder ID)
func (s *Service) ApprovePairing(requestID string, grants map[string]string) error {
	for folder, access := range grants {
		if access != "read" && access != "write" {
			return fmt.Errorf("folder %q: access must be read or write", folder)
		}
	}
	encoded, err := json.Marshal(grants)
	if err != nil {
		return err
	}
	return s.decidePairing(requestID, pairingStateApproved, string(encoded))
}

func (s *Service) DenyPairing(requestID string) error {
	return s.decidePairing(requestID, pairingStateDenied, "")
}

func (s *Service) decidePairing(requestID, state, grants string) error {
	err := s.store.tx(context.Background(), func(tx *sql.Tx) error {
		res, err := tx.Exec(`UPDATE pairing_requests SET state = ?, grants = ? WHERE request_id = ? AND state = ?
			AND expires_at > ?`, state, grants, requestID, pairingStatePending, s.store.now().Unix())
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return errors.New("no such pending pairing request")
		}
		return nil
	})
	if err == nil {
		s.pairingChanged.notify(requestID)
	}
	return err
}

// handlePairingRequest is POST /v1/pairing
func (s *Service) handlePairingRequest(w http.ResponseWriter, r *http.Request) error {
	var req PairingRequest
	if err := decodeJSON(w, r, &req); err != nil {
		return err
	}
	clientUUID, err := uuid.Parse(req.ClientID)
	if err != nil || req.Secret == "" || strings.TrimSpace(req.ClientName) == "" {
		return newAPIError(http.StatusBadRequest, codeInvalidRequest, "secret, clientID (a UUID) and clientName are required")
	}
	// One identity, however the client spelled its UUID
	clientID := clientUUID.String()
	serviceID, err := s.store.serviceID()
	if err != nil {
		return err
	}
	if req.ServiceID != "" && req.ServiceID != serviceID {
		return newAPIError(http.StatusForbidden, codePairingDenied, "the pairing code is for another service")
	}

	now := s.store.now()
	if s.pairing.blocked(remoteHost(r), now) {
		return newAPIError(http.StatusTooManyRequests, codeTooManyAttempts,
			"too many wrong pairing codes; try again later").withRetryAfter(int(pairingWrongPeriod / time.Second))
	}

	pending := PairingPending{
		Request:   newID(prefixPairing),
		PollToken: newToken(prefixPollToken),
		ServiceID: serviceID,
		Expires:   now.Add(pairingRequestLifetime).UTC().Format(time.RFC3339),
	}
	var found, expired bool
	err = s.store.tx(r.Context(), func(tx *sql.Tx) error {
		var sessionID string
		var used bool
		var expires int64
		err := tx.QueryRow(`SELECT session_id, used, expires_at FROM pairing_sessions WHERE secret_hash = ?`,
			secretHash(req.Secret)).Scan(&sessionID, &used, &expires)
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		if used || expires <= now.Unix() {
			expired = true
			return nil
		}
		found = true
		if _, err := tx.Exec(`UPDATE pairing_sessions SET used = 1 WHERE session_id = ?`, sessionID); err != nil {
			return err
		}
		_, err = tx.Exec(`INSERT INTO pairing_requests (request_id, session_id, poll_hash, client_id, client_name, platform,
			state, created_at, expires_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`, pending.Request, sessionID,
			secretHash(pending.PollToken), clientID, truncate(req.ClientName, 200),
			truncate(req.Platform, 40), pairingStatePending, now.Unix(), now.Add(pairingRequestLifetime).Unix())
		return err
	})
	if err != nil {
		return err
	}
	if expired {
		return newAPIError(http.StatusGone, codePairingExpired, "the pairing code was used or has expired")
	}
	if !found {
		s.pairing.recordWrong(remoteHost(r), now)
		return newAPIError(http.StatusForbidden, codePairingDenied, "wrong pairing code")
	}
	s.log.Info("Pairing requested", "request", pending.Request, "client", clientID)
	s.pairingChanged.notify("")
	return writeJSON(w, http.StatusAccepted, pending)
}

// handlePairingPoll is GET /v1/pairing/{requestID}
func (s *Service) handlePairingPoll(w http.ResponseWriter, r *http.Request, p principal) error {
	requestID := r.PathValue("requestID")
	if p.kind != principalPairingPoll || p.resource != requestID {
		return errUnauthenticated
	}
	wait := waitDuration(r, 30*time.Second)
	deadline := time.Now().Add(wait)
	for {
		changed := s.pairingChanged.wait(requestID)
		result, status, err := s.collectPairing(r.Context(), requestID)
		if err != nil {
			return err
		}
		if status != http.StatusAccepted || time.Now().After(deadline) {
			return writeJSON(w, status, result)
		}
		select {
		case <-changed:
		case <-time.After(time.Until(deadline)):
		case <-r.Context().Done():
			// The service is stopping (or the client left): answer with the state rather than an empty body
			return writeJSON(w, status, result)
		}
	}
}

// collectPairing returns the state of a request, creating and returning the credential the first time an approved
// request is polled
func (s *Service) collectPairing(ctx context.Context, requestID string) (PairingResult, int, error) {
	serviceID, err := s.store.serviceID()
	if err != nil {
		return PairingResult{}, 0, err
	}
	var result PairingResult
	status := http.StatusAccepted
	err = s.store.tx(ctx, func(tx *sql.Tx) error {
		var state, clientID, name, platform, grants string
		var expires int64
		err := tx.QueryRow(`SELECT state, client_id, client_name, platform, grants, expires_at FROM pairing_requests
			WHERE request_id = ?`, requestID).Scan(&state, &clientID, &name, &platform, &grants, &expires)
		if errors.Is(err, sql.ErrNoRows) {
			return errNotFound
		}
		if err != nil {
			return err
		}
		switch {
		case state == pairingStateDenied:
			return newAPIError(http.StatusForbidden, codePairingDenied, "pairing was denied on the host")
		case state == pairingStateDelivered || expires <= s.store.now().Unix():
			return newAPIError(http.StatusGone, codePairingExpired, "the pairing request has expired or was collected")
		case state == pairingStatePending:
			result = PairingResult{State: pairingPending, ServiceID: serviceID}
			return nil
		}

		credential := newToken(prefixCredential)
		now := s.store.now().Unix()
		// Scoped tokens that came with an earlier credential of the same identity stop working
		if _, err := tx.Exec(`DELETE FROM leases WHERE client_id = ?`, clientID); err != nil {
			return err
		}
		if _, err := tx.Exec(`UPDATE uploads SET token_hash = randomblob(32) WHERE client_id = ?`, clientID); err != nil {
			return err
		}
		// A client that pairs again keeps its identity, and with it its operation receipts
		if _, err := tx.Exec(`INSERT INTO clients (client_id, name, platform, credential_hash, created_at, last_seen)
			VALUES (?, ?, ?, ?, ?, ?) ON CONFLICT (client_id) DO UPDATE SET name = excluded.name,
			platform = excluded.platform, credential_hash = excluded.credential_hash, revoked_at = NULL`,
			clientID, name, platform, secretHash(credential), now, now); err != nil {
			return err
		}
		var granted map[string]string
		if grants != "" {
			if err := json.Unmarshal([]byte(grants), &granted); err != nil {
				return err
			}
		}
		if _, err := tx.Exec(`DELETE FROM permissions WHERE client_id = ?`, clientID); err != nil {
			return err
		}
		for folder, access := range granted {
			if _, err := tx.Exec(`INSERT INTO permissions (client_id, folder_id, can_write) VALUES (?, ?, ?)`, clientID,
				folder, access == "write"); err != nil {
				return err
			}
		}
		if _, err := tx.Exec(`UPDATE pairing_requests SET state = ? WHERE request_id = ?`, pairingStateDelivered,
			requestID); err != nil {
			return err
		}
		result = PairingResult{State: pairingApproved, Credential: credential, ClientID: clientID, ServiceID: serviceID}
		status = http.StatusOK
		return nil
	})
	if err == nil && status == http.StatusOK {
		s.audit(result.ClientID, "pair", "", "", "ok")
	}
	return result, status, err
}

// truncate shortens s to at most n bytes, on a character boundary
func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n]
}
