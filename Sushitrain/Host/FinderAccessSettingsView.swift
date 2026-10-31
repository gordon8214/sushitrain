// Copyright (C) 2026 Gordon
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this file,
// You can obtain one at https://mozilla.org/MPL/2.0/.
#if os(macOS)
	import SwiftUI
	import SynctrainFileProviderKit

	/// Settings › Files in Finder: turning the location on, which folders of this Mac are on demand, and other gateways
	struct FinderAccessSettingsView: View {
		private var access = FinderAccess.shared
		private var coordinator = RemoteCoordinator.shared
		@State private var showPairing = false
		@State private var isChanging = false

		var body: some View {
			NavigationStack {
				Form {
					Section {
						Toggle(
							"Show folders in Finder",
							isOn: Binding(
								get: { access.isEnabled },
								set: { enabled in
									isChanging = true
									Task {
										await access.setEnabled(enabled)
										isChanging = false
									}
								})
						)
						.disabled(isChanging || access.isWorking)
						if let error = access.lastError {
							Text(error).foregroundStyle(.red)
						}
						if access.isEnabled, access.serviceID == nil, !access.isWorking {
							Button("Try Again") {
								access.retry()
							}
						}
					} footer: {
						Text(
							"Folders that are on demand appear under Locations in Finder with all their files. Only what you open comes to this Mac, from the other devices that have it, and it can be removed again at any time. Other folders stay as they are."
						)
					}

					if access.isEnabled {
						FinderAccessFoldersSection()

						Section {
							ForEach(coordinator.gateways.filter { $0.id != access.serviceID }) { status in
								NavigationLink(destination: RemoteGatewayView(serviceID: status.id)) {
									RemoteGatewayRow(status: status)
								}
							}
							Button("Pair with a Gateway…", systemImage: "plus") {
								showPairing = true
							}
						} header: {
							Text("Gateways")
						} footer: {
							Text(
								"A gateway is another Mac running Synctrain, or a server running synctrain-gateway. Its folders can be added here without synchronizing them to this Mac."
							)
						}

						FilesLocationSettingsView()
						RemoteOperationsSection()
					}
				}
				.formStyle(.grouped)
			}
			.task {
				await access.refreshFolders()
			}
			.sheet(isPresented: $showPairing) {
				RemotePairingView()
			}
		}
	}

	/// This Mac's folders, and switching them between synchronized and on demand
	private struct FinderAccessFoldersSection: View {
		private var access = FinderAccess.shared
		@State private var confirmOnDemand: FinderAccess.Folder? = nil
		@State private var confirmSynchronizing: FinderAccess.Folder? = nil
		@State private var errorMessage: String? = nil
		@State private var keptFiles: Int? = nil

		var body: some View {
			Section {
				if access.serviceID == nil {
					if access.isWorking {
						ProgressView()
					}
				}
				else if access.folders.isEmpty {
					Text("This Mac has no folders yet.").foregroundStyle(.secondary)
				}
				else {
					ForEach(access.folders) { folder in
						row(folder)
					}
				}
			} header: {
				Text("This Mac")
			} footer: {
				Text(
					"A folder that is on demand stays where it is, but keeps only the files that were edited here, or that no other device has. Others open through Finder, from a device that is connected. Previous versions list what this Mac kept."
				)
			}
			.confirmationDialog(
				"Keep this folder on demand?", isPresented: Binding.isNotNil($confirmOnDemand), titleVisibility: .visible,
				presenting: confirmOnDemand
			) { folder in
				Button("Switch to On Demand") {
					switchToOnDemand(folder)
				}
			} message: { folder in
				Text(
					"Files of \(folder.name) that a connected device has are removed from this Mac, and open through Finder from now on. Files that only this Mac has stay here."
				)
			}
			.confirmationDialog(
				"Synchronize all of this folder?", isPresented: Binding.isNotNil($confirmSynchronizing),
				titleVisibility: .visible, presenting: confirmSynchronizing
			) { folder in
				Button("Synchronize All Files") {
					switchToSynchronizing(folder)
				}
			} message: { folder in
				Text(
					"\(folder.name) leaves Finder's Synctrain location, and all of its files are downloaded to this Mac. Edits that were not saved yet have to be saved or discarded first."
				)
			}
			.alert("Could not change the folder", isPresented: Binding.isNotNil($errorMessage), presenting: errorMessage) { _ in
				Button("OK") {}
			} message: { message in
				Text(message)
			}
			.alert("Some files stay on this Mac", isPresented: Binding.isNotNil($keptFiles), presenting: keptFiles) { _ in
				Button("OK") {}
			} message: { count in
				Text("No connected device has \(count) of the folder's files, so they stay on this Mac for now.")
			}
		}

		@ViewBuilder private func row(_ folder: FinderAccess.Folder) -> some View {
			LabeledContent {
				if access.switching == folder.id {
					ProgressView()
				}
				else if access.localFolderID(folder.id) != nil {
					Button("Show in Finder", systemImage: "folder") {
						showInFinder(folder)
					}
					.labelStyle(.iconOnly)
					.buttonStyle(.borderless)
					Button("Synchronize All") {
						confirmSynchronizing = folder
					}
					.disabled(access.isWorking)
				}
				else {
					Button("Switch to On Demand") {
						confirmOnDemand = folder
					}
					.disabled(folder.restriction != nil || access.isWorking)
					if folder.isOnDemand {
						// A switch that stopped halfway: either way out is open
						Button("Synchronize All") {
							confirmSynchronizing = folder
						}
						.disabled(access.isWorking)
					}
				}
			} label: {
				VStack(alignment: .leading) {
					Text(folder.name)
					if let restriction = folder.restriction {
						Text(restriction).font(.caption).foregroundStyle(.secondary)
					}
					else if access.localFolderID(folder.id) != nil {
						Text("On demand").font(.caption).foregroundStyle(.secondary)
					}
					else if folder.isOnDemand {
						Text("On demand, not in Finder yet").font(.caption).foregroundStyle(.secondary)
					}
					else {
						Text("Synchronized").font(.caption).foregroundStyle(.secondary)
					}
				}
			}
		}

		private func switchToOnDemand(_ folder: FinderAccess.Folder) {
			Task {
				do {
					let kept = try await access.switchToOnDemand(folderID: folder.id)
					if kept > 0 {
						keptFiles = kept
					}
				}
				catch {
					errorMessage = error.localizedDescription
				}
			}
		}

		private func switchToSynchronizing(_ folder: FinderAccess.Folder) {
			Task {
				do {
					try await access.switchToSynchronizing(folderID: folder.id)
				}
				catch {
					errorMessage = error.localizedDescription
				}
			}
		}

		private func showInFinder(_ folder: FinderAccess.Folder) {
			guard let localID = access.localFolderID(folder.id) else {
				return
			}
			Task {
				if let url = await FilesDomain.shared.userVisibleURL(folderID: localID, path: "") {
					ClientLifecycle.show(url)
				}
				else {
					errorMessage = String(localized: "The folder is not shown in Finder yet. Is the Synctrain location turned on?")
				}
			}
		}
	}
#endif
