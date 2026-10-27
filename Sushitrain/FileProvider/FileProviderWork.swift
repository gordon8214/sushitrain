// Copyright (C) 2026 Gordon
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this file,
// You can obtain one at https://mozilla.org/MPL/2.0/.
#if os(iOS)
	import Foundation
	@preconcurrency import SushitrainCore
	import SynctrainFileProviderKit

	/// Completes changes made through the Files app that only the app can complete: selecting new files in on-demand
	/// folders, finishing renames there, and having Syncthing scan the changed paths. The calls block until Syncthing is
	/// done, so this runs off the main actor.
	struct FileProviderWorkRunner: Sendable {
		let client: SushitrainClient
		let store: IndexStore
		let container: SharedContainer

		/// How often work is tried before it is given up (a folder that never becomes idle should not block the queue)
		static let maximumAttempts = 20

		/// How often to check whether another device has an item before refusing to stop keeping it (with work retried
		/// every 15 seconds, devices have about two minutes to connect)
		static let deselectAttempts = 8

		struct Outcome: Sendable {
			/// Some work failed and should be tried again later
			var shouldRetry = false
			/// Items that are no longer kept on this device (the system's copy of them can be removed as well)
			var deselected: [WorkItem] = []
			/// Why items the user wanted to stop keeping on this device were kept
			var refusals: [String] = []
		}

		/// Performs the work (other than fetches)
		func run(_ work: [WorkItem]) -> Outcome {
			var outcome = Outcome()
			var failed = false

			// Paths are selected before they are scanned: Syncthing skips what it ignores
			for (_, items) in Dictionary(grouping: work.filter { $0.kind == .select }, by: \.folderID) {
				let succeeded = perform(items) { folder in
					guard try folder.checkedIsSelective() else {
						return
					}
					try folder.setLocalPathsExplicitlySelected(SushitrainListOfStrings.from(items.map(\.path)))
				}
				failed = failed || !succeeded
			}

			for item in work where item.kind == .finishRename {
				let succeeded = perform([item]) { folder in try finishRename(item, in: folder) }
				failed = failed || !succeeded
			}

			for (_, items) in Dictionary(grouping: work.filter { $0.kind == .rescan }, by: \.folderID) {
				let paths = Array(Set(items.map(\.path))).sorted()
				let succeeded = perform(items) { folder in
					try folder.scanPaths(SushitrainListOfStrings.from(paths))
				}
				failed = failed || !succeeded
			}

			// Asked for by the user, so a refusal is reported rather than retried. Only connected devices count as
			// having a copy, so right after the app starts that is retried for a while before it counts as a refusal.
			for item in work where item.kind == .deselectSafely {
				if let folder = client.folder(withID: item.folderID) {
					do {
						if try deselectSafely(item, in: folder) {
							outcome.deselected.append(item)
						}
					}
					catch FileProviderWorkError.onlyCopy where item.attempts + 1 < Self.deselectAttempts {
						try? store.failWork(id: item.id, error: FileProviderWorkError.onlyCopy.localizedDescription)
						failed = true
						continue
					}
					catch {
						outcome.refusals.append("\(FileNames.lastComponent(item.path)): \(error.localizedDescription)")
					}
				}
				complete([item])
			}

			outcome.shouldRetry = failed
			return outcome
		}

		/// Runs the block for the folder the items belong to and records the outcome. Returns false on failure.
		private func perform(_ items: [WorkItem], _ block: (SushitrainFolder) throws -> Void) -> Bool {
			guard let folderID = items.first?.folderID else {
				return true
			}
			guard let folder = client.folder(withID: folderID) else {
				// The folder was removed: there is nothing left to do
				complete(items)
				return true
			}
			do {
				try block(folder)
				complete(items)
				return true
			}
			catch {
				Log.warn(
					"File Provider work for folder \(folderID) failed (\(items.count) items): \(error.localizedDescription)"
				)
				for item in items {
					if item.attempts + 1 >= Self.maximumAttempts {
						try? store.completeWork(id: item.id)
					}
					else {
						try? store.failWork(id: item.id, error: error.localizedDescription)
					}
				}
				return false
			}
		}

		private func complete(_ items: [WorkItem]) {
			for item in items {
				try? store.completeWork(id: item.id)
			}
		}

		/// The extension renamed a file in an on-demand folder by linking the new name. Select the new name, then remove
		/// the old one and have both scanned, so that Syncthing sends the removal (or recognizes the rename). The old name
		/// stays selected: Syncthing marks the entries of paths that become ignored as invalid, which other devices
		/// disregard, so deselecting it would keep the removal from reaching them. Every step can be repeated, so a failed
		/// attempt can simply be retried.
		private func finishRename(_ item: WorkItem, in folder: SushitrainFolder) throws {
			guard let newPath = item.extra else {
				return
			}
			try folder.setLocalPathsExplicitlySelected(SushitrainListOfStrings.from([newPath]))
			let oldPath = container.fileURL(folderID: item.folderID, path: item.path).path(percentEncoded: false)
			if unlink(oldPath) != 0 && errno != ENOENT {
				throw POSIXError(POSIXErrorCode(rawValue: errno) ?? .EIO)
			}
			try folder.scanPaths(SushitrainListOfStrings.from([item.path, newPath]))
		}

		/// Stops keeping a path on this device, but only when another device has the whole of its current version.
		/// Returns whether it was deselected.
		private func deselectSafely(_ item: WorkItem, in folder: SushitrainFolder) throws -> Bool {
			guard try folder.checkedIsSelective() else {
				return false
			}
			// Changes made on this device must be known before checking that other devices have this version
			try folder.scanPaths(SushitrainListOfStrings.from([item.path]))
			let entry = try folder.getFileInformation(item.path)
			if entry.isDeleted() {
				return false
			}
			guard try entry.peersWithFullCopy().count() > 0 else {
				throw FileProviderWorkError.onlyCopy
			}
			try entry.setExplicitlySelected(false)
			return true
		}
	}

	enum FileProviderWorkError: LocalizedError {
		case onlyCopy

		var errorDescription: String? {
			switch self {
			case .onlyCopy:
				return String(localized: "No other connected device has all of it, so it is kept on this device.")
			}
		}
	}
#endif
