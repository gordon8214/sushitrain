// Copyright (C) 2026 Gordon
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this file,
// You can obtain one at https://mozilla.org/MPL/2.0/.

package remoteclient

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"
	"unicode"

	"t-shaped.nl/sushitrain/v2/remoteapi"
)

// Kind is what went wrong talking to a gateway, as the Swift RemoteError tells them apart
type Kind int

const (
	KindOther Kind = iota
	// Missing, expired or revoked credential (401)
	KindUnauthenticated
	// Not permitted (403), with the gateway's code (forbidden, folder_read_only, reserved_name, pairing_denied…)
	KindForbidden
	// No such item, folder, lease, upload or operation (404)
	KindNotFound
	// The version of a file is no longer kept (404 version_not_found)
	KindVersionNotFound
	// The name is taken (409 name_conflict), with the item that has it when the gateway says
	KindNameConflict
	// The base revision or subtree generation is not current (412), with the current item
	KindRevisionMismatch
	// The operation ID was used with another payload (422)
	KindOperationMismatch
	// The change cursor can no longer be served (410 cursor_expired): take a fresh snapshot
	KindCursorExpired
	// The snapshot session expired (410 snapshot_expired)
	KindSnapshotExpired
	// The read lease expired or its content changed (410 lease_expired): ask for a new one
	KindLeaseExpired
	// Temporarily unavailable (503), or the credential can't be read just now
	KindUnavailable
	// The gateway sheds load (429)
	KindOverloaded
	// Storage or quota exhausted on the gateway (507)
	KindInsufficientStorage
	// The gateway could not be reached
	KindUnreachable
	// The gateway's certificate is not the pinned one, or not trusted
	KindUntrusted
	// The gateway answered in a way the contract doesn't allow (a redirect, a wrong range, an invalid manifest…)
	KindProtocol
	// The request was canceled
	KindCanceled
)

// Error is an error talking to a gateway
type Error struct {
	Kind Kind
	// The HTTP status, if the gateway answered
	Status int
	// The gateway's error code (remoteapi.Code…)
	Code    string
	Message string
	// How long to wait before trying again, when the gateway said
	RetryAfter time.Duration
	// The current item, with 409 name_conflict and 412
	Item *remoteapi.Item
	// The operation, when a submitted operation failed
	Operation *remoteapi.Operation
	Err       error
}

func (e *Error) Error() string {
	switch {
	case e.Status != 0 && e.Code != "":
		return fmt.Sprintf("gateway: %d %s: %s", e.Status, e.Code, e.Message)
	case e.Status != 0:
		return fmt.Sprintf("gateway: status %d", e.Status)
	case e.Err != nil:
		return "gateway: " + e.Err.Error()
	default:
		return "gateway: " + e.Message
	}
}

func (e *Error) Unwrap() error {
	return e.Err
}

// Is matches errors of the same kind, so that errors.Is(err, ErrUnauthenticated) works
func (e *Error) Is(target error) bool {
	t, ok := target.(*Error)
	return ok && t.Kind == e.Kind && t.Status == 0 && t.Code == "" && t.Err == nil
}

// Transient reports whether trying again later may succeed without any other change. An operation the gateway
// finished (one whose replay answers with how it ended) never is.
func (e *Error) Transient() bool {
	if e.Operation != nil && e.Operation.State != remoteapi.OperationPending {
		return false
	}
	switch e.Kind {
	case KindUnavailable, KindOverloaded, KindUnreachable, KindInsufficientStorage:
		return true
	case KindOther:
		return e.Status >= 500
	}
	return false
}

// Sentinels for errors.Is
var (
	ErrUnauthenticated   = &Error{Kind: KindUnauthenticated}
	ErrForbidden         = &Error{Kind: KindForbidden}
	ErrNotFound          = &Error{Kind: KindNotFound}
	ErrRevisionMismatch  = &Error{Kind: KindRevisionMismatch}
	ErrNameConflict      = &Error{Kind: KindNameConflict}
	ErrCursorExpired     = &Error{Kind: KindCursorExpired}
	ErrSnapshotExpired   = &Error{Kind: KindSnapshotExpired}
	ErrLeaseExpired      = &Error{Kind: KindLeaseExpired}
	ErrUnavailable       = &Error{Kind: KindUnavailable}
	ErrUnreachable       = &Error{Kind: KindUnreachable}
	ErrUntrusted         = &Error{Kind: KindUntrusted}
	ErrProtocolViolation = &Error{Kind: KindProtocol}
	ErrCanceled          = &Error{Kind: KindCanceled}
)

