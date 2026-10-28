// Copyright (C) 2026 Gordon
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this file,
// You can obtain one at https://mozilla.org/MPL/2.0/.
import Foundation

/// The blocks of a transfer that were verified and written, stored durably as a bitmap
public struct BlockBitmap: Sendable, Hashable {
	public private(set) var count: Int
	private var bytes: [UInt8]

	public init(count: Int) {
		self.count = max(0, count)
		bytes = Array(repeating: 0, count: (self.count + 7) / 8)
	}

	init(count: Int, encoded: String?) {
		self.init(count: count)
		if let encoded, let data = Data(base64Encoded: encoded) {
			for (index, byte) in data.prefix(bytes.count).enumerated() {
				bytes[index] = byte
			}
		}
	}

	var encoded: String {
		Data(bytes).base64EncodedString()
	}

	public func contains(_ index: Int) -> Bool {
		guard index >= 0, index < count else {
			return false
		}
		return bytes[index / 8] & (1 << UInt8(index % 8)) != 0
	}

	public mutating func insert(_ index: Int) {
		guard index >= 0, index < count else {
			return
		}
		bytes[index / 8] |= 1 << UInt8(index % 8)
	}

	public var verifiedCount: Int {
		bytes.reduce(0) { $0 + $1.nonzeroBitCount }
	}

	public var isComplete: Bool {
		verifiedCount == count
	}
}

/// The kinds of transfer slots the scheduler hands out across the app and the extension
public enum TransferSlotClass: String, Sendable, CaseIterable {
	/// A content transfer task (a range of a download counts as one)
	case content
	/// An upload task; it counts as a content transfer as well
	case upload
	/// A metadata request
	case metadata
	case thumbnail
	/// A request for changes held until something changes; long polls have slots of their own, so that they never
	/// delay other metadata requests (such as submitting operations)
	case longPoll
}

/// How many slots of each class may be taken at once, across both processes
public struct TransferLimits: Sendable, Hashable {
	public var contentTransfers: Int
	public var uploads: Int
	public var metadataRequests: Int
	public var thumbnails: Int
	public var longPolls: Int

	public init(
		contentTransfers: Int = 4, uploads: Int = 2, metadataRequests: Int = 2, thumbnails: Int = 1, longPolls: Int = 2
	) {
		self.contentTransfers = contentTransfers
		self.uploads = uploads
		self.metadataRequests = metadataRequests
		self.thumbnails = thumbnails
		self.longPolls = longPolls
	}

	public static let initial = TransferLimits()
	/// Fewer transfers under memory pressure
	public static let reduced = TransferLimits(
		contentTransfers: 2, uploads: 1, metadataRequests: 1, thumbnails: 0, longPolls: 1)
}

extension IndexStore {
	static let transferColumns = """
		transfer_id, kind, item_id, operation_id, lease_id, token_ref, content_id, etag, total_size, block_count, \
		verified_count, manifest_verified, owner, claim_expires, session_identifier, task_identifier, task_description, \
		task_offset, staging_path, state, last_error, created_at, updated_at
		"""

	static func transfer(from row: SQLiteStatement) -> TransferRecord {
		TransferRecord(
			transferID: row.text(0), kind: TransferKind(rawValue: row.text(1)) ?? .download, itemID: row.optionalInt64(2),
			operationID: row.optionalText(3), leaseID: row.optionalText(4), tokenReference: row.optionalText(5),
			contentID: row.optionalText(6).map { ContentIdentity($0) }, entityTag: row.optionalText(7),
			totalSize: row.int64(8), blockCount: Int(row.int64(9)), verifiedBlockCount: Int(row.int64(10)),
			isManifestVerified: row.bool(11), owner: row.optionalText(12),
			claimExpires: row.isNull(13) ? nil : Date(timeIntervalSince1970: row.double(13)),
			sessionIdentifier: row.optionalText(14), taskIdentifier: row.optionalInt64(15).map { Int($0) },
			taskDescription: row.optionalText(16), taskOffset: row.optionalInt64(17), stagingPath: row.optionalText(18),
			state: TransferState(rawValue: row.text(19)) ?? .interrupted, lastError: row.optionalText(20),
			createdAt: Date(timeIntervalSince1970: row.double(21)), updatedAt: Date(timeIntervalSince1970: row.double(22)))
	}

