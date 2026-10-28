// Copyright (C) 2026 Gordon
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this file,
// You can obtain one at https://mozilla.org/MPL/2.0/.
#if os(iOS)
	import SwiftUI

	struct FileProviderSettingsView: View {
		private var coordinator = FileProviderCoordinator.shared
		private var remote = RemoteCoordinator.shared
		@State private var showRebuildConfirmation = false
		@State private var showRebuildRefusal = false
		@State private var pendingItemCount = 0
		@State private var showPendingItemsConfirmation = false

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
							"Folders stored in the app appear in the Synctrain location in the Files app, unless they are hidden. Files that are only on your other devices can be downloaded while Synctrain is open."
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

				if !coordinator.keepNotices.isEmpty {
					Section {
						ForEach(Array(coordinator.keepNotices.enumerated()), id: \.offset) { _, notice in
							Text(notice)
						}
					} header: {
						Text("Still on this device")
					} footer: {
						Text("You asked to stop keeping these items on this device in the Files app.")
					}
				}

				if let error = coordinator.lastError {
					Section("Error") {
						Text(error).foregroundStyle(.red)
					}
				}

				Section {
					NavigationLink(destination: RemoteFoldersSettingsView()) {
						Text("Remote folders").badge(remote.bindings.count)
					}
				} footer: {
					Text(
						"Folders opened through a gateway appear in the Synctrain location too, without being synchronized on this device."
					)
				}
				RemoteMigrationsSection()
				RemoteNoticeSection()
				RemoteOperationsSection()

				Section {
					Button("Rebuild Synctrain location", role: .destructive) {
						if remote.hasUnresolvedOperations {
							showRebuildRefusal = true
						}
						else {
							showRebuildConfirmation = true
						}
					}
					.disabled(coordinator.isRebuilding)
				} footer: {
					Text(
						"Removes the Synctrain location from the Files app and sets it up again. Your files are not affected, but you will need to turn the location on again in the Files app. Edits of folders on gateways are kept."
					)
				}
			}
			.navigationTitle("Files app")
			.task {
				await coordinator.refreshDomainState()
				await remote.reloadState()
			}
			.alert("The location cannot be rebuilt now", isPresented: $showRebuildRefusal) {
				Button("OK") {}
			} message: {
				Text(FileProviderCoordinator.unresolvedRemoteEditsMessage)
			}
			.confirmationDialog(
				"Rebuild the Synctrain location?", isPresented: $showRebuildConfirmation, titleVisibility: .visible
			) {
				Button("Rebuild", role: .destructive) {
					Task {
						// Changes the system has not handed to Synctrain yet would be lost
						pendingItemCount = await coordinator.pendingItemCount()
						if pendingItemCount > 0 {
							showPendingItemsConfirmation = true
						}
						else {
							await coordinator.rebuild()
						}
					}
				}
			}
			.confirmationDialog(
				"Discard changes that were not saved?", isPresented: $showPendingItemsConfirmation,
				titleVisibility: .visible
			) {
				Button("Rebuild Anyway", role: .destructive) {
					Task {
						await coordinator.rebuild()
					}
				}
			} message: {
				Text(
					"Changes to \(pendingItemCount) items made in the Files app have not reached Synctrain yet. Rebuilding the location discards them."
				)
			}
		}
	}
#endif
