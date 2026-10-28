// Copyright (C) 2026 Gordon
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this file,
// You can obtain one at https://mozilla.org/MPL/2.0/.
import Foundation

/// A local change of an item that the gateway has not confirmed. The item's row shows the change; refreshes update
/// the remote metadata only, and the row follows it once the change is resolved.
public struct RemoteOverlay: Sendable, Hashable {
	public var itemID: Int64
	public var folderID: String
	public var pending: PendingChange
	/// The latest operation that carries the change
	public var operationID: String
	/// The staged file with the item's new contents, relative to `RemoteStorage.baseURL`
	public var stagedPath: String?
	/// Who made the row, when the change created it (nil for rows that existed before the change)
	public var origin: RemoteChangeOrigin?
}

/// What `prepareSubmission` found
public enum SubmissionPreparation: Sendable {
	/// Send this request. `body` is its stored encoding, sent unchanged on every attempt.
	case ready(claim: OutboxClaim, request: OperationRequest, body: Data)
	/// The request could not be built (its item or destination is not on the gateway); the operation failed
	case failed(String)
	/// The claim was lost
	case lost
}

/// The outcome of recording a local change of a folder accessed through a gateway
public struct RemoteMutationResult: Sendable {
	/// The item as it is now shown (nil when it was removed)
	public var item: IndexItem?
	/// The operation that carries the change (nil when the change only canceled an unsent creation)
	public var mutation: MutationRecord?
	/// Staged files nothing needs anymore (an unsent edit replaced by a newer one). Remove them from disk, then call
	/// `forgetStagedFile(path:)`.
	public var releasedStagedFiles: [String] = []
}

extension IndexStore {
	static let mutationColumns = """
		operation_id, folder_id, service_id, item_id, gateway_id, kind, payload, base_revision, subtree_generation, \
		target_parent_id, target_name, target_folder_id, item_kind, modified_ns, staged_path, upload_digest, \
		upload_length, upload_id, upload_offset, upload_token_ref, upload_expires, server_operation_id, state, \
		claim_owner, claim_expires, generation, attempts, not_before, last_error, authority_generation, \
		committed_content_id, result, depends_on, parent_operation_id, step, created_at, updated_at, on_conflict, \
		template_identifier
		"""

	static func mutation(from row: SQLiteStatement) -> MutationRecord {
		let decoder = RemoteJSON.decoder()
		func date(_ column: Int32) -> Date? {
			row.isNull(column) ? nil : Date(timeIntervalSince1970: row.double(column))
		}
		return MutationRecord(
			operationID: row.text(0), folderID: row.text(1), serviceID: row.optionalText(2),
			itemID: row.optionalInt64(3), gatewayID: row.optionalText(4),
			kind: MutationKind(rawValue: row.text(5)) ?? .create,
			payload: row.optionalText(6).flatMap { try? decoder.decode(OperationRequest.self, from: Data($0.utf8)) },
			baseRevision: row.optionalText(7).map { RemoteRevision($0) }, subtreeGeneration: row.optionalInt64(8),
			targetParentItemID: row.optionalInt64(9), targetName: row.optionalText(10),
			targetFolderID: row.optionalText(11),
			itemKind: row.optionalInt64(12).flatMap { ItemKind(rawValue: Int($0)) }, modifiedNs: row.optionalInt64(13),
			conflictPolicy: row.optionalText(37).map { RemoteConflictPolicy(rawValue: $0) },
			templateIdentifier: row.optionalText(38), stagedPath: row.optionalText(14), uploadDigest: row.optionalText(15),
			uploadLength: row.optionalInt64(16),
			uploadID: row.optionalText(17), uploadOffset: row.int64(18), uploadTokenReference: row.optionalText(19),
			uploadExpires: date(20), serverOperationID: row.optionalText(21),
			state: MutationState(rawValue: row.text(22)) ?? .failed, claimOwner: row.optionalText(23),
			claimExpires: date(24), generation: row.int64(25), attempts: Int(row.int64(26)), notBefore: date(27),
			lastError: row.optionalText(28), authorityGeneration: row.optionalInt64(29),
			committedContentID: row.optionalText(30).map { ContentIdentity($0) },
			result: row.optionalText(31).flatMap { try? decoder.decode(RemoteOperation.self, from: Data($0.utf8)) },
			dependencies: (row.optionalText(32) ?? "").split(separator: " ").map(String.init),
			parentOperationID: row.optionalText(33), step: row.optionalInt64(34).map(Int.init),
			createdAt: Date(timeIntervalSince1970: row.double(35)), updatedAt: Date(timeIntervalSince1970: row.double(36)))
	}

	static func mutation(_ connection: SQLiteConnection, operationID: String) throws(IndexError) -> MutationRecord? {
		try connection.queryOne(
			"SELECT \(mutationColumns) FROM remote_outbox WHERE operation_id = ?", [operationID], mutation)
	}

	/// States in which an operation still holds its item's overlay: not done, done without a known outcome, or a
	/// rejected change of contents (which stays visible, so that it can be exported)
	static let unresolvedCondition = """
		(state IN ('staged', 'uploading', 'uploaded', 'submitted', 'pending', 'outcomeUnknown') \
		OR (state = 'failed' AND kind IN ('create', 'replace')))
		"""

	// MARK: - Reading

	public func mutation(operationID: String) throws(IndexError) -> MutationRecord? {
		try withConnection { connection throws(IndexError) in
			try Self.mutation(connection, operationID: operationID)
		}
	}

	/// Outbox operations, oldest first (of the folder and in the states, when given)
	public func mutations(folderID: String? = nil, states: [MutationState]? = nil, limit: Int = 100)
		throws(IndexError) -> [MutationRecord]
	{
		var conditions: [String] = []
		var values: [any SQLBindable] = []
		if let folderID {
			conditions.append("folder_id = ?")
			values.append(folderID)
		}
		if let states {
			if states.isEmpty {
				return []
			}
			conditions.append("state IN (\(Array(repeating: "?", count: states.count).joined(separator: ", ")))")
			values += states.map(\.rawValue)
		}
		let filter = conditions.isEmpty ? "" : "WHERE " + conditions.joined(separator: " AND ")
		return try withConnection { connection throws(IndexError) in
			try connection.query(
				"SELECT \(Self.mutationColumns) FROM remote_outbox \(filter) ORDER BY created_at, operation_id LIMIT ?",
				values + [limit], Self.mutation)
		}
	}

	/// The steps of a compound operation (a cross-host move), in order
	public func mutationSteps(of operationID: String) throws(IndexError) -> [MutationRecord] {
		try withConnection { connection throws(IndexError) in
			try connection.query(
				"""
				SELECT \(Self.mutationColumns) FROM remote_outbox WHERE parent_operation_id = ?
				ORDER BY step, created_at, operation_id
				""", [operationID], Self.mutation)
		}
	}

	/// Whether operations of the folder (of any folder, without one) are not resolved yet
	public func hasUnresolvedMutations(folderID: String? = nil) throws(IndexError) -> Bool {
		try withConnection { connection throws(IndexError) in
			if let folderID {
				return try connection.queryOne(
					"SELECT EXISTS (SELECT 1 FROM remote_outbox WHERE folder_id = ? AND \(Self.unresolvedCondition))",
					[folderID], { $0.bool(0) }) ?? false
			}
			return try connection.queryOne(
				"SELECT EXISTS (SELECT 1 FROM remote_outbox WHERE \(Self.unresolvedCondition))", [], { $0.bool(0) })
				?? false
		}
	}

	public func remoteOverlay(itemID: Int64) throws(IndexError) -> RemoteOverlay? {
		try withConnection { connection throws(IndexError) in try Self.overlay(connection, itemID: itemID) }
	}

	static func overlay(_ connection: SQLiteConnection, itemID: Int64) throws(IndexError) -> RemoteOverlay? {
		try connection.queryOne(
			"SELECT item_id, folder_id, kind, operation_id, staged_path, origin FROM remote_overlays WHERE item_id = ?",
			[itemID]
		) { row in
			RemoteOverlay(
				itemID: row.int64(0), folderID: row.text(1),
				pending: PendingChange(rawValue: Int(row.int64(2))) ?? .modify, operationID: row.text(3),
				stagedPath: row.optionalText(4), origin: row.optionalText(5).flatMap(RemoteChangeOrigin.init(rawValue:)))
		}
	}

	/// The latest creation recorded for the system's pending item with the template identifier, while its row is live
	/// (also once committed: the system may ask again when its earlier call did not complete)
	public func remoteCreation(templateIdentifier: String) throws(IndexError) -> MutationRecord? {
		try withConnection { connection throws(IndexError) in
			try connection.queryOne(
				"""
				SELECT \(Self.mutationColumns) FROM remote_outbox WHERE operation_id = (
					SELECT o.operation_id FROM remote_outbox AS o JOIN items AS i ON i.id = o.item_id AND i.deleted = 0
					WHERE o.template_identifier = ? AND o.kind = 'create' AND o.parent_operation_id IS NULL
					ORDER BY o.created_at DESC, o.operation_id DESC LIMIT 1)
				""", [templateIdentifier], Self.mutation)
		}
	}

	// MARK: - Recording local changes

	/// Records an item created in a folder accessed through a gateway: a new row (pending creation) with an overlay, and
	/// a create operation in the outbox. Files need their staged contents. Throws `.nameCollision` when the container
	/// has an item whose name cannot coexist with the new one.
	///
	/// `origin` says who made the item: rows the system made are not enumerated while their creation is pending (the
	/// system shows its own item), the others are. `templateIdentifier` is the identifier the system gave its pending
	/// item, by which `remoteCreation(templateIdentifier:)` finds the creation again.
	public func recordRemoteCreation(
		folderID: String, parentItemID: Int64, name: String, kind: ItemKind, modifiedNanoseconds: Int64,
		stagedFile: StagedFile?, origin: RemoteChangeOrigin = .system, templateIdentifier: String? = nil
	) throws(IndexError) -> RemoteMutationResult {
		try transaction { connection throws(IndexError) in
			let recorder = try RemoteMutationRecorder(connection: connection)
			let result = try recorder.recordCreation(
				folderID: folderID, parentItemID: parentItemID, name: name, kind: kind,
				modifiedNanoseconds: modifiedNanoseconds, stagedFile: stagedFile, origin: origin,
				templateIdentifier: templateIdentifier)
			try recorder.finish()
			return result
		}
	}

