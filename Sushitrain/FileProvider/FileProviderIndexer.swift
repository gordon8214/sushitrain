// Copyright (C) 2026 Gordon
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this file,
// You can obtain one at https://mozilla.org/MPL/2.0/.
#if os(iOS)
	import Foundation
	import os
	import UIKit
	@preconcurrency import SushitrainCore
	import SynctrainFileProviderKit

	/// Keeps the shared index (that the File Provider extension serves from) in sync with the Syncthing global index of
	/// all exposed folders. Requests are coalesced; the work happens on a serial background queue.
	@MainActor final class FileProviderIndexer {
		private let worker: FileProviderIndexWorker
		private let onChanged: @MainActor () -> Void
		private var dirtyFolders: [String: Bool] = [:]
		private var foldersChanged = false
		private var isDraining = false

		init(client: SushitrainClient, store: IndexStore, onChanged: @escaping @MainActor () -> Void) {
			self.worker = FileProviderIndexWorker(client: client, store: store)
			self.onChanged = onChanged
		}

		/// Brings a folder up to date. A full pass also finds changes that do not show up as new sequence numbers.
		func schedule(folderID: String, fullPass: Bool) {
			dirtyFolders[folderID] = (dirtyFolders[folderID] ?? false) || fullPass
			drain()
		}

		/// Adds and removes folders (and updates labels), then brings all exposed folders up to date
		func scheduleFolderReconciliation(fullPass: Bool = false) {
			foldersChanged = true
			if fullPass {
				for folderID in worker.exposedFolderIDs() {
					dirtyFolders[folderID] = true
				}
			}
			drain()
		}

		private func drain() {
			guard !isDraining, !dirtyFolders.isEmpty || foldersChanged else {
				return
			}
			isDraining = true
			let folders = dirtyFolders
			let reconcile = foldersChanged
			dirtyFolders = [:]
			foldersChanged = false

			// Never leave a write transaction open when the app is suspended, or iOS terminates it (0xdead10cc)
			let worker = self.worker
			worker.isCancelled = false
			let backgroundTask = UIApplication.shared.beginBackgroundTask(withName: "Updating Files index") {
				worker.isCancelled = true
			}

			Task {
				let changed = await worker.run(folders: folders, reconcileFolders: reconcile)
				UIApplication.shared.endBackgroundTask(backgroundTask)
				isDraining = false
				if changed {
					onChanged()
				}
				drain()
			}
		}
	}

	/// Performs index passes. All methods except `run` are called on the worker's queue.
	final class FileProviderIndexWorker: @unchecked Sendable {
		private let client: SushitrainClient
		private let store: IndexStore
		private let queue = DispatchQueue(label: "nl.t-shaped.Sushitrain.FileProviderIndexer", qos: .utility)
		private let cancelled = OSAllocatedUnfairLock(initialState: false)
		private static let tombstoneRetention: TimeInterval = 30 * 86400
		private static let fullRecordBatchSize = 500

		var isCancelled: Bool {
			get { cancelled.withLock { $0 } }
			set { cancelled.withLock { $0 = newValue } }
		}

		init(client: SushitrainClient, store: IndexStore) {
			self.client = client
			self.store = store
		}

		/// Returns true when the index changed
		func run(folders: [String: Bool], reconcileFolders: Bool) async -> Bool {
			await withCheckedContinuation { continuation in
				queue.async {
					var changed = false
					if reconcileFolders {
						changed = self.reconcileFolderList() || changed
					}
					for (folderID, fullPass) in folders.sorted(by: { $0.key < $1.key }) where !self.isCancelled {
						changed = self.sync(folderID: folderID, fullPass: fullPass) || changed
					}
					continuation.resume(returning: changed)
				}
			}
		}

		func exposedFolderIDs() -> [String] {
			exposedFolders().map(\.folderID)
		}

		/// Folders that are shown in the Files app: internal folders with a native file system in the shared container
		private func exposedFolders() -> [FolderDescription] {
			(client.folders()?.asArray() ?? []).compactMap { folderID in
				guard let folder = client.folder(withID: folderID), folder.isInManagedBase(), folder.isRegularFolder,
					!folder.isPhotoFolder
				else {
					return nil
				}
				return FolderDescription(
					folderID: folderID, label: folder.displayName, folderType: folder.folderType(),
					isSelective: folder.isSelective() ?? false)
			}
		}

		private func reconcileFolderList() -> Bool {
			do {
				let exposed = exposedFolders()
				let exposedIDs = Set(exposed.map(\.folderID))
				let indexedIDs = Set(try store.folders().map(\.folderID))
				let shortID = client.shortDeviceID()

				try store.write { writer throws(IndexError) in
					if writer.localShortDeviceID != shortID {
						try writer.setLocalShortDeviceID(shortID)
					}
					for folderID in indexedIDs.subtracting(exposedIDs) {
						Log.info("Removing folder \(folderID) from the Files index")
						try writer.removeFolder(folderID: folderID)
					}
					for folder in exposed {
						try writer.upsertFolder(folder)
					}
					try writer.pruneTombstones(before: Date().addingTimeInterval(-Self.tombstoneRetention))
				}

				// Newly exposed folders still need to be populated
				var changed = indexedIDs != exposedIDs
				for folderID in exposedIDs.subtracting(indexedIDs) where !isCancelled {
					changed = sync(folderID: folderID, fullPass: true) || changed
				}
				return changed
			}
			catch {
				Log.warn("Could not reconcile folders in the Files index: \(error)")
				return false
			}
		}

		private func sync(folderID: String, fullPass: Bool) -> Bool {
			guard let folder = client.folder(withID: folderID),
				let description = exposedFolders().first(where: { $0.folderID == folderID })
			else {
				// The folder is no longer exposed
				return (try? removeIfIndexed(folderID: folderID)) ?? false
			}

			do {
				let cursor = try store.write { writer throws(IndexError) in
					try writer.upsertFolder(description)
					return try writer.cursor(folderID: folderID)
				}

				if cursor == 0 {
					return try populate(folder: folder)
				}
				if fullPass {
					return try sweep(folder: folder)
				}

				let applier = FileProviderRecordApplier(store: store, folderID: folderID, sweepToken: nil)
				let result = try folder.globalChanges(cursor, delegate: applier)
				if result.sequenceReset {
					Log.info("Syncthing index of folder \(folderID) was rebuilt, performing a full pass")
					return try sweep(folder: folder)
				}
				try store.write { writer throws(IndexError) in
					try writer.setCursor(result.highestSequence, folderID: folderID)
				}
				return applier.changed > 0
			}
			catch {
				Log.warn("Could not update the Files index for folder \(folderID): \(error)")
				return false
			}
		}

		private func removeIfIndexed(folderID: String) throws -> Bool {
			guard try store.folder(id: folderID) != nil else {
				return false
			}
			try store.write { writer throws(IndexError) in try writer.removeFolder(folderID: folderID) }
			return true
		}

		/// The first pass for a folder: full records for every entry
		private func populate(folder: SushitrainFolder) throws -> Bool {
			let folderID = folder.folderID
			Log.info("Populating the Files index for folder \(folderID)")
			let token = try store.write { writer throws(IndexError) in try writer.beginSweep(folderID: folderID) }
			let applier = FileProviderRecordApplier(store: store, folderID: folderID, sweepToken: token)
			let result = try folder.globalChanges(0, delegate: applier)
			try store.write { writer throws(IndexError) in
				try writer.finishSweep(folderID: folderID, token: token)
				try writer.setCursor(result.highestSequence, folderID: folderID)
			}
			Log.info("Populated the Files index for folder \(folderID) with \(result.count) entries")
			return true
		}

		/// Compares every entry with the index, refreshing changed entries and removing ones that no longer exist
		private func sweep(folder: SushitrainFolder) throws -> Bool {
			let folderID = folder.folderID
			let token = try store.write { writer throws(IndexError) in try writer.beginSweep(folderID: folderID) }
			let applier = FileProviderRecordApplier(store: store, folderID: folderID, sweepToken: token)
			let result = try folder.globalSnapshot(applier)

			// Entries that changed without a new sequence number we know of need their full information
			let needsFull = applier.needsFullRecords
			for start in stride(from: 0, to: needsFull.count, by: Self.fullRecordBatchSize) where !isCancelled {
				let paths = Array(needsFull[start..<min(start + Self.fullRecordBatchSize, needsFull.count)])
				let json = try folder.globalRecordsJSON(try JSONEncoder().encode(paths))
				var records = try SyncthingRecord.decodeBatch(json)
				for i in records.indices {
					records[i].seq = applier.sequences[records[i].path] ?? 0
				}
				try applier.apply(records)
			}
			if isCancelled {
				return applier.changed > 0
			}

			try store.write { writer throws(IndexError) in
				try writer.finishSweep(folderID: folderID, token: token)
				try writer.setCursor(result.highestSequence, folderID: folderID)
			}
			return true
		}
	}

	/// Receives batches of records from the core and applies them to the index
	final class FileProviderRecordApplier: NSObject, SushitrainFileProviderRecordDelegateProtocol, @unchecked Sendable {
		private let store: IndexStore
		private let folderID: String
		private let sweepToken: Int64?
		private(set) var changed = 0
		private(set) var needsFullRecords: [String] = []
		/// Sequences of partial records, which full records (that lack them) inherit
		private(set) var sequences: [String: Int64] = [:]

		init(store: IndexStore, folderID: String, sweepToken: Int64?) {
			self.store = store
			self.folderID = folderID
			self.sweepToken = sweepToken
		}

		func onRecords(_ batchJSON: Data?) throws {
			guard let batchJSON else {
				return
			}
			let records = try SyncthingRecord.decodeBatch(batchJSON)
			for record in records where !record.full {
				sequences[record.path] = record.seq
			}
			try apply(records)
		}

		func apply(_ records: [SyncthingRecord]) throws {
			let folderID = self.folderID
			let sweepToken = self.sweepToken
			let result = try store.write { writer throws(IndexError) in
				try writer.apply(records, folderID: folderID, sweepToken: sweepToken)
			}
			changed += result.changed
			needsFullRecords += result.needsFullRecords
			if result.collisions > 0 {
				Log.warn("Skipped \(result.collisions) entries in folder \(folderID) whose names collide with others")
			}
		}
	}
#endif
