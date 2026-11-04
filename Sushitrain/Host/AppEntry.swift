// Copyright (C) 2026 Gordon
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this file,
// You can obtain one at https://mozilla.org/MPL/2.0/.
#if os(macOS)
	import Foundation

	/// Starts the app for its mode. The two have no scenes in common, and only one has an engine: `SushitrainApp` makes one
	/// the moment it exists, and a scene builder can't choose between scenes by anything other than availability.
	@main @MainActor enum AppEntry {
		static func main() {
			NSSetUncaughtExceptionHandler { e in
				Log.warn(
					"Uncaught exception: \(e.name) reason=\(e.reason ?? "unknown") returnAddress=\(e.callStackReturnAddresses) symbols=\(e.callStackSymbols)"
				)
			}
			// Before anything reads them
			TestRun.redirectPreferences()
			#if DEBUG
				if TestRun.isActive, ProcessInfo.processInfo.environment["SYNCTRAIN_TEST_HISTORY_FIXTURE"] == "1" {
					HistoryFixtureApp.main()
					return
				}
				if TestRun.isActive, !TestRun.allowsFileProvider,
					ProcessInfo.processInfo.environment["SYNCTRAIN_TEST_SETTINGS_MODE"] == "files"
				{
					SettingsFixtureApp.main()
					return
				}
			#endif
			#if SYNCTRAIN_BENCHMARK
				EngineBenchmark.prepare()
			#endif
			CrashWatch.shared.start()

			AppMode.current = AppMode.resolve()
			if AppMode.current == .synchronize {
				SushitrainApp.main()
			}
			else {
				FinderIntegration.clearRegistration()
				Log.usesSystemLog = true
				FilesApp.main()
			}
		}
	}
#endif
