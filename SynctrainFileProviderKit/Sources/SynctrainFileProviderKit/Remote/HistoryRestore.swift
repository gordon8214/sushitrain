// Copyright (C) 2026 Gordon
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this file,
// You can obtain one at https://mozilla.org/MPL/2.0/.
import Foundation

public enum HistoryRestore {
	/// Only nonterminal mutations should be polled. A rejection must never be reported as a pending restore.
	public static func isCommitted(_ state: MutationState) throws -> Bool {
		switch state {
		case .committed: return true
		case .conflict, .failed: throw HistoryError.restoreRejected
		case .canceled: throw CancellationError()
		case .outcomeUnknown: throw HistoryError.restoreOutcomeUnknown
		default: return false
		}
	}

	public static func destinationExplanation(name: String, versioning: RemoteVersioning?) -> String {
		guard let versioning else {
			return String(localized: "Waiting for \(name) to confirm destination versioning before restoring.")
		}
		if versioning == .none {
			return String(
				localized:
					"Restore replaces the file on \(name) and synchronizes normally. Destination versioning is off, so its current contents will not be archived. Pending edits must finish first."
			)
		}
		return String(
			localized:
				"Restore replaces the file on \(name) and synchronizes normally. Destination versioning is enabled and archives its current contents. Pending edits must finish first."
		)
	}

	public static func recordDownloadedVersion(
		_ source: URL, version: RemoteVersion, itemID: Int64, binding: RemoteFolderBinding, expectedRevision: RemoteRevision,
		store: IndexStore, storage: RemoteStorage
	) async throws -> RemoteMutationResult {
		try storage.createDirectories()
		return try await record(
			itemID: itemID, binding: binding, expectedRevision: expectedRevision, modifiedNs: version.modifiedNs, store: store,
			storage: storage
		) {
			try await Task.detached { try RemoteUploader.stage(source, in: storage, modifiedNs: version.modifiedNs) }.value
		}
	}

	static func record(
		itemID: Int64, binding: RemoteFolderBinding, expectedRevision: RemoteRevision, modifiedNs: Int64, store: IndexStore,
		storage: RemoteStorage, stage: @Sendable () async throws -> StagedFile
	) async throws -> RemoteMutationResult {
		let staged = try await stage()
		do {
			// A detached disk copy can finish after its caller was canceled. It must never enqueue that restore.
			try Task.checkCancellation()
			guard try store.remoteBinding(folderID: binding.folderID) == binding else { throw HistoryError.destinationChanged }
			return try store.recordRemoteContentChange(
				itemID: itemID, modifiedNanoseconds: modifiedNs, stagedFile: staged, onConflict: .reject,
				expectedRevision: expectedRevision)
		}
		catch {
			try? FileManager.default.removeItem(at: storage.url(relativePath: staged.relativePath))
			throw error
		}
	}
}
