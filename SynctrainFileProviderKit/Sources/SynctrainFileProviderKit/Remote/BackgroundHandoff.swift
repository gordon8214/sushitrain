// Copyright (C) 2026 Gordon
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this file,
// You can obtain one at https://mozilla.org/MPL/2.0/.
import Foundation
import os

/// When the foreground hands the rest of an upload to the process's background session (`BackgroundTransfers`), so that
/// it goes on while neither the app nor the extension runs. The outbox and its uploader check it before every chunk:
/// the rest is handed over once a handoff was requested (the process is about to lose its execution time), or once the
/// upload ran in the foreground for longer than the budget. Only a rest of at least `minimumRemainder` bytes is handed
/// over; a shorter one is sent in the foreground. A request interrupts the chunk in flight (sent with a checksum, it
/// appends all of its bytes or none), so that the handoff does not wait for it.
public final class BackgroundHandoff: Sendable {
	/// The session of this process, which runs the tasks
	public let transfers: BackgroundTransfers
	/// The smallest rest worth a background task
	public let minimumRemainder: Int64
	/// How long one upload runs in the foreground before its rest is handed over (nil: only when requested)
	public let foregroundBudget: TimeInterval?
	private let state = OSAllocatedUnfairLock(initialState: State())

	/// One chunk of 8 MiB, the foreground's chunk size
	public static let defaultMinimumRemainder: Int64 = 8 << 20

	private struct State {
		var isRequested = false
		/// Cancels the chunks in flight that a request interrupts
		var interruptions: [UUID: @Sendable () -> Void] = [:]
		/// Transfers started for handed over uploads, by operation ID
		var handedOver: [String: String] = [:]
	}

	public init(
		transfers: BackgroundTransfers, minimumRemainder: Int64 = BackgroundHandoff.defaultMinimumRemainder,
		foregroundBudget: TimeInterval? = nil
	) {
		self.transfers = transfers
		self.minimumRemainder = max(1, minimumRemainder)
		self.foregroundBudget = foregroundBudget
	}

	/// Asks uploads in progress to hand their rest over now (the process is about to lose its execution time), and the
	/// outbox not to start others. Interrupts their chunks in flight. Lasts for the life of this object.
	public func request() {
		let interruptions = state.withLock { state in
			state.isRequested = true
			return Array(state.interruptions.values)
		}
		for interrupt in interruptions {
			interrupt()
		}
	}

	public var isRequested: Bool {
		state.withLock { $0.isRequested }
	}

	/// The operations whose rest was handed over, with their transfers
	public var handedOver: [String: String] {
		state.withLock { $0.handedOver }
	}

	func didHandOver(operationID: String, transferID: String) {
		state.withLock { $0.handedOver[operationID] = transferID }
	}

	/// Whether a rest of the size goes to the background now, for an upload sent in the foreground since `start`
	func shouldHandOver(remaining: Int64, uploadingSince start: Date, now: Date = Date()) -> Bool {
		guard remaining >= minimumRemainder else {
			return false
		}
		if isRequested {
			return true
		}
		guard let foregroundBudget else {
			return false
		}
		return now.timeIntervalSince(start) >= foregroundBudget
	}

	/// Runs the body (a chunk) so that a request cancels it: it then throws `.canceled`, while the calling task goes
	/// on. Canceling the calling task cancels the body as well.
	func interruptible<T: Sendable>(_ body: @escaping @Sendable () async throws(RemoteError) -> T)
		async throws(RemoteError) -> T
	{
		let task = Task<Result<T, RemoteError>, Never> {
			do throws(RemoteError) {
				return .success(try await body())
			}
			catch {
				return .failure(error)
			}
		}
		let id = UUID()
		let requested = state.withLock { state in
			if !state.isRequested {
				state.interruptions[id] = { task.cancel() }
			}
			return state.isRequested
		}
		if requested {
			task.cancel()
		}
		defer {
			_ = state.withLock { $0.interruptions.removeValue(forKey: id) }
		}
		let result = await withTaskCancellationHandler {
			await task.value
		} onCancel: {
			task.cancel()
		}
		return try result.get()
	}
}
