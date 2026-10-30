// Copyright (C) 2026 Gordon
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this file,
// You can obtain one at https://mozilla.org/MPL/2.0/.
import Foundation

// Types of version 1 of the remote file API (Docs/remote-api/openapi.yaml), and the shared types the app and the
// extension use to describe gateways, folder bindings, transfers and mutations. Decoding ignores unknown fields, and
// unknown values of enumerations decode to `.unknown`, as changes within v1 are additive.

// MARK: - Open enumerations

/// A string enumeration whose unknown values decode to `.unknown` instead of failing
public protocol OpenEnumeration: RawRepresentable, Codable, Hashable, Sendable where RawValue == String {
	init(rawValue: String)
}

extension OpenEnumeration {
	public init(from decoder: any Decoder) throws {
		self.init(rawValue: try decoder.singleValueContainer().decode(String.self))
	}

	public func encode(to encoder: any Encoder) throws {
		var container = encoder.singleValueContainer()
		try container.encode(rawValue)
	}
}

/// What an item is
public enum RemoteItemKind: OpenEnumeration {
	case file
	case directory
	case symlink
	case unknown(String)

	public init(rawValue: String) {
		switch rawValue {
		case "file": self = .file
		case "directory": self = .directory
		case "symlink": self = .symlink
		default: self = .unknown(rawValue)
		}
	}

	public var rawValue: String {
		switch self {
		case .file: return "file"
		case .directory: return "directory"
		case .symlink: return "symlink"
		case .unknown(let value): return value
		}
	}
}

/// Where the gateway can read an item's current content from now
public enum RemoteAvailability: OpenEnumeration {
	case local
	case peers
	case unavailable
	case unknown(String)

	public init(rawValue: String) {
		switch rawValue {
		case "local": self = .local
		case "peers": self = .peers
		case "unavailable": self = .unavailable
		default: self = .unknown(rawValue)
		}
	}

	public var rawValue: String {
		switch self {
		case .local: return "local"
		case .peers: return "peers"
		case .unavailable: return "unavailable"
		case .unknown(let value): return value
		}
	}
}

/// The Syncthing type of a folder on the gateway
public enum RemoteFolderType: OpenEnumeration {
	case sendReceive
	case sendOnly
	case receiveOnly
	case unknown(String)

	public init(rawValue: String) {
		switch rawValue {
		case "sendreceive": self = .sendReceive
		case "sendonly": self = .sendOnly
		case "receiveonly": self = .receiveOnly
		default: self = .unknown(rawValue)
		}
	}

	public var rawValue: String {
		switch self {
		case .sendReceive: return "sendreceive"
		case .sendOnly: return "sendonly"
		case .receiveOnly: return "receiveonly"
		case .unknown(let value): return value
		}
	}
}

public enum RemoteOperationKind: OpenEnumeration {
	case create
	case replace
	case move
	case delete
	case unknown(String)

	public init(rawValue: String) {
		switch rawValue {
		case "create": self = .create
		case "replace": self = .replace
		case "move": self = .move
		case "delete": self = .delete
		default: self = .unknown(rawValue)
		}
	}

	public var rawValue: String {
		switch self {
		case .create: return "create"
		case .replace: return "replace"
		case .move: return "move"
		case .delete: return "delete"
		case .unknown(let value): return value
		}
	}
}

/// The state of an operation as the gateway reports it
public enum RemoteOperationState: OpenEnumeration {
	case pending
	case committed
	case conflict
	case failed
	case unknown(String)

	public init(rawValue: String) {
		switch rawValue {
		case "pending": self = .pending
		case "committed": self = .committed
		case "conflict": self = .conflict
		case "failed": self = .failed
		default: self = .unknown(rawValue)
		}
	}

	public var rawValue: String {
		switch self {
		case .pending: return "pending"
		case .committed: return "committed"
		case .conflict: return "conflict"
		case .failed: return "failed"
		case .unknown(let value): return value
		}
	}
}

/// What a replace does when the item changed since its base revision
public enum RemoteConflictPolicy: OpenEnumeration {
	/// Keep the edit as a conflict copy
	case keepBoth
	/// Reject the edit
	case reject
	case unknown(String)

	public init(rawValue: String) {
		switch rawValue {
		case "keepBoth": self = .keepBoth
		case "reject": self = .reject
		default: self = .unknown(rawValue)
		}
	}

	public var rawValue: String {
		switch self {
		case .keepBoth: return "keepBoth"
		case .reject: return "reject"
		case .unknown(let value): return value
		}
	}
}

public enum RemotePreparationState: OpenEnumeration {
	case ready
	case preparing
	case failed
	case unknown(String)

	public init(rawValue: String) {
		switch rawValue {
		case "ready": self = .ready
		case "preparing": self = .preparing
		case "failed": self = .failed
		default: self = .unknown(rawValue)
		}
	}