	/// Records new contents of a file. An unsent change of the item is replaced (its staged file is released);
	/// otherwise a replace operation is queued, based on the revision the edit started from. `onConflict` says what
	/// the gateway does when the file changed since that revision: keep the edit as a conflict copy, or reject it.
	public func recordRemoteContentChange(
		itemID: Int64, modifiedNanoseconds: Int64, stagedFile: StagedFile, onConflict: RemoteConflictPolicy = .keepBoth
	) throws(IndexError) -> RemoteMutationResult {
		try transaction { connection throws(IndexError) in
			let recorder = try RemoteMutationRecorder(connection: connection)
			let result = try recorder.recordContentChange(
				itemID: itemID, modifiedNanoseconds: modifiedNanoseconds, stagedFile: stagedFile, onConflict: onConflict)
			try recorder.finish()
			return result
		}
	}

	/// Records a rename or move within the gateway (to another folder of the same gateway too). The item keeps its
	/// identifier. Throws `.nameCollision` when the destination has an item whose name cannot coexist with it.
	public func recordRemoteMove(itemID: Int64, toParentItemID parentItemID: Int64, name: String)
		throws(IndexError) -> RemoteMutationResult
	{
		try transaction { connection throws(IndexError) in
			let recorder = try RemoteMutationRecorder(connection: connection)
			let result = try recorder.recordMove(itemID: itemID, toParentItemID: parentItemID, name: name)
			try recorder.finish()
			return result
		}
	}

	/// Records the deletion of an item (and everything below it). Deleting an item whose creation was not sent yet
	/// cancels the creation.
	public func recordRemoteDeletion(itemID: Int64) throws(IndexError) -> RemoteMutationResult {
		try transaction { connection throws(IndexError) in
			let recorder = try RemoteMutationRecorder(connection: connection)
			let result = try recorder.recordDeletion(itemID: itemID)
			try recorder.finish()
			return result
		}
	}

	/// Records moving a file to a folder on another gateway: a compound operation that creates a copy on the
	/// destination (from the staged copy of the file's contents), verifies its commit, and then deletes the source on
	/// the condition that it did not change. Directories cannot be moved across gateways (copy, then delete).
	public func recordRemoteCrossHostMove(
		itemID: Int64, toParentItemID parentItemID: Int64, name: String, stagedFile: StagedFile
	) throws(IndexError) -> RemoteMutationResult {
		try transaction { connection throws(IndexError) in
			let recorder = try RemoteMutationRecorder(connection: connection)
			let result = try recorder.recordCrossHostMove(
				itemID: itemID, toParentItemID: parentItemID, name: name, stagedFile: stagedFile)
			try recorder.finish()
			return result
		}
	}

	/// Gives up an operation that was not sent, was rejected, or whose outcome is unknown (after the user reviewed
	/// it). Its item shows the gateway's state again; operations that depend on it are canceled as well. Operations
	/// that may be in flight cannot be discarded.
	///
	/// The discarded edit or creation is remembered (`RemoteDiscard`), in the same transaction: the system still holds
	/// the change, and makes it again once told that its error is resolved.
	public func discardMutation(operationID: String) throws(IndexError) -> RemoteMutationResult {
		try discardMutation(operationID: operationID, remembering: true)
	}

	/// Gives up an operation like `discardMutation(operationID:)`. Without `remembering`, the change is not remembered
	/// as discarded: for the extension, when it gives up an operation in a call of the system that the system then
	/// makes differently (renamed after a collision, merged after a conflict) or not at all.
	public func discardMutation(operationID: String, remembering: Bool) throws(IndexError) -> RemoteMutationResult {
		try transaction { connection throws(IndexError) in
			let recorder = try RemoteMutationRecorder(connection: connection)
			let result = try recorder.discard(operationID: operationID, remembering: remembering)
			try recorder.finish()
			return result
		}
	}

	// MARK: - Discarded changes

	/// How long a discarded change is remembered
	public static let discardLifetime: TimeInterval = 30 * 24 * 60 * 60

	static let discardColumns = "operation_id, kind, item_id, template_identifier, digest, discarded_at"

	static func discard(from row: SQLiteStatement) -> RemoteDiscard {
		RemoteDiscard(
			operationID: row.text(0), kind: MutationKind(rawValue: row.text(1)) ?? .replace, itemID: row.optionalInt64(2),
			templateIdentifier: row.optionalText(3), digest: row.optionalText(4),
			discardedAt: Date(timeIntervalSince1970: row.double(5)))
	}

	/// Takes the discarded edit of the item with these contents, if the user discarded one: the system's attempt to
	/// make it again is not recorded, and the discard is forgotten
	public func consumeDiscardedEdit(itemID: Int64, digest: String, now: Date = Date()) throws(IndexError)
		-> RemoteDiscard?
	{
		try consumeDiscard("kind = 'replace' AND item_id = ? AND digest = ?", [itemID, digest], now: now)
	}

	/// Takes the discarded creation of the system's pending item, if the user discarded it: found by the template's
	/// identifier, with the same kind and (for files) contents. Creations recorded before the identifier was kept are
	/// found by their place and contents instead (their `templateIdentifier` is nil).
	public func consumeDiscardedCreation(
		templateIdentifier: String, parentItemID: Int64, name: String, kind: ItemKind, digest: String?,
		now: Date = Date()
	) throws(IndexError) -> RemoteDiscard? {
		if let discard = try consumeDiscard(
			"kind = 'create' AND template_identifier = ? AND item_kind = ? AND digest IS ?",
			[templateIdentifier, kind.rawValue, digest], now: now)
		{
			return discard
		}
		return try consumeDiscard(
			"""
			kind = 'create' AND template_identifier IS NULL AND parent_item_id = ? AND name = ? AND item_kind = ?
				AND digest IS ?
			""", [parentItemID, FileNames.normalized(name), kind.rawValue, digest], now: now)
	}

	/// Takes what was remembered of the discarded operation (for a call of the system that was still waiting for it:
	/// the system's change ends with that call)
	public func consumeDiscard(operationID: String, now: Date = Date()) throws(IndexError) -> RemoteDiscard? {
		try consumeDiscard("operation_id = ?", [operationID], now: now)
	}

	/// The remembered discards that are not expired (of the item, when given), newest first
	public func remoteDiscards(itemID: Int64? = nil, now: Date = Date()) throws(IndexError) -> [RemoteDiscard] {
		let cutoff = now.timeIntervalSince1970 - Self.discardLifetime
		return try withConnection { connection throws(IndexError) in
			try connection.query(
				"""
				SELECT \(Self.discardColumns) FROM remote_discards
				WHERE discarded_at >= ? AND (? IS NULL OR item_id = ?) ORDER BY discarded_at DESC, id DESC
				""", [cutoff, itemID, itemID], Self.discard)
		}
	}

	/// Finds a discard that has not expired, and forgets it (with the other contents remembered for the same
	/// operation). Expired discards are ignored, and removed when another one is remembered or taken.
	private func consumeDiscard(_ condition: String, _ values: [any SQLBindable], now: Date) throws(IndexError)
		-> RemoteDiscard?
	{
		let cutoff = now.timeIntervalSince1970 - Self.discardLifetime
		let query = """
			SELECT \(Self.discardColumns) FROM remote_discards WHERE discarded_at >= ? AND \(condition)
			ORDER BY discarded_at DESC, id DESC LIMIT 1
			"""
		let parameters: [any SQLBindable] = [cutoff] + values
		// Usually there is none (this is asked for every change of contents): looked for without the write lock first
		guard
			try withConnection({ connection throws(IndexError) in
				try connection.queryOne(query, parameters, Self.discard)
			}) != nil
		else {
			return nil
		}
		return try transaction { connection throws(IndexError) in
			guard let discard = try connection.queryOne(query, parameters, Self.discard) else {
				return nil
			}
			try connection.run(
				"DELETE FROM remote_discards WHERE operation_id = ? OR discarded_at < ?", [discard.operationID, cutoff])
			return discard
		}
	}

	// MARK: - Claims

	/// Claims the oldest operation that can make progress (or the given one) for the owner, for the duration. A
	/// claim lets one process work on the operation; every update made with it checks that its generation is still
	/// current, so a process whose claim expired and was taken over (or whose operation a newer local change replaced)
	/// cannot change the operation or send a second request for it.
	public func claimMutation(
		owner: String, duration: TimeInterval, operationID: String? = nil, now: Date = Date()
	) throws(IndexError) -> OutboxClaim? {
		try transaction { connection throws(IndexError) in
			let time = now.timeIntervalSince1970
			var filter = ""
			var values: [any SQLBindable] = [time, time]
			if let operationID {
				filter = "AND operation_id = ?"
				values.append(operationID)
			}
			// A page at a time, until a candidate is ready: operations that wait for others cannot hold back the ones
			// after them, however many there are
			var after: (createdAt: Double, operationID: String)?
			while true {
				var page = ""
				var pageValues = values
				if let after {
					page = "AND (created_at > ? OR (created_at = ? AND operation_id > ?))"
					pageValues += [after.createdAt, after.createdAt, after.operationID]
				}
				let candidates = try connection.query(
					"""
					SELECT \(Self.mutationColumns) FROM remote_outbox
					WHERE state IN ('staged', 'uploading', 'uploaded', 'submitted', 'pending') AND kind != 'crossHostMove'
						AND (claim_owner IS NULL OR claim_expires <= ?) AND (not_before IS NULL OR not_before <= ?)
						\(filter) \(page)
					ORDER BY created_at, operation_id LIMIT ?
					""", pageValues + [Self.claimPage], Self.mutation)
				for candidate in candidates where try Self.isReady(connection, candidate) {
					let generation = candidate.generation + 1
					let expires = time + duration
					try connection.run(
						"""
						UPDATE remote_outbox SET claim_owner = ?, claim_expires = ?, generation = ?, updated_at = ?
						WHERE operation_id = ?
						""", [owner, expires, generation, time, candidate.operationID])
					guard let record = try Self.mutation(connection, operationID: candidate.operationID) else {
						continue
					}
					return OutboxClaim(
						record: record, owner: owner, generation: generation,
						expires: Date(timeIntervalSince1970: expires))
				}
				guard candidates.count == Self.claimPage, let last = candidates.last else {
					return nil
				}
				after = (last.createdAt.timeIntervalSince1970, last.operationID)
			}
		}
	}

