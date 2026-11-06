// Copyright (C) 2026 Gordon. SPDX-License-Identifier: MPL-2.0
import Foundation
import Testing
@testable import SynctrainHostKit

private func searchCatalog() throws -> DesktopUICatalog {
	let root = URL(filePath: #filePath).deletingLastPathComponent().deletingLastPathComponent()
		.deletingLastPathComponent().deletingLastPathComponent()
	return try DesktopUICatalog(data: Data(contentsOf: root.appending(path: "SushitrainCore/desktopui/catalog.json")))
}

@Test @MainActor func settingsSearchRanksTitlesAndMatchesAllWords() throws {
	let search = SettingsSearch(catalog: try searchCatalog(), platform: "macOS")
	search.update(engineEntries: [
		SettingSearchEntry(
			id: "fixture", title: "Café", breadcrumb: "Folders › Documents", category: "advanced",
			action: "advanced/folder/test",
			capability: "synchronize", section: "folder/test", field: "rescanIntervalS", keywords: ["scan"])
	])
	search.query = "CAFE"
	#expect(search.results.map(\.id) == ["fixture"])
	search.query = "  documents rescan  "
	#expect(search.results.map(\.id) == ["fixture"])
	search.query = "documents absent"
	#expect(search.results.isEmpty)
	search.query = ""
	#expect(search.results.isEmpty)
	search.query = "MCP"
	#expect(search.results.first?.title == "MCP")
}

@Test @MainActor func mobileSearchContainsOnlyMobileSettings() throws {
	let search = SettingsSearch(catalog: try searchCatalog(), platform: "ios")
	#expect(search.entries.allSatisfy { $0.section == nil })
	#expect(search.entries.contains { $0.id == "cacheLimit" })
	#expect(!search.entries.contains { $0.action == "proxy" || $0.action == "mcp" })
	#expect(Set(search.entries.map(\.id)).count == search.entries.count)
}

@Test @MainActor func settingsSearchReplacesDynamicSnapshotsAndKeepsQuery() throws {
	let search = SettingsSearch(catalog: try searchCatalog(), platform: "macOS")
	search.query = "removed folder"
	search.update(engineEntries: [
		SettingSearchEntry(
			id: "old", title: "Removed folder", breadcrumb: "Folders", category: "advanced", action: "advanced/folder/old")
	])
	#expect(search.results.count == 1)
	search.update(engineEntries: [])
	#expect(search.results.isEmpty)
	#expect(search.query == "removed folder")
	#expect(search.entries.contains { $0.id == "cacheLimit" })
}

@Test func macSearchDestinationsExposeControlsOrUseExpertFallback() throws {
	let catalog = try searchCatalog()
	func entry(_ section: String, _ field: String) throws -> SettingSearchEntry {
		try #require(catalog.search.first { $0.section == section && $0.field == field })
	}
	for field in ["fsWatcherDelayS", "maxConflicts", "blockIndexing"] {
		#expect(try entry("folder/*", field).macDestination(usesLDAP: false, hasTransferLimit: true) == .expertEditor)
		#expect(try entry("defaults/folder", field).macDestination(usesLDAP: false, hasTransferLimit: true) == .mappedEditor)
	}
	#expect(
		try entry("defaults/folder", "markerName").macDestination(usesLDAP: false, hasTransferLimit: true) == .expertEditor)
	#expect(try entry("gui", "useTLS").macDestination(usesLDAP: false, hasTransferLimit: true) == .guiEditor)
	#expect(try entry("gui", "user").macDestination(usesLDAP: true, hasTransferLimit: true) == .expertEditor)
	#expect(try entry("gui", "user").macDestination(usesLDAP: false, hasTransferLimit: true) == .guiEditor)
	#expect(try entry("ldap", "address").macDestination(usesLDAP: false, hasTransferLimit: true) == .expertEditor)
	#expect(try entry("ldap", "address").macDestination(usesLDAP: true, hasTransferLimit: true) == .guiEditor)
	#expect(
		try entry("options", "limitBandwidthInLan").macDestination(usesLDAP: false, hasTransferLimit: false) == .expertEditor)
	#expect(
		try entry("options", "limitBandwidthInLan").macDestination(usesLDAP: false, hasTransferLimit: true) == .mappedEditor)
	for id in ["filesLocation", "filesRebuild"] {
		let location = try #require(catalog.search.first { $0.id == id })
		#expect(location.macDestination(usesLDAP: false, hasTransferLimit: false) == .filesLocation)
	}
	let section = SettingSearchEntry(id: "gui", title: "GUI", breadcrumb: "Advanced", category: "advanced", action: "gui")
	#expect(section.macDestination(usesLDAP: false, hasTransferLimit: true) == .mappedEditor)
}