	public var rawValue: String {
		switch self {
		case .ready: return "ready"
		case .preparing: return "preparing"
		case .failed: return "failed"
		case .unknown(let value): return value
		}
	}
}

public enum PairingState: OpenEnumeration {
	case pending
	case approved
	case unknown(String)

	public init(rawValue: String) {
		switch rawValue {
		case "pending": self = .pending
		case "approved": self = .approved
		default: self = .unknown(rawValue)
		}
	}

	public var rawValue: String {
		switch self {
		case .pending: return "pending"
		case .approved: return "approved"
		case .unknown(let value): return value
		}
	}
}

// MARK: - Opaque values

/// An item revision (`r1.…`). Opaque: equal revisions mean the client's copy is current.
public struct RemoteRevision: RawRepresentable, Codable, Hashable, Sendable, CustomStringConvertible {
	public let rawValue: String

	public init(rawValue: String) {
		self.rawValue = rawValue
	}

	public init(_ rawValue: String) {
		self.rawValue = rawValue
	}

	public var description: String { rawValue }
}

/// A content identity (`c1.<base64url BlocksHash>.<length>`): one Syncthing block list of the bytes. Equal bytes can
/// have different identities. Decoding never fails; `blocksHash` and `size` are nil for values in another format.
public struct ContentIdentity: RawRepresentable, Codable, Hashable, Sendable, CustomStringConvertible {
	public let rawValue: String

	public init(rawValue: String) {
		self.rawValue = rawValue
	}

	public init(_ rawValue: String) {
		self.rawValue = rawValue
	}

	public init(blocksHash: Data, size: Int64) {
		rawValue = "c1.\(blocksHash.base64URLEncodedString()).\(size)"
	}

	public var description: String { rawValue }

	private var parts: (hash: Data, size: Int64)? {
		let fields = rawValue.split(separator: ".", omittingEmptySubsequences: false)
		guard fields.count == 3, fields[0] == "c1", let hash = Data(base64URLEncoded: String(fields[1])),
			hash.count == 32, let size = Int64(fields[2]), size >= 0
		else {
			return nil
		}
		return (hash, size)
	}

	/// Whether the value is in the `c1.` format
	public var isValid: Bool { parts != nil }

	/// The SHA-256 of the concatenated block hashes (Syncthing's BlocksHash)
	public var blocksHash: Data? { parts?.hash }

	/// The length of the content in bytes
	public var size: Int64? { parts?.size }

	/// The strong validator the gateway sends for content of this identity
	public var entityTag: String { "\"\(rawValue)\"" }

	/// Whether the value can name a file: base64url characters, digits and dots only
	var isSafeFileName: Bool {
		!rawValue.isEmpty && rawValue.count <= 128 && rawValue != "." && rawValue != ".."
			&& rawValue.unicodeScalars.allSatisfy {
				CharacterSet.alphanumerics.contains($0) && $0.isASCII || $0 == "-" || $0 == "_" || $0 == "."
			}
	}
}

// MARK: - Service

public struct Capabilities: Codable, Hashable, Sendable {
	public var apiVersion: Int
	public var serviceID: String
	public var serviceName: String
	public var syncthingDeviceID: String?
	public var authorityGeneration: Int64
	/// Known values are in `Feature`; unknown values are kept but mean nothing to this client
	public var features: [String]
	public var limits: Limits
	public var certificate: Certificate?

	public struct Limits: Codable, Hashable, Sendable {
		public var pageSize: Int
		public var maxPageSize: Int
		public var manifestPageSize: Int?
		public var longPollSeconds: Int
		public var leaseSeconds: Int
		public var uploadExpirySeconds: Int
		public var maxUploadSize: Int64
		public var maxControlBodyBytes: Int
	}

	/// An announced rotation of a pinned certificate
	public struct Certificate: Codable, Hashable, Sendable {
		public var spki: String?
		public var next: Next?

		public struct Next: Codable, Hashable, Sendable {
			public var spki: String?
			public var effective: Date?
		}
	}

	public enum Feature: String, Sendable, CaseIterable {
		case longPoll
		case imageThumbnails = "thumbnails.image"
		case videoThumbnails = "thumbnails.video"
		case pdfThumbnails = "thumbnails.pdf"
		case directoryMoves
		case crossFolderMoves
		/// Listing and previewing the versions of files, and writing them (`OperationRequest.sourceVersion`)
		case versions
	}

	public func supports(_ feature: Feature) -> Bool {
		features.contains(feature.rawValue)
	}
}

public struct RemotePermissions: Codable, Hashable, Sendable {
	public var read: Bool
	public var write: Bool

	public init(read: Bool, write: Bool) {
		self.read = read
		self.write = write
	}
}

