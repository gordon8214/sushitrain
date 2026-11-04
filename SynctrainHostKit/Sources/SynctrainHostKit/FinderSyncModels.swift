// Copyright (C) 2026 Gordon
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this file,
// You can obtain one at https://mozilla.org/MPL/2.0/.
import Foundation

/// The small, engine-independent contract shared by the Mac app and its Finder extension.
public enum FinderBadge: String, Codable, Sendable {
	case current, syncing, waiting, paused, attention
}

public struct FinderItem: Codable, Hashable, Sendable {
	public var folderID: String
	public var path: String
	public init(folderID: String, path: String) {
		self.folderID = folderID
		self.path = path
	}
}

public struct FinderItemStatus: Codable, Equatable, Sendable {
	public var item: FinderItem
	public var badge: FinderBadge?
	public var indexed: Bool
	public var directory: Bool
	public var versions: Bool
	public var regularFile: Bool?
	public init(
		item: FinderItem, badge: FinderBadge? = nil, indexed: Bool = false, directory: Bool = false, versions: Bool = false
	) {
		self.item = item
		self.badge = badge
		self.indexed = indexed
		self.directory = directory
		self.versions = versions
	}
	/// Remote history eligibility is published by the app; Finder never connects to a gateway to build a menu.
	public func includingRemoteHistory(folderIDs: Set<String>) -> Self {
		var result = self
		if indexed && regularFile == true && folderIDs.contains(item.folderID) { result.versions = true }
		return result
	}

}

public struct FinderRoot: Codable, Equatable, Sendable {
	public var folderID: String
	public var path: String
	public init(folderID: String, path: String) {
		self.folderID = folderID
		self.path = path
	}
}

public struct FinderRegistry: Codable, Equatable, Sendable {
	public var version = 1
	public var configuration: String
	public var running: Bool
	public var roots: [FinderRoot]
	public init(configuration: String, running: Bool, roots: [FinderRoot]) {
		self.configuration = configuration
		self.running = running
		self.roots = roots
	}

	/// A root can be visible in its parent. Observe that parent for visibility/end notifications, but resolve items
	/// strictly against roots before requesting any status or adding a menu.
	public var monitoredDirectories: Set<URL> {
		let parents = Set(roots.map { URL(fileURLWithPath: $0.path).standardizedFileURL.deletingLastPathComponent() })
		var result: [URL] = []
		for candidate in parents.sorted(by: { $0.pathComponents.count < $1.pathComponents.count }) {
			if !result.contains(where: { candidate.pathComponents.starts(with: $0.pathComponents) }) { result.append(candidate) }
		}
		// Finder routes sidebar menus to explicitly registered roots, even while their parent is observed.
		return Set(result).union(roots.map { URL(fileURLWithPath: $0.path).standardizedFileURL })
	}

	/// Compare path components, never string prefixes. Do not resolve the selected symlink's target.
	public func item(at url: URL) -> FinderItem? {
		guard version == 1, url.isFileURL else { return nil }
		let components = url.standardizedFileURL.pathComponents
		for root in roots.sorted(by: { $0.path.count > $1.path.count }) {
			let base = URL(fileURLWithPath: root.path).standardizedFileURL.pathComponents
			guard components.starts(with: base) else { continue }
			let path = components.dropFirst(base.count).joined(separator: "/")
			guard FinderPaths.isAllowed(path) else { return nil }
			return FinderItem(folderID: root.folderID, path: path)
		}
		return nil
	}

	public func url(for item: FinderItem) -> URL? {
		guard FinderPaths.isAllowed(item.path), let root = roots.first(where: { $0.folderID == item.folderID }) else {
			return nil
		}
		return URL(fileURLWithPath: root.path).appendingPathComponent(item.path)
	}
}

public enum FinderPaths {
	public static let maximumItems = 4096
	public static func isAllowed(_ path: String) -> Bool {
		guard !path.hasPrefix("/"), !path.contains("\0") else { return false }
		if path.isEmpty { return true }
		let components = path.split(separator: "/", omittingEmptySubsequences: false)
		guard ![".stfolder", ".stversions", ".stignore"].contains(components.first) else { return false }
		return components.allSatisfy {
			!$0.isEmpty && $0 != "." && $0 != ".." && !$0.hasPrefix(".syncthing.")
		}
	}
}

public enum FinderVisibility {
	/// Observing a parent does not keep the contents of a closed subdirectory visible.
	public static func contains(_ url: URL, in directories: Set<URL>) -> Bool {
		let components = url.pathComponents
		return directories.contains { directory in
			components == directory.pathComponents || Array(components.dropLast()) == directory.pathComponents
		}
	}
}

