// Copyright (C) 2026 Gordon
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this file,
// You can obtain one at https://mozilla.org/MPL/2.0/.
#if os(macOS)
	import AppKit
	import SwiftUI
	import SynctrainHostKit
	@preconcurrency import SushitrainCore

	extension EngineStatus {
		/// The first line of the menu bar menu: which engine this is and what it is doing
		var menuLine: String {
			let engine = "Syncthing \(SushitrainVersion())"
			return String(localized: "\(engine): \(self.title)")
		}

		private var title: String {
			switch self {
			case .starting:
				return String(localized: "Starting…")
			case .failed(let message):
				let error = String(localized: "Error")
				return message.isEmpty ? error : String(localized: "\(error): \(message)")
			case .folderErrors(let folders):
				if folders.count == 1 {
					return String(localized: "Error in \(folders[0])")
				}
				return String(localized: "Folders with errors: \(folders.count)")
			case .failedItems(let folders):
				if folders.count == 1 {
					return String(localized: "Failed items in \(folders[0])")
				}
				return String(localized: "Folders with failed items: \(folders.count)")
			case .devicesPaused:
				return String(localized: "All devices are paused")
			case .foldersPaused:
				return String(localized: "All folders are paused")
			case .offline:
				return String(localized: "Not connected")
			case .syncing:
				return String(localized: "Synchronizing...")
			case .upToDate:
				return String(localized: "Up to date")
			}
		}
	}

	/// What acts on the engine as a whole, in the app menu and the menu bar menu. Takes the app state explicitly, as the
	/// environment doesn't reliably reach menus. Restarting has no shortcut, as it interrupts every transfer.
	struct EngineMenuItems: View {
		let appState: AppState
		var hasShortcuts = false
		@Environment(\.openWindow) private var openWindow

		/// SwiftUI only updates the main menu's items when AppKit asks, as when the menu opens, which it doesn't for a key
		/// equivalent. A shortcut would otherwise go by the menu as it last opened, and do nothing if the item was disabled
		/// then, as at launch. To be called after a change; this runs once SwiftUI has taken it in.
		static func refreshAppMenu() {
			Task { @MainActor in
				if let menu = NSApplication.shared.mainMenu?.items.first?.submenu {
					menu.delegate?.menuNeedsUpdate?(menu)
				}
			}
		}

		var body: some View {
			let state = appState.engineMenuState
			let isPaused = state.allDevicesPausedByUser

			if !appState.pendingDevices.isEmpty {
				Button("Connection Requests: \(appState.pendingDevices.count)", systemImage: "person.crop.circle.badge.plus") {
					openWindow(id: "connectionRequests")
					NSApplication.shared.activate()
				}
			}

			Button(
				isPaused ? "Resume All Devices" : "Pause All Devices", systemImage: isPaused ? "play.circle" : "pause.circle"
			) {
				appState.setAllDevices(pausedByUser: !appState.engineMenuState.allDevicesPausedByUser)
			}
			.keyboardShortcut(self.shortcut("p"))
			.disabled(!state.isRunning || !state.hasOtherDevices)

			Button("Rescan All Folders", systemImage: "arrow.triangle.2.circlepath") {
				appState.rescanAllFolders()
			}
			.keyboardShortcut(self.shortcut("r"))
			.disabled(!state.isRunning)

			Button("Restart Syncthing", systemImage: "arrow.clockwise") {
				appState.restartEngine()
			}
			.disabled(!state.canRestart)

			Button("Open Web GUI", systemImage: "globe") {
				if let url = appState.engineMenuState.webGUIURL {
					NSWorkspace.shared.open(url)
				}
			}
			.keyboardShortcut(self.shortcut("o"))
			.disabled(!state.isRunning || state.webGUIURL == nil)
		}

		private func shortcut(_ key: KeyEquivalent) -> KeyboardShortcut? {
			hasShortcuts ? KeyboardShortcut(key, modifiers: [.command, .option]) : nil
		}
	}
#endif
