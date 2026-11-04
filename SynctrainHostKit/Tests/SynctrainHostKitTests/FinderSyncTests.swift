// Copyright (C) 2026 Gordon
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this file,
// You can obtain one at https://mozilla.org/MPL/2.0/.
import Foundation
import Testing
@testable import SynctrainHostKit

private let finderRegistry = FinderRegistry(
	configuration: "config", running: true,
	roots: [
		FinderRoot(folderID: "root", path: "/tmp/Sync"), FinderRoot(folderID: "nested", path: "/tmp/Sync/Nested"),
	])

@Test func finderClosingADirectoryReleasesItsContentsWhileTheParentStaysOpen() {
	let parent = URL(fileURLWithPath: "/tmp/Fixture")
	let root = parent.appending(path: "Sync", directoryHint: .isDirectory)
	let file = root.appending(path: "file.txt")
	#expect(FinderVisibility.contains(file, in: [parent, root]))
	#expect(!FinderVisibility.contains(file, in: [parent]))
	#expect(FinderVisibility.contains(root, in: [parent]))
	#expect(!FinderVisibility.contains(root, in: []))
}

@Test func finderPathsRespectComponentsAndNestedRoots() {
	#expect(finderRegistry.item(at: URL(fileURLWithPath: "/tmp/SyncSibling/a")) == nil)
	#expect(finderRegistry.item(at: URL(fileURLWithPath: "/tmp/Sync")) == FinderItem(folderID: "root", path: ""))
	#expect(
		finderRegistry.item(at: URL(fileURLWithPath: "/tmp/Sync/Nested/a")) == FinderItem(folderID: "nested", path: "a"))
	for path in ["../escape", "/absolute", "a//b", ".stversions/file", ".stfolder", ".syncthing.file.tmp"] {
		#expect(!FinderPaths.isAllowed(path))
	}
	#expect(FinderPaths.isAllowed("space %?#/café.txt"))
	for path in ["literal\\name.txt", "a/.stversions/file", "a/.stfolder", "a/.stignore"] {
		#expect(FinderPaths.isAllowed(path))
	}
	#expect(
		finderRegistry.monitoredDirectories.contains { $0.path == URL(fileURLWithPath: "/tmp").standardizedFileURL.path })
	#expect(finderRegistry.item(at: URL(fileURLWithPath: "/tmp/Unrelated")) == nil)
	#expect(finderRegistry.monitoredDirectories.count == 3)
}

@Test func finderActionQueuePreservesStartupAndBatchRequestsAndSerializesDelivery() throws {
	let first = FinderAction(configuration: "config", item: FinderItem(folderID: "root", path: "one"), kind: .info)
	let second = FinderAction(configuration: "config", item: FinderItem(folderID: "root", path: "two"), kind: .browse)
	var queue = FinderActionQueue()
	queue.enqueue([first])
	queue.enqueue([second, first])
	let request = try #require(queue.next)
	#expect(request.action == first)
	let claimed = queue.claim(request.id)
	#expect(claimed)
	#expect(queue.next == nil)
	let duplicate = queue.claim(request.id)
	#expect(!duplicate)
	queue.finish(UUID())
	#expect(queue.next == nil)
	queue.finish(request.id)
	let next = try #require(queue.next)
	#expect(next.action == second)
	let claimedNext = queue.claim(next.id)
	#expect(claimedNext)
	queue.finish(request.id)  // A late completion cannot release a newer delivery.
	#expect(queue.next == nil)
	queue.finish(next.id)
	let repeated = try #require(queue.next)
	#expect(repeated.action == first)
	#expect(repeated.id != request.id)
}

private actor FinderTestSignal {
	private var opened = false
	private var waiters: [CheckedContinuation<Void, Never>] = []
	func wait() async {
		if opened { return }
		await withCheckedContinuation { waiters.append($0) }
	}
	func open() {
		opened = true
		for waiter in waiters { waiter.resume() }
		waiters.removeAll()
	}
}

@MainActor private final class FinderReadState {
	var version = 1
	var applied = 0
	var reads = 0
}

