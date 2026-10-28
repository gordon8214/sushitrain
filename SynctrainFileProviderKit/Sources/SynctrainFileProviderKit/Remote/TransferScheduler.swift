// Copyright (C) 2026 Gordon
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this file,
// You can obtain one at https://mozilla.org/MPL/2.0/.
import Foundation

/// Limits concurrent transfers and requests across the app and the extension through slots in the shared transfer
/// registry. Every slot is claimed in a short transaction and expires unless renewed, so the slots of a process that is
/// suspended or killed free up on their own. No lock is held while waiting or transferring.
public struct TransferScheduler: Sendable {
	public let store: IndexStore
	/// The process (or worker) the slots belong to
	public let owner: String
	public var limits: TransferLimits
	/// How long a slot lasts without renewal
	public var claimDuration: TimeInterval
	/// How long to wait before looking for a free slot again
	public var pollInterval: Duration

	public init(
		store: IndexStore, owner: String, limits: TransferLimits = .initial, claimDuration: TimeInterval = 60,
		pollInterval: Duration = .milliseconds(200)
	) {
		self.store = store
		self.owner = owner
		self.limits = limits
		self.claimDuration = claimDuration
		self.pollInterval = pollInterval
	}

	/// A slot held by this process
	public struct Slot: Sendable, Hashable {
		public let id: String
		public let slotClass: TransferSlotClass
	}

	/// Takes a slot of the class if one is free now
	public func tryAcquire(_ slotClass: TransferSlotClass, transferID: String? = nil, now: Date = Date())
		throws(IndexError) -> Slot?
	{
		try store.claimSlot(
			slotClass, limits: limits, owner: owner, transferID: transferID, duration: claimDuration, now: now
		).map { Slot(id: $0, slotClass: slotClass) }
	}

	/// Waits for a slot of the class (until the task is canceled)
	public func acquire(_ slotClass: TransferSlotClass, transferID: String? = nil) async throws(RemoteError) -> Slot {
		while true {
			do {
				if let slot = try tryAcquire(slotClass, transferID: transferID) {
					return slot
				}
			}
			catch {
				throw .store(error)
			}
			do {
				try await Task.sleep(for: pollInterval)
			}
			catch {
				throw .canceled
			}
		}
	}

	/// Extends the slot; false when it expired and may have been taken
	public func renew(_ slot: Slot) throws(IndexError) -> Bool {
		try store.renewSlot(slot.id, duration: claimDuration)
	}

	public func release(_ slot: Slot) {
		try? store.releaseSlot(slot.id)
	}

	/// Runs the body while holding a slot of the class, renewing it until the body returns
	public func withSlot<T: Sendable>(
		_ slotClass: TransferSlotClass, transferID: String? = nil,
		_ body: () async throws(RemoteError) -> T
	) async throws(RemoteError) -> T {
		let slot = try await acquire(slotClass, transferID: transferID)
		let renewal = Task { [self] in
			let interval = Duration.seconds(max(claimDuration / 3, 0.05))
			while !Task.isCancelled {
				try? await Task.sleep(for: interval)
				if Task.isCancelled {
					break
				}
				_ = try? renew(slot)
			}
		}
		defer {
			renewal.cancel()
			release(slot)
		}
		return try await body()
	}
}