	// MARK: - Transfers

	/// The transfer of the item's content with the identity, creating it when there is none. A transfer of another
	/// identity of the item is replaced (its staging directory is returned in `obsolete`, to remove).
	public func downloadTransfer(itemID: Int64, contentID: ContentIdentity, totalSize: Int64)
		throws(IndexError) -> (transfer: TransferRecord, obsolete: [String])
	{
		try transaction { connection throws(IndexError) in
			let existing = try connection.query(
				"SELECT \(Self.transferColumns) FROM remote_transfers WHERE item_id = ? AND kind = 'download'", [itemID],
				Self.transfer)
			var obsolete: [String] = []
			for transfer in existing where transfer.contentID != contentID {
				try Self.releaseTokens(connection, [transfer.tokenReference])
				try connection.run("DELETE FROM remote_transfers WHERE transfer_id = ?", [transfer.transferID])
				if let path = transfer.stagingPath {
					obsolete.append(path)
				}
			}
			if let current = existing.first(where: { $0.contentID == contentID }) {
				return (current, obsolete)
			}
			let transferID = UUID().uuidString.lowercased()
			let now = Date().timeIntervalSince1970
			try connection.run(
				"""
				INSERT INTO remote_transfers (transfer_id, kind, item_id, content_id, total_size, staging_path, state,
					created_at, updated_at)
				VALUES (?, 'download', ?, ?, ?, ?, 'pending', ?, ?)
				""",
				[
					transferID, itemID, contentID.rawValue, totalSize, "\(RemoteStorage.transfersDirectory)/\(transferID)",
					now, now,
				])
			guard
				let transfer = try connection.queryOne(
					"SELECT \(Self.transferColumns) FROM remote_transfers WHERE transfer_id = ?", [transferID],
					Self.transfer)
			else {
				throw .notFound
			}
			return (transfer, obsolete)
		}
	}

	// MARK: - Background uploads

	/// How long a background task's upload stays its own without word from it: a transfer whose task was assigned
	/// longer ago is taken for lost (the process of its session reconciles it when it runs again), so that the
	/// operation goes on without it
	public static let backgroundUploadLifetime: TimeInterval = 86_400

	/// Registers the background upload of the rest of an operation's tus upload, claimed by the worker until a task
	/// takes it over (`assignTask(claimedBy:)`). An operation has one background task at a time: nil while a task runs
	/// on the same upload, or a worker is about to start one. Earlier upload transfers of the operation that nothing runs
	/// are removed (their tokens are released, and their directories returned in `obsolete`, to remove). For an upload,
	/// `lease_id` holds the tus upload the task appends to, `token_ref` its scoped token, and `staging_path` the
	/// transfer's directory (where the rest is copied).
	public func claimUploadTransfer(
		operationID: String, itemID: Int64?, uploadID: String, tokenReference: String?, totalSize: Int64,
		owner: String, duration: TimeInterval, now: Date = Date()
	) throws(IndexError) -> (transfer: TransferRecord?, obsolete: [String]) {
		try transaction { connection throws(IndexError) in
			let existing = try connection.query(
				"SELECT \(Self.transferColumns) FROM remote_transfers WHERE operation_id = ? AND kind = 'upload'",
				[operationID], Self.transfer)
			if existing.contains(where: { $0.leaseID == uploadID && Self.isBusy($0, now: now) }) {
				return (nil, [])
			}
			var obsolete: [String] = []
			// A task of an upload the operation no longer uses ends on its own; its transfer stays until then
			for transfer in existing where !Self.isBusy(transfer, now: now) {
				try Self.releaseTransferTokens(connection, where: "transfer_id = ?", [transfer.transferID])
				try connection.run("DELETE FROM remote_transfers WHERE transfer_id = ?", [transfer.transferID])
				if let path = transfer.stagingPath {
					obsolete.append(path)
				}
			}
			let transferID = UUID().uuidString.lowercased()
			let time = now.timeIntervalSince1970
			try connection.run(
				"""
				INSERT INTO remote_transfers (transfer_id, kind, item_id, operation_id, lease_id, token_ref, total_size,
					owner, claim_expires, staging_path, state, created_at, updated_at)
				VALUES (?, 'upload', ?, ?, ?, ?, ?, ?, ?, ?, 'active', ?, ?)
				""",
				[
					transferID, itemID, operationID, uploadID, tokenReference, totalSize, owner, time + duration,
					"\(RemoteStorage.transfersDirectory)/\(transferID)", time, time,
				])
			let transfer = try connection.queryOne(
				"SELECT \(Self.transferColumns) FROM remote_transfers WHERE transfer_id = ?", [transferID], Self.transfer)
			return (transfer, obsolete)
		}
	}