/// A folder the paired client may access on the gateway
public struct RemoteFolder: Codable, Hashable, Sendable {
	public var id: String
	public var label: String
	public var rootItemID: String
	public var permissions: RemotePermissions
	public var type: RemoteFolderType
	public var selective: Bool?
	public var state: String?
}

struct RemoteFolderList: Codable, Sendable {
	var folders: [RemoteFolder]
}

// MARK: - Metadata

/// An item's metadata as the gateway reports it (or a tombstone in a change list)
public struct RemoteItem: Codable, Hashable, Sendable {
	public var id: String
	public var folderID: String
	/// Nil for the folder's root
	public var parentID: String?
	public var name: String
	/// Slash-separated path within the folder; empty for the root
	public var path: String
	public var kind: RemoteItemKind
	public var size: Int64?
	public var modifiedNs: Int64
	public var mode: Int?
	public var writable: Bool?
	public var revision: RemoteRevision
	public var contentID: ContentIdentity?
	public var subtreeGeneration: Int64?
	public var availability: RemoteAvailability?
	public var deleted: Bool?
	/// On a tombstone in a folder's change list: the item moved to this folder of the same gateway, where it keeps its
	/// ID (and shows up live in that folder's change list)
	public var movedToFolderID: String?

	public init(
		id: String, folderID: String, parentID: String?, name: String, path: String, kind: RemoteItemKind,
		size: Int64? = nil, modifiedNs: Int64, mode: Int? = nil, writable: Bool? = nil, revision: RemoteRevision,
		contentID: ContentIdentity? = nil, subtreeGeneration: Int64? = nil, availability: RemoteAvailability? = nil,
		deleted: Bool? = nil, movedToFolderID: String? = nil
	) {
		self.id = id
		self.folderID = folderID
		self.parentID = parentID
		self.name = name
		self.path = path
		self.kind = kind
		self.size = size
		self.modifiedNs = modifiedNs
		self.mode = mode
		self.writable = writable
		self.revision = revision
		self.contentID = contentID
		self.subtreeGeneration = subtreeGeneration
		self.availability = availability
		self.deleted = deleted
		self.movedToFolderID = movedToFolderID
	}

	public var isDeleted: Bool { deleted == true }
	public var isRoot: Bool { parentID == nil }
}

// MARK: - Versions

/// A folder's Syncthing versioning
public enum RemoteVersioning: OpenEnumeration {
	case none
	case simple
	case staggered
	case trashcan
	/// Versions kept by an external command, which the gateway cannot list
	case external
	/// Versions kept somewhere other than the folder's default versions directory, which the gateway doesn't serve
	case customPath
	case unknown(String)

	public init(rawValue: String) {
		switch rawValue {
		case "none": self = .none
		case "simple": self = .simple
		case "staggered": self = .staggered
		case "trashcan": self = .trashcan
		case "external": self = .external
		case "customPath": self = .customPath
		default: self = .unknown(rawValue)
		}
	}

	public var rawValue: String {
		switch self {
		case .none: return "none"
		case .simple: return "simple"
		case .staggered: return "staggered"
		case .trashcan: return "trashcan"
		case .external: return "external"
		case .customPath: return "customPath"
		case .unknown(let value): return value
		}
	}
}

/// A previous version of a file, as the folder's versioning keeps it on the gateway
public struct RemoteVersion: Codable, Hashable, Sendable, Identifiable {
	/// Opaque (`ver_…`), unique among the versions of a path
	public var id: String
	/// When the version was archived
	public var versionTime: Date
	public var modifiedNs: Int64
	public var size: Int64

	public init(id: String, versionTime: Date, modifiedNs: Int64, size: Int64) {
		self.id = id
		self.versionTime = versionTime
		self.modifiedNs = modifiedNs
		self.size = size
	}

	public var modificationDate: Date {
		Date(timeIntervalSince1970: Double(modifiedNs) / 1_000_000_000)
	}
}

/// The versions the gateway keeps of a file, newest first
public struct RemoteVersionList: Codable, Hashable, Sendable {
	public var itemID: String
	/// The file's current revision
	public var revision: RemoteRevision
	public var versioning: RemoteVersioning
	public var versions: [RemoteVersion]
}

public struct SnapshotPage: Codable, Hashable, Sendable {
	public var snapshot: String
	/// The change cursor at the start of the snapshot session
	public var cursor: String
	public var items: [RemoteItem]
	/// Absent after the last page
	public var next: String?
}

public struct ChangesPage: Codable, Hashable, Sendable {
	public var changes: [RemoteItem]
	public var cursor: String
	public var more: Bool
}

// MARK: - Content

