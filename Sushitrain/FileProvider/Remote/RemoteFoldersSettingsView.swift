// Copyright (C) 2026 Gordon
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this file,
// You can obtain one at https://mozilla.org/MPL/2.0/.
#if os(iOS)
	import SwiftUI
	import SynctrainFileProviderKit

	/// Settings › Remote folders: the paired gateways, pairing with another one, folder switches in progress and edits
	/// waiting for gateways
	struct RemoteFoldersSettingsView: View {
		private var coordinator = RemoteCoordinator.shared
		@State private var showPairing = false

		var body: some View {
			Form {
				Section {
					ForEach(coordinator.gateways) { status in
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
						"A gateway is a Mac running Synctrain, or a server running synctrain-gateway, that synchronizes folders with Syncthing. Folders opened through a gateway can be browsed and edited in the Files app without keeping copies of them on this device. The gateway has to be reachable, on the same network or through a VPN, to open files that are not on this device and to save edits."
					)
				}

				RemoteMigrationsSection()
				RemoteNoticeSection()
				RemoteOperationsSection()
			}
			.navigationTitle("Remote folders")
			.refreshable {
				await coordinator.refresh()
			}
			.task {
				await coordinator.refresh()
			}
			.sheet(isPresented: $showPairing) {
				RemotePairingView()
			}
		}
	}

	private struct RemoteGatewayRow: View {
		let status: RemoteGatewayStatus

		var body: some View {
			VStack(alignment: .leading, spacing: 2) {
				Text(status.displayName)
				HStack(spacing: 4) {
					Circle().fill(color).frame(width: 8, height: 8)
					Text(status.summary)
				}
				.font(.caption)
				.foregroundStyle(.secondary)
			}
		}

		private var color: Color {
			switch status.connection {
			case .unknown: return .gray
			case .reachable: return status.isCompatible == false ? .orange : .green
			case .unreachable: return .orange
			default: return .red
			}
		}
	}

	/// Folder switches between Syncthing on this device and a gateway that are under way
	struct RemoteMigrationsSection: View {
		private var coordinator = RemoteCoordinator.shared

		var body: some View {
			let migrations = coordinator.migrations.values.sorted { $0.folderID < $1.folderID }
			if !migrations.isEmpty {
				Section {
					ForEach(migrations, id: \.folderID) { migration in
						RemoteMigrationRow(migration: migration)
					}
				} header: {
					Text("Switching")
				} footer: {
					Text(
						"Edits of these folders wait until the switch is complete. A switch that was interrupted continues the next time Synctrain runs."
					)
				}
			}
		}
	}

	struct RemoteMigrationRow: View {
		let migration: RemoteMigration
		private var coordinator = RemoteCoordinator.shared

		init(migration: RemoteMigration) {
			self.migration = migration
		}

		var body: some View {
			VStack(alignment: .leading, spacing: 2) {
				Text(label)
				Text(stateDescription).font(.caption).foregroundStyle(.secondary)
				if let error = migration.journal.lastError {
					Text(error).font(.caption).foregroundStyle(.red)
				}
			}
		}

		private var label: String {
			migration.journal.label ?? coordinator.syncthingFolder(id: migration.folderID)?.displayName
				?? migration.folderID
		}

		private var stateDescription: String {
			switch migration.state {
			case .fenced:
				return String(localized: "Checking that the gateway has every change made on this device")
			case .engineStopped:
				return String(localized: "Syncthing on this device stopped; connecting the folder to the gateway")
			case .bound:
				return String(localized: "Listing the folder on the gateway")
			case .returning:
				return String(localized: "Switching back to Syncthing")
			}
		}
	}

	/// A problem with remote folders the user should know about
	struct RemoteNoticeSection: View {
		private var coordinator = RemoteCoordinator.shared

		var body: some View {
			if let notice = coordinator.notice {
				Section {
					Text(notice).foregroundStyle(.orange)
					Button("OK") {
						coordinator.dismissNotice()
					}
				}
			}
		}
	}
#endif
