// Copyright (C) 2026 Gordon
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this file,
// You can obtain one at https://mozilla.org/MPL/2.0/.
import Foundation

public struct RemotePathVersions: Codable, Sendable {
	public var folderID: String
	public var path: String
	public var versioning: RemoteVersioning
	public var versions: [RemoteVersion]
}

/// The exact source of a displayed archive. Item IDs are used only on the file's primary gateway.
public struct HistorySource: Sendable, Hashable, Identifiable {
	public var endpoint: RemoteEndpoint
	public var folderID: String
	public var path: String
	public var itemID: String?
	public var requiresEnabled: Bool
	public var id: String { endpoint.serviceID }
	public init(
		endpoint: RemoteEndpoint, folderID: String, path: String, itemID: String? = nil, requiresEnabled: Bool = true
	) {
		self.endpoint = endpoint
		self.folderID = folderID
		self.path = path
		self.itemID = itemID
		self.requiresEnabled = requiresEnabled
	}
}

public struct HistoryListing: Sendable {
	public var deviceID: String?
	public var versioning: RemoteVersioning
	public var versions: [RemoteVersion]
}

public enum HistoryError: LocalizedError, Sendable, Equatable {
	case sourceChanged
	case needsUpdate
	case clientNeedsUpdate
	case identityMismatch
	case restoreOutcomeUnknown
	case invalidDownload
	case destinationChanged
	case restorePending(String?)
	case restoreRejected
	case cannotOpen
	public static func message(for error: any Error) -> String {
		if let failure = error as? RemoteVersionBrowser.Failure {
			switch failure {
			case .remote(let remote): return message(for: remote)
			case .pendingChange: return String(localized: "Wait for pending edits to finish before restoring this file.")
			case .readOnly: return String(localized: "This destination is read only. You can open or export this version.")
			case .unsupported: return HistoryError.needsUpdate.localizedDescription
			case .outdated: return HistoryError.clientNeedsUpdate.localizedDescription
			case .outcomeUnknown: return HistoryError.restoreOutcomeUnknown.localizedDescription
			default: return String(localized: "This file is no longer available. Refresh before trying again.")
			}
		}
		guard let remote = error as? RemoteError else { return error.localizedDescription }
		switch remote {
		case .unreachable(.serverCertificateUntrusted):
			return String(localized: "The gateway's certificate is not trusted. Pair with it again.")
		case .unreachable: return String(localized: "The gateway could not be reached. Check your connection and try again.")
		case .unauthenticated: return String(localized: "This gateway no longer accepts this device. Pair with it again.")
		case .forbidden: return String(localized: "This gateway does not permit access to this folder.")
		case .notFound: return String(localized: "This folder or file is not available on the source.")
		case .versionNotFound:
			return String(localized: "This version is no longer available on the source. Refresh history and try again.")
		case .revisionMismatch: return String(localized: "The destination file changed. Nothing was restored.")
		case .unavailable, .overloaded: return String(localized: "The gateway is busy. Try again in a moment.")
		case .canceled: return String(localized: "The download was canceled.")
		case .insufficientStorage: return String(localized: "There is not enough space to save this version.")
		default: return String(localized: "The gateway could not complete this request. Refresh history and try again.")
		}
	}

	public var errorDescription: String? {
		switch self {
		case .sourceChanged:
			String(localized: "The history source changed or was disabled. Reopen Previous Versions to refresh its sources.")
		case .clientNeedsUpdate:
			String(localized: "Update this Synctrain app to connect to this gateway's newer API.")
		case .identityMismatch:
			String(localized: "This address belongs to a different gateway. Pair with the intended gateway again.")
		case .restoreOutcomeUnknown:
			String(
				localized:
					"The gateway could not confirm whether this restore completed. Check the current file and Pending Edits before trying again."
			)
		case .needsUpdate:
			String(localized: "Update Synctrain or synctrain-gateway on this history source to browse versions by path.")
		case .cannotOpen:
			String(localized: "The default app could not open this version. Use Save a Copy to open it elsewhere.")
		case .restoreRejected:
			String(
				localized:
					"The destination refused this restore. Its current file was not replaced. Review the saved edit in Pending Edits.")
		case .destinationChanged: String(localized: "The destination changed or has pending edits. Refresh before restoring.")
		case .restorePending(let detail):
			detail
				?? String(
					localized:
						"The restore is saved in Pending Edits and is still waiting for its gateway. It has not been confirmed as restored."
				)
		case .invalidDownload: String(localized: "The downloaded version was incomplete or did not match the requested file.")
		}
	}
}