public struct ReadLease: Codable, Hashable, Sendable {
	public var lease: String
	/// Read token scoped to the lease, for background tasks
	public var token: String
	public var itemID: String
	public var revision: RemoteRevision
	public var contentID: ContentIdentity
	public var size: Int64
	public var blockSize: Int
	public var blockCount: Int
	public var expires: Date
	public var preparation: Preparation

	public struct Preparation: Codable, Hashable, Sendable {
		public var state: RemotePreparationState
		/// Bytes verified and ready from the start of the file
		public var readyBytes: Int64
		public var retryAfter: Int?
	}
}

public struct ManifestPage: Codable, Hashable, Sendable {
	public var contentID: ContentIdentity
	public var blockSize: Int
	public var blocks: [Block]
	/// Index of the next block; absent after the last page
	public var next: Int?

	public struct Block: Codable, Hashable, Sendable {
		public var offset: Int64
		public var size: Int
		/// The SHA-256 of the block, base64url
		public var hash: String
	}
}

/// A staged upload (tus)
public struct UploadInfo: Codable, Hashable, Sendable {
	public var upload: String
	/// Upload token scoped to the upload, for background tasks
	public var token: String
	public var offset: Int64
	public var length: Int64
	public var expires: Date
}

/// The confirmed state of an upload, from `HEAD /v1/uploads/{id}`
public struct UploadStatus: Hashable, Sendable {
	public var offset: Int64
	public var length: Int64?
	public var expires: Date?
}

/// What `OPTIONS /v1/uploads` announces
public struct TusOptions: Hashable, Sendable {
	public var versions: [String]
	public var extensions: [String]
	public var maxSize: Int64?
	public var checksumAlgorithms: [String]
}

// MARK: - Operations

/// A mutation request. The client operation ID together with the stable client identity is the idempotency key, so the
/// encoded request must be identical on every replay (see `canonicalJSON`).
public struct OperationRequest: Codable, Hashable, Sendable {
	/// The client operation ID (a lowercase UUID)
	public var id: String
	public var kind: RemoteOperationKind
	public var folderID: String
	public var itemID: String?
	public var baseRevision: RemoteRevision?
	public var subtreeGeneration: Int64?
	public var parentID: String?
	public var name: String?
	public var destinationFolderID: String?
	public var itemKind: RemoteItemKind?
	public var upload: String?
	public var uploadDigest: String?
	public var modifiedNs: Int64?
	public var onConflict: RemoteConflictPolicy?
	/// The file whose version a creation writes (a replacement writes a version of its own item)
	public var sourceItemID: String?
	/// The version (`RemoteVersion.id`) written instead of an upload
	public var sourceVersion: String?

	public init(
		id: String, kind: RemoteOperationKind, folderID: String, itemID: String? = nil,
		baseRevision: RemoteRevision? = nil, subtreeGeneration: Int64? = nil, parentID: String? = nil,
		name: String? = nil, destinationFolderID: String? = nil, itemKind: RemoteItemKind? = nil, upload: String? = nil,
		uploadDigest: String? = nil, modifiedNs: Int64? = nil, onConflict: RemoteConflictPolicy? = nil,
		sourceItemID: String? = nil, sourceVersion: String? = nil
	) {
		self.id = id
		self.kind = kind
		self.folderID = folderID
		self.itemID = itemID
		self.baseRevision = baseRevision
		self.subtreeGeneration = subtreeGeneration
		self.parentID = parentID
		self.name = name
		self.destinationFolderID = destinationFolderID
		self.itemKind = itemKind
		self.upload = upload
		self.uploadDigest = uploadDigest
		self.modifiedNs = modifiedNs
		self.onConflict = onConflict
		self.sourceItemID = sourceItemID
		self.sourceVersion = sourceVersion
	}

	public static func create(
		id: String, folderID: String, parentID: String, name: String, itemKind: RemoteItemKind,
		upload: String? = nil, uploadDigest: String? = nil, modifiedNs: Int64? = nil
	) -> OperationRequest {
		OperationRequest(
			id: id, kind: .create, folderID: folderID, parentID: parentID, name: name, itemKind: itemKind,
			upload: upload, uploadDigest: uploadDigest, modifiedNs: modifiedNs)
	}

	public static func replace(
		id: String, folderID: String, itemID: String, baseRevision: RemoteRevision, upload: String,
		uploadDigest: String, modifiedNs: Int64?, onConflict: RemoteConflictPolicy = .keepBoth
	) -> OperationRequest {
		OperationRequest(
			id: id, kind: .replace, folderID: folderID, itemID: itemID, baseRevision: baseRevision, upload: upload,
			uploadDigest: uploadDigest, modifiedNs: modifiedNs, onConflict: onConflict)
	}