	/// Whether a task runs the transfer (assigned less than `backgroundUploadLifetime` ago), or a worker holds a current
	/// claim on it (and may be about to start one)
	static func isBusy(_ transfer: TransferRecord, now: Date) -> Bool {
		if transfer.taskDescription != nil {
			return now.timeIntervalSince(transfer.updatedAt) < backgroundUploadLifetime
		}
		guard transfer.owner != nil, let expires = transfer.claimExpires else {
			return false
		}
		return expires > now
	}

	/// The upload transfers of an operation (at most one runs on its current upload)
	public func uploadTransfers(operationID: String) throws(IndexError) -> [TransferRecord] {
		try withConnection { connection throws(IndexError) in
			try connection.query(
				"""
				SELECT \(Self.transferColumns) FROM remote_transfers WHERE operation_id = ? AND kind = 'upload'
				ORDER BY created_at
				""", [operationID], Self.transfer)
		}
	}

	/// The transfer of the background task that uploads the rest of the operation's current upload now, if one does
	public func runningBackgroundUpload(of record: MutationRecord, now: Date = Date()) throws(IndexError)
		-> TransferRecord?
	{
		guard let uploadID = record.uploadID else {
			return nil
		}
		return try uploadTransfers(operationID: record.operationID).first {
			$0.leaseID == uploadID && Self.isBusy($0, now: now)
		}
	}

	/// Removes the upload transfers that no task runs and no worker holds (their task finished, failed or was lost, or
	/// was never started), releasing their tokens. Returns their directories, to remove. The bytes are the gateway's
	/// business once a task ended: the outbox asks it for the offset.
	public func removeIdleUploadTransfers(now: Date = Date()) throws(IndexError) -> [String] {
		try transaction { connection throws(IndexError) in
			let idle = try connection.query(
				"""
				SELECT \(Self.transferColumns) FROM remote_transfers
				WHERE kind = 'upload' AND task_description IS NULL AND (owner IS NULL OR claim_expires IS NULL
					OR claim_expires <= ?)
				""", [now.timeIntervalSince1970], Self.transfer)
			var paths: [String] = []
			for transfer in idle {
				try Self.releaseTransferTokens(connection, where: "transfer_id = ?", [transfer.transferID])
				try connection.run("DELETE FROM remote_transfers WHERE transfer_id = ?", [transfer.transferID])
				if let path = transfer.stagingPath {
					paths.append(path)
				}
			}
			return paths
		}
	}

	/// Releases what earlier instances of a process left claimed (their owners start with the prefix; the current
	/// instance's start with `current`): transfer claims, which a new instance would otherwise wait out, and transfer
	/// slots. Transfers a background task took over are not claims of an instance, and stay.
	public func releaseClaims(ownerPrefix prefix: String, except current: String) throws(IndexError) {
		try transaction { connection throws(IndexError) in
			let pattern = Self.likePattern(prefix)
			let currentPattern = Self.likePattern(current)
			try connection.run(
				"""
				UPDATE remote_transfers SET owner = NULL, claim_expires = NULL, state = 'interrupted', updated_at = ?
				WHERE task_description IS NULL AND owner LIKE ? ESCAPE '\\' AND owner NOT LIKE ? ESCAPE '\\'
				""", [Date().timeIntervalSince1970, pattern, currentPattern])
			try connection.run(
				"""
				DELETE FROM remote_transfer_slots WHERE owner LIKE ? ESCAPE '\\' AND owner NOT LIKE ? ESCAPE '\\'
				""", [pattern, currentPattern])
		}
	}