	/// Candidates read at once when claiming
	private static let claimPage = 50

	/// Whether every operation the candidate depends on is committed (or gone)
	private static func isReady(_ connection: SQLiteConnection, _ candidate: MutationRecord) throws(IndexError) -> Bool {
		for dependency in candidate.dependencies {
			let state = try connection.queryOne(
				"SELECT state FROM remote_outbox WHERE operation_id = ?", [dependency], { $0.text(0) })
			if state != nil && state != MutationState.committed.rawValue {
				return false
			}
		}
		return true
	}

	/// Lets an operation that waits before its next attempt (and the steps of a compound one) be tried right away, for
	/// instance when the system retries the change it carries. Operations another process holds a current claim on are
	/// left alone, and so is the order: an operation still waits for the ones it depends on.
	public func expediteMutation(operationID: String, now: Date = Date()) throws(IndexError) {
		try withConnection { connection throws(IndexError) in
			try connection.run(
				"""
				UPDATE remote_outbox SET not_before = NULL, updated_at = ?
				WHERE (operation_id = ? OR parent_operation_id = ?) AND not_before IS NOT NULL
					AND state IN ('staged', 'uploading', 'uploaded', 'submitted', 'pending')
					AND (claim_owner IS NULL OR claim_expires <= ?)
				""",
				[now.timeIntervalSince1970, operationID, operationID, now.timeIntervalSince1970])
		}
	}

	/// Extends a claim that is still current (before every request made with it). Nil when it was lost.
	public func renewClaim(_ claim: OutboxClaim, duration: TimeInterval, now: Date = Date()) throws(IndexError)
		-> OutboxClaim?
	{
		let expires = now.timeIntervalSince1970 + duration
		return try updateClaimed(claim, "claim_expires = ?", [expires]).map {
			var renewed = $0
			renewed.expires = Date(timeIntervalSince1970: expires)
			return renewed
		}
	}

	/// Gives up a claim, so that another process can continue the operation right away
	public func releaseClaim(_ claim: OutboxClaim) throws(IndexError) {
		_ = try updateClaimed(claim, "claim_owner = NULL, claim_expires = NULL", [])
	}

	/// Runs an update of a claimed operation, if the claim is current. Returns the updated claim, or nil when the claim
	/// was lost.
	private func updateClaimed(_ claim: OutboxClaim, _ assignments: String, _ values: [any SQLBindable])
		throws(IndexError) -> OutboxClaim?
	{
		try transaction { connection throws(IndexError) in
			try Self.updateClaimed(connection, claim, assignments, values)
		}
	}

	static func updateClaimed(
		_ connection: SQLiteConnection, _ claim: OutboxClaim, _ assignments: String, _ values: [any SQLBindable]
	) throws(IndexError) -> OutboxClaim? {
		try connection.run(
			"""
			UPDATE remote_outbox SET \(assignments), updated_at = ?
			WHERE operation_id = ? AND generation = ? AND claim_owner = ?
			""", values + [Date().timeIntervalSince1970, claim.operationID, claim.generation, claim.owner])
		guard connection.changes > 0, let record = try mutation(connection, operationID: claim.operationID) else {
			return nil
		}
		var updated = claim
		updated.record = record
		return updated
	}

	// MARK: - Progress of claimed operations

	/// Records the upload created for the operation's staged file
	public func recordUploadCreated(_ claim: OutboxClaim, upload: UploadInfo, tokenReference: String?)
		throws(IndexError) -> OutboxClaim?
	{
		try transaction { connection throws(IndexError) in
			guard try Self.holds(connection, claim) else {
				return nil
			}
			try Self.releaseUploadTokens(
				connection, where: "operation_id = ? AND upload_token_ref IS NOT ?", [claim.operationID, tokenReference])
			return try Self.updateClaimed(
				connection, claim,
				"""
				upload_id = ?, upload_offset = ?, upload_token_ref = ?, upload_expires = ?, state = 'uploading'
				""", [upload.upload, upload.offset, tokenReference, upload.expires.timeIntervalSince1970])
		}
	}

	/// Whether the claim is still current (checked before changes that must not happen with a lost claim)
	private static func holds(_ connection: SQLiteConnection, _ claim: OutboxClaim) throws(IndexError) -> Bool {
		guard let current = try mutation(connection, operationID: claim.operationID) else {
			return false
		}
		return current.generation == claim.generation && current.claimOwner == claim.owner
	}

	/// Records the offset the gateway confirmed; at the full length the upload is complete
	public func recordUploadOffset(_ claim: OutboxClaim, offset: Int64) throws(IndexError) -> OutboxClaim? {
		let complete = claim.record.uploadLength.map { offset >= $0 } ?? false
		return try updateClaimed(
			claim, "upload_offset = ?, state = ?",
			[offset, complete ? MutationState.uploaded.rawValue : MutationState.uploading.rawValue])
	}

	/// Forgets an expired or terminated upload, so that it starts again from the staged file. Only for operations that
	/// were never sent (a submitted operation pins its upload).
	public func restartUpload(_ claim: OutboxClaim) throws(IndexError) -> OutboxClaim? {
		guard claim.record.state.isUnsent else {
			return nil
		}
		return try transaction { connection throws(IndexError) in
			guard try Self.holds(connection, claim) else {
				return nil
			}
			try Self.releaseUploadTokens(connection, where: "operation_id = ?", [claim.operationID])
			return try Self.updateClaimed(
				connection, claim,
				"upload_id = NULL, upload_offset = 0, upload_token_ref = NULL, upload_expires = NULL, state = 'staged'",
				[])
		}
	}

	/// After the gateway refused the operation's first submission with `404` for its upload (and so stored nothing):
	/// forgets the request, so that it is built again on the next attempt, and with `restartUpload` (the upload is
	/// gone, for instance expired) the upload too, so that the staged file is uploaded again first
	func withdrawSubmission(_ claim: OutboxClaim, restartUpload: Bool) throws(IndexError) -> OutboxClaim? {
		try transaction { connection throws(IndexError) in
			guard try Self.holds(connection, claim) else {
				return nil
			}
			guard restartUpload else {
				return try Self.updateClaimed(
					connection, claim, "payload = NULL, authority_generation = NULL, state = 'uploaded'", [])
			}
			try Self.releaseUploadTokens(connection, where: "operation_id = ?", [claim.operationID])
			return try Self.updateClaimed(
				connection, claim,
				"""
				payload = NULL, authority_generation = NULL, upload_id = NULL, upload_offset = 0, upload_token_ref = NULL,
				upload_expires = NULL, state = 'staged'
				""", [])
		}
	}

	/// Builds the operation's request (once: replays send the stored request unchanged) and marks the operation as
	/// submitted before it is sent, so that after an interruption it is recovered rather than sent anew. An operation
	/// whose request cannot be built fails.
	public func prepareSubmission(_ claim: OutboxClaim, authorityGeneration: Int64?) throws(IndexError)
		-> SubmissionPreparation
	{
		try transaction { connection throws(IndexError) -> SubmissionPreparation in
			guard let current = try Self.mutation(connection, operationID: claim.operationID),
				current.generation == claim.generation, current.claimOwner == claim.owner
			else {
				return .lost
			}
			if let payload = current.payload,
				let body = try connection.queryOne(
					"SELECT payload FROM remote_outbox WHERE operation_id = ?", [current.operationID], { $0.text(0) })
			{
				guard
					let updated = try Self.updateClaimed(
						connection, claim, "state = CASE WHEN state = 'pending' THEN state ELSE 'submitted' END", [])
				else {
					return .lost
				}
				return .ready(claim: updated, request: payload, body: Data(body.utf8))
			}
			let request: OperationRequest
			do throws(IndexError) {
				request = try Self.buildRequest(connection, current)
			}
			catch {
				let message: String
				if case .notAllowed(let reason) = error {
					message = reason
				}
				else {
					message = "\(error)"
				}
				let recorder = try RemoteMutationRecorder(connection: connection)
				try recorder.fail(operationID: current.operationID, error: message, currentItem: nil)
				try recorder.finish()
				return .failed(message)
			}
			guard let json = try? request.canonicalJSON(),
				let updated = try Self.updateClaimed(
					connection, claim, "payload = ?, state = 'submitted', authority_generation = ?",
					[String(decoding: json, as: UTF8.self), authorityGeneration])
			else {
				return .lost
			}
			return .ready(claim: updated, request: request, body: json)
		}
	}

