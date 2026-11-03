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

	"t-shaped.nl/sushitrain/v2/remoteapi"
)

// Error codes (remoteapi, Docs/remote-api/openapi.yaml)
const (
	codeInvalidRequest      = remoteapi.CodeInvalidRequest
	codeUnauthenticated     = remoteapi.CodeUnauthenticated
	codeForbidden           = remoteapi.CodeForbidden
	codeFolderReadOnly      = remoteapi.CodeFolderReadOnly
	codeReservedName        = remoteapi.CodeReservedName
	codeNotFound            = remoteapi.CodeNotFound
	codeNameConflict        = remoteapi.CodeNameConflict
	codeIgnoredPath         = remoteapi.CodeIgnoredPath
	codeNotEmpty            = remoteapi.CodeNotEmpty
	codeCrossFolder         = remoteapi.CodeCrossFolder
	codeRevisionMismatch    = remoteapi.CodeRevisionMismatch
	codeUploadMismatch      = remoteapi.CodeUploadMismatch
	codeCursorExpired       = remoteapi.CodeCursorExpired
	codeSnapshotExpired     = remoteapi.CodeSnapshotExpired
	codeLeaseExpired        = remoteapi.CodeLeaseExpired
	codeOperationMismatch   = remoteapi.CodeOperationMismatch
	codeOverloaded          = remoteapi.CodeOverloaded
	codeTooManyAttempts     = remoteapi.CodeTooManyAttempts
	codeContentUnavailable  = remoteapi.CodeContentUnavailable
	codeVersionNotFound     = remoteapi.CodeVersionNotFound
	codeInsufficientStorage = remoteapi.CodeInsufficientStorage
	codePairingDenied       = remoteapi.CodePairingDenied
	codePairingExpired      = remoteapi.CodePairingExpired
	codeInternal            = remoteapi.CodeInternal
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