	public static func move(
		id: String, folderID: String, itemID: String, baseRevision: RemoteRevision, parentID: String, name: String,
		subtreeGeneration: Int64? = nil, destinationFolderID: String? = nil
	) -> OperationRequest {
		OperationRequest(
			id: id, kind: .move, folderID: folderID, itemID: itemID, baseRevision: baseRevision,
			subtreeGeneration: subtreeGeneration, parentID: parentID, name: name,
			destinationFolderID: destinationFolderID)
	}

	public static func delete(
		id: String, folderID: String, itemID: String, baseRevision: RemoteRevision, subtreeGeneration: Int64? = nil
	) -> OperationRequest {
		OperationRequest(
			id: id, kind: .delete, folderID: folderID, itemID: itemID, baseRevision: baseRevision,
			subtreeGeneration: subtreeGeneration)
	}

	/// Restores a file to one of its versions. A file changed since `baseRevision` is not overwritten.
	public static func restore(
		id: String, folderID: String, itemID: String, baseRevision: RemoteRevision, version: String
	) -> OperationRequest {
		OperationRequest(
			id: id, kind: .replace, folderID: folderID, itemID: itemID, baseRevision: baseRevision, onConflict: .reject,
			sourceVersion: version)
	}

	/// Creates a file holding a version of another file of the folder
	public static func copyVersion(
		id: String, folderID: String, parentID: String, name: String, sourceItemID: String, version: String
	) -> OperationRequest {
		OperationRequest(
			id: id, kind: .create, folderID: folderID, parentID: parentID, name: name, itemKind: .file,
			sourceItemID: sourceItemID, sourceVersion: version)
	}

	/// The request as it is sent and stored: sorted keys, no optional fields that are not set
	public func canonicalJSON() throws -> Data {
		let encoder = JSONEncoder()
		encoder.outputFormatting = [.sortedKeys, .withoutEscapingSlashes]
		return try encoder.encode(self)
	}
}

/// An operation's state and result
public struct RemoteOperation: Codable, Hashable, Sendable {
	/// The client operation ID
	public var id: String
	/// The server operation ID (`op_…`)
	public var operation: String
	public var kind: RemoteOperationKind
	public var state: RemoteOperationState
	/// The resulting item (committed), or the current item (conflict)
	public var item: RemoteItem?
	/// The conflict copy holding the edit (conflict)
	public var conflictItem: RemoteItem?
	/// The content identity of the committed upload
	public var contentID: ContentIdentity?
	public var error: ErrorDetail?
	public var authorityGeneration: Int64
}

// MARK: - Errors

public struct ErrorDetail: Codable, Hashable, Sendable {
	public var code: String
	public var message: String
	public var correlationID: String?
	public var retryAfter: Int?
	/// The current item, for `revision_mismatch` and `name_conflict`
	public var item: RemoteItem?

	public init(
		code: String, message: String, correlationID: String? = nil, retryAfter: Int? = nil, item: RemoteItem? = nil
	) {
		self.code = code
		self.message = message
		self.correlationID = correlationID
		self.retryAfter = retryAfter
		self.item = item
	}
}

public struct ErrorBody: Codable, Hashable, Sendable {
	public var error: ErrorDetail

	public init(error: ErrorDetail) {
		self.error = error
	}
}

/// Machine-readable error codes of v1. Unknown codes are treated by their HTTP status.
public enum RemoteErrorCode {
	public static let invalidRequest = "invalid_request"
	public static let unauthenticated = "unauthenticated"
	public static let forbidden = "forbidden"
	public static let folderReadOnly = "folder_read_only"
	public static let reservedName = "reserved_name"
	public static let notFound = "not_found"
	public static let nameConflict = "name_conflict"
	public static let ignoredPath = "ignored_path"
	public static let notEmpty = "not_empty"
	public static let crossFolder = "cross_folder"
	public static let revisionMismatch = "revision_mismatch"
	public static let uploadMismatch = "upload_mismatch"
	public static let cursorExpired = "cursor_expired"
	public static let snapshotExpired = "snapshot_expired"
	public static let leaseExpired = "lease_expired"
	public static let operationMismatch = "operation_mismatch"
	public static let overloaded = "overloaded"
	public static let tooManyAttempts = "too_many_attempts"
	public static let contentUnavailable = "content_unavailable"
	public static let insufficientStorage = "insufficient_storage"
	public static let pairingDenied = "pairing_denied"
	public static let pairingExpired = "pairing_expired"
	public static let versionNotFound = "version_not_found"
	public static let internalError = "internal"
}

// MARK: - Pairing

public struct PairingRequest: Codable, Hashable, Sendable {
	public var secret: String
	public var serviceID: String?
	/// The stable client identity, kept across credential rotation and re-pairing
	public var clientID: String
	public var clientName: String
	public var platform: String?

	public init(secret: String, serviceID: String?, clientID: String, clientName: String, platform: String?) {
		self.secret = secret
		self.serviceID = serviceID
		self.clientID = clientID
		self.clientName = clientName
		self.platform = platform
	}
}

