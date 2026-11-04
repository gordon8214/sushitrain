// Copyright (C) 2026 Gordon
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this file,
// You can obtain one at https://mozilla.org/MPL/2.0/.
import Foundation
import Observation
#if os(macOS)
	import AppKit
#endif
import SynctrainFileProviderKit

@MainActor struct LocalHistoryProvider {
	var deviceID: String
	var load: () async throws -> [RemoteVersion]
	var export: (RemoteVersion, URL) async throws -> Void
}

/// Preparing before downloading captures the destination revision the user agreed to replace.
@MainActor struct HistoryDestination {
	var name: String
	var canRestore: Bool
	var explanation: String
	var primarySourceID: String? = nil
	var validateExport: (URL) throws -> Void = { _ in }
	var isAvailable: () -> Bool = { true }
	var restoreInPlace: ((HistoryRow) async throws -> Bool)? = nil
	var prepare: () async throws -> (@MainActor (URL, RemoteVersion) async throws -> Void)
}

struct HistoryRow: Identifiable {
	var sourceID: String
	var sourceName: String
	var version: RemoteVersion
	var id: String { sourceID + ":" + version.id }
}

@Observable @MainActor final class HistoryBrowserModel {
	struct SourceState: Identifiable {
		var id: String
		var name: String
		var isLoading = true
		var message: String?
		var alias: String?
	}
	let fileName: String
	let destination: HistoryDestination
	private(set) var destinationAvailable = true
	private(set) var destinationVersioning: RemoteVersioning?
	private(set) var isLoading = false
	var canRestore: Bool {
		destination.canRestore && destinationAvailable && (destination.primarySourceID == nil || destinationVersioning != nil)
	}
	var restoreExplanation: String {
		guard destination.canRestore, destination.primarySourceID != nil else { return destination.explanation }
		return HistoryRestore.destinationExplanation(name: destination.name, versioning: destinationVersioning)
	}
	private(set) var sources: [SourceState] = []
	private(set) var rows: [HistoryRow] = []
	private(set) var activity: String?
	private(set) var progress: Double?
	var previewURL: URL?
	var exportURL: URL?
	var problem: String?
	private(set) var notice: String?
	var restoreCandidate: HistoryRow?
	@ObservationIgnored private let store: IndexStore
	@ObservationIgnored private let histories: [RemoteHistory]
	@ObservationIgnored private let local: LocalHistoryProvider?
	@ObservationIgnored private var listings: [String: [RemoteVersion]] = [:]
	@ObservationIgnored private var devices: [String: String] = [:]
	@ObservationIgnored private var action: Task<Void, Never>?
	@ObservationIgnored private var hasLoaded = false
	@ObservationIgnored private var keepOpenedCopy = false
	@ObservationIgnored private let directory = URL.temporaryDirectory.appending(
		path: "Synctrain-history-\(UUID().uuidString)")

	init(
		fileName: String, store: IndexStore, histories: [RemoteHistory], local: LocalHistoryProvider? = nil,
		destination: HistoryDestination
	) {
		self.fileName = fileName
		self.store = store
		self.histories = histories
		self.local = local
		self.destination = destination
	}

	func load() async {
		guard !hasLoaded, !isLoading else { return }
		isLoading = true
		defer { isLoading = false }
		hasLoaded = true
		destinationAvailable = destination.isAvailable()
		sources =
			(local == nil ? [] : [SourceState(id: "local", name: String(localized: "This Mac"))])
			+ histories.map { SourceState(id: $0.source.id, name: $0.source.endpoint.displayName) }
		await withTaskGroup(of: Void.self) { group in
			if let local { group.addTask { await self.loadLocal(local) } }
			for history in histories { group.addTask { await self.loadRemote(history) } }
		}
	}

	private func loadLocal(_ provider: LocalHistoryProvider) async {
		do {
			listings["local"] = try await provider.load()
			devices["local"] = provider.deviceID
			loaded("local", message: nil)
		}
		catch { loaded("local", message: HistoryError.message(for: error)) }
	}

