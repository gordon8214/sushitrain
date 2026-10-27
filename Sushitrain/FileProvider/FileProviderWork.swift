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
		/// Since when devices have been able to connect (the app started or became active)
		let devicesSince: Date

		/// How long devices have to connect before stopping to keep an item is refused because no other device has it
		static let deselectGracePeriod: TimeInterval = 120

		struct Outcome: Sendable {
			/// Some work failed and should be tried again later
			var shouldRetry = false
			/// How many work items were completed
			var completed = 0
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
			// Selections and renames are never given up: without them, Syncthing ignores the new names. They fail while a
			// folder is busy (e.g. scanning) or short on space, which passes.
			for (_, items) in Dictionary(grouping: work.filter { $0.kind == .select }, by: \.folderID) {
				let succeeded = perform(items, &outcome) { folder in
					guard try folder.checkedIsSelective() else {
						return
					}
					try folder.setLocalPathsExplicitlySelected(SushitrainListOfStrings.from(items.map(\.path)))
				}
				failed = failed || !succeeded
			}

			for item in work where item.kind == .finishRename {
				let succeeded = perform([item], &outcome) { folder in try finishRename(item, in: folder) }
				failed = failed || !succeeded
			}

			for (folderID, items) in Dictionary(grouping: work.filter { $0.kind == .rescan }, by: \.folderID) {
				// Completed before scanning: a change made during the scan queues a new request instead of joining this one
				complete(items, &outcome)
				guard let folder = client.folder(withID: folderID) else {
					continue
				}
				let paths = Array(Set(items.map(\.path))).sorted()
				do {
					try folder.scanPaths(SushitrainListOfStrings.from(paths))
				}
				catch {
					Log.warn("Could not scan \(paths.count) paths in folder \(folderID): \(error.localizedDescription)")
					for path in paths {
						try? store.enqueue(.rescan, folderID: folderID, path: path)
					}
					failed = true
				}
			}

			// Asked for by the user, so a refusal is reported rather than retried forever. Only connected devices count
			// as having a copy, and the folder may be busy, so it is retried for a while after devices could connect.
			for item in work where item.kind == .deselectSafely {
				if let folder = client.folder(withID: item.folderID) {
					do {
						if try deselectSafely(item, in: folder) {
							outcome.deselected.append(item)
						}
					}
					catch let error where Date() < max(item.createdAt, devicesSince).addingTimeInterval(Self.deselectGracePeriod) {
						try? store.failWork(id: item.id, error: error.localizedDescription)
						failed = true
						continue
					}
					catch {
						outcome.refusals.append("\(FileNames.lastComponent(item.path)): \(error.localizedDescription)")
					}
				}
				complete([item], &outcome)
			}

			outcome.shouldRetry = failed
			return outcome
		}

		/// Runs the block for the folder the items belong to and records the outcome. Returns false on failure.
		private func perform(_ items: [WorkItem], _ outcome: inout Outcome, _ block: (SushitrainFolder) throws -> Void)
			-> Bool
		{
			guard let folderID = items.first?.folderID else {
				return true
			}
			guard let folder = client.folder(withID: folderID) else {
				// The folder was removed: there is nothing left to do
				complete(items, &outcome)
				return true
			}
			do {
				try block(folder)
				complete(items, &outcome)
				return true
			}
			catch {
				Log.warn(
					"File Provider work for folder \(folderID) failed (\(items.count) items): \(error.localizedDescription)"
				)
				for item in items {
					try? store.failWork(id: item.id, error: error.localizedDescription)
				}
				return false
			}
		}

		private func complete(_ items: [WorkItem], _ outcome: inout Outcome) {
			for item in items {
				try? store.completeWork(id: item.id)
			}
			outcome.completed += items.count
		}

		/// The extension renamed a file or directory in an on-demand folder by linking (or cloning) the new name. Select
		/// the new name, then remove the old one and have both scanned, so that Syncthing sends the removal (or
		/// recognizes the rename). The old name stays selected: Syncthing marks the entries of paths that become ignored as
		/// invalid, which other devices disregard, so deselecting it would keep the removal from reaching them. Every step
		/// can be repeated, so a failed attempt can simply be retried.
		private func finishRename(_ item: WorkItem, in folder: SushitrainFolder) throws {
			guard let newPath = item.extra else {
				return
			}
			try folder.setLocalPathsExplicitlySelected(SushitrainListOfStrings.from([newPath]))
			try removeOldName(
				container.fileURL(folderID: item.folderID, path: item.path),
				keeping: container.fileURL(folderID: item.folderID, path: newPath), renamedAt: item.createdAt)
			try folder.scanPaths(SushitrainListOfStrings.from([item.path, newPath]))
		}

		/// Removes the old name of a renamed item, keeping what changed under it since the rename (the work was queued
		/// right after the new name was linked or cloned). Syncthing replaces a file by renaming a new one into place, so
		/// a file at the old name created after the rename arrived from another device: it replaces its counterpart at
		/// the new name if that is unchanged, and is kept next to it as a conflict copy if both changed. Files that were
		/// there at the time of the rename are not moved over: the new name has them, or they were removed there since.
		private func removeOldName(_ old: URL, keeping new: URL, renamedAt: Date) throws {
			let oldPath = old.path(percentEncoded: false)
			let newPath = new.path(percentEncoded: false)
			guard let oldEntry = FileFingerprint(path: oldPath) else {
				return
			}
			guard let newEntry = FileFingerprint(path: newPath) else {
				// The new name was removed since; removing the old one too would remove the item on all devices
				Log.warn("Not removing the old name of a renamed item, as the new name no longer exists: \(oldPath)")
				return
			}
			if oldEntry.isDirectory {
				if let entries = FileManager.default.enumerator(atPath: oldPath) {
					while let relative = entries.nextObject() as? String {
						let target = newPath + "/" + relative
						guard !FileNames.isReservedPath(relative),
							let entry = FileFingerprint(path: oldPath + "/" + relative), Self.isNewer(entry, than: renamedAt)
						else {
							continue
						}
						if entry.isDirectory && FileFingerprint(path: target)?.isDirectory == true {
							// Both have it: compared entry by entry
							continue
						}
						try keep(oldPath + "/" + relative, entry, at: target, renamedAt: renamedAt)
						if entry.isDirectory {
							entries.skipDescendants()
						}
					}
				}
			}
			else if oldEntry.inode != newEntry.inode && Self.isNewer(oldEntry, than: renamedAt) {
				try keep(oldPath, oldEntry, at: newPath, renamedAt: renamedAt)
				return
			}
			try FileManager.default.removeItem(atPath: oldPath)
		}

		/// Moves something that arrived at the old name since the rename to the new name
		private func keep(_ source: String, _ entry: FileFingerprint, at target: String, renamedAt: Date) throws {
			let fileManager = FileManager.default
			guard let existing = FileFingerprint(path: target) else {
				try fileManager.createDirectory(atPath: FileNames.parentPath(target), withIntermediateDirectories: true)
				try fileManager.moveItem(atPath: source, toPath: target)
				return
			}
			if !Self.isNewer(existing, than: renamedAt) && !existing.isDirectory && !entry.isDirectory {
				// Unchanged at the new name: take the newer version
				guard rename(source, target) == 0 else {
					throw POSIXError(POSIXErrorCode(rawValue: errno) ?? .EIO)
				}
				return
			}
			let conflict = FileNames.join(
				FileNames.parentPath(target),
				FileNames.conflictCopyName(
					for: FileNames.lastComponent(target), date: Date(), shortDeviceID: client.shortDeviceID()))
			try fileManager.moveItem(atPath: source, toPath: conflict)
		}

		/// Whether the entry was created (or replaced) after the date
		private static func isNewer(_ entry: FileFingerprint, than date: Date) -> Bool {
			Double(entry.birthNanoseconds) / 1_000_000_000 > date.timeIntervalSince1970
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