public struct FinderAction: Codable, Hashable, Identifiable, Sendable {
	public static let infoLaunchEnvironmentKey = "SYNCTRAIN_FINDER_INFO"
	public static let windowLaunchEnvironmentKey = "SYNCTRAIN_FINDER_WINDOW"
	public enum Kind: String, Codable, Sendable { case browse, info, settings, versions }
	public var version = 1
	public var configuration: String
	public var item: FinderItem
	public var kind: Kind
	public var id: Self { self }
	public init(configuration: String, item: FinderItem, kind: Kind) {
		self.configuration = configuration
		self.item = item
		self.kind = kind
	}
	/// Settings belong to the synchronized folder, so every entry in it reuses the same window.
	public var windowAction: Self? {
		switch kind {
		case .info, .versions: return self
		case .settings:
			var action = self
			action.item.path = ""
			return action
		case .browse: return nil
		}
	}
	public var url: URL? {
		var components = URLComponents()
		components.scheme = "sushitrain"
		components.host = "finder"
		components.queryItems = [
			URLQueryItem(name: "request", value: try? String(data: JSONEncoder().encode(self), encoding: .utf8))
		]
		return components.url
	}
	public init?(url: URL) {
		guard url.scheme == "sushitrain", url.host == "finder",
			let components = URLComponents(url: url, resolvingAgainstBaseURL: false), components.queryItems?.count == 1,
			let payload = components.queryItems?.first, payload.name == "request", let data = payload.value?.data(using: .utf8),
			data.count < 32768, let action = try? JSONDecoder().decode(Self.self, from: data), action.version == 1,
			FinderPaths.isAllowed(action.item.path), !action.item.folderID.isEmpty, !action.configuration.isEmpty
		else { return nil }
		self = action
	}
	public func isValid(in registry: FinderRegistry) -> Bool {
		version == 1 && registry.version == 1 && configuration == registry.configuration && registry.url(for: item) != nil
	}
}

public struct FinderMenu: Sendable {
	public var item: FinderItem
	public var kinds: [FinderAction.Kind]
	public init?(selection: [FinderItem], statuses: [FinderItem: FinderItemStatus]) {
		guard let first = selection.first, selection.allSatisfy({ $0.folderID == first.folderID }) else { return nil }
		if selection.count == 1 {
			item = first
			kinds = [.browse]
			if statuses[first]?.indexed == true && !first.path.isEmpty { kinds.append(.info) }
			kinds.append(.settings)
			if statuses[first]?.versions == true { kinds.append(.versions) }
		}
		else {
			var common = first.path.split(separator: "/").dropLast().map(String.init)
			for selected in selection.dropFirst() {
				let parent = selected.path.split(separator: "/").dropLast().map(String.init)
				common = Array(zip(common, parent).prefix(while: { $0 == $1 }).map(\.0))
			}
			item = FinderItem(folderID: first.folderID, path: common.joined(separator: "/"))
			kinds = [.browse, .settings]
		}
	}
}

/// Each delivery has its own identity, including repeated actions for the same item.
public struct FinderActionRequest: Equatable, Identifiable, Sendable {
	public let id: UUID
	public let action: FinderAction
	public init(action: FinderAction) {
		id = UUID()
		self.action = action
	}
}

/// Keep startup requests in order and allow only one validation or presentation at a time.
public struct FinderActionQueue: Sendable {
	private var pending: [FinderActionRequest] = []
	private var active: UUID?
	public init() {}
	public var next: FinderActionRequest? { active == nil ? pending.first : nil }
	public mutating func enqueue(_ actions: [FinderAction]) {
		pending.append(contentsOf: actions.map { FinderActionRequest(action: $0) })
	}
	public mutating func claim(_ id: UUID) -> Bool {
		guard next?.id == id else { return false }
		active = pending.removeFirst().id
		return true
	}
	public mutating func finish(_ id: UUID) {
		if active == id { active = nil }
	}
}

public struct FinderDemand: Codable, Sendable {
	public var instance: UUID
	public var configuration: String
	public var updated: Date
	public var items: [FinderItem]
	public init(instance: UUID, configuration: String, items: [FinderItem], now: Date = Date()) {
		self.instance = instance
		self.configuration = configuration
		self.items = Array(items.prefix(FinderPaths.maximumItems))
		self.updated = now
	}
	public func isFresh(at now: Date) -> Bool {
		now.timeIntervalSince(updated) >= -5 && now.timeIntervalSince(updated) < 15
	}
}

public struct FinderSnapshot: Codable, Sendable {
	public var configuration: String
	public var updated: Date
	public var statuses: [FinderItemStatus]
	public init(configuration: String, statuses: [FinderItemStatus], now: Date = Date()) {
		self.configuration = configuration
		self.updated = now
		self.statuses = statuses
	}
	public func isFresh(configuration: String, at now: Date) -> Bool {
		self.configuration == configuration && now.timeIntervalSince(updated) >= -5 && now.timeIntervalSince(updated) < 8
	}
}

/// Previously queried menu capabilities survive the app quitting; they never confer live badge freshness.
public struct FinderCapabilities: Codable, Equatable, Sendable {
	public var configuration: String
	public var statuses: [FinderItemStatus] = []
	public init(configuration: String) { self.configuration = configuration }
	public mutating func merge(_ updates: [FinderItemStatus]) {
		let items = Set(updates.map(\.item))
		statuses.removeAll { items.contains($0.item) }
		statuses.append(
			contentsOf: updates.filter(\.indexed).map { value in
				var value = value
				value.badge = nil
				return value
			})
		if statuses.count > FinderPaths.maximumItems { statuses.removeFirst(statuses.count - FinderPaths.maximumItems) }
	}
}
