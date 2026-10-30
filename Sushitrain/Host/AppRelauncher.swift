// Copyright (C) 2026 Gordon
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this file,
// You can obtain one at https://mozilla.org/MPL/2.0/.
#if os(macOS)
	import AppKit
	import SynctrainHostKit

	/// Quits the app and opens it again, as after choosing another configuration folder
	enum AppRelauncher {
		/// Whether the app can open itself again: outside the App Sandbox, and not in a test run (whose environment
		/// opening it again would lose). Otherwise it only quits.
		static var canRelaunch: Bool {
			!FolderAccess.usesSecurityScope && EngineLocations.testRoot == nil
		}

		/// Quits the app normally (stopping the engine), and opens it again once this process has exited, so that the
		/// two never run at the same time. Should the app still run after two minutes, it is not opened again.
		@MainActor static func relaunch() {
			guard canRelaunch else {
				NSApp.terminate(nil)
				return
			}
			let opener = Process()
			opener.executableURL = URL(fileURLWithPath: "/bin/sh")
			// By its path, or else (as when the app ran translocated) by its bundle identifier
			let script = """
				i=0; while /bin/kill -0 "$1" 2>/dev/null && [ $i -lt 600 ]; do /bin/sleep 0.2; i=$((i+1)); done
				/bin/kill -0 "$1" 2>/dev/null || /usr/bin/open "$2" || /usr/bin/open -b "$3"
				"""
			opener.arguments = [
				"-c", script, "sh", String(ProcessInfo.processInfo.processIdentifier),
				Bundle.main.bundleURL.path(percentEncoded: false), Bundle.main.bundleIdentifier ?? "",
			]
			do {
				try opener.run()
			}
			catch {
				Log.warn("Could not arrange to open the app again: \(error.localizedDescription)")
			}
			NSApp.terminate(nil)
		}
	}
#endif
