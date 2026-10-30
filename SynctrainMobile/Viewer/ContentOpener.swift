// Copyright (C) 2026 Gordon
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this file,
// You can obtain one at https://mozilla.org/MPL/2.0/.
import Foundation
import SynctrainFileProviderKit

/// Why a file could not be opened in the app
enum ContentError: LocalizedError, Sendable {
	case gone
	case notListedYet
	case notAvailable
	case remote(RemoteError)
	case store(IndexError)

	var errorDescription: String? {
		switch self {
		case .gone:
			return String(localized: "This file is no longer on the gateway.")
		case .notListedYet:
			return String(localized: "This file just changed. Try again in a moment.")
		case .notAvailable:
			return String(localized: "The gateway of this folder is not paired.")
		case .remote(let error):
			return error.userDescription
		case .store(let error):
			return error.userDescription
		}
	}
}

/// Provides the contents a row shows as a file the app can show. An unsent edit wins over the gateway's version. Files
/// are downloaded into the cache the app shares with the extension (content-addressed, so the two cannot disturb each
/// other), never read from the system's copy, which would make it download them itself.
enum ContentOpener {
	/// Where the copies being viewed are, under their own names: viewers go by the extension, and cached files have none.
	/// Clones take no space until the original is gone.
	private static var viewingDirectory: URL {
		URL.temporaryDirectory.appending(path: "Viewer", directoryHint: .isDirectory)
	}

	/// Removes what the last run left behind
	static func removeViewingCopies() {
		try? FileManager.default.removeItem(at: viewingDirectory)
	}

	/// A copy of the file to show. `progress` receives the verified and total bytes.
	static func viewingCopy(
		of item: IndexItem, environment: RemoteEnvironment, progress: (@Sendable (Int64, Int64) -> Void)? = nil
	) async throws(ContentError) -> URL {
		let store = environment.store
		var current = item
		// A newer version on the gateway replaces the one that was asked for, once
		for _ in 0..<2 {
			let binding: RemoteFolderBinding?
			let source: RemoteContentSource?
			do throws(IndexError) {
				binding = try store.remoteBinding(folderID: current.folderID)
				source = try store.contentSource(of: current, storage: environment.storage)
			}
			catch {
				throw .store(error)
			}
			guard let binding, let client = environment.client(serviceID: binding.serviceID) else {
				throw .notAvailable
			}
			guard let source else {
				throw .notListedYet
			}
			switch source {
			case .staged(let url):
				return try copy(url, name: current.name)
			case .remote(let state):
				if current.size == 0 {
					return try emptyFile(name: current.name)
				}
				guard let request = RemoteDownloader.Request(state) else {
					throw .notListedYet
				}
				let downloader = RemoteDownloader(
					store: store, client: client, storage: environment.storage, scheduler: environment.scheduler,
					credentials: environment.credentials, owner: environment.owner)
				let url: URL
				do throws(RemoteError) {
					url = try await downloader.download(request, progress: progress)
				}
				catch .notFound {
					throw .gone
				}
				catch .revisionMismatch(let newer) {
					guard let newer else {
						throw .notListedYet
					}
					current = try applied(newer, to: current, in: binding, known: state.revision, store: store)
					continue
				}
				catch {
					throw .remote(error)
				}
				// Copied first: the file just downloaded may be larger than the cache may hold
				let viewingCopy = try copy(url, name: current.name)
				environment.storage.trimContent(toBytes: CacheSettings.limit)
				return viewingCopy
			}
		}
		throw .notListedYet
	}

	/// Stores a newer version of the item the gateway answered with, and returns the item as it is now
	private static func applied(
		_ newer: RemoteItem, to item: IndexItem, in binding: RemoteFolderBinding, known: RemoteRevision, store: IndexStore
	) throws(ContentError) -> IndexItem {
		do throws(IndexError) {
			_ = try store.applyRemoteItems([newer], serviceID: binding.serviceID, knownRevisions: [newer.id: known])
			guard let updated = try store.item(id: item.id) else {
				throw .notFound
			}
			return updated
		}
		catch .notFound {
			throw .gone
		}
		catch {
			throw .store(error)
		}
	}

	private static func newDirectory() throws(ContentError) -> URL {
		let directory = viewingDirectory.appending(path: UUID().uuidString, directoryHint: .isDirectory)
		do {
			try FileManager.default.createDirectory(at: directory, withIntermediateDirectories: true)
		}
		catch {
			throw .remote(.fileSystem(error.localizedDescription))
		}
		return directory
	}

	private static func copy(_ source: URL, name: String) throws(ContentError) -> URL {
		let destination = try newDirectory().appending(path: name, directoryHint: .notDirectory)
		do {
			// FileManager clones where the volume allows it
			try FileManager.default.copyItem(at: source, to: destination)
			// Staged files are read-only
			try FileManager.default.setAttributes(
				[.posixPermissions: 0o644], ofItemAtPath: destination.path(percentEncoded: false))
		}
		catch {
			throw .remote(.fileSystem(error.localizedDescription))
		}
		return destination
	}

	private static func emptyFile(name: String) throws(ContentError) -> URL {
		let destination = try newDirectory().appending(path: name, directoryHint: .notDirectory)
		guard FileManager.default.createFile(atPath: destination.path(percentEncoded: false), contents: nil) else {
			throw .remote(.fileSystem("could not create the file"))
		}
		return destination
	}
}

/// How much space downloaded files may take
enum CacheSettings {
	static let storageKey = "cacheLimitBytes"
	static let defaultLimit = 5 << 30
	static let choices = [1 << 30, 2 << 30, 5 << 30, 10 << 30, 20 << 30]

	static var limit: Int64 {
		Int64(UserDefaults.standard.object(forKey: storageKey) as? Int ?? defaultLimit)
	}
}