public struct PairingPending: Codable, Hashable, Sendable {
	public var request: String
	public var pollToken: String
	public var serviceID: String
	public var expires: Date
}

public struct PairingResult: Codable, Hashable, Sendable {
	public var state: PairingState
	/// The client credential, returned once when approved
	public var credential: String?
	public var clientID: String?
	public var serviceID: String?
}

/// The contents of a pairing QR code:
/// `synctrain+pair://<host>:<port>/?service=<service ID>&secret=<secret>[&spki=<hash>]`
public struct PairingInvitation: Hashable, Sendable {
	public var host: String
	public var port: Int
	public var serviceID: String
	public var secret: String
	public var pinnedSPKIHash: String?

	public static let scheme = "synctrain+pair"

	public init?(url: URL) {
		guard let components = URLComponents(url: url, resolvingAgainstBaseURL: false),
			components.scheme?.lowercased() == Self.scheme, let host = components.host, !host.isEmpty,
			let port = components.port, (1...65535).contains(port)
		else {
			return nil
		}
		let items = components.queryItems ?? []
		func value(_ name: String) -> String? {
			items.first { $0.name == name }?.value.flatMap { $0.isEmpty ? nil : $0 }
		}
		guard let service = value("service"), let secret = value("secret") else {
			return nil
		}
		self.host = host.hasPrefix("[") && host.hasSuffix("]") ? String(host.dropFirst().dropLast()) : host
		self.port = port
		serviceID = service
		self.secret = secret
		pinnedSPKIHash = value("spki")
	}

	/// The endpoint to pair with (named after the host until the gateway tells its name)
	public var endpoint: RemoteEndpoint {
		RemoteEndpoint(
			serviceID: serviceID, host: host, port: port, pinnedSPKIHash: pinnedSPKIHash, displayName: host)
	}
}

// MARK: - Shared types

/// A gateway the client is paired with. Credentials are kept separately, in the Keychain (see
/// `RemoteCredentialStore`).
public struct RemoteEndpoint: Codable, Hashable, Sendable {
	public var serviceID: String
	public var host: String
	public var port: Int
	/// The base64url SHA-256 of the certificate's SubjectPublicKeyInfo. Without one, the certificate must be trusted
	/// normally.
	public var pinnedSPKIHash: String?
	/// A pin announced for a certificate rotation (`capabilities.certificate.next`), accepted alongside the current one
	public var nextPinnedSPKIHash: String?
	public var displayName: String

	public init(
		serviceID: String, host: String, port: Int, pinnedSPKIHash: String? = nil, nextPinnedSPKIHash: String? = nil,
		displayName: String
	) {
		self.serviceID = serviceID
		self.host = host
		self.port = port
		self.pinnedSPKIHash = pinnedSPKIHash
		self.nextPinnedSPKIHash = nextPinnedSPKIHash
		self.displayName = displayName
	}

	/// The origin every request goes to (`https://host:port`)
	public var baseURL: URL {
		let host = self.host.contains(":") && !self.host.hasPrefix("[") ? "[\(self.host)]" : self.host
		return URL(string: "https://\(host):\(port)") ?? URL(filePath: "/nonexistent")
	}

	/// The pins accepted for the gateway's certificate (empty when it must be trusted normally)
	public var acceptedSPKIHashes: [String] {
		[pinnedSPKIHash, nextPinnedSPKIHash].compactMap { $0 }
	}

	/// Whether the URL has this endpoint's origin (scheme, host and port)
	public func hasOrigin(of url: URL?) -> Bool {
		guard let url, url.scheme?.lowercased() == "https", let host = url.host(percentEncoded: false) else {
			return false
		}
		let bare = host.hasPrefix("[") && host.hasSuffix("]") ? String(host.dropFirst().dropLast()) : host
		return bare.caseInsensitiveCompare(self.host) == .orderedSame && (url.port ?? 443) == port
	}
}

/// Ties a local folder (and its rows in the index) to a folder on a gateway
public struct RemoteFolderBinding: Codable, Hashable, Sendable {
	/// The local folder ID (`items.folder_id`)
	public var folderID: String
	/// The endpoint, by service ID
	public var serviceID: String
	/// The folder ID on the gateway
	public var remoteFolderID: String
	public var permissions: RemotePermissions

	public init(folderID: String, serviceID: String, remoteFolderID: String, permissions: RemotePermissions) {
		self.folderID = folderID
		self.serviceID = serviceID
		self.remoteFolderID = remoteFolderID
		self.permissions = permissions
	}
}

