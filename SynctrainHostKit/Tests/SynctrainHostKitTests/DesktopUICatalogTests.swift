// Copyright (C) 2026 Gordon. SPDX-License-Identifier: MPL-2.0
import Foundation
import Testing
@testable import SynctrainHostKit

@Test func desktopCatalogDecodesSharedResource() throws {
	let root = URL(filePath: #filePath).deletingLastPathComponent().deletingLastPathComponent()
		.deletingLastPathComponent().deletingLastPathComponent()
	let catalog = try DesktopUICatalog(
		data: Data(contentsOf: root.appending(path: "SushitrainCore/desktopui/catalog.json")))
	#expect(
		catalog.destinations.map(\.id) == [
			"overview", "folders", "devices", "files", "remoteAccess", "history", "activity", "settings",
		])
	#expect(
		catalog.settings.map(\.id) == [
			"general", "filesAndStorage", "network", "synchronization", "integrations", "defaults", "advanced", "about",
		])
	#expect(
		catalog.detailDescriptors["folder"]?.map(\.id) == [
			"overview", "synchronization", "sharing", "versioning", "ignorePatterns", "advanced",
		])
	#expect(catalog.detailDescriptors["device"]?.map(\.id) == ["overview", "sharedFolders", "connections", "advanced"])
	#expect(
		catalog.detailDescriptors["fileActions"]?.map(\.id) == [
			"open", "preview", "offline", "renameMove", "previousVersions", "information", "delete",
		])
	for mode in ["synchronize", "files"] {
		for category in catalog.settings {
			#expect(!category.labelKey.isEmpty)
			for section in category.sections {
				#expect(!section.labelKey.isEmpty)
				#expect(section.isAvailable(mode: mode) == (section.capability == "always" || section.capability == mode))
			}
		}
	}
	#expect(catalog.aliases["filesAndPreviews"] == "filesAndStorage")
	#expect(catalog.settings[2].sections.allSatisfy { !$0.isAvailable(mode: "files") })
}

@Test func desktopRouteURLsPreserveDestinationIdentities() throws {
	let root = URL(filePath: #filePath).deletingLastPathComponent().deletingLastPathComponent()
		.deletingLastPathComponent().deletingLastPathComponent()
	let catalog = try DesktopUICatalog(
		data: Data(contentsOf: root.appending(path: "SushitrainCore/desktopui/catalog.json")))
	for destination in catalog.destinations {
		#expect(DesktopRouteURL.destinationID(from: DesktopRouteURL.url(destinationID: destination.id)) == destination.id)
		let legacy = try #require(URL(string: "sushitrain://\(destination.id)/desktop"))
		#expect(DesktopRouteURL.destinationID(from: legacy) == destination.id)
	}
	#expect(DesktopRouteURL.destinationID(from: URL(string: "sushitrain:folders")!) == nil)
	#expect(DesktopRouteURL.destinationID(from: URL(string: "https://example.com/desktop")!) == nil)
}