	private static func buildRequest(_ connection: SQLiteConnection, _ record: MutationRecord) throws(IndexError)
		-> OperationRequest
	{
		guard let kind = record.kind.remoteKind,
			let binding = try remoteBinding(connection, folderID: record.folderID), let remoteFolderID = binding.remoteFolderID
		else {
			throw .notAllowed("the folder is not accessed through a gateway")
		}
		func gatewayID(ofItem itemID: Int64?) throws(IndexError) -> String? {
			guard let itemID else {
				return nil
			}
			if let folderID = try connection.queryOne(
				"SELECT folder_id FROM folders WHERE root_id = ?", [itemID], { $0.text(0) })
			{
				return try rootGatewayID(connection, folderID: folderID)
			}
			return try connection.queryOne(
				"SELECT gateway_id FROM remote_items WHERE item_id = ? AND deleted = 0", [itemID], { $0.text(0) })
		}
		func targetGatewayID() throws(IndexError) -> String? {
			if let gatewayID = record.gatewayID {
				return gatewayID
			}
			return try gatewayID(ofItem: record.itemID)
		}
		let id = record.operationID
		switch kind {
		case .create:
			guard let parentID = try gatewayID(ofItem: record.targetParentItemID), let name = record.targetName else {
				throw .notAllowed("the destination directory is not on the gateway")
			}
			if record.itemKind == .directory {
				return .create(
					id: id, folderID: remoteFolderID, parentID: parentID, name: name, itemKind: .directory,
					modifiedNs: record.modifiedNs)
			}
			guard let upload = record.uploadID, let digest = record.uploadDigest else {
				throw .notAllowed("the contents have not been uploaded")
			}
			return .create(
				id: id, folderID: remoteFolderID, parentID: parentID, name: name, itemKind: .file, upload: upload,
				uploadDigest: digest, modifiedNs: record.modifiedNs)
		case .replace:
			guard let itemID = try targetGatewayID(), let base = record.baseRevision
			else {
				throw .notAllowed("the version the edit is based on is not known")
			}
			guard let upload = record.uploadID, let digest = record.uploadDigest else {
				throw .notAllowed("the contents have not been uploaded")
			}
			return .replace(
				id: id, folderID: remoteFolderID, itemID: itemID, baseRevision: base, upload: upload,
				uploadDigest: digest, modifiedNs: record.modifiedNs, onConflict: record.conflictPolicy ?? .keepBoth)
		case .move:
			guard let itemID = try targetGatewayID(), let base = record.baseRevision,
				let parentID = try gatewayID(ofItem: record.targetParentItemID), let name = record.targetName
			else {
				throw .notAllowed("the item or its destination is not on the gateway")
			}
			var destination: String?
			if let targetFolderID = record.targetFolderID, targetFolderID != record.folderID {
				destination = try remoteBinding(connection, folderID: targetFolderID)?.remoteFolderID
			}
			return .move(
				id: id, folderID: remoteFolderID, itemID: itemID, baseRevision: base, parentID: parentID, name: name,
				subtreeGeneration: record.subtreeGeneration, destinationFolderID: destination)
		case .delete:
			guard let itemID = try targetGatewayID(), let base = record.baseRevision
			else {
				throw .notAllowed("the item is not on the gateway")
			}
			return .delete(
				id: id, folderID: remoteFolderID, itemID: itemID, baseRevision: base,
				subtreeGeneration: record.subtreeGeneration)
		case .unknown:
			throw .notAllowed("unknown operation")
		}
	}

	/// Records the gateway's answer for the operation: its server ID while pending; once final, the resulting items
	/// are stored, the item's overlay is resolved, and operations depending on it continue (or fail with it).
	public func recordOperation(_ claim: OutboxClaim, _ operation: RemoteOperation) throws(IndexError) -> OutboxClaim? {
		try transaction { connection throws(IndexError) in
			guard let current = try Self.mutation(connection, operationID: claim.operationID),
				current.generation == claim.generation, current.claimOwner == claim.owner
			else {
				return nil
			}
			let recorder = try RemoteMutationRecorder(connection: connection)
			try recorder.recordResult(operation, of: current)
			try recorder.finish()
			guard let record = try Self.mutation(connection, operationID: claim.operationID) else {
				return nil
			}
			var updated = claim
			updated.record = record
			return updated
		}
	}

	/// Records that the gateway rejected the operation (`409`, `412`, `403`…): it fails, with the current item when
	/// the gateway returned one. A rejected move or deletion shows the gateway's state again; rejected contents stay
	/// visible until the user exports or discards them.
	public func recordRejection(_ claim: OutboxClaim, error: String, currentItem: RemoteItem?) throws(IndexError)
		-> OutboxClaim?
	{
		try transaction { connection throws(IndexError) in
			guard let current = try Self.mutation(connection, operationID: claim.operationID),
				current.generation == claim.generation, current.claimOwner == claim.owner
			else {
				return nil
			}
			let recorder = try RemoteMutationRecorder(connection: connection)
			try recorder.fail(operationID: current.operationID, error: error, currentItem: currentItem)
			try recorder.finish()
			guard let record = try Self.mutation(connection, operationID: claim.operationID) else {
				return nil
			}
			var updated = claim
			updated.record = record
			return updated
		}
	}

	/// Records a failed attempt that is tried again later (no earlier than `notBefore`), and releases the claim
	public func recordRetry(_ claim: OutboxClaim, error: String, notBefore: Date?) throws(IndexError) -> OutboxClaim? {
		try updateClaimed(
			claim, "attempts = attempts + 1, last_error = ?, not_before = ?, claim_owner = NULL, claim_expires = NULL",
			[error, notBefore?.timeIntervalSince1970])
	}

	/// Records that the gateway still works on the operation: it is polled again no earlier than `notBefore`, and the
	/// claim is released. The gateway answered, so this is not a failed attempt: the attempts start over.
	public func recordPending(_ claim: OutboxClaim, notBefore: Date?) throws(IndexError) -> OutboxClaim? {
		try updateClaimed(
			claim, "attempts = 0, not_before = ?, claim_owner = NULL, claim_expires = NULL",
			[notBefore?.timeIntervalSince1970])
	}

	/// Records that the gateway can no longer report the operation's outcome: it needs review by the user
	public func recordOutcomeUnknown(_ claim: OutboxClaim, reason: String) throws(IndexError) -> OutboxClaim? {
		try transaction { connection throws(IndexError) in
			guard let current = try Self.mutation(connection, operationID: claim.operationID),
				current.generation == claim.generation, current.claimOwner == claim.owner
			else {
				return nil
			}
			let recorder = try RemoteMutationRecorder(connection: connection)
			try recorder.resolve(operationID: current.operationID, state: .outcomeUnknown, error: reason)
			try recorder.finish()
			guard let record = try Self.mutation(connection, operationID: claim.operationID) else {
				return nil
			}
			var updated = claim
			updated.record = record
			return updated
		}
	}

	/// After a change of the gateway's authority generation: operations that may have reached it but are not
	/// confirmed become outcome-unknown
	static func markUnconfirmedOutcomeUnknown(_ connection: SQLiteConnection, serviceID: String, generation: Int64)
		throws(IndexError)
	{
		// Not a compound operation: it is never sent itself, and follows its steps
		let affected = try connection.query(
			"""
			SELECT operation_id FROM remote_outbox
			WHERE service_id = ? AND state IN ('submitted', 'pending') AND kind != 'crossHostMove'
				AND (authority_generation IS NULL OR authority_generation != ?)
			""", [serviceID, generation], { $0.text(0) })
		guard !affected.isEmpty else {
			return
		}
		let recorder = try RemoteMutationRecorder(connection: connection)
		for operationID in affected {
			try recorder.resolve(
				operationID: operationID, state: .outcomeUnknown,
				error: "the gateway may have lost the operation's state (authority generation \(generation))")
		}
		try recorder.finish()
	}
}

/// Records local changes of remote-managed folders and resolves the operations that carry them, inside one
/// transaction
final class RemoteMutationRecorder {
	private let connection: SQLiteConnection
	private let projector: RemoteProjector
	private let now = Date().timeIntervalSince1970

	init(connection: SQLiteConnection) throws(IndexError) {
		self.connection = connection
		projector = try RemoteProjector(connection: connection)
	}

	func finish() throws(IndexError) {
		try projector.finish()
	}

	// MARK: - Helpers

	private func liveItem(_ id: Int64) throws(IndexError) -> IndexItem {
		guard
			let item = try connection.queryOne(
				"SELECT \(IndexStore.itemColumns) FROM items WHERE id = ? AND deleted = 0", [id], IndexStore.item)
		else {
			throw .notFound
		}
		return item
	}

	private func item(_ id: Int64) throws(IndexError) -> IndexItem? {
		try connection.queryOne(
			"SELECT \(IndexStore.itemColumns) FROM items WHERE id = ? AND deleted = 0", [id], IndexStore.item)
	}

	/// The binding of a folder that is accessed through a gateway and may be changed
	private func writableBinding(_ folderID: String) throws(IndexError) -> RemoteFolderBinding {
		guard let binding = try IndexStore.remoteBinding(connection, folderID: folderID), binding.isRemote,
			binding.serviceID != nil
		else {
			throw .notAllowed("the folder is not accessed through a gateway")
		}
		guard binding.permissions.write else {
			throw .notAllowed("the folder is read-only on its gateway")
		}
		// Checked in the transaction that records the edit, so no edit slips in while the folder is being switched
		if try IndexStore.isRemoteMigrationFenced(connection, folderID: folderID) {
			throw .notAllowed("the folder is being switched to another way of access; try again when that is done")
		}
		return binding
	}

	private func checkName(_ name: String) throws(IndexError) -> String {
		let name = FileNames.normalized(name)
		guard FileNames.isValid(name), !FileNames.isReserved(name) else {
			throw .notAllowed("\(name) cannot be used as a name")
		}
		return name
	}

	private func checkNameIsFree(_ name: String, in parentID: Int64, except itemID: Int64? = nil) throws(IndexError) {
		if let existing = try LocalChangeRecorder.child(connection, of: parentID, named: name), existing.id != itemID {
			throw .nameCollision(existing: existing.id)
		}
	}

	private func container(_ id: Int64) throws(IndexError) -> IndexItem {
		let parent = try liveItem(id)
		guard parent.kind.isContainer else {
			throw .notAllowed("the destination is not a directory")
		}
		return parent
	}

	private func gatewayID(ofItem itemID: Int64) throws(IndexError) -> String? {
		if let folderID = try connection.queryOne(
			"SELECT folder_id FROM folders WHERE root_id = ?", [itemID], { $0.text(0) })
		{
			return try IndexStore.rootGatewayID(connection, folderID: folderID)
		}
		return try connection.queryOne(
			"SELECT gateway_id FROM remote_items WHERE item_id = ? AND deleted = 0", [itemID], { $0.text(0) })
	}

	private func remoteState(ofItem itemID: Int64) throws(IndexError) -> RemoteItemState? {
		try connection.queryOne(
			"SELECT \(IndexStore.remoteItemColumns) FROM remote_items WHERE item_id = ? AND deleted = 0", [itemID],
			IndexStore.remoteItemState)
	}

