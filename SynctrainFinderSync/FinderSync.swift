// Copyright (C) 2026 Gordon
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this file,
// You can obtain one at https://mozilla.org/MPL/2.0/.
import AppKit
import FinderSync
import OSLog
import SynctrainHostKit

/// Finder callbacks only consult memory. Shared metadata and engine requests run asynchronously.
// The Objective-C extension entry points are nonisolated. All mutable state belongs to the main-actor model.
final class FinderSync: FIFinderSync, @unchecked Sendable {
	@MainActor private var model: FinderExtensionModel?
	override init() {
		super.init()
		Task { @MainActor in self.model = FinderExtensionModel(owner: self) }
	}
	override func beginObservingDirectory(at url: URL) { Task { @MainActor in model?.beginObservingDirectory(at: url) } }
	override func endObservingDirectory(at url: URL) { Task { @MainActor in model?.endObservingDirectory(at: url) } }
	override func requestBadgeIdentifier(for url: URL) { Task { @MainActor in model?.requestBadgeIdentifier(for: url) } }
	override func menu(for kind: FIMenuKind) -> NSMenu? {
		// This synchronous Objective-C API returns an AppKit object to Finder. All construction stays on the main actor.
		nonisolated(unsafe) var result: NSMenu?
		if Thread.isMainThread {
			MainActor.assumeIsolated { result = model?.menu(for: kind) }
		}
		else {
			DispatchQueue.main.sync { MainActor.assumeIsolated { result = model?.menu(for: kind) } }
		}
		return result
	}
	@objc func performAction(_ sender: NSMenuItem) {
		let tag = sender.tag
		Task { @MainActor in model?.performAction(tag: tag) }
	}
}

@MainActor private final class FinderExtensionModel: NSObject {
	private let instance = UUID()
	private let menuIcon: NSImage?
	private var store: FinderSyncStore?
	private var demandWriter: FinderDemandWriter?
	private var watcher: FinderDirectoryWatcher?
	private var registry = FinderRegistry(configuration: "", running: false, roots: [])
	private var statuses: [FinderItem: FinderItemStatus] = [:]
	private var requested: [URL: Date] = [:]
	private var observed: Set<URL> = []
	private var menuRequested: [URL: Date] = [:]
	private var badgesFresh = false
	private var timer: Timer?
	private let reloads = FinderReloadCoordinator()
	private var lastDemand = Date.distantPast
	private weak var owner: FinderSync?
	private var actions: [Int: FinderAction] = [:]
	private var nextAction = 0
	private static let logger = Logger(subsystem: "gordonbyrnes.Sushitrain.FinderSync", category: "Finder")

	init(owner: FinderSync) {
		self.owner = owner
		let app = Bundle.main.bundleURL.deletingLastPathComponent().deletingLastPathComponent().deletingLastPathComponent()
		menuIcon = NSWorkspace.shared.icon(forFile: app.path).copy() as? NSImage
		menuIcon?.size = NSSize(width: 16, height: 16)
		menuIcon?.isTemplate = false
		super.init()
		let controller = FIFinderSyncController.default()
		controller.directoryURLs = []
		for badge in [FinderBadge.current, .syncing, .waiting, .paused, .attention] {
			let symbol: String
			let color: NSColor
			let label: String
			switch badge {
			case .current:
				symbol = "checkmark.circle.fill"
				color = .systemGreen
				label = String(localized: "Up to Date")
			case .syncing:
				symbol = "arrow.triangle.2.circlepath.circle.fill"
				color = .systemBlue
				label = String(localized: "Syncing")
			case .waiting:
				symbol = "clock.fill"
				color = .systemBlue
				label = String(localized: "Waiting to Sync")
			case .paused:
				symbol = "pause.circle.fill"
				color = .systemGray
				label = String(localized: "Paused")
			case .attention:
				symbol = "exclamationmark.circle.fill"
				color = .systemOrange
				label = String(localized: "Needs Attention")
			}
			if let image = NSImage(systemSymbolName: symbol, accessibilityDescription: label)?.withSymbolConfiguration(
				.init(paletteColors: [color]))
			{
				controller.setBadgeImage(image, label: label, forBadgeIdentifier: badge.rawValue)
			}
		}
		guard let directory = FinderSyncStore.sharedDirectory() else { return }
		let store = FinderSyncStore(directory: directory)
		do {
			try store.prepare()
			self.store = store
			demandWriter = FinderDemandWriter(store: store)
			watcher = try FinderDirectoryWatcher(directory: directory) { Task { @MainActor in self.reload() } }
			reload()
		}
		catch { Self.logger.error("Could not open Finder metadata: \(error.localizedDescription)") }
	}

	private func reload() {
		reloads.request { [weak self] in await self?.load() }
	}
	private func load() async {
		guard let store else { return }
		let instance = instance
		let (next, snapshot, capabilities) = await Task.detached(priority: .utility) {
			(
				store.read(FinderRegistry.self, name: "registry.json"),
				store.read(FinderSnapshot.self, name: FinderSyncStore.responseName(instance)),
				store.read(FinderCapabilities.self, name: "capabilities.json")
			)
		}.value
		if let next, next.version == 1 {
			if registry != next {
				if registry.configuration != next.configuration { statuses = [:] }
				registry = next
				FIFinderSyncController.default().directoryURLs = next.monitoredDirectories
			}
		}
		else {
			registry = FinderRegistry(configuration: "", running: false, roots: [])
			FIFinderSyncController.default().directoryURLs = []
		}
		badgesFresh = registry.running && (snapshot?.isFresh(configuration: registry.configuration, at: Date()) ?? false)
		if let capabilities, capabilities.configuration == registry.configuration {
			statuses = Dictionary(capabilities.statuses.map { ($0.item, $0) }, uniquingKeysWith: { _, last in last })
		}
		if let snapshot, snapshot.configuration == registry.configuration {
			for status in snapshot.statuses { statuses[status.item] = status }
		}
		else if registry.roots.isEmpty {
			statuses = [:]
		}
		for url in requested.keys { applyBadge(url) }
	}

