// Copyright (C) 2026 Gordon
// SPDX-License-Identifier: MPL-2.0
import Foundation

public struct HistoryBridgeRequest: Codable, Sendable {
	public var protocolVersion = 1
	public var action: String
	public var filePath: String?
	public var handle: String?
	public var leaseID: String?
	public init(action: String, filePath: String? = nil, handle: String? = nil, leaseID: String? = nil) {
		self.action = action
		self.filePath = filePath
		self.handle = handle
		self.leaseID = leaseID
	}
}
public struct HistoryBridgeVersion: Codable, Sendable, Identifiable {
	public var id: String
	public var versionTime: String
	public var modifiedNs: Int64
	public var size: Int64
	public var metadataStatus: String?
	public var snapshotETag: String?
	public var handle: String?
	public init(
		id: String, versionTime: String, modifiedNs: Int64, size: Int64, metadataStatus: String?, snapshotETag: String?
	) {
		self.id = id
		self.versionTime = versionTime
		self.modifiedNs = modifiedNs
		self.size = size
		self.metadataStatus = metadataStatus
		self.snapshotETag = snapshotETag
	}
}
public struct HistoryBridgeResponse: Codable, Sendable {
	public var protocolVersion = 1
	public var event: String
	public var sourceID: String?
	public var sourceName: String?
	public var deviceID: String?
	public var folderID: String?
	public var relativePath: String?
	public var canRestore: Bool?
	public var exclusiveExport: Bool?
	public var versioningEnabled: Bool?
	public var receiveOnly: Bool?
	public var versions: [HistoryBridgeVersion]?
	public var message: String?
	public var leaseID: String?
	public var filePath: String?
	public init(event: String) { self.event = event }
}
public enum HistoryBridgeError: LocalizedError, Sendable {
	case message(String)
	case requestFailed(String)
	public var errorDescription: String? {
		switch self {
		case .message(let text), .requestFailed(let text): return text
		}
	}
}