extension RemoteFileClient {
	public func pathVersions(folderID: String, path: String) async throws(RemoteError) -> RemotePathVersions {
		try await json(
			RemotePathVersions.self,
			request("GET", url(["v1", "folders", folderID, "versions"], query: [URLQueryItem(name: "path", value: path)])))
	}

	public func downloadHistory(
		_ source: HistorySource, version: RemoteVersion, to destination: URL,
		progress: (@Sendable (Int64, Int64) -> Void)? = nil
	) async throws(RemoteError) {
		// By-path listings always carry a strong entity tag. Legacy item listings do not.
		// Keep retrieval on the listing's route even when a primary gateway has a stale item ID.
		let itemID = version.entityTag == nil ? source.itemID : nil
		let segments =
			itemID.map { ["v1", "items", $0, "versions", version.id, "content"] }
			?? ["v1", "folders", source.folderID, "versions", version.id, "content"]
		let query = itemID == nil ? [URLQueryItem(name: "path", value: source.path)] : []
		var downloadRequest = try request("GET", url(segments, query: query))
		if let entityTag = version.entityTag { downloadRequest.setValue(entityTag, forHTTPHeaderField: "If-Match") }
		let response: ContentResponse
		do throws(RemoteError) { response = try await download(downloadRequest, to: destination, progress: progress) }
		catch {
			if case .revisionMismatch = error { throw .versionNotFound }
			throw error
		}
		guard response.status == 200, response.receivedLength == version.size,
			version.entityTag == nil || response.entityTag == version.entityTag
		else {
			try? FileManager.default.removeItem(at: destination)
			throw .protocolViolation("the downloaded version was incomplete")
		}
	}
}

/// Read-only access to one paired history source. Rechecks stored configuration after network waits.
public struct RemoteHistory: Sendable {
	public let store: IndexStore
	public let source: HistorySource
	public let client: RemoteFileClient
	public init(store: IndexStore, source: HistorySource, client: RemoteFileClient) {
		self.store = store
		self.source = source
		self.client = client
	}

	static func isEntityTag(_ value: String?) -> Bool {
		guard let value, value.count <= 512, value.count >= 2 else { return false }
		return value.hasPrefix("\"") && value.hasSuffix("\"") && !value.contains("\r") && !value.contains("\n")
	}

	public func checkSource() throws {
		guard try store.remoteEndpoint(serviceID: source.id) == source.endpoint else { throw HistoryError.sourceChanged }
		if source.requiresEnabled {
			guard try store.gatewayUsage(serviceID: source.id).contains(.history) else { throw HistoryError.sourceChanged }
		}
	}

	public func versions() async throws -> HistoryListing {
		try checkSource()
		let capabilities = try await client.capabilities()
		guard capabilities.serviceID == source.id else { throw HistoryError.identityMismatch }
		guard capabilities.apiVersion <= 1 else { throw HistoryError.clientNeedsUpdate }
		guard capabilities.apiVersion == 1 else { throw HistoryError.needsUpdate }
		let list: HistoryListing
		if let itemID = source.itemID, !capabilities.supports(.versionsByPath) {
			guard capabilities.supports(.versions) else { throw HistoryError.needsUpdate }
			let response = try await client.versions(itemID: itemID)
			guard response.itemID == itemID else { throw HistoryError.invalidDownload }
			list = HistoryListing(
				deviceID: capabilities.syncthingDeviceID, versioning: response.versioning, versions: response.versions)
		}
		else {
			guard capabilities.supports(.versionsByPath) else { throw HistoryError.needsUpdate }
			let response = try await client.pathVersions(folderID: source.folderID, path: source.path)
			guard response.folderID == source.folderID, response.path == source.path,
				response.versions.allSatisfy({ Self.isEntityTag($0.entityTag) })
			else { throw HistoryError.invalidDownload }
			list = HistoryListing(
				deviceID: capabilities.syncthingDeviceID, versioning: response.versioning, versions: response.versions)
		}
		guard list.versions.allSatisfy({ RemoteVersionBrowser.isVersionID($0.id) && $0.size >= 0 }) else {
			throw HistoryError.invalidDownload
		}
		try Task.checkCancellation()
		try checkSource()
		return list
	}

	public func download(
		_ version: RemoteVersion, to destination: URL, progress: (@Sendable (Int64, Int64) -> Void)? = nil
	) async throws {
		guard RemoteVersionBrowser.isVersionID(version.id), version.size >= 0,
			version.entityTag == nil || Self.isEntityTag(version.entityTag)
		else { throw HistoryError.invalidDownload }
		try checkSource()
		do {
			try await client.downloadHistory(source, version: version, to: destination, progress: progress)
			try Task.checkCancellation()
			try checkSource()
		}
		catch {
			try? FileManager.default.removeItem(at: destination)
			throw error
		}
	}
}