@MainActor @Test func finderReloadRetainsNotificationsDuringARead() async {
	let coordinator = FinderReloadCoordinator()
	let entered = FinderTestSignal()
	let release = FinderTestSignal()
	let finished = FinderTestSignal()
	let state = FinderReadState()
	let read: @MainActor @Sendable () async -> Void = {
		state.reads += 1
		let snapshot = state.version
		if state.reads == 1 {
			await entered.open()
			await release.wait()
		}
		state.applied = snapshot
		if state.reads == 2 { await finished.open() }
	}
	coordinator.request(read)
	await entered.wait()
	state.version = 2
	coordinator.request(read)
	await release.open()
	await finished.wait()
	#expect(state.reads == 2)
	#expect(state.applied == 2)
}

@Test func finderDemandCleanupFollowsADelayedWrite() async throws {
	let directory = URL.temporaryDirectory.appending(path: UUID().uuidString)
	defer { try? FileManager.default.removeItem(at: directory) }
	let store = FinderSyncStore(directory: directory)
	try store.prepare()
	let entered = DispatchSemaphore(value: 0)
	let release = DispatchSemaphore(value: 0)
	let writer = FinderDemandWriter { demand in
		if demand.items.isEmpty {
			store.remove(name: FinderSyncStore.requestName(demand.instance))
		}
		else {
			entered.signal()
			release.wait()
			try? store.write(demand, name: FinderSyncStore.requestName(demand.instance))
		}
	}
	let instance = UUID()
	writer.publish(
		FinderDemand(instance: instance, configuration: "config", items: [FinderItem(folderID: "root", path: "one")]))
	await withCheckedContinuation { continuation in
		DispatchQueue.global().async {
			entered.wait()
			continuation.resume()
		}
	}
	writer.publish(FinderDemand(instance: instance, configuration: "config", items: []))
	release.signal()
	await writer.waitUntilIdle()
	#expect(store.demands().isEmpty)
}

@Test func finderActionsRoundTripAndRejectStaleConfigurations() throws {
	let action = FinderAction(
		configuration: "config", item: FinderItem(folderID: "root", path: "space %?#/café.txt"), kind: .versions)
	#expect(FinderAction(url: try #require(action.url)) == action)
	#expect(action.isValid(in: finderRegistry))
	var registry = finderRegistry
	registry.configuration = "other"
	#expect(!action.isValid(in: registry))
	registry = finderRegistry
	registry.roots = []
	#expect(!action.isValid(in: registry))
	var future = action
	future.version = 2
	#expect(FinderAction(url: try #require(future.url)) == nil)
	#expect(FinderAction(url: URL(string: "sushitrain://finder?request=invalid")!) == nil)
}

@Test func finderMenusUseHistoryCapabilitiesAndCommonParents() throws {
	let first = FinderItem(folderID: "root", path: "dir/a.txt")
	let second = FinderItem(folderID: "root", path: "dir/child/b.txt")
	let status = FinderItemStatus(item: first, indexed: true, versions: true)
	#expect(FinderMenu(selection: [first], statuses: [first: status])?.kinds == [.browse, .info, .settings, .versions])
	#expect(FinderMenu(selection: [first], statuses: [:])?.kinds == [.browse, .settings])
	let multiple = try #require(FinderMenu(selection: [first, second], statuses: [:]))
	#expect(multiple.item.path == "dir")
	#expect(multiple.kinds == [.browse, .settings])
	#expect(FinderMenu(selection: [first, FinderItem(folderID: "other", path: "a")], statuses: [:]) == nil)
}

@Test func finderSettingsWindowsReuseTheFolderWhileInfoWindowsKeepTheirItem() throws {
	let file = FinderItem(folderID: "root", path: "dir/a.txt")
	let settings = FinderAction(configuration: "config", item: file, kind: .settings)
	let window = try #require(settings.windowAction)
	#expect(window.item == FinderItem(folderID: "root", path: ""))
	#expect(window.kind == .settings)
	#expect(window.windowAction == window)
	#expect(settings.item == file)
	let otherEntry = FinderAction(
		configuration: "config", item: FinderItem(folderID: "root", path: "other/b.txt"), kind: .settings)
	#expect(otherEntry.windowAction == window)
	#expect(FinderAction(configuration: "other", item: file, kind: .settings).windowAction != window)
	#expect(
		FinderAction(configuration: "config", item: FinderItem(folderID: "other", path: ""), kind: .settings).windowAction
			!= window)
	#expect(FinderAction(url: try #require(window.url)) == window)
	let info = FinderAction(configuration: "config", item: file, kind: .info)
	#expect(info.windowAction == info)
	for kind in [FinderAction.Kind.browse, .versions] {
		#expect(FinderAction(configuration: "config", item: file, kind: kind).windowAction == nil)
	}
}

