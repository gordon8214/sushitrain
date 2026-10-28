// Copyright (C) 2026 Gordon
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this file,
// You can obtain one at https://mozilla.org/MPL/2.0/.

package gateway

import (
	"errors"
	"fmt"
	"net/http"
)

// Error codes (Docs/remote-api/openapi.yaml, ErrorDetail.code)
const (
	codeInvalidRequest      = "invalid_request"
	codeUnauthenticated     = "unauthenticated"
	codeForbidden           = "forbidden"
	codeFolderReadOnly      = "folder_read_only"
	codeReservedName        = "reserved_name"
	codeNotFound            = "not_found"
	codeNameConflict        = "name_conflict"
	codeIgnoredPath         = "ignored_path"
	codeNotEmpty            = "not_empty"
	codeCrossFolder         = "cross_folder"
	codeRevisionMismatch    = "revision_mismatch"
	codeUploadMismatch      = "upload_mismatch"
	codeCursorExpired       = "cursor_expired"
	codeSnapshotExpired     = "snapshot_expired"
	codeLeaseExpired        = "lease_expired"
	codeOperationMismatch   = "operation_mismatch"
	codeOverloaded          = "overloaded"
	codeTooManyAttempts     = "too_many_attempts"
	codeContentUnavailable  = "content_unavailable"
	codeInsufficientStorage = "insufficient_storage"
	codePairingDenied       = "pairing_denied"
	codePairingExpired      = "pairing_expired"
	codeInternal            = "internal"
)

// An error with an HTTP status and machine-readable code, as returned to clients
type apiError struct {
	status     int
	code       string
	message    string
	retryAfter int
	item       *Item
}

func (e *apiError) Error() string {
	return fmt.Sprintf("%d %s: %s", e.status, e.code, e.message)
}

func newAPIError(status int, code string, format string, args ...any) *apiError {
	return &apiError{status: status, code: code, message: fmt.Sprintf(format, args...)}
}

func (e *apiError) withItem(item *Item) *apiError {
	c := *e
	c.item = item
	return &c
}

func (e *apiError) withRetryAfter(seconds int) *apiError {
	c := *e
	c.retryAfter = seconds
	return &c
}

func (e *apiError) detail(correlationID string) ErrorDetail {
	return ErrorDetail{
		Code:          e.code,
		Message:       e.message,
		CorrelationID: correlationID,
		RetryAfter:    e.retryAfter,
		Item:          e.item,
	}
}

var (
	errUnauthenticated = newAPIError(http.StatusUnauthorized, codeUnauthenticated, "missing, expired or revoked credential")
	errForbidden       = newAPIError(http.StatusForbidden, codeForbidden, "not permitted")
	errNotFound        = newAPIError(http.StatusNotFound, codeNotFound, "not found")
	errOverloaded      = newAPIError(http.StatusTooManyRequests, codeOverloaded, "too many requests").withRetryAfter(2)
)

// asAPIError returns the client-facing form of err: its own if it is an apiError, otherwise an internal error
func asAPIError(err error) *apiError {
	var ae *apiError
	if errors.As(err, &ae) {
		return ae
	}
	return newAPIError(http.StatusInternalServerError, codeInternal, "internal error")
}
