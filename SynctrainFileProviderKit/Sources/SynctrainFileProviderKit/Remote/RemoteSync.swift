// Copyright (C) 2026 Gordon
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this file,
// You can obtain one at https://mozilla.org/MPL/2.0/.
import Foundation

/// Keeps the metadata of folders accessed through a gateway up to date: snapshots (page by page, then the changes after
/// the snapshot's start), and incremental changes. Every page is stored together with the position it leads to, in one
/// transaction, so a cursor never moves past changes that were not stored, and an interrupted snapshot continues
/// where it stopped. Nothing is inferred from a page that could not be fetched.
public struct RemoteSync: Sendable {
	public let store: IndexStore
	public let client: RemoteFileClient
	/// Limits metadata requests across the app and the extension (a long poll occupies a slot)
	public let scheduler: TransferScheduler?
	/// Records per page (the gateway's default when nil)
	public var pageLimit: Int?
	/// How many fresh snapshots one refresh may start (after expired sessions or cursors) before it gives up
	public var maximumRestarts = 3

	public init(store: IndexStore, client: RemoteFileClient, scheduler: TransferScheduler? = nil, pageLimit: Int? = nil) {
		self.store = store
		self.client = client
		self.scheduler = scheduler
		self.pageLimit = pageLimit
	}

	private func metadata<T: Sendable>(
		_ slotClass: TransferSlotClass = .metadata, _ body: () async throws(RemoteError) -> T
	) async throws(RemoteError) -> T {
		guard let scheduler else {
			return try await body()
		}
		return try await scheduler.withSlot(slotClass, body)
	}

	/// A page that was not stored although nobody else moved the folder's position cannot be stored by asking for it
	/// again: the gateway's answer does not continue the session
	private func checkProgress(_ result: RemoteApplyResult, folderID: String, from state: RemoteSyncState)
		throws(RemoteError)
	{
		guard !result.stored else {
			return
		}
		if try stored({ () throws(IndexError) in try store.remoteSyncState(folderID: folderID) }) == state {
			throw .protocolViolation("the gateway's page does not continue where the folder's metadata stands")
		}
	}

	private func stored<T>(_ body: () throws(IndexError) -> T) throws(RemoteError) -> T {
		do {
			return try body()
		}
		catch {
			throw .store(error)
		}
	}

	/// Fetches the gateway's capabilities and records its authority generation: when that changed, its folders take a
	/// fresh snapshot and its unconfirmed operations become outcome-unknown. An announced certificate rotation is
	/// recorded as the next accepted pin.
	@discardableResult
	public func refreshCapabilities() async throws(RemoteError) -> Capabilities {
		let capabilities = try await metadata { () async throws(RemoteError) -> Capabilities in
			try await client.capabilities()
		}
		guard capabilities.serviceID == client.endpoint.serviceID else {
			throw .protocolViolation("the gateway is another service (\(capabilities.serviceID))")
		}
		try stored { () throws(IndexError) in
			try store.recordAuthorityGeneration(capabilities.authorityGeneration, serviceID: capabilities.serviceID)
			if let next = capabilities.certificate?.next?.spki, client.endpoint.pinnedSPKIHash != nil,
				var endpoint = try store.remoteEndpoint(serviceID: capabilities.serviceID),
				endpoint.nextPinnedSPKIHash != next
			{
				// Announced over the current pinned connection, so it can be trusted once it takes effect
				endpoint.nextPinnedSPKIHash = next
				try store.saveRemoteEndpoint(endpoint)
			}
		}
		return capabilities
	}

	/// Fetches the folders the client may access and records the effective permissions of the bound ones
	@discardableResult
	public func refreshFolders() async throws(RemoteError) -> [RemoteFolder] {
		let folders = try await metadata { () async throws(RemoteError) -> [RemoteFolder] in
			try await client.folders()
		}
		try stored { () throws(IndexError) in
			for binding in try store.remoteBindings()
			where binding.isRemote && binding.serviceID == client.endpoint.serviceID {
				let remote = folders.first { $0.id == binding.remoteFolderID }
				let permissions = remote?.permissions ?? RemotePermissions(read: false, write: false)
				if permissions != binding.permissions {
					try store.updateRemotePermissions(permissions, folderID: binding.folderID)
				}
			}
		}
		return folders
	}

