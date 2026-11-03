// Copyright (C) 2026 Gordon
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this file,
// You can obtain one at https://mozilla.org/MPL/2.0/.

package gateway

import "t-shaped.nl/sushitrain/v2/remoteapi"

// The wire types of the v1 API are in remoteapi, which clients share; these are the names the service uses for them.

const apiVersion = remoteapi.APIVersion

const (
	featureLongPoll         = remoteapi.FeatureLongPoll
	featureThumbnailsImage  = remoteapi.FeatureThumbnailsImage
	featureThumbnailsVideo  = remoteapi.FeatureThumbnailsVideo
	featureThumbnailsPDF    = remoteapi.FeatureThumbnailsPDF
	featureDirectoryMoves   = remoteapi.FeatureDirectoryMoves
	featureCrossFolderMoves = remoteapi.FeatureCrossFolderMoves
	featureVersions         = remoteapi.FeatureVersions
)

const (
	kindFile      = remoteapi.KindFile
	kindDirectory = remoteapi.KindDirectory
	kindSymlink   = remoteapi.KindSymlink
)

const (
	availabilityLocal       = remoteapi.AvailabilityLocal
	availabilityPeers       = remoteapi.AvailabilityPeers
	availabilityUnavailable = remoteapi.AvailabilityUnavailable
)

const (
	preparationReady     = remoteapi.PreparationReady
	preparationPreparing = remoteapi.PreparationPreparing
	preparationFailed    = remoteapi.PreparationFailed
)

const (
	opCreate  = remoteapi.OpCreate
	opReplace = remoteapi.OpReplace
	opMove    = remoteapi.OpMove
	opDelete  = remoteapi.OpDelete
)

const (
	stateResultPending   = remoteapi.OperationPending
	stateResultCommitted = remoteapi.OperationCommitted
	stateResultConflict  = remoteapi.OperationConflict
	stateResultFailed    = remoteapi.OperationFailed
)

const (
	onConflictKeepBoth = remoteapi.OnConflictKeepBoth
	onConflictReject   = remoteapi.OnConflictReject
)

const (
	pairingPending  = remoteapi.PairingStatePending
	pairingApproved = remoteapi.PairingStateApproved
)

type (
	Capabilities        = remoteapi.Capabilities
	Limits              = remoteapi.Limits
	CertificateInfo     = remoteapi.CertificateInfo
	CertificateRotation = remoteapi.CertificateRotation
	FolderPermissions   = remoteapi.FolderPermissions
	Folder              = remoteapi.Folder
	FoldersResponse     = remoteapi.FoldersResponse
	Item                = remoteapi.Item
	SnapshotPage        = remoteapi.SnapshotPage
	ChangesPage         = remoteapi.ChangesPage
	ReadLeaseRequest    = remoteapi.ReadLeaseRequest
	Preparation         = remoteapi.Preparation
	ReadLease           = remoteapi.ReadLease
	ManifestBlock       = remoteapi.ManifestBlock
	ManifestPage        = remoteapi.ManifestPage
	Upload              = remoteapi.Upload
	OperationRequest    = remoteapi.OperationRequest
	ItemVersionList     = remoteapi.ItemVersionList
	ItemVersion         = remoteapi.ItemVersion
	Operation           = remoteapi.Operation
	PairingRequest      = remoteapi.PairingRequest
	PairingPending      = remoteapi.PairingPending
	PairingResult       = remoteapi.PairingResult
	ErrorDetail         = remoteapi.ErrorDetail
	ErrorResponse       = remoteapi.ErrorResponse
)