	/// The latest operation of the item that is not resolved yet
	private func latestUnresolved(itemID: Int64) throws(IndexError) -> MutationRecord? {
		try connection.queryOne(
			"""
			SELECT \(IndexStore.mutationColumns) FROM remote_outbox
			WHERE item_id = ? AND parent_operation_id IS NULL AND \(IndexStore.unresolvedCondition)
			ORDER BY created_at DESC, operation_id DESC LIMIT 1
			""", [itemID], IndexStore.mutation)
	}

	/// The operations a new operation of the item must wait for: the item's unresolved operation, and the creation of
	/// a destination directory that is not on the gateway yet
	private func dependencies(itemID: Int64?, parentItemID: Int64?) throws(IndexError) -> [String] {
		var result: [String] = []
		if let itemID, let latest = try latestUnresolved(itemID: itemID) {
			guard latest.state.mayHaveReachedGateway || latest.state.isUnsent else {
				throw .notAllowed("an earlier change of this item needs review first")
			}
			result.append(latest.operationID)
		}
		if let parentItemID, try gatewayID(ofItem: parentItemID) == nil {
			guard
				let creation = try connection.queryOne(
					"""
					SELECT operation_id FROM remote_outbox
					WHERE item_id = ? AND kind = 'create' AND state IN ('staged', 'uploading', 'uploaded', 'submitted',
						'pending')
					ORDER BY created_at DESC LIMIT 1
					""", [parentItemID], { $0.text(0) })
			else {
				throw .notAllowed("the destination directory is not on the gateway")
			}
			result.append(creation)
		}
		return result
	}

	private static func combine(_ existing: PendingChange?, _ new: PendingChange) -> PendingChange {
		guard let existing, existing != .none else {
			return new
		}
		if existing == .create || new == .delete {
			return existing == .create && new != .delete ? .create : new
		}
		return existing == .modify || new == .modify ? .modify : .rename
	}

	/// Sets the item's overlay. The origin is that of the row: set when the change created it, and kept by later
	/// changes of the row.
	private func setOverlay(
		itemID: Int64, folderID: String, pending: PendingChange, operationID: String, stagedPath: String?,
		origin: RemoteChangeOrigin? = nil
	) throws(IndexError) {
		try connection.run(
			"""
			INSERT INTO remote_overlays (item_id, folder_id, kind, operation_id, staged_path, created_at, origin)
			VALUES (?, ?, ?, ?, ?, ?, ?)
			ON CONFLICT(item_id) DO UPDATE SET folder_id = excluded.folder_id, kind = excluded.kind,
				operation_id = excluded.operation_id, staged_path = COALESCE(excluded.staged_path, staged_path),
				origin = COALESCE(excluded.origin, origin)
			""", [itemID, folderID, pending.rawValue, operationID, stagedPath, now, origin?.rawValue])
	}

	private func insertMutation(
		_ kind: MutationKind, folderID: String, serviceID: String?, itemID: Int64?, gatewayID: String?,
		baseRevision: RemoteRevision?, subtreeGeneration: Int64?, targetParentID: Int64?, targetName: String?,
		targetFolderID: String?, itemKind: ItemKind?, modifiedNs: Int64?, staged: StagedFile?, dependencies: [String],
		parentOperationID: String? = nil, step: Int? = nil, createdAt: Double? = nil,
		conflictPolicy: RemoteConflictPolicy? = nil, templateIdentifier: String? = nil
	) throws(IndexError) -> String {
		let operationID = UUID().uuidString.lowercased()
		try connection.run(
			"""
			INSERT INTO remote_outbox (operation_id, folder_id, service_id, item_id, gateway_id, kind, base_revision,
				subtree_generation, target_parent_id, target_name, target_folder_id, item_kind, modified_ns, staged_path,
				upload_digest, upload_length, state, depends_on, parent_operation_id, step, created_at, updated_at,
				on_conflict, template_identifier)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, 'staged', ?, ?, ?, ?, ?, ?, ?)
			""",
			[
				operationID, folderID, serviceID, itemID, gatewayID, kind.rawValue, baseRevision?.rawValue,
				subtreeGeneration, targetParentID, targetName, targetFolderID, itemKind?.rawValue, modifiedNs,
				staged?.relativePath, staged?.digest, staged?.size,
				dependencies.isEmpty ? nil : dependencies.joined(separator: " "), parentOperationID, step,
				createdAt ?? now, now, conflictPolicy?.rawValue, templateIdentifier,
			])
		if let staged {
			try IndexStore.registerStagedFile(connection, staged, itemID: itemID, operationID: operationID)
		}
		return operationID
	}

	private func mutation(_ operationID: String) throws(IndexError) -> MutationRecord? {
		try IndexStore.mutation(connection, operationID: operationID)
	}

	/// Resets an unsent operation's upload and claim, for a newer local change that replaces it. The generation changes,
	/// so the process working on it (if any) can no longer update or send it.
	private func supersede(_ operationID: String, _ assignments: String, _ values: [any SQLBindable]) throws(IndexError) {
		try connection.run(
			"""
			UPDATE remote_outbox SET \(assignments), generation = generation + 1, claim_owner = NULL,
				claim_expires = NULL, attempts = 0, not_before = NULL, last_error = NULL, updated_at = ?
			WHERE operation_id = ?
			""", values + [now, operationID])
	}

	// MARK: - Recording

	func recordCreation(
		folderID: String, parentItemID: Int64, name: String, kind: ItemKind, modifiedNanoseconds: Int64,
		stagedFile: StagedFile?, origin: RemoteChangeOrigin = .system, templateIdentifier: String? = nil
	) throws(IndexError) -> RemoteMutationResult {
		let binding = try writableBinding(folderID)
		guard kind == .file || kind == .directory else {
			throw .notAllowed("only files and directories can be created")
		}
		guard kind == .directory || stagedFile != nil else {
			throw .notAllowed("a new file needs its contents")
		}
		let parent = try container(parentItemID)
		guard parent.folderID == folderID else {
			throw .notAllowed("the parent is not in folder \(folderID)")
		}
		let name = try checkName(name)
		try checkNameIsFree(name, in: parent.id)
		let dependencies = try dependencies(itemID: nil, parentItemID: parent.id)
		let path = FileNames.join(parent.path, name)
		let size = kind == .file ? stagedFile?.size ?? 0 : 0
		try connection.run(
			"""
			INSERT INTO items (folder_id, path, parent_id, name, name_key, kind, size, mod_ns, changed_seq, selected,
				pending, pending_size, pending_mod_ns)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
			""",
			[
				folderID, path, parent.id, name, FileNames.collisionKey(name), kind.rawValue, size, modifiedNanoseconds,
				projector.nextChangeSequence(), parent.isSelected, PendingChange.create.rawValue, size,
				modifiedNanoseconds,
			])
		let itemID = connection.lastInsertRowID
		let operationID = try insertMutation(
			.create, folderID: folderID, serviceID: binding.serviceID, itemID: itemID, gatewayID: nil,
			baseRevision: nil, subtreeGeneration: nil, targetParentID: parent.id, targetName: name, targetFolderID: nil,
			itemKind: kind, modifiedNs: modifiedNanoseconds, staged: kind == .file ? stagedFile : nil,
			dependencies: dependencies, templateIdentifier: templateIdentifier)
		try setOverlay(
			itemID: itemID, folderID: folderID, pending: .create, operationID: operationID,
			stagedPath: stagedFile?.relativePath, origin: origin)
		projector.countVisibleChange()
		return RemoteMutationResult(item: try item(itemID), mutation: try mutation(operationID))
	}

	func recordContentChange(
		itemID: Int64, modifiedNanoseconds: Int64, stagedFile: StagedFile, onConflict: RemoteConflictPolicy = .keepBoth
	) throws(IndexError) -> RemoteMutationResult {
		let current = try liveItem(itemID)
		guard current.kind == .file else {
			throw .notAllowed("only files have contents")
		}
		let binding = try writableBinding(current.folderID)
		let overlay = try IndexStore.overlay(connection, itemID: itemID)
		var released: [String] = []
		let operationID: String
		if let latest = try latestUnresolved(itemID: itemID), latest.state.isUnsent,
			latest.kind == .create || latest.kind == .replace
		{
			// Nothing was sent for the earlier edit: this one takes its place
			operationID = latest.operationID
			if let old = latest.stagedPath, old != stagedFile.relativePath {
				released.append(old)
			}
			try IndexStore.releaseUploadTokens(connection, where: "operation_id = ?", [operationID])
			try supersede(
				operationID,
				"""
				staged_path = ?, upload_digest = ?, upload_length = ?, modified_ns = ?, upload_id = NULL,
				upload_offset = 0, upload_token_ref = NULL, upload_expires = NULL, state = 'staged', on_conflict = ?
				""",
				[
					stagedFile.relativePath, stagedFile.digest, stagedFile.size, modifiedNanoseconds,
					latest.kind == .replace ? onConflict.rawValue : nil,
				])
			try IndexStore.registerStagedFile(connection, stagedFile, itemID: itemID, operationID: operationID)
		}
		else {
			let dependencies = try dependencies(itemID: itemID, parentItemID: nil)
			let remote = try remoteState(ofItem: itemID)
			guard dependencies.isEmpty ? remote != nil : true else {
				throw .notAllowed("the file is not on the gateway")
			}
			operationID = try insertMutation(
				.replace, folderID: current.folderID, serviceID: binding.serviceID, itemID: itemID,
				gatewayID: remote?.gatewayID, baseRevision: dependencies.isEmpty ? remote?.revision : nil,
				subtreeGeneration: nil, targetParentID: nil, targetName: nil, targetFolderID: nil, itemKind: .file,
				modifiedNs: modifiedNanoseconds, staged: stagedFile, dependencies: dependencies,
				conflictPolicy: onConflict)
		}
		let pending = Self.combine(overlay?.pending, .modify)
		try setOverlay(
			itemID: itemID, folderID: current.folderID, pending: pending, operationID: operationID,
			stagedPath: stagedFile.relativePath)
		try connection.run(
			"""
			UPDATE items SET size = ?, mod_ns = ?, pending = ?, pending_size = ?, pending_mod_ns = ?,
				content_rev = content_rev + 1, meta_rev = meta_rev + 1, changed_seq = ?
			WHERE id = ?
			""",
			[
				stagedFile.size, modifiedNanoseconds, pending.rawValue, stagedFile.size, modifiedNanoseconds,
				projector.nextChangeSequence(), itemID,
			])
		projector.countVisibleChange()
		return RemoteMutationResult(
			item: try item(itemID), mutation: try mutation(operationID), releasedStagedFiles: released)
	}

