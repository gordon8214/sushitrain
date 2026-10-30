// Copyright (C) 2026 Gordon
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this file,
// You can obtain one at https://mozilla.org/MPL/2.0/.
import Foundation
import Testing

@testable import SynctrainHostKit

private let home = URL(fileURLWithPath: "/Users/test", isDirectory: true)
private let applicationSupport = home.appending(path: "Library/Application Support", directoryHint: .isDirectory)
private let documents = home.appending(path: "Documents", directoryHint: .isDirectory)

@Test func macOSUsesItsOwnFoldersAndTheRealHome() {
	let locations = EngineLocations(
		platform: .macOS, home: home, applicationSupport: applicationSupport, documents: documents)
	#expect(locations.configDirectory.path == "/Users/test/Library/Application Support/Synctrain")
	#expect(locations.logs.path == "/Users/test/Library/Logs/Synctrain")
	#expect(locations.files.path == "/Users/test")
	#expect(locations.remoteFileService.path == "/Users/test/Library/Application Support/Synctrain/RemoteFileService")
}

@Test func macOSWritesNothingLooseIntoSharedFolders() {
	let locations = EngineLocations(
		platform: .macOS, home: home, applicationSupport: applicationSupport, documents: documents)
	// Everything the engine writes on its own is in a folder of the app's, inside the user's Library
	let library = home.appending(path: "Library", directoryHint: .isDirectory).pathComponents
	for written in [locations.configDirectory, locations.logs, locations.remoteFileService] {
		let components = written.standardizedFileURL.pathComponents
		#expect(Array(components.prefix(library.count)) == library, "\(written.path)")
		#expect(components.dropFirst(library.count).dropFirst().first == "Synctrain", "\(written.path)")
	}
}

@Test(arguments: [EngineLocations.Platform.iOS, .sandboxedMacOS])
func containersKeepTheirLayout(platform: EngineLocations.Platform) {
	let locations = EngineLocations(
		platform: platform, home: home, applicationSupport: applicationSupport, documents: documents)
	#expect(locations.configDirectory == applicationSupport)
	#expect(locations.logs == documents)
	#expect(locations.files == documents)
	#expect(locations.remoteFileService.path == "/Users/test/Library/Application Support/RemoteFileService")
}
