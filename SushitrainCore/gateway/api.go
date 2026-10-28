// Copyright (C) 2026 Gordon
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this file,
// You can obtain one at https://mozilla.org/MPL/2.0/.

package gateway

// Wire types of the v1 API (Docs/remote-api/openapi.yaml). Fixtures in Docs/remote-api/fixtures are tested against
// these in gateway_test.go.

const apiVersion = 1

// Known values of Capabilities.Features
const (
	featureLongPoll         = "longPoll"
	featureThumbnailsImage  = "thumbnails.image"
	featureThumbnailsVideo  = "thumbnails.video"
	featureThumbnailsPDF    = "thumbnails.pdf"
	featureDirectoryMoves   = "directoryMoves"
	featureCrossFolderMoves = "crossFolderMoves"
)

type Capabilities struct {
	APIVersion          int              `json:"apiVersion"`
	ServiceID           string           `json:"serviceID"`
	ServiceName         string           `json:"serviceName"`
	SyncthingDeviceID   string           `json:"syncthingDeviceID,omitempty"`
	AuthorityGeneration int64            `json:"authorityGeneration"`
	Features            []string         `json:"features"`
	Limits              Limits           `json:"limits"`
	Certificate         *CertificateInfo `json:"certificate,omitempty"`
}

type Limits struct {
	PageSize            int   `json:"pageSize"`
	MaxPageSize         int   `json:"maxPageSize"`
	ManifestPageSize    int   `json:"manifestPageSize,omitempty"`
	LongPollSeconds     int   `json:"longPollSeconds"`
	LeaseSeconds        int   `json:"leaseSeconds"`
	UploadExpirySeconds int   `json:"uploadExpirySeconds"`
	MaxUploadSize       int64 `json:"maxUploadSize"`
	MaxControlBodyBytes int   `json:"maxControlBodyBytes"`
}

type CertificateInfo struct {
	SPKI string               `json:"spki,omitempty"`
	Next *CertificateRotation `json:"next,omitempty"`
}

type CertificateRotation struct {
	SPKI      string `json:"spki"`
	Effective string `json:"effective"`
}

type FolderPermissions struct {
	Read  bool `json:"read"`
	Write bool `json:"write"`
}

type Folder struct {
	ID          string            `json:"id"`
	Label       string            `json:"label"`
	RootItemID  string            `json:"rootItemID"`
	Permissions FolderPermissions `json:"permissions"`
	Type        string            `json:"type"`
	Selective   bool              `json:"selective"`
	State       string            `json:"state,omitempty"`
}

type FoldersResponse struct {
	Folders []Folder `json:"folders"`
}

// Item kinds
const (
	kindFile      = "file"
	kindDirectory = "directory"
	kindSymlink   = "symlink"
)

// Availability values
const (
	availabilityLocal       = "local"
	availabilityPeers       = "peers"
	availabilityUnavailable = "unavailable"
)

type Item struct {
	ID                string  `json:"id"`
	FolderID          string  `json:"folderID"`
	ParentID          *string `json:"parentID"`
	Name              string  `json:"name"`
	Path              string  `json:"path"`
	Kind              string  `json:"kind"`
	Size              int64   `json:"size"`
	ModifiedNs        int64   `json:"modifiedNs"`
	Mode              uint32  `json:"mode"`
	Writable          bool    `json:"writable"`
	Revision          string  `json:"revision"`
	ContentID         string  `json:"contentID,omitempty"`
	SubtreeGeneration int64   `json:"subtreeGeneration,omitempty"`
	Availability      string  `json:"availability,omitempty"`
	Deleted           bool    `json:"deleted,omitempty"`
	// On a tombstone in the change log of a folder the item left for another folder of this service: that folder.
	// The item keeps its ID there.
	MovedToFolderID string `json:"movedToFolderID,omitempty"`
}

type SnapshotPage struct {
	Snapshot string `json:"snapshot"`
	Cursor   string `json:"cursor"`
	Items    []Item `json:"items"`
	Next     string `json:"next,omitempty"`
}

type ChangesPage struct {
	Changes []Item `json:"changes"`
	Cursor  string `json:"cursor"`
	More    bool   `json:"more"`
}

type ReadLeaseRequest struct {
	Revision string `json:"revision"`
}

