// Copyright (C) 2026 Gordon
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this file,
// You can obtain one at https://mozilla.org/MPL/2.0/.
#if os(macOS)
	import Foundation
	import ObjectiveC
	import SynctrainHostKit

	/// A debug run with `SYNCTRAIN_TEST_ROOT` (EngineLocations.testRoot): the app's preferences are kept in the scratch
	/// directory instead of the real ones, and nothing is changed in other apps' settings, so that the real app is as it
	/// was afterwards
	enum TestRun {
		static var isActive: Bool {
			EngineLocations.testRoot != nil
		}

		/// Keeps this run's preferences in the scratch directory, as `Library/Preferences/<bundle identifier>.plist` there.
		/// To be called before anything reads them. It changes what `UserDefaults.standard` returns, rather than each of
		/// the many places that use it, so that it also covers `@AppStorage`, AppKit and what comes later. A preference left
		/// behind in the real ones can change what the real app does: `lastRunBuild` hides the offer to take over Syncthing.
		static func redirectPreferences() {
			#if DEBUG
				guard let root = EngineLocations.testRoot, let bundleIdentifier = Bundle.main.bundleIdentifier else {
					return
				}
				let directory = root.appending(path: "Library/Preferences", directoryHint: .isDirectory)
				let method = class_getClassMethod(UserDefaults.self, NSSelectorFromString("standardUserDefaults"))
				guard (try? FileManager.default.createDirectory(at: directory, withIntermediateDirectories: true)) != nil,
					let scratch = UserDefaults(
						suiteName: directory.appending(path: bundleIdentifier).path(percentEncoded: false)),
					let method
				else {
					// Going on would use the real ones
					fatalError("A test run could not keep its preferences in \(directory.path(percentEncoded: false))")
				}
				let standard: @convention(block) (AnyClass) -> UserDefaults = { _ in scratch }
				method_setImplementation(method, imp_implementationWithBlock(standard))
			#endif
		}
	}
#endif
