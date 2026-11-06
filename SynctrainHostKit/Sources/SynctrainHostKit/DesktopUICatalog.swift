// Copyright (C) 2026 Gordon. SPDX-License-Identifier: MPL-2.0
import Foundation

/// The same resource is embedded by Go and packaged with each native desktop.
public struct DesktopUICatalog: Decodable, Sendable {
	public struct Section: Decodable, Identifiable, Sendable {
		public let id, title, labelKey, capability, action: String
		public let platforms: [String]?
		public func isAvailable(mode: String) -> Bool { capability == "always" || capability == mode }
		public func isAvailable(platform: String, mode: String) -> Bool {
			isAvailable(mode: mode) && (platforms?.contains(platform) ?? true)
		}
	}
	public struct Destination: Decodable, Identifiable, Sendable {
		public let id, title, labelKey, symbol, icon: String
		public let sections: [String]
		public let capability: String
		public let platforms: [String]?
	}
	public struct Category: Decodable, Identifiable, Sendable {
		public let id, title, labelKey, symbol, icon: String
		public let sections: [Section]
	}
	public let version: Int
	public let destinations: [Destination]
	public let settings: [Category]
	public let details: [String: [String]]
	public let aliases: [String: String]
	public let detailDescriptors: [String: [Detail]]
	public let search: [SettingSearchEntry]
	public let navigationGroups: [NavigationGroup]
	public struct NavigationGroup: Decodable, Identifiable, Sendable {
		public let id, title, labelKey: String
		public let items: [String]
	}
	public struct NavigationItem: Identifiable, Sendable {
		public let id, title, symbol: String
		public var category: Category?
		public var localizedTitle: String { String(localized: String.LocalizationValue(title), bundle: .main) }
	}
	public struct NavigationSection: Identifiable, Sendable {
		public let id, title: String
		public let items: [NavigationItem]
		public var localizedTitle: String { String(localized: String.LocalizationValue(title), bundle: .main) }
	}
	public func navigation(platform: String, mode: String) -> [NavigationSection] {
		navigationGroups.compactMap { group in
			let items: [NavigationItem] = group.items.compactMap { id in
				if let d = destinations.first(where: { $0.id == id }),
					d.platforms?.contains(platform) ?? true, d.capability == "always" || d.capability == mode
				{
					return NavigationItem(id: id, title: d.title, symbol: d.symbol)
				}
				if let category = settings.first(where: { $0.id == id }),
					category.sections.contains(where: { $0.isAvailable(platform: platform, mode: mode) })
				{
					return NavigationItem(id: id, title: category.title, symbol: category.symbol, category: category)
				}
				return nil
			}
			return items.isEmpty ? nil : NavigationSection(id: group.id, title: group.title, items: items)
		}
	}
	public struct Detail: Decodable, Identifiable, Sendable {
		public let id, title, labelKey, capability: String
	}

	public init(data: Data) throws {
		self = try JSONDecoder().decode(Self.self, from: data)
		guard version == 1 else { throw CocoaError(.coderReadCorrupt) }
	}

	public static let desktop: Self = {
		guard let url = Bundle.main.url(forResource: "catalog", withExtension: "json"),
			let data = try? Data(contentsOf: url), let catalog = try? Self(data: data)
		else { preconditionFailure("The desktop UI catalog is missing or invalid.") }
		return catalog
	}()
}

/// Encodes desktop destinations without treating their case-sensitive IDs as URL hosts.
public enum DesktopRouteURL {
	public static func url(destinationID: String) -> URL {
		var components = URLComponents()
		components.scheme = "sushitrain"
		components.path = "desktop"
		components.queryItems = [URLQueryItem(name: "id", value: destinationID)]
		return components.url!
	}

	public static func destinationID(from url: URL) -> String? {
		guard url.scheme == "sushitrain", url.path == "desktop" || url.path == "/desktop" else { return nil }
		let components = URLComponents(url: url, resolvingAgainstBaseURL: false)
		return components?.queryItems?.first { $0.name == "id" }?.value ?? url.host ?? "overview"
	}
}