@Test func finderSnapshotsExpireAndRejectOtherConfigurations() {
	let now = Date()
	let snapshot = FinderSnapshot(configuration: "config", statuses: [], now: now)
	#expect(snapshot.isFresh(configuration: "config", at: now.addingTimeInterval(7)))
	#expect(!snapshot.isFresh(configuration: "config", at: now.addingTimeInterval(8)))
	#expect(!snapshot.isFresh(configuration: "other", at: now))
	#expect(!snapshot.isFresh(configuration: "config", at: now.addingTimeInterval(-10)))
}

@Test func finderCapabilitiesPersistWithoutLiveBadgeState() throws {
	var capabilities = FinderCapabilities(configuration: "config")
	let first = FinderItem(folderID: "root", path: "one")
	capabilities.merge([FinderItemStatus(item: first, badge: .current, indexed: true, versions: true)])
	#expect(capabilities.statuses.first?.badge == nil)
	#expect(FinderMenu(selection: [first], statuses: [first: capabilities.statuses[0]])?.kinds.contains(.versions) == true)
	let data = try JSONEncoder().encode(capabilities)
	#expect(try JSONDecoder().decode(FinderCapabilities.self, from: data) == capabilities)
	capabilities.merge([FinderItemStatus(item: first)])
	#expect(capabilities.statuses.isEmpty)
	capabilities.merge(
		(0...FinderPaths.maximumItems).map {
			FinderItemStatus(item: FinderItem(folderID: "root", path: String($0)), indexed: true)
		})
	#expect(capabilities.statuses.count == FinderPaths.maximumItems)
	#expect(capabilities.statuses.first?.item.path == "1")
}

@Test func finderInstancesKeepTheirRequestsSeparateAndExpire() throws {
	let directory = URL.temporaryDirectory.appending(path: UUID().uuidString)
	defer { try? FileManager.default.removeItem(at: directory) }
	let store = FinderSyncStore(directory: directory)
	try store.prepare()
	let now = Date()
	let first = FinderDemand(
		instance: UUID(), configuration: "config", items: [FinderItem(folderID: "a", path: "one")], now: now)
	let second = FinderDemand(
		instance: UUID(), configuration: "config", items: [FinderItem(folderID: "b", path: "two")], now: now)
	for demand in [first, second] {
		try store.write(demand, name: FinderSyncStore.requestName(demand.instance))
		try store.write(
			FinderSnapshot(configuration: "config", statuses: [], now: now), name: FinderSyncStore.responseName(demand.instance))
	}
	#expect(Set(store.demands(now: now).map(\.instance)) == [first.instance, second.instance])
	#expect(store.demands(now: now.addingTimeInterval(15)).isEmpty)
	#expect(store.read(FinderSnapshot.self, name: FinderSyncStore.responseName(first.instance)) == nil)
}

@Test func remoteHistoryOffersVersionsWithoutLocalVersioning() throws {
	let file = FinderItem(folderID: "docs", path: "note.txt")
	var status = FinderItemStatus(item: file, indexed: true)
	status.regularFile = true
	let remote = status.includingRemoteHistory(folderIDs: ["docs"])
	#expect(FinderMenu(selection: [file], statuses: [file: remote])?.kinds.contains(.versions) == true)
	#expect(FinderMenu(selection: [file], statuses: [file: status])?.kinds.contains(.versions) == false)
	#expect(status.includingRemoteHistory(folderIDs: ["different"]).versions == false)
	status.regularFile = false
	#expect(status.includingRemoteHistory(folderIDs: ["docs"]).versions == false)
	status.regularFile = true
	status.indexed = false
	#expect(status.includingRemoteHistory(folderIDs: ["docs"]).versions == false)
}