	private func loadRemote(_ history: RemoteHistory) async {
		do {
			let listing = try await history.versions()
			if history.source.id == destination.primarySourceID { destinationVersioning = listing.versioning }
			devices[history.source.id] = listing.deviceID
			listings[history.source.id] = listing.versions
			let message: String?
			switch listing.versioning {
			case .none: message = String(localized: "File versioning is off on this source.")
			case .external: message = String(localized: "Versions kept by an external command cannot be browsed.")
			case .customPath:
				message = String(localized: "This source keeps versions in a custom location, which the gateway does not serve.")
			default: message = nil
			}
			loaded(history.source.id, message: message)
		}
		catch { loaded(history.source.id, message: HistoryError.message(for: error)) }
	}

	private func loaded(_ id: String, message: String?) {
		guard let index = sources.firstIndex(where: { $0.id == id }) else { return }
		sources[index].isLoading = false
		sources[index].message = message
		var seen: [String: String] = [:]
		rows = []
		for index in sources.indices {
			let source = sources[index]
			sources[index].alias = nil
			if let device = devices[source.id], !device.isEmpty {
				if let original = seen[device] {
					sources[index].alias = String(localized: "Same device as \(original).")
					continue
				}
				seen[device] = source.name
			}
			rows += (listings[source.id] ?? []).map { HistoryRow(sourceID: source.id, sourceName: source.name, version: $0) }
		}
		rows.sort {
			$0.version.versionTime == $1.version.versionTime ? $0.id < $1.id : $0.version.versionTime > $1.version.versionTime
		}
	}

	func retry() async {
		guard !isLoading else { return }
		destinationVersioning = nil
		hasLoaded = false
		listings = [:]
		devices = [:]
		await load()
	}

	private func download(_ row: HistoryRow) async throws -> URL {
		try FileManager.default.createDirectory(at: directory, withIntermediateDirectories: true)
		let parent = directory.appending(path: UUID().uuidString)
		try FileManager.default.createDirectory(at: parent, withIntermediateDirectories: true)
		let target = parent.appending(path: fileName)
		if row.sourceID == "local", let local {
			try await local.export(row.version, target)
		}
		else {
			guard let history = histories.first(where: { $0.source.id == row.sourceID }) else {
				throw HistoryError.sourceChanged
			}
			try await history.download(row.version, to: target) { [weak self] received, expected in
				Task { @MainActor in self?.progress = expected > 0 ? Double(received) / Double(expected) : nil }
			}
		}
		try Task.checkCancellation()
		return target
	}

	func preview(_ row: HistoryRow) {
		run(String(localized: "Downloading…")) {
			let url = try await self.download(row)
			#if os(macOS)
				guard NSWorkspace.shared.open(url) else { throw HistoryError.cannotOpen }
				self.keepOpenedCopy = true
			#else
				self.previewURL = url
			#endif
		}
	}
	func export(_ row: HistoryRow) {
		run(String(localized: "Downloading…")) { self.exportURL = try await self.download(row) }
	}
	func restore(_ row: HistoryRow) {
		guard canRestore else { return }
		run(String(localized: "Restoring…")) {
			if try await self.destination.restoreInPlace?(row) == true {
				self.notice = String(localized: "Restored this version to \(self.destination.name).")
				await self.retry()
				return
			}
			let commit = try await self.destination.prepare()
			let file = try await self.download(row)
			try Task.checkCancellation()
			try await commit(file, row.version)
			self.notice = String(localized: "Restored this version to \(self.destination.name).")
			await self.retry()
		}
	}

	private func run(_ description: String, body: @escaping @MainActor () async throws -> Void) {
		guard action == nil else { return }
		activity = description
		progress = nil
		action = Task {
			defer {
				activity = nil
				progress = nil
				action = nil
			}
			do { try await body() }
			catch is CancellationError {}
			catch { if !Task.isCancelled { problem = HistoryError.message(for: error) } }
		}
	}

	func cancel() { action?.cancel() }
	func finish() {
		cancel()
		if !keepOpenedCopy && previewURL == nil && exportURL == nil { try? FileManager.default.removeItem(at: directory) }
	}

	static func removeLeftovers() {
		let temporary = URL.temporaryDirectory
		let entries =
			(try? FileManager.default.contentsOfDirectory(
				at: temporary, includingPropertiesForKeys: [.contentModificationDateKey])) ?? []
		for entry in entries where entry.lastPathComponent.hasPrefix("Synctrain-history-") {
			if let date = try? entry.resourceValues(forKeys: [.contentModificationDateKey]).contentModificationDate,
				date < .now.addingTimeInterval(-3600)
			{
				try? FileManager.default.removeItem(at: entry)
			}
		}
	}
}
