// Copyright (C) 2026 Gordon
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this file,
// You can obtain one at https://mozilla.org/MPL/2.0/.
#if os(iOS)
	import SwiftUI

	struct FileProviderSettingsView: View {
		private var coordinator = FileProviderCoordinator.shared
		@State private var showRebuildConfirmation = false
		@State private var isRebuilding = false

		var body: some View {
			Form {
				Section {
					LabeledContent("Synctrain location") {
						switch coordinator.isDomainEnabled {
						case .some(true): Text("On")
						case .some(false): Text("Off")
						case .none: ProgressView()
						}
					}
				} footer: {
					if coordinator.isDomainEnabled != true {
						Text(
							"To browse your folders in the Files app, open Files, tap Browse, then tap Synctrain under Locations and turn it on."
						)
					}
					else {
						Text(
							"All folders stored in the app appear in the Synctrain location in the Files app. Files that are only on your other devices can be downloaded while Synctrain is open."
						)
					}
				}

				if !coordinator.requestedDownloads.isEmpty {
					Section {
						ForEach(coordinator.requestedDownloads) { download in
							LabeledContent(download.name) {
								Button("Show in Files") {
									coordinator.showInFiles(download)
								}
							}
						}
					} header: {
						Text("Requested downloads")
					} footer: {
						Text(
							"You tried to open these files in the Files app while Synctrain was not running. Show a file in Files now to download it from your other devices."
						)
					}
				}

				if let error = coordinator.lastError {
					Section("Error") {
						Text(error).foregroundStyle(.red)
					}
				}

				Section {
					Button("Rebuild Synctrain location", role: .destructive) {
						showRebuildConfirmation = true
					}
					.disabled(isRebuilding)
				} footer: {
					Text(
						"Removes the Synctrain location from the Files app and sets it up again. Your files are not affected, but you will need to turn the location on again in the Files app."
					)
				}
			}
			.navigationTitle("Files app")
			.task {
				await coordinator.refreshDomainState()
			}
			.confirmationDialog(
				"Rebuild the Synctrain location?", isPresented: $showRebuildConfirmation, titleVisibility: .visible
			) {
				Button("Rebuild", role: .destructive) {
					isRebuilding = true
					Task {
						await coordinator.rebuild()
						isRebuilding = false
					}
				}
			}
		}
	}
#endif
