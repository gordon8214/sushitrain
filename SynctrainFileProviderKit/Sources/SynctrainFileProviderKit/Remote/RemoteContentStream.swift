// Copyright (C) 2026 Gordon
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this file,
// You can obtain one at https://mozilla.org/MPL/2.0/.
import Foundation

/// Reads the contents of one version of a file from its gateway in parts, for playing it while it downloads. Unlike
/// `RemoteDownloader`, nothing here is verified block by block, so what is read is never stored as a cached copy: it is
/// only to be shown, as with the previews of versions (invariant 12 of the API).
public actor RemoteContentStream {
	/// The most that is read at a time: a player asks for much more than it needs right now
	public static let maximumChunkSize: Int64 = 1 << 20

	private let client: RemoteFileClient
	private let scheduler: TransferScheduler?
	private let tracker: LeaseTracker
	private var entityTag: String? = nil
	private var size: Int64 = 0

	public init(client: RemoteFileClient, scheduler: TransferScheduler?, request: RemoteDownloader.Request) {
		self.client = client
		self.scheduler = scheduler
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