	func recordMove(itemID: Int64, toParentItemID parentItemID: Int64, name: String) throws(IndexError)
		-> RemoteMutationResult
	{
		let moving = try liveItem(itemID)
		guard moving.kind != .folderRoot else {
			throw .notAllowed("folder roots cannot be moved")
		}
		let binding = try writableBinding(moving.folderID)
		let parent = try container(parentItemID)
		let targetBinding = try writableBinding(parent.folderID)
		guard targetBinding.serviceID == binding.serviceID else {
			throw .notAllowed("moving to a folder on another gateway is a cross-host move")
		}
		let name = try checkName(name)
		let path = FileNames.join(parent.path, name)
		if parent.folderID == moving.folderID, path.utf8.elementsEqual(moving.path.utf8) {
			return RemoteMutationResult(item: moving, mutation: nil)
		}
		if moving.kind.isContainer, parent.folderID == moving.folderID,
			path.utf8.starts(with: (moving.path + "/").utf8) || parent.id == moving.id
		{
			throw .notAllowed("a directory cannot be moved into itself")
		}
		try checkNameIsFree(name, in: parent.id, except: moving.id)

		let overlay = try IndexStore.overlay(connection, itemID: itemID)
		let operationID: String
		if let latest = try latestUnresolved(itemID: itemID), latest.state.isUnsent,
			latest.kind == .create || latest.kind == .move
		{
			// Nothing was sent yet: the earlier creation or move goes to the new place instead
			operationID = latest.operationID
			let parentDependency = try dependencies(itemID: nil, parentItemID: parent.id)
			// Keep what the item itself waits for; the destination's creation is recomputed
			var all: [String] = []
			for dependency in latest.dependencies where try mutation(dependency)?.itemID == itemID {
				all.append(dependency)
			}
			all += parentDependency
			var assignments = "target_parent_id = ?, target_name = ?, target_folder_id = ?, folder_id = ?, depends_on = ?"
			if latest.kind == .create, parent.folderID != latest.folderID {
				// The gateway binds an upload to its folder: the creation in another folder uploads its file again
				try IndexStore.releaseUploadTokens(connection, where: "operation_id = ?", [operationID])
				assignments += """
					, upload_id = NULL, upload_offset = 0, upload_token_ref = NULL, upload_expires = NULL,
					state = 'staged'
					"""
			}
			try supersede(
				operationID, assignments,
				[
					parent.id, name, parent.folderID == latest.folderID ? nil : parent.folderID,
					latest.kind == .create ? parent.folderID : latest.folderID,
					all.isEmpty ? nil : all.joined(separator: " "),
				])
		}
		else {
			var dependencies = try dependencies(itemID: itemID, parentItemID: parent.id)
			let remote = try remoteState(ofItem: itemID)
			let ownDependency = try latestUnresolved(itemID: itemID) != nil
			guard remote != nil || ownDependency else {
				throw .notAllowed("the item is not on the gateway")
			}
			dependencies = Array(Set(dependencies)).sorted()
			operationID = try insertMutation(
				.move, folderID: moving.folderID, serviceID: binding.serviceID, itemID: itemID,
				gatewayID: remote?.gatewayID, baseRevision: ownDependency ? nil : remote?.revision,
				subtreeGeneration: moving.kind.isContainer ? remote?.subtreeGeneration : nil,
				targetParentID: parent.id, targetName: name,
				targetFolderID: parent.folderID == moving.folderID ? nil : parent.folderID, itemKind: moving.kind,
				modifiedNs: nil, staged: nil, dependencies: dependencies)
		}
		let pending = Self.combine(overlay?.pending, .rename)
		try setOverlay(
			itemID: itemID, folderID: parent.folderID, pending: pending, operationID: operationID, stagedPath: nil)
		try connection.run(
			"""
			UPDATE items SET folder_id = ?, path = ?, parent_id = ?, name = ?, name_key = ?, pending = ?,
				meta_rev = meta_rev + 1, changed_seq = ?
			WHERE id = ?
			""",
			[
				parent.folderID, path, parent.id, name, FileNames.collisionKey(name), pending.rawValue,
				projector.nextChangeSequence(), itemID,
			])
		if moving.kind.isContainer {
			let (lower, upper) = LocalChangeRecorder.descendantRange(of: moving.path)
			try connection.run(
				"""
				UPDATE items SET folder_id = ?, path = ? || substr(path, ?)
				WHERE folder_id = ? AND deleted = 0 AND path >= ? AND path < ?
				""", [parent.folderID, path, moving.path.unicodeScalars.count + 1, moving.folderID, lower, upper])
			try connection.run(
				"UPDATE remote_overlays SET folder_id = ? WHERE item_id IN (SELECT id FROM items WHERE folder_id = ?)",
				[parent.folderID, parent.folderID])
		}
		projector.countVisibleChange()
		return RemoteMutationResult(item: try item(itemID), mutation: try mutation(operationID))
	}

	func recordDeletion(itemID: Int64) throws(IndexError) -> RemoteMutationResult {
		guard let current = try item(itemID) else {
			return RemoteMutationResult(item: nil, mutation: nil)
		}
		guard current.kind != .folderRoot else {
			throw .notAllowed("folder roots cannot be deleted")
		}
		let binding = try writableBinding(current.folderID)
		let (subtree, subtreeValues) = LocalChangeRecorder.subtree(below: current.path)
		let scope = current.kind.isContainer ? "(id = ? OR (folder_id = ? AND deleted = 0 AND \(subtree)))" : "id = ?"
		let scopeValues: [any SQLBindable] =
			current.kind.isContainer ? [itemID, current.folderID] + subtreeValues : [itemID]

		// Unresolved operations below a directory: unsent ones become moot, sent ones must finish first
		var released: [String] = []
		let below = try connection.query(
			"""
			SELECT \(IndexStore.mutationColumns) FROM remote_outbox
			WHERE item_id IN (SELECT id FROM items WHERE \(scope)) AND item_id != ? AND \(IndexStore.unresolvedCondition)
			""", scopeValues + [itemID], IndexStore.mutation)
		for operation in below {
			guard operation.state.isUnsent || operation.state == .failed else {
				throw .notAllowed("changes inside the directory are still being sent")
			}
		}

		if let latest = try latestUnresolved(itemID: itemID), latest.kind == .create, latest.state.isUnsent {
			// The gateway never heard of the item: cancel its creation (and everything created inside it)
			if try connection.queryOne(
				"""
				SELECT EXISTS (SELECT 1 FROM remote_items WHERE item_id IN (SELECT id FROM items WHERE \(scope)))
				""", scopeValues, { $0.bool(0) }) == true
			{
				throw .notAllowed("items moved into the new directory are still being sent")
			}
			for operation in below + [latest] {
				released += try cancel(operation, reason: "the item was deleted before it was sent")
			}
			try connection.run(
				"DELETE FROM remote_overlays WHERE item_id IN (SELECT id FROM items WHERE \(scope))", scopeValues)
			let start = try projector.sequencesByID()
			try connection.run(
				"""
				UPDATE items SET deleted = 1, deleted_at = ?, pending = 0, pending_size = NULL, pending_mod_ns = NULL,
					meta_rev = meta_rev + 1, changed_seq = ? + id
				WHERE \(scope)
				""", [now, start] + scopeValues)
			projector.countVisibleChange()
			return RemoteMutationResult(item: nil, mutation: nil, releasedStagedFiles: released)
		}

		for operation in below where operation.state.isUnsent {
			released += try cancel(operation, reason: "the directory was deleted")
		}
		let dependencies = try dependencies(itemID: itemID, parentItemID: nil)
		let remote = try remoteState(ofItem: itemID)
		guard remote != nil || !dependencies.isEmpty else {
			throw .notAllowed("the item is not on the gateway")
		}
		let operationID = try insertMutation(
			.delete, folderID: current.folderID, serviceID: binding.serviceID, itemID: itemID,
			gatewayID: remote?.gatewayID, baseRevision: dependencies.isEmpty ? remote?.revision : nil,
			subtreeGeneration: current.kind.isContainer ? remote?.subtreeGeneration : nil, targetParentID: nil,
			targetName: nil, targetFolderID: nil, itemKind: current.kind, modifiedNs: nil, staged: nil,
			dependencies: dependencies)
		// Every removed row gets an overlay, so that refreshes do not bring it back before the deletion is resolved
		let overlay = try IndexStore.overlay(connection, itemID: itemID)
		try connection.run(
			"""
			INSERT INTO remote_overlays (item_id, folder_id, kind, operation_id, staged_path, created_at)
			SELECT id, folder_id, ?, ?, NULL, ? FROM items WHERE \(scope)
			ON CONFLICT(item_id) DO UPDATE SET folder_id = excluded.folder_id, kind = excluded.kind,
				operation_id = excluded.operation_id, staged_path = NULL, created_at = excluded.created_at
			""", [PendingChange.delete.rawValue, operationID, now] + scopeValues)
		if let staged = overlay?.stagedPath {
			try connection.run("UPDATE remote_overlays SET staged_path = ? WHERE item_id = ?", [staged, itemID])
		}
		let start = try projector.sequencesByID()
		try connection.run(
			"""
			UPDATE items SET deleted = 1, deleted_at = ?, pending = CASE WHEN id = ? THEN ? ELSE 0 END,
				meta_rev = meta_rev + 1, changed_seq = ? + id
			WHERE \(scope)
			""", [now, itemID, PendingChange.delete.rawValue, start] + scopeValues)
		projector.countVisibleChange()
		return RemoteMutationResult(item: nil, mutation: try mutation(operationID), releasedStagedFiles: released)
	}