// Preparation states
const (
	preparationReady     = "ready"
	preparationPreparing = "preparing"
	preparationFailed    = "failed"
)

type Preparation struct {
	State      string `json:"state"`
	ReadyBytes int64  `json:"readyBytes"`
	RetryAfter int    `json:"retryAfter,omitempty"`
}

type ReadLease struct {
	Lease       string      `json:"lease"`
	Token       string      `json:"token"`
	ItemID      string      `json:"itemID"`
	Revision    string      `json:"revision"`
	ContentID   string      `json:"contentID"`
	Size        int64       `json:"size"`
	BlockSize   int         `json:"blockSize"`
	BlockCount  int         `json:"blockCount"`
	Expires     string      `json:"expires"`
	Preparation Preparation `json:"preparation"`
}

type ManifestBlock struct {
	Offset int64  `json:"offset"`
	Size   int    `json:"size"`
	Hash   string `json:"hash"`
}

type ManifestPage struct {
	ContentID string          `json:"contentID"`
	BlockSize int             `json:"blockSize"`
	Blocks    []ManifestBlock `json:"blocks"`
	Next      *int            `json:"next,omitempty"`
}

type Upload struct {
	Upload  string `json:"upload"`
	Token   string `json:"token"`
	Offset  int64  `json:"offset"`
	Length  int64  `json:"length"`
	Expires string `json:"expires"`
}

// Operation kinds
const (
	opCreate  = "create"
	opReplace = "replace"
	opMove    = "move"
	opDelete  = "delete"
)

// Operation states
const (
	stateResultPending   = "pending"
	stateResultCommitted = "committed"
	stateResultConflict  = "conflict"
	stateResultFailed    = "failed"
)

// Conflict policies
const (
	onConflictKeepBoth = "keepBoth"
	onConflictReject   = "reject"
)

type OperationRequest struct {
	ID                  string `json:"id"`
	Kind                string `json:"kind"`
	FolderID            string `json:"folderID"`
	ItemID              string `json:"itemID,omitempty"`
	BaseRevision        string `json:"baseRevision,omitempty"`
	SubtreeGeneration   *int64 `json:"subtreeGeneration,omitempty"`
	ParentID            string `json:"parentID,omitempty"`
	Name                string `json:"name,omitempty"`
	DestinationFolderID string `json:"destinationFolderID,omitempty"`
	ItemKind            string `json:"itemKind,omitempty"`
	Upload              string `json:"upload,omitempty"`
	UploadDigest        string `json:"uploadDigest,omitempty"`
	ModifiedNs          *int64 `json:"modifiedNs,omitempty"`
	OnConflict          string `json:"onConflict,omitempty"`
}

type Operation struct {
	ID                  string       `json:"id"`
	Operation           string       `json:"operation"`
	Kind                string       `json:"kind"`
	State               string       `json:"state"`
	Item                *Item        `json:"item,omitempty"`
	ConflictItem        *Item        `json:"conflictItem,omitempty"`
	ContentID           string       `json:"contentID,omitempty"`
	Error               *ErrorDetail `json:"error,omitempty"`
	AuthorityGeneration int64        `json:"authorityGeneration"`
}

type PairingRequest struct {
	Secret     string `json:"secret"`
	ServiceID  string `json:"serviceID,omitempty"`
	ClientID   string `json:"clientID"`
	ClientName string `json:"clientName"`
	Platform   string `json:"platform,omitempty"`
}

type PairingPending struct {
	Request   string `json:"request"`
	PollToken string `json:"pollToken"`
	ServiceID string `json:"serviceID"`
	Expires   string `json:"expires"`
}

// Pairing result states
const (
	pairingPending  = "pending"
	pairingApproved = "approved"
)

type PairingResult struct {
	State      string `json:"state"`
	Credential string `json:"credential,omitempty"`
	ClientID   string `json:"clientID,omitempty"`
	ServiceID  string `json:"serviceID,omitempty"`
}

type ErrorDetail struct {
	Code          string `json:"code"`
	Message       string `json:"message"`
	CorrelationID string `json:"correlationID,omitempty"`
	RetryAfter    int    `json:"retryAfter,omitempty"`
	Item          *Item  `json:"item,omitempty"`
}

type ErrorResponse struct {
	Error ErrorDetail `json:"error"`
}
