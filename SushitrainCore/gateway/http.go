// Copyright (C) 2026 Gordon
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this file,
// You can obtain one at https://mozilla.org/MPL/2.0/.

package gateway

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"runtime/debug"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	headerCorrelationID = "Synctrain-Correlation-ID"
	headerUploadToken   = "Synctrain-Upload-Token"
	maxControlBodyBytes = 1 << 20
	// How long a client may take to send a control request's body
	controlBodyTimeout = 30 * time.Second
	// Concurrent requests beyond these are shed with 429: in all, without credentials (pairing), and per client
	maxConcurrentRequests = 256
	maxPublicRequests     = 16
	maxClientRequests     = 32
)

var correlationIDPattern = regexp.MustCompile(`^[A-Za-z0-9._-]{1,64}$`)

type handlerFunc func(w http.ResponseWriter, r *http.Request) error
type authedHandlerFunc func(w http.ResponseWriter, r *http.Request, p principal) error

// clientRequests counts each client's requests in progress
type clientRequests struct {
	mutex  sync.Mutex
	counts map[string]int
}

func (c *clientRequests) acquire(clientID string) bool {
	c.mutex.Lock()
	defer c.mutex.Unlock()
	if c.counts == nil {
		c.counts = make(map[string]int)
	}
	if c.counts[clientID] >= maxClientRequests {
		return false
	}
	c.counts[clientID]++
	return true
}

func (c *clientRequests) release(clientID string) {
	c.mutex.Lock()
	defer c.mutex.Unlock()
	if c.counts[clientID]--; c.counts[clientID] <= 0 {
		delete(c.counts, clientID)
	}
}

func (s *Service) routes() http.Handler {
	mux := http.NewServeMux()
	handle := func(pattern string, h authedHandlerFunc) {
		mux.Handle(pattern, s.wrap(s.requestSlots, func(w http.ResponseWriter, r *http.Request) error {
			p, err := s.authenticate(r)
			if err != nil {
				return err
			}
			if !s.clientRequests.acquire(p.clientID) {
				return errOverloaded
			}
			defer s.clientRequests.release(p.clientID)
			return h(w, r, p)
		}))
	}
	// Requests without credentials have their own small budget, so that they can't crowd out clients
	public := func(pattern string, h handlerFunc) {
		mux.Handle(pattern, s.wrap(s.publicSlots, h))
	}

	handle("GET /v1/capabilities", s.handleCapabilities)
	handle("GET /v1/folders", s.handleFolders)
	handle("GET /v1/folders/{folderID}/snapshot", s.handleSnapshot)
	handle("GET /v1/folders/{folderID}/changes", s.handleChanges)
	handle("GET /v1/items/{itemID}", s.handleItem)
	handle("POST /v1/items/{itemID}/read-leases", s.handleCreateLease)
	handle("GET /v1/items/{itemID}/thumbnail", s.handleThumbnail)
	handle("GET /v1/items/{itemID}/versions", s.handleVersions)
	handle("GET /v1/items/{itemID}/versions/{versionID}/content", s.handleVersionContent)
	handle("HEAD /v1/items/{itemID}/versions/{versionID}/content", s.handleVersionContent)
	handle("GET /v1/content/{leaseID}/manifest", s.handleManifest)
	handle("GET /v1/content/{leaseID}", s.handleContent)
	handle("HEAD /v1/content/{leaseID}", s.handleContent)
	public("OPTIONS /v1/uploads", s.handleUploadOptions)
	handle("POST /v1/uploads", s.handleCreateUpload)
	handle("HEAD /v1/uploads/{uploadID}", s.handleUploadHead)
	handle("PATCH /v1/uploads/{uploadID}", s.handleUploadPatch)
	handle("DELETE /v1/uploads/{uploadID}", s.handleUploadDelete)
	handle("POST /v1/operations", s.handleCreateOperation)
	handle("GET /v1/operations/{operationID}", s.handleOperation)
	public("POST /v1/pairing", s.handlePairingRequest)
	handle("GET /v1/pairing/{requestID}", s.handlePairingPoll)
	public("/", func(http.ResponseWriter, *http.Request) error { return errNotFound })
	return mux
}

