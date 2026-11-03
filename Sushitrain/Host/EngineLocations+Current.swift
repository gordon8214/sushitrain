// Copyright (C) 2026 Gordon
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this file,
// You can obtain one at https://mozilla.org/MPL/2.0/.
import Foundation
import SynctrainHostKit

extension EngineLocations {
	/// This app's engine locations.
	///
	/// In the App Sandbox on macOS they stay where earlier versions kept them in the container, until the app moves out
	/// of it. In debug or dedicated benchmark builds on macOS, `SYNCTRAIN_TEST_ROOT` puts them under a scratch
	/// home directory, so a test run leaves the real ones alone (the engine expands `~` with `HOME`, so set that
	/// too).
	static let current: EngineLocations = {
		#if os(macOS)
			if let root = EngineLocations.testRoot {
				return EngineLocations(
					platform: .macOS, home: root,
					applicationSupport: root.appending(path: "Library/Application Support", directoryHint: .isDirectory),
					documents: root.appending(path: "Documents", directoryHint: .isDirectory))
			}
			return EngineLocations(
				platform: AppLocations.systemDirectoriesArePrivate ? .sandboxedMacOS : .macOS,
				home: FileManager.default.homeDirectoryForCurrentUser,
				applicationSupport: URL.applicationSupportDirectory, documents: URL.documentsDirectory)
		#else
			return EngineLocations(
				platform: .iOS, home: URL.homeDirectory, applicationSupport: URL.applicationSupportDirectory,
				documents: URL.documentsDirectory)
		#endif
	}()

	/// The scratch directory `SYNCTRAIN_TEST_ROOT` names, in debug or dedicated benchmark builds on macOS
	static var testRoot: URL? {
		#if (DEBUG || SYNCTRAIN_BENCHMARK) && os(macOS)
			#if SYNCTRAIN_BENCHMARK
				let environment = ProcessInfo.processInfo.environment
				guard let path = environment["SYNCTRAIN_TEST_ROOT"], path.hasPrefix("/"),
					FileManager.default.fileExists(atPath: path + "/.synctrain-benchmark")
				else {
					fatalError("A benchmark build requires a disposable SYNCTRAIN_TEST_ROOT with its harness marker")
				}
			#endif
			if let root = ProcessInfo.processInfo.environment["SYNCTRAIN_TEST_ROOT"], !root.isEmpty {
				return URL(fileURLWithPath: root, isDirectory: true)
			}
		#endif
		return nil
	}
}