@MainActor private final class SearchSnapshotGate {
	var calls = 0
	var active = 0
	var maximumActive = 0
	private var pending: [CheckedContinuation<[SettingSearchEntry], Error>] = []
	func load() async throws -> [SettingSearchEntry] {
		calls += 1
		active += 1
		maximumActive = max(maximumActive, active)
		defer { active -= 1 }
		return try await withCheckedThrowingContinuation { pending.append($0) }
	}
	func finish(_ id: String) {
		pending.removeFirst().resume(returning: [
			SettingSearchEntry(id: id, title: id, breadcrumb: "Advanced", category: "advanced", action: "advanced/options")
		])
	}
}

private enum SearchWaitError: Error { case didNotStart }
@MainActor private func waitForSearch(_ condition: () -> Bool) async throws {
	for _ in 0..<1000 {
		if condition() { return }
		await Task.yield()
	}
	throw SearchWaitError.didNotStart
}

@Test @MainActor func settingsSearchCoalescesRefreshesAndRejectsCanceledSnapshots() async throws {
	let search = SettingsSearch(catalog: try searchCatalog(), platform: "macOS")
	let gate = SearchSnapshotGate()
	let first = Task { await search.refresh { try await gate.load() } }
	try await waitForSearch { gate.calls == 1 }
	var waiting = 0
	let second = Task {
		waiting += 1
		await search.refresh { try await gate.load() }
	}
	let third = Task {
		waiting += 1
		await search.refresh { try await gate.load() }
	}
	try await waitForSearch { waiting == 2 }
	#expect(gate.calls == 1)
	first.cancel()
	gate.finish("obsolete")
	await first.value
	try await waitForSearch { gate.calls == 2 }
	#expect(!search.entries.contains { $0.id == "obsolete" })
	gate.finish("current")
	await second.value
	await third.value
	#expect(gate.calls == 2)
	#expect(gate.maximumActive == 1)
	#expect(search.entries.contains { $0.id == "current" })
}

@Test @MainActor func settingsSearchResetRejectsAnInFlightSnapshot() async throws {
	let search = SettingsSearch(catalog: try searchCatalog(), platform: "macOS")
	let gate = SearchSnapshotGate()
	let loading = Task { await search.refresh { try await gate.load() } }
	try await waitForSearch { gate.calls == 1 }
	search.resetEngine()
	gate.finish("stopped-engine")
	await loading.value
	#expect(!search.entries.contains { $0.id == "stopped-engine" })
}

@Test @MainActor func applicableSearchTargetsStayInUnifiedNavigation() throws {
	let root = URL(filePath: #filePath).deletingLastPathComponent().deletingLastPathComponent().deletingLastPathComponent()
		.deletingLastPathComponent()
	let catalog = try DesktopUICatalog(
		data: Data(contentsOf: root.appending(path: "SushitrainCore/desktopui/catalog.json")))
	for platform in ["macOS", "windows", "gnome", "kde", "ios"] {
		for mode in ["files", "synchronize"] where platform != "ios" || mode == "files" {
			let visible = Set(catalog.navigation(platform: platform, mode: mode).flatMap(\.items).map(\.id))
			let search = SettingsSearch(catalog: catalog, platform: platform)
			search.mode = mode
			for entry in search.entries where entry.isAvailable(mode: mode) { #expect(visible.contains(entry.category)) }
		}
	}
}
