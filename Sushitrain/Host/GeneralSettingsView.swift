// Copyright (C) 2024-2026 Tommy van der Vorst
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this file,
// You can obtain one at https://mozilla.org/MPL/2.0/.
import Foundation
import SwiftUI
import SushitrainCore
import SynctrainHostKit

struct GeneralSettingsView: View {
	@Environment(AppState.self) private var appState
	@ObservedObject var userSettings: AppUserSettings
	@Binding var presence: AppPresence

	var body: some View {
		Form {
			Section {
				TextField(
					"Device name",
					text: Binding(
						get: {
							var err: NSError? = nil
							return appState.client.getName(&err)
						},
						set: { nn in
							try? appState.client.setName(nn)
						}))
			}

			Section {
				AppPresenceToggles(presence: $presence)
			}

			Section {
				Picker("Folder access from menu", selection: userSettings.$menuFolderAction) {
					Text("Do not show folders").tag(MenuFolderAction.hide)
					Text("Open in Finder").tag(MenuFolderAction.finder)
					Text("Open in the app").tag(MenuFolderAction.browser)
				}.disabled(!presence.inMenuBar)
			}

			FinderIntegrationSection()

			Section {
				AutoStartToggleView()
			} footer: {
				Text("While this is on, Synctrain also opens again by itself after a crash.")
			}

			Section {
				Button("Switch to Files in Finder…") {
					ModeSwitch.askToSwitchToFilesInFinder()
				}
			} footer: {
				Text(
					"This Mac then runs no engine. Finder shows the folders of a gateway on another system, and keeps what you open."
				)
			}
		}
	}
}

struct ScanningPrioritySection: View {
	@AppStorage(EnginePriority.storageKey) private var priority = EnginePriority.defaultValue

	var body: some View {
		Section {
			Picker("Scanning priority", selection: $priority) {
				Text("Normal").tag(EnginePriority.normal)
				Text("Utility").tag(EnginePriority.utility)
				Text("Background").tag(EnginePriority.background)
			}
			.onChange(of: priority) { _, priority in
				priority.apply()
			}
		} footer: {
			Text(
				"How hashing files competes with your other work. Normal runs at full speed. Utility gives way when this Mac is busy. Background uses only the efficiency cores and gives way to other disk use, which saves energy but can make large scans much slower. Transfers are not affected. Changes take effect at once."
			)
		}

	}
}
