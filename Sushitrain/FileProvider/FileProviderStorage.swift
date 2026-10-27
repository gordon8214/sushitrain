// Copyright (C) 2026 Gordon
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this file,
// You can obtain one at https://mozilla.org/MPL/2.0/.
#if os(iOS)
	@preconcurrency import FileProvider
	import Foundation
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

		/// Whether the URL lies inside the shared container (where folders cannot be added as external folders)
		static func contains(_ url: URL) -> Bool {
			guard let container else {
				return false
			}
			return url.resolvingSymlinksInPath().path(percentEncoded: false)
				.hasPrefix(container.baseURL.resolvingSymlinksInPath().path(percentEncoded: false))
		}

		/// Where a file in a shared folder appears in the Files app (inside the Synctrain location), if it does
		static func userVisibleURL(for localURL: URL) async -> URL? {
			guard let container, let manager = NSFileProviderManager(for: FileProviderCoordinator.domain) else {
				return nil
			}
			let foldersPath = container.foldersURL.resolvingSymlinksInPath().path(percentEncoded: false)
			let localPath = localURL.resolvingSymlinksInPath().path(percentEncoded: false)
			guard localPath.hasPrefix(foldersPath) else {
				return nil
			}
			let components = localPath.dropFirst(foldersPath.count).split(separator: "/").map(String.init)
			guard let folderID = components.first else {
				return nil
			}
			let path = components.dropFirst().joined(separator: "/")
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
