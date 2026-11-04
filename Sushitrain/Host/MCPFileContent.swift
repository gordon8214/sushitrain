// Copyright (C) 2026 Gordon
// SPDX-License-Identifier: MPL-2.0
#if os(macOS)
	import Foundation
	import FileProvider
	import SynctrainFileProviderKit
	import SynctrainHostKit
	@preconcurrency import SushitrainCore
	extension MCPIntegration {
		func versionFile(_ method: String, parameters: [String: Any], itemID: Int64, environment: RemoteEnvironment)
			async throws -> Data {
			let store = environment.store
			func text(_ key: String) -> String { parameters[key] as? String ?? "" }

			let browser = try RemoteVersionBrowser(store: store, itemID: itemID, makeClient: environment.client(for:))
			let listing = try await browser.versions()
			if method == "mcp.files.versions" { return try Self.encoded(listing) }
			guard let version = listing.versions.first(where: { $0.id == text("Version") }) else {
				throw MCPAppError("The selected version is no longer kept.")
			}
			if let validator = version.entityTag, !text("Token").isEmpty, text("Token") != validator {
				throw MCPAppError("The selected version changed; list versions again.")
			}
			switch method {
			case "mcp.files.versionRestore": return try Self.encoded(try await browser.restore(version))
			case "mcp.files.versionCopy": return try Self.encoded(try await browser.saveCopy(of: version))
			case "mcp.files.versionExport":
				let directory = AppLocations.current.temporary.appending(path: "MCP-\(UUID().uuidString)")
				try FileManager.default.createDirectory(at: directory, withIntermediateDirectories: true)
				defer { try? FileManager.default.removeItem(at: directory) }
				try await Self.export(try await browser.download(version, into: directory), to: text("Path"))
				return Data("null".utf8)
			default: break
			}

			throw MCPAppError("Unsupported version operation.")
		}
		func readFile(
			_ method: String, parameters: [String: Any], item: IndexItem, environment: RemoteEnvironment,
			cancellation: MCPCancellation?
		) async throws -> Data {
			let store = environment.store
			let itemID = item.id
			func text(_ key: String) -> String { parameters[key] as? String ?? "" }
			func number(_ key: String) -> Int64 { (parameters[key] as? NSNumber)?.int64Value ?? 0 }

			let local = try store.remoteOverlay(itemID: itemID)?.stagedPath.map {
				environment.storage.url(relativePath: $0)
			}
			let count = number("Limit") == 0 ? 65536 : number("Limit")
			let offset = number("Offset")
			guard count > 0, count <= 65536, offset >= 0, offset <= item.size else {
				throw MCPAppError("Invalid content range; maximum 65536 bytes.")
			}
			let bytes: Data
			if let local {
				if method == "mcp.files.download" {
					try await Self.export(local, to: text("Path"))
					return Data("null".utf8)
				}
				bytes = try await Self.readRange(local, offset: offset, count: count)
			} else {
				guard let remote = try store.remoteItem(itemID: itemID), let request = RemoteDownloader.Request(remote),
					let binding = try store.remoteBinding(folderID: item.folderID),
					let client = environment.client(serviceID: binding.serviceID)
				else {
					throw MCPAppError("This file is unavailable from its gateway.")
				}
				let downloader = RemoteDownloader(
					store: store, client: client, storage: environment.storage,
					scheduler: environment.scheduler, credentials: environment.credentials, owner: environment.owner)
				if method == "mcp.files.download" {
					let url = try await downloader.download(request) { completed, total in
						cancellation?.reportProgress(completed, total: total)
					}
					try Task.checkCancellation()
					try await Self.export(url, to: text("Path"))
					return Data("null".utf8)
				}
				if let contentID = request.contentID, let cached = try downloader.cachedContent(contentID) {
					bytes = try await Self.readRange(cached, offset: offset, count: count)
				} else {
					let stream = RemoteContentStream(client: client, scheduler: environment.scheduler, request: request)
					_ = try await stream.open()
					bytes = try await stream.read(at: offset, length: count)
				}
			}
			try Task.checkCancellation()
			return try Self.contentResult(bytes: bytes, offset: offset, size: item.size)

		}
		nonisolated private static func readRange(_ url: URL, offset: Int64, count: Int64) async throws -> Data {
			try await Task.detached {
				let handle = try FileHandle(forReadingFrom: url)
				defer { try? handle.close() }
				try handle.seek(toOffset: UInt64(offset))
				return try handle.read(upToCount: Int(count)) ?? Data()
			}.value
		}

		nonisolated private static func contentResult(bytes: Data, offset: Int64, size: Int64) throws -> Data {
			let string = String(data: bytes, encoding: .utf8)
			return try Self.json([
				"content": string ?? bytes.base64EncodedString(), "encoding": string == nil ? "base64" : "utf-8",
				"bytes": bytes.count, "offset": offset, "nextOffset": offset + Int64(bytes.count), "size": size,
				"eof": offset + Int64(bytes.count) >= size
			])

		}
	}
#endif