	/// A `LIKE` pattern matching the strings that start with the prefix
	private static func likePattern(_ prefix: String) -> String {
		prefix.replacingOccurrences(of: "\\", with: "\\\\").replacingOccurrences(of: "%", with: "\\%")
			.replacingOccurrences(of: "_", with: "\\_") + "%"
	}

	public func transfer(id: String) throws(IndexError) -> TransferRecord? {
		try withConnection { connection throws(IndexError) in
			try connection.queryOne(
				"SELECT \(Self.transferColumns) FROM remote_transfers WHERE transfer_id = ?", [id], Self.transfer)
		}
	}

	public func transfer(taskDescription: String) throws(IndexError) -> TransferRecord? {
		try withConnection { connection throws(IndexError) in
			try connection.queryOne(
				"SELECT \(Self.transferColumns) FROM remote_transfers WHERE task_description = ?", [taskDescription],
				Self.transfer)
		}
	}

	/// Transfers with a task in the background session
	public func transfers(sessionIdentifier: String) throws(IndexError) -> [TransferRecord] {
		try withConnection { connection throws(IndexError) in
			try connection.query(
				"SELECT \(Self.transferColumns) FROM remote_transfers WHERE session_identifier = ? ORDER BY created_at",
				[sessionIdentifier], Self.transfer)
		}
	}

	public func transfers(kind: TransferKind? = nil, limit: Int = 100) throws(IndexError) -> [TransferRecord] {
		try withConnection { connection throws(IndexError) in
			if let kind {
				return try connection.query(
					"SELECT \(Self.transferColumns) FROM remote_transfers WHERE kind = ? ORDER BY created_at LIMIT ?",
					[kind.rawValue, limit], Self.transfer)
			}
			return try connection.query(
				"SELECT \(Self.transferColumns) FROM remote_transfers ORDER BY created_at LIMIT ?", [limit],
				Self.transfer)
		}
	}

	/// Claims a transfer for one worker. Fails while any claim is current (the worker's own too: a claim is exclusive,
	/// so a worker claims each transfer once and extends it with `renewTransferClaim`), or a background task runs it.
	public func claimTransfer(id: String, owner: String, duration: TimeInterval, now: Date = Date())
		throws(IndexError) -> Bool
	{
		try withConnection { connection throws(IndexError) in
			try connection.run(
				"""
				UPDATE remote_transfers SET owner = ?, claim_expires = ?, state = 'active', updated_at = ?
				WHERE transfer_id = ? AND (owner IS NULL OR claim_expires IS NULL OR claim_expires <= ?)
					AND task_description IS NULL
				""",
				[
					owner, now.timeIntervalSince1970 + duration, now.timeIntervalSince1970, id,
					now.timeIntervalSince1970,
				])
			return connection.changes > 0
		}
	}

	/// Extends the worker's claim. False when the worker no longer holds it (it expired and another worker or a
	/// background task took the transfer over, or the transfer is gone): the worker must stop.
	public func renewTransferClaim(id: String, owner: String, duration: TimeInterval, now: Date = Date())
		throws(IndexError) -> Bool
	{
		try withConnection { connection throws(IndexError) in
			try connection.run(
				"""
				UPDATE remote_transfers SET claim_expires = ?, updated_at = ?
				WHERE transfer_id = ? AND owner = ? AND task_description IS NULL
				""", [now.timeIntervalSince1970 + duration, now.timeIntervalSince1970, id, owner])
			return connection.changes > 0
		}
	}

	public func releaseTransfer(id: String, owner: String, state: TransferState = .interrupted, error: String? = nil)
		throws(IndexError)
	{
		try withConnection { connection throws(IndexError) in
			try connection.run(
				"""
				UPDATE remote_transfers SET owner = NULL, claim_expires = NULL, state = ?, last_error = ?, updated_at = ?
				WHERE transfer_id = ? AND owner = ?
				""", [state.rawValue, error, Date().timeIntervalSince1970, id, owner])
		}
	}

	/// Records the lease the transfer reads from (a new lease of the same content keeps the verified blocks)
	public func setTransferLease(id: String, leaseID: String, tokenReference: String?) throws(IndexError) {
		try transaction { connection throws(IndexError) in
			// A token the transfer no longer refers to is removed from the credential store
			try Self.releaseTransferTokens(
				connection, where: "transfer_id = ? AND token_ref IS NOT ?", [id, tokenReference])
			try connection.run(
				"UPDATE remote_transfers SET lease_id = ?, token_ref = ?, updated_at = ? WHERE transfer_id = ?",
				[leaseID, tokenReference, Date().timeIntervalSince1970, id])
		}
	}