	func recordCrossHostMove(itemID: Int64, toParentItemID parentItemID: Int64, name: String, stagedFile: StagedFile)
		throws(IndexError) -> RemoteMutationResult
	{
		let moving = try liveItem(itemID)
		guard moving.kind == .file else {
			throw .notAllowed("directories cannot be moved to another gateway; copy them, then delete them")
		}
		let source = try writableBinding(moving.folderID)
		let parent = try container(parentItemID)
		let destination = try writableBinding(parent.folderID)
		guard source.serviceID != destination.serviceID else {
			throw .notAllowed("the folders are on the same gateway")
		}
		guard try latestUnresolved(itemID: itemID) == nil, let remote = try remoteState(ofItem: itemID) else {
			throw .notAllowed("earlier changes of the item are still being sent")
		}
		let name = try checkName(name)
		try checkNameIsFree(name, in: parent.id)
		let parentDependencies = try dependencies(itemID: nil, parentItemID: parent.id)

		let compound = try insertMutation(
			.crossHostMove, folderID: moving.folderID, serviceID: source.serviceID, itemID: itemID,
			gatewayID: remote.gatewayID, baseRevision: remote.revision, subtreeGeneration: nil,
			targetParentID: parent.id, targetName: name, targetFolderID: parent.folderID, itemKind: .file,
			modifiedNs: moving.modifiedNanoseconds, staged: nil, dependencies: [])
		let copy = try insertMutation(
			.create, folderID: parent.folderID, serviceID: destination.serviceID, itemID: itemID, gatewayID: nil,
			baseRevision: nil, subtreeGeneration: nil, targetParentID: parent.id, targetName: name,
			targetFolderID: nil, itemKind: .file, modifiedNs: moving.modifiedNanoseconds, staged: stagedFile,
			dependencies: parentDependencies, parentOperationID: compound, step: 1)
		_ = try insertMutation(
			.delete, folderID: moving.folderID, serviceID: source.serviceID, itemID: nil, gatewayID: remote.gatewayID,
			baseRevision: remote.revision, subtreeGeneration: nil, targetParentID: nil, targetName: nil,
			targetFolderID: nil, itemKind: .file, modifiedNs: nil, staged: nil, dependencies: [copy],
			parentOperationID: compound, step: 2)
		try connection.run(
			"UPDATE remote_outbox SET staged_path = ? WHERE operation_id = ?", [stagedFile.relativePath, compound])

		let path = FileNames.join(parent.path, name)
		try setOverlay(
			itemID: itemID, folderID: parent.folderID, pending: .create, operationID: compound,
			stagedPath: stagedFile.relativePath)
		try connection.run(
			"""
			UPDATE items SET folder_id = ?, path = ?, parent_id = ?, name = ?, name_key = ?, pending = ?,
				meta_rev = meta_rev + 1, changed_seq = ?
			WHERE id = ?
			""",
			[
				parent.folderID, path, parent.id, name, FileNames.collisionKey(name), PendingChange.create.rawValue,
				projector.nextChangeSequence(), itemID,
			])
		projector.countVisibleChange()
		return RemoteMutationResult(item: try item(itemID), mutation: try mutation(compound))
	}

	// MARK: - Resolving

	/// Cancels an operation that was not sent (and the ones depending on it). Returns the staged files released.
	private func cancel(_ operation: MutationRecord, reason: String) throws(IndexError) -> [String] {
		try IndexStore.releaseUploadTokens(
			connection, where: "operation_id = ? OR parent_operation_id = ?",
			[operation.operationID, operation.operationID])
		try connection.run(
			"""
			UPDATE remote_outbox SET state = 'canceled', last_error = ?, generation = generation + 1, claim_owner = NULL,
				claim_expires = NULL, updated_at = ?
			WHERE operation_id = ? OR parent_operation_id = ?
			""", [reason, now, operation.operationID, operation.operationID])
		return operation.stagedPath.map { [$0] } ?? []
	}

	func discard(operationID: String, remembering: Bool = true) throws(IndexError) -> RemoteMutationResult {
		guard let operation = try mutation(operationID) else {
			throw .notFound
		}
		let steps = try connection.query(
			"SELECT \(IndexStore.mutationColumns) FROM remote_outbox WHERE parent_operation_id = ?", [operationID],
			IndexStore.mutation)
		for record in [operation] + steps where record.state.mayHaveReachedGateway {
			throw .notAllowed("the operation may be in progress on the gateway")
		}
		if operation.state == .committed || operation.state == .conflict || operation.state == .canceled {
			return RemoteMutationResult(item: try currentItem(operation.itemID), mutation: operation)
		}
		// What is remembered of the discard has to be read before the discard resolves the overlay
		var origin: RemoteChangeOrigin?
		var open: Set<String> = []
		if remembering, let itemID = operation.itemID {
			origin = try IndexStore.overlay(connection, itemID: itemID)?.origin
			open = Set(
				try connection.query(
					"SELECT operation_id FROM remote_outbox WHERE item_id = ? AND state != 'canceled'", [itemID],
					{ $0.text(0) }))
		}
		try resolve(operationID: operationID, state: .canceled, error: "discarded")
		if remembering {
			try remember(operation, origin: origin, canceled: open)
		}
		var released: [String] = []
		for record in [operation] + steps {
			if let staged = record.stagedPath {
				released.append(staged)
			}
		}
		return RemoteMutationResult(
			item: try currentItem(operation.itemID), mutation: try mutation(operationID), releasedStagedFiles: released)
	}

	/// Remembers a discarded change of contents (`RemoteDiscard`), with the contents of every operation of the item the
	/// discard canceled (`open` are the item's operations that were not canceled before): the system holds the newest
	/// of them, and may try to make the change again. When that leaves nothing of an item the system created (the
	/// gateway never had it), the system still holds its pending creation: the creation is remembered, by the system's
	/// identifier of its item. Otherwise an edit is remembered by the item. Moves and deletions are not remembered, nor
	/// are creations Synctrain made itself (the system learns from enumerations that those are gone).
	private func remember(_ operation: MutationRecord, origin: RemoteChangeOrigin?, canceled open: Set<String>)
		throws(IndexError)
	{
		guard let itemID = operation.itemID, operation.parentOperationID == nil,
			operation.kind == .replace || operation.kind == .create
		else {
			return
		}
		var creation: MutationRecord?
		if origin == .system, try item(itemID) == nil {
			if operation.kind == .create {
				creation = operation
			}
			else {
				creation = try connection.queryOne(
					"""
					SELECT \(IndexStore.mutationColumns) FROM remote_outbox
					WHERE item_id = ? AND kind = 'create' AND parent_operation_id IS NULL
					ORDER BY created_at DESC, operation_id DESC LIMIT 1
					""", [itemID], IndexStore.mutation)
			}
		}
		guard creation != nil || operation.kind == .replace else {
			return
		}
		let digests = try connection.query(
			"""
			SELECT operation_id, upload_digest FROM remote_outbox
			WHERE item_id = ? AND state = 'canceled' AND upload_digest IS NOT NULL AND parent_operation_id IS NULL
			ORDER BY created_at, operation_id
			""", [itemID]
		) { (operationID: $0.text(0), digest: $0.text(1)) }
		.filter { open.contains($0.operationID) }.map(\.digest)
		// A directory has no contents: its creation is known by the system's identifier alone
		let contents: [String?] = digests.isEmpty && creation?.itemKind == .directory ? [nil] : digests
		try connection.run("DELETE FROM remote_discards WHERE discarded_at < ?", [now - IndexStore.discardLifetime])
		for digest in contents {
			try connection.run(
				"""
				INSERT INTO remote_discards (operation_id, folder_id, kind, item_id, template_identifier, parent_item_id,
					name, item_kind, digest, discarded_at)
				VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
				""",
				[
					operation.operationID, operation.folderID,
					(creation == nil ? MutationKind.replace : .create).rawValue, itemID, creation?.templateIdentifier,
					creation?.targetParentItemID, creation?.targetName, creation?.itemKind?.rawValue, digest, now,
				])
		}
	}

	private func currentItem(_ id: Int64?) throws(IndexError) -> IndexItem? {
		guard let id else {
			return nil
		}
		return try item(id)
	}

	/// Records an answer of the gateway for the operation
	func recordResult(_ operation: RemoteOperation, of record: MutationRecord) throws(IndexError) {
		let resultJSON = (try? RemoteJSON.encoder().encode(operation)).map { String(decoding: $0, as: UTF8.self) }
		try connection.run(
			"""
			UPDATE remote_outbox SET server_operation_id = ?, result = ?, authority_generation = ?,
				committed_content_id = COALESCE(?, committed_content_id), updated_at = ?
			WHERE operation_id = ?
			""",
			[
				operation.operation, resultJSON, operation.authorityGeneration, operation.contentID?.rawValue, now,
				record.operationID,
			])
		let serviceID = record.serviceID ?? ""
		switch operation.state {
		case .pending, .unknown:
			try connection.run(
				"UPDATE remote_outbox SET state = 'pending' WHERE operation_id = ?", [record.operationID])
		case .committed:
			try commit(operation, of: record, serviceID: serviceID)
		case .conflict:
			if let staged = record.stagedPath, let contentID = operation.contentID {
				try connection.run(
					"UPDATE remote_staged_files SET content_id = ? WHERE path = ?", [contentID.rawValue, staged])
			}
			try storeItems(
				[operation.item, operation.conflictItem].compactMap { $0 }, serviceID: serviceID, sentFor: record)
			try resolve(
				operationID: record.operationID, state: .conflict,
				error: "the item changed on another device; the edit was kept as a conflict copy")
		case .failed:
			try fail(
				operationID: record.operationID,
				error: operation.error.map { "\($0.code): \($0.message)" } ?? "the gateway could not apply the change",
				currentItem: operation.error?.item ?? operation.item, serviceID: serviceID)
		}
	}