// KindOf returns the kind of a gateway error, or KindOther
func KindOf(err error) Kind {
	var e *Error
	if errors.As(err, &e) {
		return e.Kind
	}
	return KindOther
}

// IsTransient reports whether err is a gateway error that trying again later may get past
func IsTransient(err error) bool {
	var e *Error
	return errors.As(err, &e) && e.Transient()
}

func protocolError(format string, args ...any) *Error {
	return &Error{Kind: KindProtocol, Message: fmt.Sprintf(format, args...)}
}

// statusError is the error for a status of 400 or above. gone is the kind of a 410 without a known code.
func statusError(resp *http.Response, body []byte, gone Kind) *Error {
	var detail struct {
		Error     remoteapi.ErrorDetail `json:"error"`
		Operation *remoteapi.Operation  `json:"operation"`
	}
	_ = json.Unmarshal(body, &detail)
	e := &Error{Status: resp.StatusCode, Code: printable(detail.Error.Code, 64),
		Message: printable(detail.Error.Message, 512), Item: detail.Error.Item, Operation: detail.Operation}
	if after, ok := parseRetryAfter(resp.Header.Get("Retry-After")); ok {
		e.RetryAfter = after
	} else if detail.Error.RetryAfter > 0 {
		e.RetryAfter = time.Duration(min(detail.Error.RetryAfter, int(maxRetryAfter/time.Second))) * time.Second
	}
	e.RetryAfter = min(e.RetryAfter, maxRetryAfter)
	switch resp.StatusCode {
	case http.StatusUnauthorized:
		e.Kind = KindUnauthenticated
	case http.StatusForbidden:
		e.Kind = KindForbidden
		if e.Code == "" {
			e.Code = remoteapi.CodeForbidden
		}
	case http.StatusNotFound:
		e.Kind = KindNotFound
		if e.Code == remoteapi.CodeVersionNotFound {
			e.Kind = KindVersionNotFound
		}
	case http.StatusConflict:
		if e.Code == remoteapi.CodeNameConflict {
			e.Kind = KindNameConflict
		}
	case http.StatusGone:
		switch e.Code {
		case remoteapi.CodeCursorExpired:
			e.Kind = KindCursorExpired
		case remoteapi.CodeSnapshotExpired:
			e.Kind = KindSnapshotExpired
		case remoteapi.CodeLeaseExpired:
			e.Kind = KindLeaseExpired
		case "":
			e.Kind = gone
		}
	case http.StatusPreconditionFailed:
		e.Kind = KindRevisionMismatch
	case http.StatusUnprocessableEntity:
		e.Kind = KindOperationMismatch
	case http.StatusTooManyRequests:
		e.Kind = KindOverloaded
	case http.StatusServiceUnavailable:
		e.Kind = KindUnavailable
	case http.StatusInsufficientStorage:
		e.Kind = KindInsufficientStorage
	}
	if e.Message == "" {
		e.Message = http.StatusText(resp.StatusCode)
	}
	return e
}

// The longest wait a gateway's Retry-After gets: one asking for more is checked on again after this
const maxRetryAfter = time.Hour

// printable is text from the gateway as errors show it: in terminals, logs and the window. Control characters (as
// terminal escape sequences) and bidirectional text controls are replaced, and it is cut short at limit bytes.
func printable(s string, limit int) string {
	s = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) || unicode.Is(unicode.Bidi_Control, r) {
			return unicode.ReplacementChar
		}
		return r
	}, s)
	if len(s) > limit {
		s = strings.ToValidUTF8(s[:limit], "") + "…"
	}
	return s
}

// parseRetryAfter reads seconds or an HTTP date
func parseRetryAfter(value string) (time.Duration, bool) {
	value = strings.TrimSpace(value)
	if value == "" {
		return 0, false
	}
	if seconds, err := strconv.Atoi(value); err == nil {
		return time.Duration(min(max(seconds, 0), int(maxRetryAfter/time.Second))) * time.Second, true
	}
	if t, err := http.ParseTime(value); err == nil {
		return max(time.Until(t), 0), true
	}
	return 0, false
}