// wrap adds correlation IDs, load shedding (with slots as the budget), panic recovery and error responses to a handler
func (s *Service) wrap(slots chan struct{}, h handlerFunc) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		correlationID := r.Header.Get(headerCorrelationID)
		if !correlationIDPattern.MatchString(correlationID) {
			correlationID = b64.EncodeToString(randomBytes(9))
		}
		w.Header().Set(headerCorrelationID, correlationID)
		w.Header().Set("Cache-Control", "no-store")

		select {
		case slots <- struct{}{}:
			defer func() { <-slots }()
		default:
			writeError(w, r, errOverloaded, correlationID)
			return
		}

		started := time.Now()
		defer func() {
			if p := recover(); p != nil {
				if p == http.ErrAbortHandler {
					panic(p)
				}
				s.log.Error("Request handler panicked", "correlation", correlationID, "panic", fmt.Sprint(p),
					"stack", string(debug.Stack()))
				writeError(w, r, errors.New("panic"), correlationID)
			}
		}()
		err := h(w, r)
		if err != nil {
			ae := asAPIError(err)
			// A client that went away is no failure of the service
			if ae.status >= 500 && r.Context().Err() == nil {
				s.log.Warn("Request failed", "method", r.Method, "route", routeOf(r), "correlation", correlationID,
					"status", ae.status, "error", err)
			}
			writeError(w, r, err, correlationID)
		}
		s.log.Debug("Request", "method", r.Method, "route", routeOf(r), "correlation", correlationID,
			"duration", time.Since(started))
	})
}

// routeOf returns the route pattern of a request rather than its path, which can contain file names and IDs
func routeOf(r *http.Request) string {
	if r.Pattern != "" {
		return r.Pattern
	}
	return "unmatched"
}

func writeError(w http.ResponseWriter, r *http.Request, err error, correlationID string) {
	ae := asAPIError(err)
	if ae.status == http.StatusUnauthorized {
		w.Header().Set("WWW-Authenticate", `Bearer realm="synctrain"`)
	}
	if ae.retryAfter > 0 {
		w.Header().Set("Retry-After", strconv.Itoa(ae.retryAfter))
	}
	if r.Method == http.MethodHead {
		w.WriteHeader(ae.status)
		return
	}
	_ = writeJSON(w, ae.status, ErrorResponse{Error: ae.detail(correlationID)})
}

func writeJSON(w http.ResponseWriter, status int, value any) error {
	body, err := json.Marshal(value)
	if err != nil {
		return err
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Content-Length", strconv.Itoa(len(body)))
	w.WriteHeader(status)
	_, err = w.Write(body)
	return err
}

// decodeJSON reads a bounded JSON request body
func decodeJSON(w http.ResponseWriter, r *http.Request, value any) error {
	if ct := r.Header.Get("Content-Type"); ct != "" && !strings.HasPrefix(ct, "application/json") {
		return newAPIError(http.StatusUnsupportedMediaType, codeInvalidRequest, "expected application/json")
	}
	// A client that never finishes sending its body must not hold a request slot indefinitely
	rc := http.NewResponseController(w)
	_ = rc.SetReadDeadline(time.Now().Add(controlBodyTimeout))
	defer func() { _ = rc.SetReadDeadline(time.Time{}) }()
	body := http.MaxBytesReader(w, r.Body, maxControlBodyBytes)
	dec := json.NewDecoder(body)
	if err := dec.Decode(value); err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			return newAPIError(http.StatusRequestEntityTooLarge, codeInvalidRequest, "request body too large")
		}
		return newAPIError(http.StatusBadRequest, codeInvalidRequest, "malformed JSON")
	}
	if _, err := dec.Token(); err != io.EOF {
		return newAPIError(http.StatusBadRequest, codeInvalidRequest, "trailing data after JSON")
	}
	return nil
}

func pageLimit(r *http.Request) (int, error) {
	value := r.URL.Query().Get("limit")
	if value == "" {
		return defaultPageSize, nil
	}
	n, err := strconv.Atoi(value)
	if err != nil || n < 1 {
		return 0, newAPIError(http.StatusBadRequest, codeInvalidRequest, "invalid limit")
	}
	return min(n, maxPageSize), nil
}

func waitDuration(r *http.Request, maximum time.Duration) time.Duration {
	value := r.URL.Query().Get("wait")
	if value == "" {
		return 0
	}
	n, err := strconv.Atoi(value)
	if err != nil || n <= 0 {
		return 0
	}
	return min(time.Duration(n)*time.Second, maximum)
}
