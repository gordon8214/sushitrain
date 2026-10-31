// Copyright (C) 2026 Gordon
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this file,
// You can obtain one at https://mozilla.org/MPL/2.0/.
import Foundation
import SynctrainFileProviderKit

/// An edit made in a folder on a gateway that is being sent, or needs the user's attention, as the app lists it
struct RemoteOperationSummary: Identifiable, Hashable, Sendable {
	var id: String
	var folderID: String
	var folderLabel: String
	var kind: MutationKind
	var state: MutationState
	/// The item's name (the new name of a creation or move)
	var name: String
	var lastError: String?
	var updatedAt: Date
	/// The staged copy of the edited contents, when it is still on this device
	var stagedPath: String?

	/// States listed for review until resolved: being sent, rejected, or with an unknown outcome
	static let openStates: [MutationState] = [
		.staged, .uploading, .uploaded, .submitted, .pending, .failed, .outcomeUnknown,
	]

	/// How long edits kept as conflict copies stay listed (they are done; the list only tells about them)
	static let conflictRetention: TimeInterval = 7 * 86400
	/// How many edits kept as conflict copies are listed at most
	static let conflictLimit = 50

	/// Whether the user can give up the edit (it was not sent, was rejected, or its outcome is unknown)
	var canDiscard: Bool {
		state.isUnsent || state == .failed || state == .outcomeUnknown
	}

	/// Whether the edit's contents can be exported (they only exist on this device until the gateway confirms them)
	var canExport: Bool {
		stagedPath != nil && (kind == .create || kind == .replace || kind == .crossHostMove)
	}

	/// Whether the edit needs a decision of the user
	var needsAttention: Bool {
		state == .failed || state == .outcomeUnknown
	}

	var stateDescription: String {
		switch state {
		case .staged, .uploading, .uploaded:
			return String(localized: "Waiting to be sent")
		case .submitted, .pending:
			return String(localized: "Being saved on the gateway")
		case .committed:
			return String(localized: "Saved")
		case .conflict:
			return String(localized: "Kept as a conflict copy on the gateway")
		case .failed:
			return String(localized: "Rejected by the gateway")
		case .outcomeUnknown:
			return String(localized: "Outcome unknown: the gateway may have lost it")
		case .canceled:
			return String(localized: "Discarded")
		}
	}

	var kindDescription: String {
		switch kind {
		case .create: return String(localized: "New item")
		case .replace: return String(localized: "Edit")
		case .move, .crossHostMove: return String(localized: "Move or rename")
		case .delete: return String(localized: "Deletion")
		}
	}

	/// The summaries of the listed operations (of one folder, when given), newest first. Reads the index.
	static func load(store: IndexStore, folderID: String?) throws(IndexError) -> [RemoteOperationSummary] {
		let open = try store.mutations(folderID: folderID, states: openStates, limit: 200)
		// Oldest first: the recent ones are at the end
		let cutoff = Date().addingTimeInterval(-conflictRetention)
		let conflicts = try store.mutations(folderID: folderID, states: [.conflict], limit: 5000)
			.filter { $0.updatedAt >= cutoff }.suffix(conflictLimit)
		let labels = Dictionary(uniqueKeysWithValues: try store.folders().map { ($0.folderID, $0.label) })
		var summaries: [RemoteOperationSummary] = []
		for record in open + conflicts where record.parentOperationID == nil {
			summaries.append(
				RemoteOperationSummary(
					id: record.operationID, folderID: record.folderID,
					folderLabel: labels[record.folderID] ?? record.folderID, kind: record.kind, state: record.state,
					name: try name(of: record, store: store), lastError: record.lastError, updatedAt: record.updatedAt,
					stagedPath: record.stagedPath))
		}
		return summaries.sorted { $0.updatedAt > $1.updatedAt }
	}

	private static func name(of record: MutationRecord, store: IndexStore) throws(IndexError) -> String {
		if let name = record.targetName, !name.isEmpty {
			return name
		}
		if let itemID = record.itemID {
			if let item = try store.item(id: itemID) {
				return item.name
			}
			if let remote = try store.remoteItem(itemID: itemID) {
				return remote.name
			}
		}
		if let gatewayID = record.gatewayID, let remote = try store.remoteItem(gatewayID: gatewayID) {
			return remote.name
		}
		return String(localized: "Unnamed item")
	}
}
