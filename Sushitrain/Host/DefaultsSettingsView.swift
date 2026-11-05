// Copyright (C) 2026 Gordon
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this file,
// You can obtain one at https://mozilla.org/MPL/2.0/.
#if os(macOS)
	import SushitrainCore
	import SwiftUI

	/// What new folders and devices start with
	struct DefaultsSettingsView: View {
		@State private var showResetSettings = false
		var body: some View {
			Form {
				Section {
					NavigationLink("New folders") {
						DefaultFolderSettingsView()
					}
					Button("Reset New Folder Defaults…", systemImage: "arrow.counterclockwise") {
						showResetSettings = true
					}
					.sheet(isPresented: $showResetSettings) {
						NavigationStack { FolderSyncResetSettingsView() }
					}
					NavigationLink("Metadata of new folders") {
						DefaultFolderMetadataView()
					}
					NavigationLink("File versioning of new folders") {
						DefaultVersioningSettingsView()
					}
					NavigationLink("Files to ignore in new folders") {
						DefaultIgnoresView()
					}
				} footer: {
					Text(
						"A folder keeps its settings once it is added, whatever changes here. Folders offered by devices that are accepted automatically start from these as well."
					)
				}
				Section {
					NavigationLink("New devices") {
						DefaultDeviceSettingsView()
					}
				}
			}
			.formStyle(.grouped)
			.navigationTitle("Defaults")
		}
	}

	private struct DefaultFolderSettingsView: View {
		@Environment(AppState.self) private var appState

		var body: some View {
			let client = appState.client
			SettingsPage(
				title: "New folders", load: { client.defaultFolderSettings() }, isEqual: { $0.equal($1) },
				save: { try client.setDefaultFolderSettings($0) },
				content: { draft in
					Section {
						TextField("Location", text: draft.path, prompt: Text(verbatim: "~/Sync"))
						Picker("Folder type", selection: draft.type) {
							Text("Send and receive").tag("sendreceive")
							Text("Send only").tag("sendonly")
							Text("Receive only").tag("receiveonly")
						}
						SettingsNumberField(title: "Rescan interval", unit: "seconds", text: draft.rescanIntervalS)
						Toggle("Watch for changes", isOn: draft.fsWatcherEnabled)
					} header: {
						Text("New folders")
					} footer: {
						Text(
							"New folders go in a directory of their own in this location. The location has to be a full path, and can start with ~; empty means ~/Sync. A rescan interval of 0 turns rescanning off."
						)
					}
					FolderSyncSections(draft: draft, isDefaults: true)
				}
			)
		}
	}

	private struct DefaultFolderMetadataView: View {
		@Environment(AppState.self) private var appState

		var body: some View {
			let client = appState.client
			SettingsPage(
				title: "Metadata of new folders", load: { client.defaultFolderSettings() }, isEqual: { $0.equal($1) },
				save: { try client.setDefaultFolderSettings($0) },
				content: { draft in
					FolderMetadataSections(draft: draft)
				}
			)
		}
	}

	private struct DefaultVersioningSettingsView: View {
		@Environment(AppState.self) private var appState

		var body: some View {
			let client = appState.client
			SettingsPage(
				title: "File versioning of new folders", load: { client.defaultVersioningSettings() },
				isEqual: { $0.equal($1) }, save: { try client.setDefaultVersioningSettings($0) },
				content: { draft in
					VersioningSections(draft: draft, isDefaults: true)
				}
			)
		}
	}

	private struct DefaultDeviceSettingsView: View {
		@Environment(AppState.self) private var appState

		var body: some View {
			let client = appState.client
			SettingsPage(
				title: "New devices", load: { client.defaultDeviceSettings() }, isEqual: { $0.equal($1) },
				save: { try client.setDefaultDeviceSettings($0) },
				content: { draft in
					DeviceSettingsSections(draft: draft)
				}
			)
		}
	}

	private struct DefaultIgnoresView: View {
		@Environment(AppState.self) private var appState
		@State private var text = ""
		@State private var saved = ""
		@State private var isSaving = false
		@State private var errorMessage: ErrorMessage? = nil

		var body: some View {
			Form {
				Section {
					TextEditor(text: $text)
						.font(.body.monospaced())
						.frame(minHeight: 200)
						.accessibilityLabel(Text("Files to ignore in new folders"))
				} footer: {
					Text(
						"Patterns for a new folder, one per line, as in a folder's own list of files to ignore. A folder that already has such a list keeps it. Receive-encrypted folders don't use patterns."
					)
				}
			}
			.formStyle(.grouped)
			.autocorrectionDisabled()
			.disabled(isSaving)
			.navigationTitle("Files to ignore in new folders")
			.toolbar {
				ToolbarItem(placement: .confirmationAction) {
					Button("Save") {
						self.save()
					}
					.disabled(text == saved || isSaving || appState.isEngineRestarting)
				}
			}
			.task(id: appState.isEngineRestarting) {
				self.refresh()
			}
			.onChange(of: appState.eventCounter) {
				self.refresh()
			}
			.errorAlert($errorMessage)
		}

		/// Follows the configuration, unless that would throw away what is being edited
		private func refresh() {
			if text == saved {
				self.load()
			}
		}

		private func load() {
			saved = appState.client.defaultIgnoreLines()
			text = saved
		}

		private func save() {
			let client = appState.client
			let lines = text
			isSaving = true
			Task {
				do {
					try await goBlocking { try client.setDefaultIgnoreLines(lines) }
					self.load()
				}
				catch {
					errorMessage = ErrorMessage(error)
				}
				isSaving = false
			}
		}
	}
#endif