	/// Stores items the gateway returned for the operation (outside the change feed). They may predate changes the
	/// feed stored since, so an item is stored only when nothing of it is stored yet, or the stored revision is still
	/// the one the operation was based on (see `RemoteProjector.apply`).
	private func storeItems(_ items: [RemoteItem], serviceID: String, sentFor record: MutationRecord?)
		throws(IndexError)
	{
		let known = Set([record?.baseRevision].compactMap { $0 })
		for item in items {
			guard
				let folderID = try IndexStore.localFolderID(
					connection, serviceID: serviceID, remoteFolderID: item.folderID)
			else {
				continue
			}
			try projector.apply(item, folderID: folderID, mark: nil, onlyOver: known)
		}
	}

	private func commit(_ operation: RemoteOperation, of record: MutationRecord, serviceID: String) throws(IndexError) {
		if let staged = record.stagedPath, let contentID = operation.contentID {
			// The staged copy is now a cached copy of that version: no download needed
			try connection.run(
				"UPDATE remote_staged_files SET content_id = ? WHERE path = ?", [contentID.rawValue, staged])
		}
		if let itemID = record.itemID, let staged = record.stagedPath,
			let contentID = operation.contentID ?? operation.item?.contentID,
			try IndexStore.overlay(connection, itemID: itemID)?.stagedPath == staged
		{
			// The row shows these very bytes already (its content revision changed with the edit), and no newer edit
			// replaced them: the commit names their content identity, which is no change of the row's contents
			try connection.run("UPDATE items SET blocks_hash = ? WHERE id = ?", [contentID.rawValue, itemID])
		}
		if let item = operation.item {
			try storeItems([item], serviceID: serviceID, sentFor: record)
			if record.kind == .create, let itemID = record.itemID {
				try projector.map(item.id, to: itemID)
			}
		}
		if record.kind == .delete, let gatewayID = record.gatewayID ?? operation.item?.id {
			try connection.run(
				"UPDATE remote_items SET deleted = 1, updated_at = ? WHERE gateway_id = ?", [now, gatewayID])
		}
		if let parentID = record.parentOperationID, record.step == 1,
			let deleteStep = try connection.queryOne(
				"SELECT gateway_id FROM remote_outbox WHERE parent_operation_id = ? AND step = 2", [parentID],
				{ $0.optionalText(0) }), let source = deleteStep
		{
			// The copy of a cross-host move now shows the moved item; the source is hidden until it is deleted
			try projector.hide(source)
		}
		// Operations of the same item continue from the committed revision
		if let item = operation.item, let itemID = record.itemID {
			try connection.run(
				"""
				UPDATE remote_outbox SET base_revision = ?, gateway_id = ?,
					subtree_generation = CASE WHEN kind IN ('move', 'delete') THEN ? ELSE subtree_generation END
				WHERE item_id = ? AND (' ' || depends_on || ' ') LIKE ? AND state IN ('staged', 'uploading', 'uploaded')
					AND payload IS NULL
				""", [item.revision.rawValue, item.id, item.subtreeGeneration, itemID, "% \(record.operationID) %"])
		}
		try resolve(operationID: record.operationID, state: .committed, error: nil)
	}

	/// Fails the operation (and the ones depending on it)
	func fail(operationID: String, error: String, currentItem: RemoteItem?, serviceID: String? = nil)
		throws(IndexError)
	{
		if let currentItem, let record = try mutation(operationID), let service = serviceID ?? record.serviceID {
			try storeItems([currentItem], serviceID: service, sentFor: record)
		}
		try resolve(operationID: operationID, state: .failed, error: error)
	}

	/// Moves the operation to a final state, fails or cancels what depends on it, completes its compound operation,
	/// and resolves the overlays it held
	func resolve(operationID: String, state: MutationState, error: String?) throws(IndexError) {
		guard let record = try mutation(operationID) else {
			return
		}
		// A final operation needs its upload's token no more
		try IndexStore.releaseUploadTokens(connection, where: "operation_id = ?", [operationID])
		try connection.run(
			"""
			UPDATE remote_outbox SET state = ?, last_error = COALESCE(?, last_error), claim_owner = NULL,
				claim_expires = NULL, not_before = NULL, updated_at = ?
			WHERE operation_id = ?
			""", [state.rawValue, error, now, operationID])
		if state == .committed, record.kind == .crossHostMove, let itemID = record.itemID,
			let remote = try remoteState(ofItem: itemID)
		{
			// Changes made while the item moved continue from the copy's revision
			try connection.run(
				"""
				UPDATE remote_outbox SET base_revision = ?, gateway_id = ?
				WHERE item_id = ? AND (' ' || depends_on || ' ') LIKE ? AND state IN ('staged', 'uploading', 'uploaded')
					AND payload IS NULL
				""", [remote.revision.rawValue, remote.gatewayID, itemID, "% \(operationID) %"])
		}
		if state == .canceled {
			let unsentSteps = "parent_operation_id = ? AND state IN ('staged', 'uploading', 'uploaded')"
			try IndexStore.releaseUploadTokens(connection, where: unsentSteps, [operationID])
			try connection.run(
				"UPDATE remote_outbox SET state = 'canceled', updated_at = ? WHERE \(unsentSteps)", [now, operationID])
		}

		// The compound operation follows its step before the steps that depend on this one are resolved, so that it
		// reports why this step ended (not the consequence for the next step)
		if let parentID = record.parentOperationID, let parent = try mutation(parentID), !parent.state.isTerminal {
			let steps = try connection.query(
				"SELECT state FROM remote_outbox WHERE parent_operation_id = ?", [parentID],
				{ MutationState(rawValue: $0.text(0)) ?? .failed })
			if steps.allSatisfy({ $0 == .committed }) {
				try resolve(operationID: parentID, state: .committed, error: nil)
			}
			else if state != .committed {
				let message: String
				let parentState: MutationState
				switch state {
				case .canceled:
					(parentState, message) = (.canceled, "discarded")
				case .outcomeUnknown:
					(parentState, message) = (.outcomeUnknown, "the gateway may have lost the state of the move")
				default:
					parentState = .failed
					message =
						record.step == 2
						? "the copy was created, but the original could not be deleted: both remain"
						: "the item could not be copied to the other gateway"
				}
				try resolve(operationID: parentID, state: parentState, error: message)
			}
			else {
				try connection.run(
					"UPDATE remote_outbox SET state = 'submitted', updated_at = ? WHERE operation_id = ?", [now, parentID])
			}
		}

		if state != .committed {
			// What depends on it cannot proceed
			let dependents = try connection.query(
				"""
				SELECT operation_id FROM remote_outbox
				WHERE (' ' || depends_on || ' ') LIKE ? AND state IN ('staged', 'uploading', 'uploaded')
				""", ["% \(operationID) %"], { $0.text(0) })
			for dependent in dependents {
				try resolve(
					operationID: dependent, state: state == .canceled ? .canceled : .failed,
					error: state == .canceled
						? "an earlier change was discarded" : "an earlier change of the item was not accepted")
			}
		}

		try resolveOverlays(of: record)
	}

	private func isUnresolved(_ operationID: String) throws(IndexError) -> Bool {
		try connection.queryOne(
			"SELECT \(IndexStore.unresolvedCondition) FROM remote_outbox WHERE operation_id = ?", [operationID],
			{ $0.bool(0) }) ?? false
	}

	/// Clears the overlays the operation held, unless an unresolved operation of the item still needs them, and shows
	/// the gateway's state of those items again
	private func resolveOverlays(of record: MutationRecord) throws(IndexError) {
		var itemIDs = try connection.query(
			"SELECT item_id FROM remote_overlays WHERE operation_id = ?", [record.operationID], { $0.int64(0) })
		if let itemID = record.itemID, record.parentOperationID == nil, !itemIDs.contains(itemID) {
			itemIDs.append(itemID)
		}
		var cleared: [(itemID: Int64, path: String)] = []
		for itemID in itemIDs {
			guard let overlay = try IndexStore.overlay(connection, itemID: itemID) else {
				continue
			}
			if let latest = try latestUnresolved(itemID: itemID) {
				// Another change of the item (or this one, rejected or of unknown outcome) still needs the overlay
				if latest.operationID != overlay.operationID {
					try connection.run(
						"UPDATE remote_overlays SET operation_id = ? WHERE item_id = ?", [latest.operationID, itemID])
				}
				continue
			}
			if overlay.operationID != record.operationID, try isUnresolved(overlay.operationID) {
				// Held by an operation of a directory above it
				continue
			}
			let path = try connection.queryOne("SELECT path FROM items WHERE id = ?", [itemID], { $0.text(0) }) ?? ""
			try connection.run("DELETE FROM remote_overlays WHERE item_id = ?", [itemID])
			cleared.append((itemID, path))
		}
		// Parents before children, so that every item finds its parent's row
		for entry in cleared.sorted(by: { $0.path.utf8.count < $1.path.utf8.count }) {
			if let gatewayID = try connection.queryOne(
				"SELECT gateway_id FROM remote_items WHERE item_id = ?", [entry.itemID], { $0.text(0) })
			{
				try projector.reproject(gatewayID)
				try connection.run(
					"UPDATE items SET pending = 0, pending_size = NULL, pending_mod_ns = NULL WHERE id = ? AND deleted = 1",
					[entry.itemID])
			}
			else {
				// Never on the gateway (a canceled or rejected creation)
				try connection.run(
					"""
					UPDATE items SET deleted = 1, deleted_at = COALESCE(deleted_at, ?), pending = 0, pending_size = NULL,
						pending_mod_ns = NULL, meta_rev = meta_rev + CASE WHEN deleted = 0 THEN 1 ELSE 0 END,
						changed_seq = CASE WHEN deleted = 0 THEN ? ELSE changed_seq END
					WHERE id = ?
					""", [now, projector.nextChangeSequence(), entry.itemID])
				projector.countVisibleChange()
			}
		}
		if record.kind == .crossHostMove,
			let source = try connection.queryOne(
				"SELECT gateway_id FROM remote_outbox WHERE parent_operation_id = ? AND step = 2",
				[record.operationID], { $0.optionalText(0) }) ?? nil
		{
			// A source the move did not delete is shown again (next to the copy, if that was made)
			try projector.reproject(source)
		}
	}
}