	/// Records the strong validator of the first response, sent as `If-Range` from then on
	public func setTransferEntityTag(id: String, entityTag: String) throws(IndexError) {
		try withConnection { connection throws(IndexError) in
			try connection.run(
				"UPDATE remote_transfers SET etag = COALESCE(etag, ?) WHERE transfer_id = ?", [entityTag, id])
		}
	}

	/// Records that the manifest was stored and matches the content identity
	public func setManifestVerified(id: String, blockCount: Int) throws(IndexError) {
		try withConnection { connection throws(IndexError) in
			try connection.run(
				"""
				UPDATE remote_transfers SET manifest_verified = 1, block_count = ?,
					verified_blocks = CASE WHEN block_count = ? THEN verified_blocks END,
					verified_count = CASE WHEN block_count = ? THEN verified_count ELSE 0 END
				WHERE transfer_id = ?
				""", [blockCount, blockCount, blockCount, id])
		}
	}

	/// The blocks verified so far
	public func verifiedBlocks(transferID: String) throws(IndexError) -> BlockBitmap {
		try withConnection { connection throws(IndexError) in
			try connection.queryOne(
				"SELECT block_count, verified_blocks FROM remote_transfers WHERE transfer_id = ?", [transferID]
			) { BlockBitmap(count: Int($0.int64(0)), encoded: $0.optionalText(1)) } ?? BlockBitmap(count: 0)
		}
	}

	/// Records blocks verified and written to the partial file (after the file was synced). Returns the bitmap.
	@discardableResult
	public func recordVerifiedBlocks(transferID: String, blocks: [Int]) throws(IndexError) -> BlockBitmap {
		try transaction { connection throws(IndexError) in
			guard
				var bitmap = try connection.queryOne(
					"SELECT block_count, verified_blocks FROM remote_transfers WHERE transfer_id = ?", [transferID],
					{ BlockBitmap(count: Int($0.int64(0)), encoded: $0.optionalText(1)) })
			else {
				throw .notFound
			}
			for block in blocks {
				bitmap.insert(block)
			}
			try connection.run(
				"""
				UPDATE remote_transfers SET verified_blocks = ?, verified_count = ?, updated_at = ? WHERE transfer_id = ?
				""", [bitmap.encoded, bitmap.verifiedCount, Date().timeIntervalSince1970, transferID])
			return bitmap
		}
	}

	/// Forgets a transfer's progress (its validator changed: the content must be transferred anew)
	public func resetTransferProgress(id: String) throws(IndexError) {
		try transaction { connection throws(IndexError) in
			try Self.releaseTransferTokens(connection, where: "transfer_id = ?", [id])
			try connection.run(
				"""
				UPDATE remote_transfers SET etag = NULL, verified_blocks = NULL, verified_count = 0, manifest_verified = 0,
					lease_id = NULL, token_ref = NULL, updated_at = ?
				WHERE transfer_id = ?
				""", [Date().timeIntervalSince1970, id])
		}
	}

	/// Records the background task running the transfer. With `claimedBy`, the task takes the claim of that worker
	/// over in the same update, and nothing is recorded (false) unless the worker still holds the claim.
	@discardableResult
	public func assignTask(
		transferID: String, sessionIdentifier: String, taskIdentifier: Int, taskDescription: String, offset: Int64?,
		claimedBy worker: String? = nil
	) throws(IndexError) -> Bool {
		try withConnection { connection throws(IndexError) in
			try connection.run(
				"""
				UPDATE remote_transfers SET session_identifier = ?, task_identifier = ?, task_description = ?,
					task_offset = ?, owner = ?, claim_expires = NULL, state = 'active', updated_at = ?
				WHERE transfer_id = ? AND (? IS NULL OR (owner = ? AND task_description IS NULL))
				""",
				[
					sessionIdentifier, taskIdentifier, taskDescription, offset, sessionIdentifier,
					Date().timeIntervalSince1970, transferID, worker, worker,
				])
			return connection.changes > 0
		}
	}

