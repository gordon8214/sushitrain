// Copyright (C) 2026 Gordon
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this file,
// You can obtain one at https://mozilla.org/MPL/2.0/.
#if os(macOS)
	import AppKit

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

		/// Where files of a folder that the Synctrain location cannot synchronize go when the folder leaves it and has
		/// no directory of its own on this Mac (a folder of a gateway). It is a new directory where new folders are
		/// created, which the engine picks: in the configured default folder path, or else `~/Sync` or the home
		/// directory, never inside a folder, and never one that exists already.
		static func directoryForFilesOutsideTheLocation(named name: String) -> URL? {
			guard let client = FinderAccess.shared.client else {
				return nil
			}
			var error: NSError? = nil
			// The engine names a new folder's directory after the folder, so the name is given as the folder
			let path = client.defaultFolderPath(String(localized: "\(name) (not synchronized)"), error: &error)
			guard error == nil, !path.isEmpty else {
				Log.warn(
					"Could not find a place for what the Synctrain location cannot synchronize: \(error?.localizedDescription ?? "-")")
				return nil
			}
			return URL(filePath: path, directoryHint: .isDirectory)
		}

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
