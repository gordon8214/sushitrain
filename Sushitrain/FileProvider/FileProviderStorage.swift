// Copyright (C) 2026 Gordon
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this file,
// You can obtain one at https://mozilla.org/MPL/2.0/.
#if os(iOS)
	@preconcurrency import FileProvider
	import Foundation
	import os
	@preconcurrency import SushitrainCore
	import SynctrainFileProviderKit

	/// Where the app keeps the data it shares with the File Provider extension
	enum FileProviderStorage {
		/// The shared app group container, or nil when the app group is not available (e.g. unsigned builds)
		static let container: SharedContainer? = SharedContainer.appGroup()

		/// Makes the core create internal folders in the shared container. Must be called before the client is created.
		static func configureManagedFolderBase() {
			guard let container else {
				Log.warn("App group container is not available; folders stay in the documents directory")
				return
			}
			do {
				try container.createDirectories()
				SushitrainSetManagedFolderBase(container.foldersURL.path(percentEncoded: false))
			}
			catch {
				Log.warn("Could not create shared directories: \(error.localizedDescription)")
			}
		}

		/// Where the Synctrain location is on disk (inside the system's File Provider storage), once known
		private static let locationPath = OSAllocatedUnfairLock<String?>(initialState: nil)

		static func setLocationURL(_ url: URL?) {
			let path = url.map(standardizedPath)
			locationPath.withLock { $0 = path }
		}

		/// Whether the URL lies inside the shared container or the Synctrain location. Folders there cannot be added as
		/// external folders: Syncthing would then synchronize its own files a second time.
		static func contains(_ url: URL) -> Bool {
			if let location = locationPath.withLock({ $0 }), isPath(standardizedPath(url), inside: location) {
				return true
			}
			return isInSharedContainer(url)
		}

		/// Whether the URL lies inside the shared container (where the app keeps internal folders)
		static func isInSharedContainer(_ url: URL) -> Bool {
			guard let container else {
				return false
			}
			return isPath(standardizedPath(url), inside: standardizedPath(container.baseURL))
		}

		/// Where a file in a shared folder appears in the Files app (inside the Synctrain location), if it does
		static func userVisibleURL(for localURL: URL) async -> URL? {
			guard let container else {
				return nil
			}
			let foldersPath = standardizedPath(container.foldersURL)
			let localPath = standardizedPath(localURL)
			guard isPath(localPath, inside: foldersPath) else {
				return nil
			}
			let components = localPath.dropFirst(foldersPath.count).split(separator: "/").map(String.init)
			guard let folderID = components.first else {
				return nil
			}
			return await userVisibleURL(folderID: folderID, path: components.dropFirst().joined(separator: "/"))
		}

		/// Where an item of the index appears in the Files app, if it does
		static func userVisibleURL(folderID: String, path: String) async -> URL? {
			guard let container, let manager = NSFileProviderManager(for: FileProviderCoordinator.domain) else {
				return nil
			}
			do {
				guard let item = try IndexStore(container: container).item(folderID: folderID, path: path) else {
					return nil
				}
				return try await manager.getUserVisibleURL(for: NSFileProviderItemIdentifier(ItemIdentifiers.string(for: item.id)))
			}
			catch {
				Log.warn("Could not find \(folderID):\(path) in the Files app: \(error.localizedDescription)")
				return nil
			}
		}

		/// The path of a URL in one form, for comparisons. Resolving symbolic links only removes the `/private` prefix of
		/// paths that exist, so it is removed here for all paths.
		private static func standardizedPath(_ url: URL) -> String {
			let path = url.resolvingSymlinksInPath().path(percentEncoded: false)
			for prefix in ["/private/var/", "/private/tmp/"] where path.hasPrefix(prefix) {
				return String(path.dropFirst("/private".count))
			}
			return path
		}

		private static func isPath(_ path: String, inside base: String) -> Bool {
			let base = base.hasSuffix("/") ? String(base.dropLast()) : base
			return path == base || path.hasPrefix(base + "/")
		}

		/// Moves internal folders from the documents directory into the shared container, so that the File Provider
		/// extension can serve their files. This is a rename on the same volume: nothing is copied or downloaded again.
		/// Call after the client has loaded and before it starts.
		nonisolated static func moveFoldersToSharedContainer(client: SushitrainClient) {
			guard !SushitrainManagedFolderBase().isEmpty else {
				return
			}
			for folderID in client.folders()?.asArray() ?? [] {
				guard let folder = client.folder(withID: folderID), folder.isNativeFilesystem(), !folder.isInManagedBase(),
					folder.isExternal == false
				else {
					continue
				}
				do {
					try folder.moveToManagedBase()
					Log.info("Moved folder \(folderID) to the shared container")
				}
				catch {
					Log.warn("Could not move folder \(folderID) to the shared container: \(error.localizedDescription)")
				}
			}
		}
	}
#endif