	/// Records how a background task ended, and detaches it from the transfer. A transfer without a task (which a
	/// worker may have claimed since) is left alone.
	public func finishTask(transferID: String, state: TransferState, error: String?) throws(IndexError) {
		try withConnection { connection throws(IndexError) in
			try connection.run(
				"""
				UPDATE remote_transfers SET state = ?, last_error = ?, task_identifier = NULL, task_description = NULL,
					owner = NULL, claim_expires = NULL, updated_at = ?
				WHERE transfer_id = ? AND task_description IS NOT NULL
				""", [state.rawValue, error, Date().timeIntervalSince1970, transferID])
		}
	}

	public func removeTransfer(id: String) throws(IndexError) {
		try transaction { connection throws(IndexError) in
			try Self.releaseTransferTokens(connection, where: "transfer_id = ?", [id])
			try connection.run("DELETE FROM remote_transfers WHERE transfer_id = ?", [id])
			try connection.run("DELETE FROM remote_transfer_slots WHERE transfer_id = ?", [id])
		}
	}

	// MARK: - Slots

	/// Takes a slot of the class if one is free, in one short transaction. Expired slots (of a process that was
	/// suspended or killed) are free again.
	func claimSlot(
		_ slotClass: TransferSlotClass, limits: TransferLimits, owner: String, transferID: String?,
		duration: TimeInterval, now: Date = Date()
	) throws(IndexError) -> String? {
		try transaction { connection throws(IndexError) in
			let time = now.timeIntervalSince1970
			try connection.run("DELETE FROM remote_transfer_slots WHERE expires <= ?", [time])
			func count(_ classes: [TransferSlotClass]) throws(IndexError) -> Int {
				let placeholders = Array(repeating: "?", count: classes.count).joined(separator: ", ")
				return Int(
					try connection.queryOne(
						"SELECT COUNT(*) FROM remote_transfer_slots WHERE slot_class IN (\(placeholders))",
						classes.map(\.rawValue), { $0.int64(0) }) ?? 0)
			}
			let free: Bool
			switch slotClass {
			case .content:
				free = try count([.content, .upload]) < limits.contentTransfers
			case .upload:
				let uploads = try count([.upload])
				let content = try count([.content, .upload])
				free = uploads < limits.uploads && content < limits.contentTransfers
			case .metadata:
				free = try count([.metadata]) < limits.metadataRequests
			case .thumbnail:
				free = try count([.thumbnail]) < limits.thumbnails
			case .longPoll:
				free = try count([.longPoll]) < limits.longPolls
			}
			guard free else {
				return nil
			}
			let slotID = UUID().uuidString.lowercased()
			try connection.run(
				"""
				INSERT INTO remote_transfer_slots (slot_id, slot_class, owner, transfer_id, expires)
				VALUES (?, ?, ?, ?, ?)
				""", [slotID, slotClass.rawValue, owner, transferID, time + duration])
			return slotID
		}
	}

	/// Extends a slot; false when it expired and was freed
	func renewSlot(_ slotID: String, duration: TimeInterval, now: Date = Date()) throws(IndexError) -> Bool {
		try withConnection { connection throws(IndexError) in
			try connection.run(
				"UPDATE remote_transfer_slots SET expires = ? WHERE slot_id = ? AND expires > ?",
				[now.timeIntervalSince1970 + duration, slotID, now.timeIntervalSince1970])
			return connection.changes > 0
		}
	}

	func releaseSlot(_ slotID: String) throws(IndexError) {
		try withConnection { connection throws(IndexError) in
			try connection.run("DELETE FROM remote_transfer_slots WHERE slot_id = ?", [slotID])
		}
	}

	/// The number of slots of the class taken now (by both processes)
	public func activeTransferSlots(_ slotClass: TransferSlotClass, now: Date = Date()) throws(IndexError) -> Int {
		try withConnection { connection throws(IndexError) in
			Int(
				try connection.queryOne(
					"SELECT COUNT(*) FROM remote_transfer_slots WHERE slot_class = ? AND expires > ?",
					[slotClass.rawValue, now.timeIntervalSince1970], { $0.int64(0) }) ?? 0)
		}
	}
}
