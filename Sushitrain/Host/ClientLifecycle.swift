// Copyright (C) 2026 Gordon
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this file,
// You can obtain one at https://mozilla.org/MPL/2.0/.
#if os(macOS)
	import AppKit
	import SynctrainFileProviderKit
	import SynctrainHostKit

	/// What the work with gateways (SynctrainClient) needs from the app's life on this platform. A Mac app keeps running
	/// behind its windows, so only the Mac going to sleep stops it, which it comes back from as from an outage.
	@MainActor enum ClientLifecycle {
		enum Event {
			/// The app returns after being away
			case willResume
			/// The app can reach out freely
			case didBecomeActive
			/// The app won't be able to keep connections open
			case didStop

			fileprivate var notification: Notification.Name {
				switch self {
				case .willResume, .didBecomeActive: return NSWorkspace.didWakeNotification
				case .didStop: return NSWorkspace.willSleepNotification
				}
			}
		}

		/// Whether the app may use the app group, the Keychain and the system's location
		nonisolated static var allowsSharedState: Bool { TestRun.allowsFileProvider }

		static var isActive: Bool { true }
		static var isInBackground: Bool { false }
		static var backgroundTimeRemaining: TimeInterval { .greatestFiniteMagnitude }

		static func observe(_ event: Event, handler: @escaping @MainActor @Sendable () -> Void) -> any NSObjectProtocol {
			NSWorkspace.shared.notificationCenter.addObserver(forName: event.notification, object: nil, queue: .main) { _ in
				MainActor.assumeIsolated(handler)
			}
		}

		/// Shows a location of the Synctrain location where the system shows files
		static func show(_ url: URL) {
			NSWorkspace.shared.activateFileViewerSelecting([url])
		}

		/// Where files of a folder that the Synctrain location cannot synchronize go when the folder leaves it: a new
		/// directory in `~/Sync`, as a Mac with the location runs no engine to ask, and never one that exists already.
		/// `~/Sync` can be a folder of the engine, which would send these to its devices the day this Mac switches back, so
		/// they go in the home directory then, as new folders do.
		static func directoryForFilesOutsideTheLocation(named label: String) -> URL? {
			let fileManager = FileManager.default
			let home = EngineLocations.testRoot ?? fileManager.homeDirectoryForCurrentUser
			var parent = home.appending(path: "Sync", directoryHint: .isDirectory)
			if fileManager.fileExists(atPath: parent.appending(path: ".stfolder").path(percentEncoded: false)) {
				parent = home
			}
			let base = FileNames.directoryName(forLabel: label)
			let name = String(localized: "\(base.isEmpty ? "Synctrain" : base) (not synchronized)")
			for attempt in 1...directoryNameAttempts {
				let directory = parent.appending(path: attempt == 1 ? name : "\(name) \(attempt)", directoryHint: .isDirectory)
				if !fileManager.fileExists(atPath: directory.path(percentEncoded: false)) {
					return directory
				}
			}
			Log.warn("Could not find a place for what the Synctrain location cannot synchronize of \(label)")
			return nil
		}
		private static let directoryNameAttempts = 100

		/// Opens the settings where macOS asks the user to allow the Synctrain location
		static func showLocationSettings() {
			if let url = URL(string: "x-apple.systempreferences:com.apple.LoginItems-Settings.extension") {
				NSWorkspace.shared.open(url)
			}
		}
	}

	/// Work that should finish. On iOS this is time the app is given in the background; here it keeps App Nap away.
	@MainActor final class BackgroundActivity {
		private var activity: (any NSObjectProtocol)?

		init(name: String, onExpiration: (@MainActor @Sendable () -> Void)? = nil) {
			activity = ProcessInfo.processInfo.beginActivity(
				options: [.userInitiatedAllowingIdleSystemSleep], reason: name)
		}

		func end() {
			if let activity {
				ProcessInfo.processInfo.endActivity(activity)
				self.activity = nil
			}
		}
	}

	/// The name gateways show for this device
	enum DeviceName {
		static let storageKey = "deviceName"

		@MainActor static var systemName: String {
			GatewaySettings.computerName
		}

		@MainActor static var current: String {
			let name = UserDefaults.standard.string(forKey: storageKey)?.trimmingCharacters(in: .whitespacesAndNewlines)
			return name?.isEmpty == false ? name! : systemName
		}
	}
#endif
