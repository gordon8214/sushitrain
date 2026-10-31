// Copyright (C) 2026 Gordon
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this file,
// You can obtain one at https://mozilla.org/MPL/2.0/.
import SwiftUI
import SynctrainFileProviderKit

/// Settings › Gateways: the paired gateways, pairing with another one, the Files location and edits waiting for
/// gateways. On a Mac in Files in Finder mode this is the app's window, which adds sections of its own.
struct RemoteFoldersSettingsView<Extra: View>: View {
	private let extra: Extra
	private var coordinator = RemoteCoordinator.shared
	@State private var showPairing = false

	init(@ViewBuilder extra: () -> Extra) {
		self.extra = extra()
	}

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
				#if os(macOS)
					Text(
						"A gateway is a server running synctrain-gateway, or another Mac running Synctrain in Synchronize mode, that keeps all the files of its folders. Add a gateway's folders to see them in Finder without keeping copies of them on this Mac. The gateway has to be reachable, on the same network or through a VPN, to open files that are not on this Mac and to save edits."
					)
				#else
					Text(
						"A gateway is a Mac running Synctrain, or a server running synctrain-gateway, that synchronizes folders with Syncthing. Its folders can be browsed here and edited in the Files app without keeping copies of them on this device. The gateway has to be reachable, on the same network or through a VPN, to open files that are not on this device and to save edits."
					)
				#endif
			}

			if !coordinator.gateways.isEmpty {
				FilesLocationSettingsView()
			}
			RemoteOperationsSection()
			extra
		}
		#if os(macOS)
			.formStyle(.grouped)
		#endif
		.navigationTitle("Gateways")
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

extension RemoteFoldersSettingsView where Extra == EmptyView {
	init() {
		self.init {
			EmptyView()
		}
	}
}

struct RemoteGatewayRow: View {
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
