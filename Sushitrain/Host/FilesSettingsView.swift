// Copyright (C) 2026 Gordon
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this file,
// You can obtain one at https://mozilla.org/MPL/2.0/.
#if os(macOS)
	import SwiftUI
	import SynctrainHostKit

	struct FilesSettingsView: View {
		@Binding var presence: AppPresence

		var body: some View {
			MacSettingsView(mode: .filesInFinder) { category in
				FilesSettingsDetail(category: category, presence: $presence)
			}
			.task { await RemoteCoordinator.shared.refresh() }
		}
	}

	private struct FilesSettingsDetail: View {
		let category: MacSettingsCategory
		@Binding var presence: AppPresence

		var body: some View {
			switch category {
			case .historySources: HistorySourcesView()
			case .general: FilesGeneralSettingsView(presence: $presence)
			case .gateways: FilesGatewaysSettingsView()
			case .finder: FinderSettingsView()
			case .pendingEdits: PendingEditsSettingsView()
			default: EmptyView()
			}
		}
	}

	private struct FilesGeneralSettingsView: View {
		@Binding var presence: AppPresence

		var body: some View {
			Form {
				Section {
					AutoStartToggleView()
					AppPresenceToggles(presence: $presence)
				}
				Section {
					SwitchToSynchronizeButton()
				} footer: {
					Text("Folders then leave Finder and stay on their gateways, and this Mac runs an engine again. Pairings stay.")
				}
			}
		}
	}

	private struct FilesGatewaysSettingsView: View {
		var body: some View {
			Form {
				CrashNoticeSection()
				RemoteGatewaysSection()
			}
			.refreshable { await RemoteCoordinator.shared.refresh() }
		}
	}

	private struct FinderSettingsView: View {
		private var coordinator = RemoteCoordinator.shared
		@Environment(\.settingsNavigation) private var navigation

		var body: some View {
			if coordinator.gateways.isEmpty {
				ContentUnavailableView {
					Label("No gateways paired", systemImage: "folder")
				} description: {
					Text("Pair with a gateway to see its folders in Finder.")
				} actions: {
					Button("Go to Gateways") { navigation?.request(.gateways) }
				}
			}
			else {
				Form {
					FilesLocationSettingsView()
					Section {
						Button("Show in Finder", systemImage: "folder") {
							Task {
								if let url = await FilesDomain.shared.locationURL() { ClientLifecycle.show(url) }
							}
						}
					}
				}
			}
		}
	}

	private struct PendingEditsSettingsView: View {
		private var coordinator = RemoteCoordinator.shared

		var body: some View {
			Group {
				if coordinator.operations.isEmpty {
					ContentUnavailableView(
						"No pending edits", systemImage: "checkmark.circle",
						description: Text("Edits waiting to reach a gateway or needing your review appear here."))
				}
				else {
					Form { RemoteOperationsSection() }
				}
			}
			.refreshable { await coordinator.refresh() }
		}
	}
#endif