	/// Brings the folder's metadata up to date: continues or starts a snapshot when one is needed, then applies the
	/// changes after the folder's cursor. With `wait`, the first request for changes is held up to that many seconds
	/// until something changes (only while the process has execution time). Returns whether anything the system shows
	/// changed.
	@discardableResult
	public func refresh(folderID: String, wait: Int? = nil) async throws(RemoteError) -> Bool {
		guard let binding = try stored({ () throws(IndexError) in try store.remoteBinding(folderID: folderID) }),
			binding.isRemote, binding.serviceID == client.endpoint.serviceID, let remoteFolderID = binding.remoteFolderID
		else {
			throw .store(.notAllowed("folder \(folderID) is not accessed through this gateway"))
		}
		var changed = false
		var restarts = 0
		var waited = false
		func restart() throws(RemoteError) {
			restarts += 1
			if restarts > maximumRestarts {
				throw .protocolViolation("the gateway keeps expiring the snapshot")
			}
		}

		while true {
			try Task.checkCancellation(or: .canceled)
			let state = try stored { () throws(IndexError) in try store.remoteSyncState(folderID: folderID) }

			if state.needsSnapshot {
				let session: RemoteSyncState.SnapshotSession
				if let current = state.snapshot {
					session = current
				}
				else {
					guard
						let started = try stored({ () throws(IndexError) in
							try store.beginRemoteSnapshot(folderID: folderID)
						}).snapshot
					else {
						continue
					}
					session = started
				}

				if let catchUpCursor = session.catchUpCursor {
					// All pages are stored: apply the changes after the snapshot's start
					let page: ChangesPage
					do {
						page = try await metadata { () async throws(RemoteError) -> ChangesPage in
							try await client.changes(folderID: remoteFolderID, cursor: catchUpCursor, limit: pageLimit)
						}
					}
					catch .cursorExpired {
						try restart()
						try stored { () throws(IndexError) in try store.resetRemoteCursor(folderID: folderID) }
						continue
					}
					guard !page.more || page.cursor != catchUpCursor else {
						throw .protocolViolation("the gateway has more changes, but its cursor does not advance")
					}
					let result = try stored { () throws(IndexError) in
						try store.applyRemoteChangesPage(page, folderID: folderID, after: catchUpCursor)
					}
					try checkProgress(result, folderID: folderID, from: state)
					changed = changed || result.changed > 0
					if result.completedSnapshot {
						// Complete up to the newest change
						return changed
					}
					continue
				}

				let page: SnapshotPage
				do {
					page = try await metadata { () async throws(RemoteError) -> SnapshotPage in
						try await client.snapshotPage(
							folderID: remoteFolderID, snapshot: session.snapshotID, page: session.nextPage,
							limit: pageLimit)
					}
				}
				catch .snapshotExpired {
					try restart()
					try stored { () throws(IndexError) in try store.abandonRemoteSnapshot(folderID: folderID) }
					continue
				}
				if let next = page.next, next == session.nextPage {
					throw .protocolViolation("the gateway's next snapshot page is the one it answered")
				}
				let result = try stored { () throws(IndexError) in
					try store.applyRemoteSnapshotPage(page, folderID: folderID, requestedPage: session.nextPage)
				}
				// A session that was begun in this pass is in the state read next, not in `state`: compared then
				if state.snapshot != nil {
					try checkProgress(result, folderID: folderID, from: state)
				}
				changed = changed || result.changed > 0
				continue
			}

			guard let cursor = state.cursor else {
				continue
			}
			let longPoll = waited ? nil : wait
			waited = true
			let page: ChangesPage
			do {
				// A long poll waits in a slot of its own, so that it does not hold back other metadata requests
				page = try await metadata((longPoll ?? 0) > 0 ? .longPoll : .metadata) {
					() async throws(RemoteError) -> ChangesPage in
					try await client.changes(folderID: remoteFolderID, cursor: cursor, limit: pageLimit, wait: longPoll)
				}
			}
			catch .cursorExpired {
				// A fresh snapshot, keeping overlays, outbox operations and staged files
				try restart()
				try stored { () throws(IndexError) in try store.resetRemoteCursor(folderID: folderID) }
				continue
			}
			guard !page.more || page.cursor != cursor else {
				throw .protocolViolation("the gateway has more changes, but its cursor does not advance")
			}
			let result = try stored { () throws(IndexError) in
				try store.applyRemoteChangesPage(page, folderID: folderID, after: cursor)
			}
			try checkProgress(result, folderID: folderID, from: state)
			changed = changed || result.changed > 0
			if result.stored, !page.more {
				return changed
			}
		}
	}

	/// Refreshes every folder bound to this gateway, after its capabilities. Returns the folders whose shown items
	/// changed.
	public func refreshAll() async throws(RemoteError) -> [String] {
		try await refreshCapabilities()
		try await refreshFolders()
		let bindings = try stored { () throws(IndexError) in try store.remoteBindings() }
		var changed: [String] = []
		for binding in bindings
		where binding.isRemote && binding.serviceID == client.endpoint.serviceID && binding.permissions.read {
			if try await refresh(folderID: binding.folderID) {
				changed.append(binding.folderID)
			}
		}
		return changed
	}
}

extension Task where Success == Never, Failure == Never {
	static func checkCancellation(or error: RemoteError) throws(RemoteError) {
		if isCancelled {
			throw error
		}
	}
}
