// Copyright (C) 2026 Gordon
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this file,
// You can obtain one at https://mozilla.org/MPL/2.0/.
import Foundation
import os

/// Reads the contents of one version of a file from its gateway in parts, for playing it while it downloads. Unlike
/// `RemoteDownloader`, nothing here is verified block by block, so what is read is never stored as a cached copy: it is
/// only to be shown, as with the previews of versions (invariant 12 of the API).
public actor RemoteContentStream {
	/// The most that `read(at:length:)` reads at a time, and how far past the offset the gateway must have prepared the
	/// content before `stream(from:to:receive:)` asks for it
	public static let maximumChunkSize: Int64 = 1 << 20
	/// The most that one request of `stream(from:to:receive:)` asks for, so that it gives its transfer slot (shared with
	/// downloads and uploads) back now and then: as much as a range of `RemoteDownloader`
	public static let maximumRequestSize: Int64 = 8 << 20

	private let client: RemoteFileClient
	private let scheduler: TransferScheduler?
	private let tracker: LeaseTracker
	private var entityTag: String? = nil
	private var size: Int64 = 0
	private let requestSize: Int64

	public init(client: RemoteFileClient, scheduler: TransferScheduler?, request: RemoteDownloader.Request) {
		self.init(client: client, scheduler: scheduler, request: request, requestSize: Self.maximumRequestSize)
	}

	init(client: RemoteFileClient, scheduler: TransferScheduler?, request: RemoteDownloader.Request, requestSize: Int64) {
		self.client = client
		self.scheduler = scheduler
		self.requestSize = max(requestSize, 1)
		tracker = LeaseTracker(client: client, request: request)
	}

	/// Asks the gateway for the file, and returns its size
	public func open() async throws(RemoteError) -> Int64 {
		size = try await tracker.start().size
		return size
	}

	/// Up to `maximumChunkSize` bytes from the offset; empty at the end of the file. Waits while the gateway prepares
	/// them. A file that changed meanwhile ends the stream with `.revisionMismatch`.
	public func read(at offset: Int64, length: Int64) async throws(RemoteError) -> Data {
		let end = min(offset + min(length, Self.maximumChunkSize), size)
		guard offset >= 0, end > offset else {
			return Data()
		}
		var lease = try await tracker.ensureReady(through: end)
		do throws(RemoteError) {
			return try await read(offset..<end, lease: lease)
		}
		catch .leaseExpired {
			lease = try await tracker.renew()
			return try await read(offset..<end, lease: lease)
		}
	}

	/// Hands the bytes from `start` to `end` over in order, chunk by chunk as they arrive, in as few requests as it can:
	/// each for up to `maximumRequestSize` bytes of what the gateway has prepared (for content on its disk, all of it),
	/// waiting while it prepares until it has at least `maximumChunkSize` past the offset. A response that stops after
	/// handing bytes over continues where it stopped. A file that changed meanwhile ends the stream with
	/// `.revisionMismatch`. `receive` runs on a queue that every request to the gateway shares, so it must return
	/// quickly; it returns false to stop the stream (which then throws `.canceled`), as canceling the task does.
	public func stream(from start: Int64, to end: Int64, receive: @escaping @Sendable (Data) -> Bool)
		async throws(RemoteError)
	{
		let end = min(end, size)
		var offset = max(start, 0)
		var renewed = false
		while offset < end {
			let lease = try await tracker.ensureReady(through: min(offset + Self.maximumChunkSize, end))
			let prepared = lease.preparation.state == .ready ? end : min(end, lease.preparation.readyBytes)
			let last = min(prepared, offset + requestSize) - 1
			let entityTag = entityTag ?? lease.contentID.entityTag
			let received = OSAllocatedUnfairLock(initialState: Int64(0))
			let client = client
			let body = { () async throws(RemoteError) in
				try await client.streamContent(leaseID: lease.lease, range: offset...last, entityTag: entityTag) {
					data in
					received.withLock { $0 += Int64(data.count) }
					return receive(data)
				}
			}
			var failure: RemoteError?
			do throws(RemoteError) {
				if let scheduler {
					try await scheduler.withSlot(.content, body)
				}
				else {
					try await body()
				}
			}
			catch {
				failure = error
			}
			let count = received.withLock { $0 }
			if count > 0 {
				self.entityTag = entityTag
				offset += count
				renewed = false
			}
			switch failure {
			case nil where count == 0:
				throw .protocolViolation("the gateway sent none of the range")
			case nil:
				continue
			case .leaseExpired? where !renewed:
				_ = try await tracker.renew()
				renewed = true
			case .unreachable? where count > 0:
				// The connection dropped part of the way: what arrived is handed over, the rest is asked for again
				continue
			case let error?:
				throw error
			}
		}
	}

	private func read(_ range: Range<Int64>, lease: ReadLease) async throws(RemoteError) -> Data {
		let client = client
		let closed = range.lowerBound...(range.upperBound - 1)
		let ifRange = entityTag
		let body = { () async throws(RemoteError) -> (data: Data, entityTag: String?) in
			try await client.readContent(leaseID: lease.lease, range: closed, ifRange: ifRange)
		}
		let result: (data: Data, entityTag: String?)
		if let scheduler {
			result = try await scheduler.withSlot(.content, body)
		}
		else {
			result = try await body()
		}
		if let current = result.entityTag {
			if let known = entityTag, known != current {
				throw .revisionMismatch(current: nil)
			}
			entityTag = current
		}
		return result.data
	}
}
