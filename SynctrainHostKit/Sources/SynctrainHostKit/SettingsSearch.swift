// Copyright (C) 2026 Gordon. SPDX-License-Identifier: MPL-2.0
import Foundation
import Observation

public struct SettingSearchEntry: Codable, Hashable, Identifiable, Sendable {
	public enum MacDestination: Equatable, Sendable {
		case mappedEditor, expertEditor, guiEditor, filesLocation
	}
	public var id, title, breadcrumb, category, action, capability: String
	public var section: String?
	public var field: String?
	public var anchor: String?
	public var platforms: [String]?
	public var keywords: [String]?

	public init(
		id: String, title: String, breadcrumb: String, category: String, action: String,
		capability: String = "always", section: String? = nil, field: String? = nil, anchor: String? = nil,
		platforms: [String]? = nil, keywords: [String]? = nil
	) {
		self.id = id
		self.title = title
		self.breadcrumb = breadcrumb
		self.category = category
		self.action = action
		self.capability = capability
		self.section = section
		self.field = field
		self.anchor = anchor
		self.platforms = platforms
		self.keywords = keywords
	}

	public var localizedTitle: String { String(localized: String.LocalizationValue(title), bundle: .main) }
	public var localizedBreadcrumb: String { String(localized: String.LocalizationValue(breadcrumb)) }
	public func isAvailable(mode: String) -> Bool { capability == "always" || capability == mode }

	/// A dedicated editor can omit controls that depend on its current configuration.
	public func macDestination(usesLDAP: Bool, hasTransferLimit: Bool) -> MacDestination {
		if action.hasPrefix("advanced/") { return .expertEditor }
		if let section, let field {
			if section.hasPrefix("folder/"), ["fsWatcherDelayS", "maxConflicts", "blockIndexing"].contains(field) {
				return .expertEditor
			}
			if section == "defaults/folder", field == "markerName" { return .expertEditor }
			if field == "limitBandwidthInLan", !hasTransferLimit { return .expertEditor }
			if action == "gui" {
				if section == "ldap" && !usesLDAP || section == "gui" && ["user", "password"].contains(field) && usesLDAP {
					return .expertEditor
				}
				return .guiEditor
			}
		}
		if action == "fileManager", ["filesLocation", "filesRebuild"].contains(id) { return .filesLocation }
		return .mappedEditor
	}

	public func score(query: String) -> Int? {
		let query = Self.normalize(query).trimmingCharacters(in: .whitespacesAndNewlines)
		guard !query.isEmpty else { return nil }
		let label = Self.normalize(localizedTitle)
		let corpus = Self.normalize(
			([localizedTitle, title, localizedBreadcrumb, breadcrumb, field ?? ""] + (keywords ?? [])).joined(separator: " "))
		guard query.split(whereSeparator: \.isWhitespace).allSatisfy({ corpus.contains($0) }) else { return nil }
		if label == query { return 0 }
		if label.hasPrefix(query) { return 1 }
		if label.contains(query) { return 2 }
		return 3
	}

	private static func normalize(_ value: String) -> String {
		value.folding(options: [.caseInsensitive, .diacriticInsensitive], locale: .current).lowercased()
	}
}

@Observable @MainActor public final class SettingsSearch {
	public var query = ""
	public var mode: String?
	public var error: String?
	public private(set) var entries: [SettingSearchEntry]
	private let localEntries: [SettingSearchEntry]
	@ObservationIgnored private var refreshGeneration: UInt64 = 0
	@ObservationIgnored private var loading: (id: UUID, task: Task<[SettingSearchEntry], Error>)?

	public init(catalog: DesktopUICatalog, platform: String) {
		var entries: [SettingSearchEntry] = []
		if platform != "ios" {
			entries = catalog.settings.flatMap { category in
				category.sections.map { section in
					SettingSearchEntry(
						id: category.id + "." + section.id, title: section.title, breadcrumb: category.title,
						category: category.id, action: section.action, capability: section.capability, platforms: section.platforms)
				}
			}
		}
		entries += catalog.search.filter { $0.section == nil && ($0.platforms?.contains(platform) ?? true) }
		localEntries = entries.filter { $0.platforms?.contains(platform) ?? true }
		self.entries = localEntries
	}

	public var results: [SettingSearchEntry] {
		let matches: [(entry: SettingSearchEntry, score: Int)] = entries.compactMap { entry in
			guard mode.map({ entry.isAvailable(mode: $0) }) ?? true else { return nil }
			guard let score = entry.score(query: query) else { return nil }
			return (entry, score)
		}
		return matches.sorted { first, second in
			if first.score != second.score { return first.score < second.score }
			return first.entry.id < second.entry.id
		}.map { $0.entry }
	}

	public func update(engineEntries: [SettingSearchEntry]) {
		entries = localEntries + engineEntries
		error = nil
	}
	/// Coalesce invalidations while an uncancelable native snapshot read finishes.
	public func refresh(engineEntries loader: @escaping @Sendable () async throws -> [SettingSearchEntry]) async {
		refreshGeneration &+= 1
		let generation = refreshGeneration
		if let previous = loading {
			_ = try? await previous.task.value
			if loading?.id == previous.id { loading = nil }
		}
		guard generation == refreshGeneration, !Task.isCancelled else { return }
		let id = UUID()
		let task = Task { try await loader() }
		loading = (id, task)
		defer { if loading?.id == id { loading = nil } }
		do {
			let snapshot = try await task.value
			guard generation == refreshGeneration, !Task.isCancelled else { return }
			update(engineEntries: snapshot)
		}
		catch {
			if generation == refreshGeneration, !Task.isCancelled { self.error = error.localizedDescription }
		}
	}

	public func resetEngine() {
		refreshGeneration &+= 1
		entries = localEntries
		error = nil
	}
}