/// The confirmed remote metadata of a local item
public struct RemoteItemState: Hashable, Sendable {
	/// The local row (`items.id`), or nil when the item is not shown (waiting for its parent, or colliding with
	/// another name)
	public var itemID: Int64?
	public var gatewayID: String
	public var folderID: String
	public var parentGatewayID: String?
	public var path: String
	public var name: String
	public var kind: RemoteItemKind
	public var revision: RemoteRevision
	public var contentID: ContentIdentity?
	public var size: Int64
	public var modifiedNs: Int64
	public var mode: Int
	public var isWritable: Bool
	public var subtreeGeneration: Int64?
	public var availability: RemoteAvailability?
	public var isDeleted: Bool
}

/// A file staged immutably in the app group for an upload
public struct StagedFile: Hashable, Sendable {
	/// Relative to `RemoteStorage.baseURL`
	public var relativePath: String
	public var size: Int64
	/// The SHA-256 of the whole file, base64url without padding (`uploadDigest`)
	public var digest: String
	public var modifiedNs: Int64?

	public init(relativePath: String, size: Int64, digest: String, modifiedNs: Int64? = nil) {
		self.relativePath = relativePath
		self.size = size
		self.digest = digest
		self.modifiedNs = modifiedNs
	}
}

public enum TransferKind: String, Codable, Hashable, Sendable {
	case download
	case upload
	case thumbnail
}

public enum TransferState: String, Codable, Hashable, Sendable {
	/// Created, nothing transferred yet
	case pending
	/// A foreground worker or background task is running it
	case active
	/// A background task finished; its bytes wait for verification
	case downloaded
	/// Interrupted; resume from the verified blocks or the confirmed upload offset
	case interrupted
	case completed
	case failed
}

/// A transfer in the shared registry
public struct TransferRecord: Hashable, Sendable {
	public var transferID: String
	public var kind: TransferKind
	public var itemID: Int64?
	public var operationID: String?
	public var leaseID: String?
	/// Reference of the lease or upload token in the credential store
	public var tokenReference: String?
	public var contentID: ContentIdentity?
	/// The strong validator of the first response, sent as `If-Range` when resuming
	public var entityTag: String?
	public var totalSize: Int64
	public var blockCount: Int
	public var verifiedBlockCount: Int
	public var isManifestVerified: Bool
	/// The process or session that owns the transfer
	public var owner: String?
	public var claimExpires: Date?
	public var sessionIdentifier: String?
	public var taskIdentifier: Int?
	public var taskDescription: String?
	/// Where the background task's range starts
	public var taskOffset: Int64?
	/// Relative to `RemoteStorage.baseURL`
	public var stagingPath: String?
	public var state: TransferState
	public var lastError: String?
	public var createdAt: Date
	public var updatedAt: Date
}

/// The client-side state of an outbox operation
public enum MutationState: String, Codable, Hashable, Sendable, CaseIterable {
	/// Recorded; its bytes (if any) are staged
	case staged
	/// Uploading the staged bytes
	case uploading
	/// The gateway holds all staged bytes
	case uploaded
	/// The request may have reached the gateway, without a known answer
	case submitted
	/// Accepted (202); poll the operation
	case pending
	case committed
	/// The edit was kept as a conflict copy
	case conflict
	case failed
	/// The gateway can no longer report the result (authority generation change); needs user review
	case outcomeUnknown
	/// Removed by the user before it completed
	case canceled

	/// Whether the operation is done
	public var isTerminal: Bool {
		switch self {
		case .committed, .conflict, .failed, .outcomeUnknown, .canceled: return true
		default: return false
		}
	}

	/// Whether a request for the operation may have reached the gateway
	public var mayHaveReachedGateway: Bool {
		self == .submitted || self == .pending
	}

	/// Whether nothing has been sent for the operation yet, so that a newer local change can replace it
	public var isUnsent: Bool {
		self == .staged || self == .uploading || self == .uploaded
	}
}

/// What an outbox operation does. A cross-host move is a compound of a create on the destination and a delete of
/// the source, each its own step.
public enum MutationKind: String, Codable, Hashable, Sendable {
	case create
	case replace
	case move
	case delete
	case crossHostMove

	var remoteKind: RemoteOperationKind? {
		switch self {
		case .create: return .create
		case .replace: return .replace
		case .move: return .move
		case .delete: return .delete
		case .crossHostMove: return nil
		}
	}
}

