// Copyright (C) 2026 Gordon
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this file,
// You can obtain one at https://mozilla.org/MPL/2.0/.
import Foundation
import Testing

@testable import SynctrainFileProviderKit

/// A gateway's metadata, served from a script of answers per request
final class MetadataGateway: Sendable {
	let script = Locked<[String: [MockResponse]]>([:])
	let requests = Locked<[MockRequest]>([])

	/// Answers a request (by path and query, e.g. `snapshot?snapshot=snp_1&page=pg_2`) with the responses in turn
	func on(_ key: String, _ responses: MockResponse...) {
		script.update { $0[key, default: []] += responses }
	}

	func handle(_ request: MockRequest) -> MockResponse {
		requests.update { $0.append(request) }
		let prefix = "/v1/folders/\(testRemoteFolder)/"
		var key = request.path.hasPrefix(prefix) ? String(request.path.dropFirst(prefix.count)) : request.path
		if let query = request.url.query(percentEncoded: false), !query.isEmpty {
			key += "?" + query
		}
		let route = key
		return script.update { script -> MockResponse in
			guard var responses = script[route], !responses.isEmpty else {
				return .error(500, code: "unexpected \(route)")
			}
			let response = responses.removeFirst()
			script[route] = responses.isEmpty ? [response] : responses
			return response
		}
	}
}

@Suite struct RemoteSyncTests {
	@Test func unavailablePageNeverRemovesAnything() async throws {
		let store = try makeRemoteStore()
		try store.snapshot([remoteRoot, remoteFile("itm_old", "old.txt")], cursor: "g1.1")
		let old = try #require(try store.item(folderID: "docs", path: "old.txt"))
		let gateway = MetadataGateway()
		gateway.on("changes?cursor=g1.1", .error(410, code: "cursor_expired"))
		gateway.on(
			"snapshot",
			.json(
				SnapshotPage(
					snapshot: "snp_1", cursor: "g2.5", items: [remoteRoot, remoteFile("itm_a", "a.txt")], next: "pg_2")))
		gateway.on(
			"snapshot?snapshot=snp_1&page=pg_2", .error(503, code: "content_unavailable"),
			.json(SnapshotPage(snapshot: "snp_1", cursor: "g2.5", items: [remoteFile("itm_b", "b.txt")], next: nil)))
		gateway.on(
			"changes?cursor=g2.5",
			.json(ChangesPage(changes: [remoteFile("itm_c", "c.txt")], cursor: "g2.7", more: false)))
		let sync = RemoteSync(store: store, client: mockClient { gateway.handle($0) })

		await #expect(throws: RemoteError.unavailable(retryAfter: nil)) {
			try await sync.refresh(folderID: "docs")
		}
		// The cursor expired: a new snapshot started, and stopped at the unavailable page
		let state = try store.remoteSyncState(folderID: "docs")
		#expect(state.cursor == nil && state.snapshot?.nextPage == "pg_2" && state.snapshot?.snapshotID == "snp_1")
		#expect(try store.item(id: old.id) != nil, "nothing is inferred from a page that could not be fetched")
		#expect(try store.item(folderID: "docs", path: "a.txt") != nil)