	func beginObservingDirectory(at url: URL) {
		observed.insert(url)
		startTimer()
	}
	func endObservingDirectory(at url: URL) {
		observed.remove(url)
		let before = Set(requested.keys)
		requested = requested.filter { FinderVisibility.contains($0.key, in: observed) }
		for removed in before.subtracting(requested.keys) {
			FIFinderSyncController.default().setBadgeIdentifier("", for: removed)
		}
		if observed.isEmpty {
			requested.removeAll()
			menuRequested.removeAll()
			timer?.invalidate()
			timer = nil
		}
		publishDemand(force: true)
	}
	func requestBadgeIdentifier(for url: URL) {
		guard registry.item(at: url) != nil else { return }
		requested[url] = Date()
		if requested.count > FinderPaths.maximumItems, let oldest = requested.min(by: { $0.value < $1.value })?.key {
			FIFinderSyncController.default().setBadgeIdentifier("", for: oldest)
			requested.removeValue(forKey: oldest)
		}
		applyBadge(url)
		startTimer()
	}
	private func applyBadge(_ url: URL) {
		let badge = badgesFresh ? registry.item(at: url).flatMap { statuses[$0]?.badge?.rawValue } : nil
		FIFinderSyncController.default().setBadgeIdentifier(badge ?? "", for: url)
	}
	private func startTimer() {
		guard timer == nil else { return }
		timer = Timer.scheduledTimer(withTimeInterval: 1, repeats: true) { [weak self] _ in
			Task { @MainActor in self?.tick() }
		}
	}
	private func tick() {
		menuRequested = menuRequested.filter { Date().timeIntervalSince($0.value) < 5 }
		let before = Set(requested.keys)
		requested = requested.filter { url, date in
			FinderVisibility.contains(url, in: observed) || Date().timeIntervalSince(date) < 10
		}
		for url in before.subtracting(requested.keys) {
			FIFinderSyncController.default().setBadgeIdentifier("", for: url)
		}
		reload()
		publishDemand()
		if requested.isEmpty && menuRequested.isEmpty {
			timer?.invalidate()
			timer = nil
			publishDemand(force: true)
		}
	}
	private func publishDemand(force: Bool = false) {
		guard let demandWriter, force || Date().timeIntervalSince(lastDemand) >= 2 else { return }
		lastDemand = Date()
		let items = Set(Array(requested.keys) + Array(menuRequested.keys)).compactMap { registry.item(at: $0) }
		let demand = FinderDemand(instance: instance, configuration: registry.configuration, items: items)
		demandWriter.publish(demand)
	}

	func menu(for menuKind: FIMenuKind) -> NSMenu? {
		guard menuKind != .toolbarItemMenu else { return nil }
		let controller = FIFinderSyncController.default()

		let urls: [URL]
		// Finder targets the enclosing directory for sidebar menus; its selection identifies the sidebar item.
		if menuKind == .contextualMenuForItems || menuKind == .contextualMenuForSidebar {
			urls = controller.selectedItemURLs() ?? []
		}
		else {
			urls = controller.targetedURL().map { [$0] } ?? []
		}
		let items = urls.compactMap { registry.item(at: $0) }
		guard items.count == urls.count, let descriptor = FinderMenu(selection: items, statuses: statuses) else { return nil }
		for url in urls.prefix(FinderPaths.maximumItems) { menuRequested[url] = Date() }
		while menuRequested.count > FinderPaths.maximumItems, let oldest = menuRequested.min(by: { $0.value < $1.value })?.key
		{ menuRequested.removeValue(forKey: oldest) }
		startTimer()
		let menu = NSMenu()
		for kind in descriptor.kinds {
			let title: String
			switch kind {
			case .browse: title = String(localized: "Show in Synctrain")
			case .info: title = String(localized: "Show Info…")
			case .settings: title = String(localized: "Folder Settings…")
			case .versions: title = String(localized: "Previous Versions…")
			}
			let entry = menu.addItem(withTitle: title, action: #selector(FinderSync.performAction(_:)), keyEquivalent: "")
			entry.image = menuIcon
			entry.target = owner
			nextAction += 1
			entry.tag = nextAction
			actions[nextAction] = FinderAction(configuration: registry.configuration, item: descriptor.item, kind: kind)
			if actions.count > 256 { actions.removeValue(forKey: nextAction - 256) }
		}
		return menu
	}
	func performAction(tag: Int) {
		guard let action = actions[tag], let url = action.url else { return }
		let app = Bundle.main.bundleURL.deletingLastPathComponent().deletingLastPathComponent().deletingLastPathComponent()
		let configuration = NSWorkspace.OpenConfiguration()
		if action.windowAction != nil {
			configuration.environment = [FinderAction.windowLaunchEnvironmentKey: "1"]
			// Finder transfers foreground activation; the launch flag suppresses unrelated app windows.
			configuration.activates = true
		}
		Task {
			do {
				_ = try await NSWorkspace.shared.open([url], withApplicationAt: app, configuration: configuration)
			}
			catch { Self.logger.error("Could not open Synctrain: \(error.localizedDescription)") }
		}
	}
}
