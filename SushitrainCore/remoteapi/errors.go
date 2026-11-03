// Copyright (C) 2026 Gordon
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this file,
// You can obtain one at https://mozilla.org/MPL/2.0/.

package remoteapi

// Error codes (Docs/remote-api/openapi.yaml, ErrorDetail.code). Clients treat codes they don't know by their HTTP
// status.
const (
	CodeInvalidRequest      = "invalid_request"
	CodeUnauthenticated     = "unauthenticated"
	CodeForbidden           = "forbidden"
	CodeFolderReadOnly      = "folder_read_only"
	CodeReservedName        = "reserved_name"
	CodeNotFound            = "not_found"
	CodeNameConflict        = "name_conflict"
	CodeIgnoredPath         = "ignored_path"
	CodeNotEmpty            = "not_empty"
	CodeCrossFolder         = "cross_folder"
	CodeRevisionMismatch    = "revision_mismatch"
	CodeUploadMismatch      = "upload_mismatch"
	CodeCursorExpired       = "cursor_expired"
	CodeSnapshotExpired     = "snapshot_expired"
	CodeLeaseExpired        = "lease_expired"
	CodeOperationMismatch   = "operation_mismatch"
	CodeOverloaded          = "overloaded"
	CodeTooManyAttempts     = "too_many_attempts"
	CodeContentUnavailable  = "content_unavailable"
	CodeVersionNotFound     = "version_not_found"
	CodeInsufficientStorage = "insufficient_storage"
	CodePairingDenied       = "pairing_denied"
	CodePairingExpired      = "pairing_expired"
	CodeInternal            = "internal"
)