		// It continues with the same page, then the changes after the snapshot's start; then what it did not show goes
		#expect(try await sync.refresh(folderID: "docs"))
		#expect(try store.remoteSyncState(folderID: "docs").cursor == "g2.7")
		#expect(try store.item(id: old.id) == nil)
		for path in ["a.txt", "b.txt", "c.txt"] {
			#expect(try store.item(folderID: "docs", path: path) != nil, "\(path)")
		}
		let pageRequests = gateway.requests.value.filter { $0.query("page") == "pg_2" }
		#expect(pageRequests.count == 2, "the page is asked for again with the same token")
	}

	@Test func cursorExpiredKeepsPendingWork() async throws {
		let store = try makeRemoteStore()
		try store.snapshot([remoteRoot, remoteFile("itm_a", "a.txt"), remoteFile("itm_b", "b.txt")], cursor: "g1.1")
		let a = try #require(try store.item(folderID: "docs", path: "a.txt"))
		let b = try #require(try store.item(folderID: "docs", path: "b.txt"))
		let edit = try store.recordRemoteContentChange(itemID: a.id, modifiedNanoseconds: 9, stagedFile: staged("e"))
		let deletion = try store.recordRemoteDeletion(itemID: b.id)
		let gateway = MetadataGateway()
		gateway.on("changes?cursor=g1.1", .error(410, code: "cursor_expired"))
		gateway.on(
			"snapshot",
			.json(SnapshotPage(snapshot: "snp_2", cursor: "g4.1", items: [remoteRoot, remoteFile("itm_a", "a.txt")])))
		gateway.on("changes?cursor=g4.1", .json(ChangesPage(changes: [], cursor: "g4.1", more: false)))
		try await RemoteSync(store: store, client: mockClient { gateway.handle($0) }).refresh(folderID: "docs")

		#expect(try store.remoteSyncState(folderID: "docs").cursor == "g4.1")
		#expect(try store.item(id: a.id)?.pending == .modify)
		#expect(
			try store.mutations().map(\.operationID).sorted()
				== [edit.mutation?.operationID, deletion.mutation?.operationID].compactMap { $0 }.sorted())
		#expect(try store.remoteOverlay(itemID: b.id)?.pending == .delete)
	}

	@Test func longPollAndCapabilities() async throws {
		let store = try makeRemoteStore()
		try store.snapshot([remoteRoot], cursor: "g3.1")
		let gateway = MetadataGateway()
		var capabilities: Capabilities = try fixture("capabilities.json")
		gateway.on("/v1/capabilities", .json(capabilities))
		gateway.on(
			"changes?cursor=g3.1&wait=30",
			.json(ChangesPage(changes: [remoteFile("itm_n", "n.txt")], cursor: "g3.2", more: false)))
		let sync = RemoteSync(
			store: store, client: mockClient { gateway.handle($0) },
			scheduler: TransferScheduler(store: store, owner: "app"))
		try await sync.refreshCapabilities()
		#expect(try store.remoteAuthorityGeneration(serviceID: testServiceID) == 3)
		#expect(try await sync.refresh(folderID: "docs", wait: 30))
		#expect(try store.activeTransferSlots(.longPoll) == 0, "the long poll's slot is released")
		#expect(try store.activeTransferSlots(.metadata) == 0)

		// A new authority generation: the folder takes a fresh snapshot
		capabilities.authorityGeneration = 4
		let changed = capabilities
		gateway.script.update { $0["/v1/capabilities"] = [.json(changed)] }
		try await sync.refreshCapabilities()
		#expect(try store.remoteSyncState(folderID: "docs").needsSnapshot)
	}

	/// A page that cannot be stored (it does not continue the session), and nobody else moved on: asking for it again
	/// would loop forever
	@Test func pageThatDoesNotContinueTheSessionStops() async throws {
		let store = try makeRemoteStore()
		let gateway = MetadataGateway()
		gateway.on(
			"snapshot", .json(SnapshotPage(snapshot: "snp_1", cursor: "g1.1", items: [remoteRoot], next: "pg_2")))
		gateway.on(
			"snapshot?snapshot=snp_1&page=pg_2",
			.json(SnapshotPage(snapshot: "snp_other", cursor: "g1.1", items: [], next: nil)))
		let sync = RemoteSync(store: store, client: mockClient { gateway.handle($0) })
		await #expect {
			try await sync.refresh(folderID: "docs")
		} throws: { error in
			guard case RemoteError.protocolViolation = error else {
				return false
			}
			return true
		}
		#expect(gateway.requests.value.filter { $0.query("page") == "pg_2" }.count == 1, "asked for once")
	}

	@Test func moreChangesMustAdvanceTheCursor() async throws {
		let store = try makeRemoteStore()
		try store.snapshot([remoteRoot], cursor: "g1.1")
		let gateway = MetadataGateway()
		gateway.on("changes?cursor=g1.1", .json(ChangesPage(changes: [], cursor: "g1.1", more: true)))
		let sync = RemoteSync(store: store, client: mockClient { gateway.handle($0) })
		let stuck = RemoteError.protocolViolation("the gateway has more changes, but its cursor does not advance")
		await #expect(throws: stuck) { try await sync.refresh(folderID: "docs") }
	}
}

@Suite struct TransferSchedulerTests {
	@Test func limitsAcrossProcesses() throws {
		let store = try makeStore()
		let app = TransferScheduler(store: store, owner: "app")
		let extensionScheduler = TransferScheduler(store: IndexStore(url: store.url), owner: "extension")
		var slots: [TransferScheduler.Slot] = []
		for scheduler in [app, app, extensionScheduler, extensionScheduler] {
			slots.append(try #require(try scheduler.tryAcquire(.content)))
		}
		#expect(try app.tryAcquire(.content) == nil, "four content transfers in all")
		#expect(try extensionScheduler.tryAcquire(.upload) == nil, "uploads count as content transfers")
		app.release(slots.removeFirst())
		app.release(slots.removeFirst())
		#expect(try app.tryAcquire(.upload) != nil && extensionScheduler.tryAcquire(.upload) != nil)
		#expect(try app.tryAcquire(.upload) == nil, "two uploads")
		#expect(try app.tryAcquire(.metadata) != nil && extensionScheduler.tryAcquire(.metadata) != nil)
		#expect(try app.tryAcquire(.metadata) == nil, "two metadata requests")
		#expect(try app.tryAcquire(.thumbnail) != nil && extensionScheduler.tryAcquire(.thumbnail) == nil)
	}

	/// Long polls have slots of their own: while two are held, operations can still be submitted
	@Test func longPollsDoNotTakeMetadataSlots() throws {
		let store = try makeStore()
		let app = TransferScheduler(store: store, owner: "app")
		#expect(try app.tryAcquire(.longPoll) != nil && app.tryAcquire(.longPoll) != nil)
		#expect(try app.tryAcquire(.longPoll) == nil, "two long polls")
		#expect(try app.tryAcquire(.metadata) != nil && app.tryAcquire(.metadata) != nil)
	}

	@Test func slotsOfAKilledProcessExpire() throws {
		let store = try makeStore()
		let killed = TransferScheduler(store: store, owner: "killed", claimDuration: 30)
		for _ in 0..<4 {
			_ = try #require(try killed.tryAcquire(.content))
		}
		let other = TransferScheduler(store: store, owner: "other")
		#expect(try other.tryAcquire(.content) == nil)
		#expect(try other.tryAcquire(.content, now: Date().addingTimeInterval(31)) != nil)
	}

	@Test func waitsForAFreeSlot() async throws {
		let store = try makeStore()
		let scheduler = TransferScheduler(
			store: store, owner: "app", limits: TransferLimits(thumbnails: 1), pollInterval: .milliseconds(20))
		let held = try #require(try scheduler.tryAcquire(.thumbnail))
		let waiter = Task { try await scheduler.acquire(.thumbnail) }
		try await Task.sleep(for: .milliseconds(100))
		#expect(try store.activeTransferSlots(.thumbnail) == 1)
		scheduler.release(held)
		let slot = try await waiter.value
		#expect(slot.slotClass == .thumbnail)
		let renewed = try scheduler.renew(slot)
		#expect(renewed)
	}
}
