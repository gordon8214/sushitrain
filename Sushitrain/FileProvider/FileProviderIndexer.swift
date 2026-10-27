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
		private var fullPassRequested = false
		private var isDraining = false
		private static let retryInterval: TimeInterval = 30

		/// Called when a full pass over all folders has completed
		var onFullPassCompleted: (@MainActor () -> Void)? = nil

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
				fullPassRequested = true
				for folderID in worker.exposedFolderIDs() {
					dirtyFolders[folderID] = true
				}
			}
			drain()
		}

		/// Empties the index (after the pass in progress, if any) and fills it again
		func reset() async throws {
			try await worker.reset()
			scheduleFolderReconciliation(fullPass: true)
		}

		private func drain() {
			guard !isDraining, !dirtyFolders.isEmpty || foldersChanged else {
				return
			}
			isDraining = true
			let folders = dirtyFolders
			let reconcile = foldersChanged
			let fullPass = fullPassRequested
			dirtyFolders = [:]
			foldersChanged = false
			fullPassRequested = false

			// Never leave a write transaction open when the app is suspended, or iOS terminates it (0xdead10cc): when
			// the time is up, the pass stops after the current batch
			let worker = self.worker
			worker.isCancelled = false
			let activity = BackgroundActivity(name: "Updating Files index") {
				worker.isCancelled = true
			}

			Task {
				let outcome = await worker.run(folders: folders, reconcileFolders: reconcile)
				activity.end()
				isDraining = false
				if outcome.changed {
					onChanged()
				}
				let isComplete = outcome.unfinished.isEmpty && outcome.reconciled
				if fullPass && isComplete {
					onFullPassCompleted?()
				}
				drain()
				if !isComplete {
					scheduleRetry(outcome.unfinished, reconcile: !outcome.reconciled)
				}
			}
		}

		/// Passes that failed or were cut short are tried again after a while
		private func scheduleRetry(_ folders: [String: Bool], reconcile: Bool) {
			Task {
				try? await Task.sleep(for: .seconds(Self.retryInterval))
				for (folderID, fullPass) in folders {
					dirtyFolders[folderID] = (dirtyFolders[folderID] ?? false) || fullPass
				}
				foldersChanged = foldersChanged || reconcile
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

		struct Outcome: Sendable {
			/// Whether the index changed
			var changed = false
			/// Whether the folder list was brought up to date (if asked)
			var reconciled = true
			/// Folders whose pass failed or was cut short (with whether it was a full pass)
			var unfinished: [String: Bool] = [:]
		}

		func run(folders: [String: Bool], reconcileFolders: Bool) async -> Outcome {
			await withCheckedContinuation { continuation in
				queue.async {
					var outcome = Outcome()
					var folders = folders
					if reconcileFolders {
						do {
							let (changed, added) = try self.reconcileFolderList()
							outcome.changed = changed
							// Newly exposed folders still need to be populated
							for folderID in added {
								folders[folderID] = true
							}
						}
						catch {
							Log.warn("Could not reconcile folders in the Files index: \(error)")
							outcome.reconciled = false
						}
					}
					for (folderID, fullPass) in folders.sorted(by: { $0.key < $1.key }) {
						if self.isCancelled {
							outcome.unfinished[folderID] = fullPass
							continue
						}
						do {
							outcome.changed = try self.sync(folderID: folderID, fullPass: fullPass) || outcome.changed
						}
						catch {
							if !self.isCancelled {
								Log.warn("Could not update the Files index for folder \(folderID): \(error)")
							}
							outcome.unfinished[folderID] = fullPass
						}
					}
					continuation.resume(returning: outcome)
				}
			}
		}

		/// Empties the index, after the pass in progress
		func reset() async throws {
			try await withCheckedThrowingContinuation { (continuation: CheckedContinuation<Void, any Error>) in
				queue.async {
					do {
						try self.store.reset()
						continuation.resume()
					}
					catch {
						continuation.resume(throwing: error)
					}
				}
			}
		}

		func exposedFolderIDs() -> [String] {
			exposedFolders().map(\.folderID)
		}

		/// Folders that are shown in the Files app: internal folders with a native file system in the shared container,
		/// unless they are hidden (the folder setting that hides them in the Files app)
		private func exposedFolders() -> [FolderDescription] {
			(client.folders()?.asArray() ?? []).compactMap { folderID in
				guard let folder = client.folder(withID: folderID), folder.isInManagedBase(), folder.isRegularFolder,
					!folder.isPhotoFolder, folder.isHidden != true
				else {
					return nil
				}
				return FolderDescription(
					folderID: folderID, label: folder.displayName, folderType: folder.folderType(),
					isSelective: folder.isSelective() ?? false)
			}
		}

		/// Adds and removes folders. Returns whether the index changed, and the folders that were added (which still need
		/// to be populated).
		private func reconcileFolderList() throws -> (Bool, Set<String>) {
			let exposed = exposedFolders()
			let exposedIDs = Set(exposed.map(\.folderID))
			let indexed = Dictionary(uniqueKeysWithValues: try store.folders().map { ($0.folderID, $0) })
			let indexedIDs = Set(indexed.keys)
			let shortID = client.shortDeviceID()
			// Renamed folders, and ones whose type or mode changed (which changes what can be done with their items)
			let changedFolders = exposed.contains { folder in
				indexed[folder.folderID].map {
					$0.label != folder.label || $0.folderType != folder.folderType || $0.isSelective != folder.isSelective
				} ?? false
			}

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
			return (indexedIDs != exposedIDs || changedFolders, exposedIDs.subtracting(indexedIDs))
		}

		private func sync(folderID: String, fullPass: Bool) throws -> Bool {
			guard let folder = client.folder(withID: folderID),
				let description = exposedFolders().first(where: { $0.folderID == folderID })
			else {
				// The folder is no longer exposed
				return try removeIfIndexed(folderID: folderID)
			}

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

			let applier = makeApplier(folderID: folderID, sweepToken: nil)
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

		private func makeApplier(folderID: String, sweepToken: Int64?) -> FileProviderRecordApplier {
			FileProviderRecordApplier(store: store, folderID: folderID, sweepToken: sweepToken) { [weak self] in
				self?.isCancelled ?? true
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
			let applier = makeApplier(folderID: folderID, sweepToken: token)
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
			let applier = makeApplier(folderID: folderID, sweepToken: token)
			let result = try folder.globalSnapshot(applier)

			// Entries that changed without a new sequence number we know of need their full information
			let needsFull = applier.needsFullRecords
			for start in stride(from: 0, to: needsFull.count, by: Self.fullRecordBatchSize) {
				if isCancelled {
					throw CancellationError()
				}
				let paths = Array(needsFull[start..<min(start + Self.fullRecordBatchSize, needsFull.count)])
				let json = try folder.globalRecordsJSON(try JSONEncoder().encode(paths))
				var records = try SyncthingRecord.decodeBatch(json)
				for i in records.indices {
					records[i].seq = applier.sequences[records[i].path] ?? 0
				}
				try applier.apply(records)
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
		private let isCancelled: @Sendable () -> Bool
		private(set) var changed = 0
		private(set) var needsFullRecords: [String] = []
		/// Sequences of the partial records that need full records (which lack them and inherit these)
		private(set) var sequences: [String: Int64] = [:]

		init(store: IndexStore, folderID: String, sweepToken: Int64?, isCancelled: @escaping @Sendable () -> Bool) {
			self.store = store
			self.folderID = folderID
			self.sweepToken = sweepToken
			self.isCancelled = isCancelled
		}

		/// Throwing stops the pass in the core
		func onRecords(_ batchJSON: Data?) throws {
			if isCancelled() {
				throw CancellationError()
			}
			guard let batchJSON else {
				return
			}
			let records = try SyncthingRecord.decodeBatch(batchJSON)
			let needed = try apply(records)
			if !needed.isEmpty {
				let partial = Dictionary(
					records.lazy.filter { !$0.full }.map { ($0.path, $0.seq) }, uniquingKeysWith: { _, last in last })
				for path in needed {
					sequences[path] = partial[path]
				}
			}
		}

		/// Applies the records; returns the paths that need full records
		@discardableResult func apply(_ records: [SyncthingRecord]) throws -> [String] {
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
			return result.needsFullRecords
		}
	}
#endif