/// An outbox operation
public struct MutationRecord: Hashable, Sendable {
	/// The client operation ID (a lowercase UUID), sent on every attempt
	public var operationID: String
	/// The local folder
	public var folderID: String
	public var serviceID: String?
	public var itemID: Int64?
	/// The gateway item the operation targets, when known
	public var gatewayID: String?
	public var kind: MutationKind
	/// The request as first submitted, replayed unchanged
	public var payload: OperationRequest?
	public var baseRevision: RemoteRevision?
	public var subtreeGeneration: Int64?
	public var targetParentItemID: Int64?
	public var targetName: String?
	public var targetFolderID: String?
	public var itemKind: ItemKind?
	public var modifiedNs: Int64?
	/// What a replace does when the file changed since its base revision (nil: the edit is kept as a conflict copy)
	public var conflictPolicy: RemoteConflictPolicy?
	/// For a creation the system asked for: the identifier the system gave its pending item (the template's), which
	/// it keeps when it asks again, also after the item was renamed or moved
	public var templateIdentifier: String?
	public var stagedPath: String?
	public var uploadDigest: String?
	public var uploadLength: Int64?
	public var uploadID: String?
	public var uploadOffset: Int64
	/// Reference of the upload token in the credential store
	public var uploadTokenReference: String?
	public var uploadExpires: Date?
	public var serverOperationID: String?
	public var state: MutationState
	public var claimOwner: String?
	public var claimExpires: Date?
	public var generation: Int64
	public var attempts: Int
	public var notBefore: Date?
	public var lastError: String?
	public var authorityGeneration: Int64?
	public var committedContentID: ContentIdentity?
	public var result: RemoteOperation?
	/// Operations that must be committed before this one is sent (an earlier change of the item, or the creation of
	/// its parent)
	public var dependencies: [String]
	/// The compound operation (a cross-host move) this one is a step of
	public var parentOperationID: String?
	public var step: Int?
	public var createdAt: Date
	public var updatedAt: Date
}

/// Who made the row a local change created, in a folder accessed through a gateway
public enum RemoteChangeOrigin: String, Codable, Hashable, Sendable {
	/// The system (`createItem`): until the creation completes, the system shows its own pending item for it, so the
	/// row is not enumerated
	case system
	/// Synctrain itself (a conflict copy of an edit): the system learns about the item from enumerations only
	case provider
}

/// A change the user discarded in the app, remembered so that the system does not make it again: the system still
/// holds the change it was told is not synchronized, and tries it again once told that the error is resolved. A
/// discard is remembered for 30 days, and forgotten once the system's attempt was matched.
public struct RemoteDiscard: Hashable, Sendable {
	/// The operation that was discarded
	public var operationID: String
	/// `.replace` for an edit of an item, `.create` for an item the system created
	public var kind: MutationKind
	/// The item the edit was of (for a creation: the row it had, which was removed)
	public var itemID: Int64?
	/// For a creation: the identifier the system gave its pending item (nil for creations recorded before it was kept)
	public var templateIdentifier: String?
	/// The SHA-256 of the discarded contents (`StagedFile.digest`); nil for a directory
	public var digest: String?
	public var discardedAt: Date
}

/// An outbox operation claimed by one process. Updates made with it apply only while the claim and its generation
/// are current.
public struct OutboxClaim: Hashable, Sendable {
	public var record: MutationRecord
	public var owner: String
	public var generation: Int64
	public var expires: Date

	public var operationID: String { record.operationID }
}

// MARK: - JSON

enum RemoteJSON {
	static func decoder() -> JSONDecoder {
		let decoder = JSONDecoder()
		decoder.dateDecodingStrategy = .custom { decoder in
			let string = try decoder.singleValueContainer().decode(String.self)
			if let date = parseDate(string) {
				return date
			}
			throw DecodingError.dataCorrupted(
				DecodingError.Context(codingPath: decoder.codingPath, debugDescription: "not an RFC 3339 time: \(string)"))
		}
		return decoder
	}

	static func encoder() -> JSONEncoder {
		let encoder = JSONEncoder()
		encoder.outputFormatting = [.sortedKeys, .withoutEscapingSlashes]
		encoder.dateEncodingStrategy = .custom { date, encoder in
			var container = encoder.singleValueContainer()
			try container.encode(date.formatted(.iso8601))
		}
		return encoder
	}

	/// RFC 3339 times, with or without fractional seconds
	static func parseDate(_ string: String) -> Date? {
		if let date = try? Date(string, strategy: .iso8601) {
			return date
		}
		return try? Date(string, strategy: Date.ISO8601FormatStyle(includingFractionalSeconds: true))
	}
}

extension Data {
	/// Decodes base64url, with or without padding
	init?(base64URLEncoded string: String) {
		var base64 = string.replacingOccurrences(of: "-", with: "+").replacingOccurrences(of: "_", with: "/")
		if base64.contains("=") {
			base64 = String(base64.prefix { $0 != "=" })
		}
		switch base64.count % 4 {
		case 1: return nil
		case 2: base64 += "=="
		case 3: base64 += "="
		default: break
		}
		self.init(base64Encoded: base64)
	}

	/// Encodes as base64url without padding
	func base64URLEncodedString() -> String {
		base64EncodedString().replacingOccurrences(of: "+", with: "-").replacingOccurrences(of: "/", with: "_")
			.replacingOccurrences(of: "=", with: "")
	}
}
