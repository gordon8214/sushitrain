// Copyright (C) 2026 Gordon
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this file,
// You can obtain one at https://mozilla.org/MPL/2.0/.
#if os(macOS)
	import AppKit
	@preconcurrency import SushitrainCore
	import SynctrainHostKit

	/// Syncthing.app (com.github.xor-gate.syncthing-macosx) and the home directory of its Syncthing backend, which this
	/// app can take over
	enum SyncthingInstallation {
		static let bundleIdentifier = "com.github.xor-gate.syncthing-macosx"

		/// What Syncthing's home directory holds (sthome.Summary)
		struct Summary: Decodable, Sendable {
			struct Folder: Decodable, Sendable {
				let id: String
				let label: String
				let path: String
				let filesystemType: String
				let paused: Bool
			}

			let deviceID: String
			/// Why the device certificate and key can't be used, if they can't
			let identityError: String?
			let configVersion: Int
			let folders: [Folder]?
			let devices: Int
			let pausedDevices: [String]?
			let guiEnabled: Bool
			let guiAddress: String
			let guiNetwork: String?
			let listenAddresses: [String]?
			let schemaVersion: Int
			let tooNew: Bool
			let locked: Bool
		}

		/// Syncthing's home directory on a Mac, which Syncthing.app's backend uses
		static var home: URL {
			(EngineLocations.testRoot ?? FileManager.default.homeDirectoryForCurrentUser).appending(
				path: "Library/Application Support/Syncthing", directoryHint: .isDirectory)
		}

		static var hasConfiguration: Bool {
			FileManager.default.fileExists(atPath: home.appending(path: "config.xml").path(percentEncoded: false))
		}

		static func summary() throws -> Summary {
			var error: NSError? = nil
			let json = SushitrainSummarizeSyncthingHome(home.path(percentEncoded: false), &error)
			if let error {
				throw error
			}
			return try JSONDecoder().decode(Summary.self, from: json ?? Data())
		}

		/// The running Syncthing.app (none in a test run, which has a home of its own)
		static var runningApp: NSRunningApplication? {
			guard !TestRun.isActive else {
				return nil
			}
			return NSRunningApplication.runningApplications(withBundleIdentifier: bundleIdentifier).first
		}

		/// Whether a Syncthing runs on its home directory; a lock the client holds itself is none
		static func isEngineRunning(besides client: SushitrainClient?) -> Bool {
			let path = home.path(percentEncoded: false)
			if let client, client.holdsLock(on: path) {
				return false
			}
			return SushitrainIsSyncthingHomeLocked(path)
		}

		/// Whether Syncthing.app or a Syncthing on its home directory runs
		static func isRunning(besides client: SushitrainClient?) -> Bool {
			runningApp != nil || isEngineRunning(besides: client)
		}

		/// Syncthing.app's own setting to start its backend at login (and itself with it), if it has one
		static var startsAtLogin: Bool? {
			guard !TestRun.isActive else {
				return nil
			}
			return UserDefaults(suiteName: bundleIdentifier)?.object(forKey: "StartAtLogin") as? Bool
		}

		static func turnOffStartAtLogin() {
			guard !TestRun.isActive else {
				return
			}
			UserDefaults(suiteName: bundleIdentifier)?.set(false, forKey: "StartAtLogin")
		}

		/// Quits Syncthing.app, which would otherwise start its backend again, then the backend, and waits until it
		/// released its home directory. Syncthing.app gets a SIGTERM if it doesn't quit within 15 seconds when asked.
		/// Returns whether Syncthing stopped.
		@MainActor static func quit() async -> Bool {
			if let app = runningApp {
				app.terminate()
				if !(await self.wait(seconds: 15, until: { app.isTerminated })) {
					let pid = app.processIdentifier
					if pid > 0 && !app.isTerminated {
						Log.warn("Syncthing.app did not quit when asked; sending it SIGTERM")
						kill(pid, SIGTERM)
						_ = await self.wait(seconds: 10, until: { app.isTerminated })
					}
				}
			}
			if self.isEngineRunning(besides: nil) {
				// A backend started otherwise, or one still finishing
				let path = home.path(percentEncoded: false)
				let problem: String? = await Task.detached {
					var error: NSError? = nil
					_ = SushitrainRequestSyncthingShutdown(path, &error)
					return error?.localizedDescription
				}.value
				if let problem {
					Log.info("Could not ask Syncthing to shut down: \(problem)")
				}
			}
			_ = await self.wait(seconds: 45, until: { !self.isEngineRunning(besides: nil) })
			return !self.isRunning(besides: nil)
		}

		@MainActor private static func wait(seconds: Int, until done: () -> Bool) async -> Bool {
			for _ in 0..<(seconds * 4) {
				if done() {
					return true
				}
				try? await Task.sleep(for: .milliseconds(250))
			}
			return done()
		}

		enum LoginItemError: LocalizedError {
			case notAllowed
			case failed(String)

			var errorDescription: String? {
				switch self {
				case .notAllowed:
					return String(
						localized: "Synctrain may not change the login items. Remove Syncthing in System Settings instead.")
				case .failed(let message):
					return message
				}
			}
		}

		/// Removes Syncthing.app's login items (through System Events, which macOS asks to allow once), away from the
		/// main thread, as the request may wait for the user. Returns whether there was one. Nothing in a test run.
		static func removeLoginItem() async throws -> Bool {
			guard !TestRun.isActive else {
				return false
			}
			let script = """
				tell application "System Events"
					set found to count of (every login item whose path contains "Syncthing.app")
					if found > 0 then delete (every login item whose path contains "Syncthing.app")
					return found
				end tell
				"""
			let (status, output, errors) = try await Task.detached(priority: .userInitiated) {
				let process = Process()
				process.executableURL = URL(fileURLWithPath: "/usr/bin/osascript")
				process.arguments = ["-e", script]
				let stdout = Pipe()
				let stderr = Pipe()
				process.standardOutput = stdout
				process.standardError = stderr
				try process.run()
				let output = stdout.fileHandleForReading.readDataToEndOfFile()
				let errors = stderr.fileHandleForReading.readDataToEndOfFile()
				process.waitUntilExit()
				return (
					process.terminationStatus, String(decoding: output, as: UTF8.self),
					String(decoding: errors, as: UTF8.self)
				)
			}.value
			guard status == 0 else {
				// Not allowed to control System Events
				if errors.contains("-1743") {
					throw LoginItemError.notAllowed
				}
				throw LoginItemError.failed(errors.trimmingCharacters(in: .whitespacesAndNewlines))
			}
			return (Int(output.trimmingCharacters(in: .whitespacesAndNewlines)) ?? 0) > 0
		}

		static func openLoginItemsSettings() {
			if let url = URL(string: "x-apple.systempreferences:com.apple.LoginItems-Settings.extension") {
				NSWorkspace.shared.open(url)
			}
		}
	}
#endif
