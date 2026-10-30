// Copyright (C) 2026 Gordon
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this file,
// You can obtain one at https://mozilla.org/MPL/2.0/.
import SwiftUI

/// What the app shows, from its first launch on: pairing, choosing folders, then the app itself
struct RootView: View {
	let model: AppModel

	var body: some View {
		switch model.phase {
		case .unavailable:
			ContentUnavailableView(
				"Synctrain cannot start", systemImage: "exclamationmark.triangle",
				description: Text("The storage it shares with the Files app is not available."))
		case .loading:
			ProgressView()
		case .loadFailed(let message):
			ContentUnavailableView {
				Label("Synctrain cannot start", systemImage: "exclamationmark.triangle")
			} description: {
				Text(message)
			} actions: {
				Button("Try Again") {
					Task { await model.start() }
				}
			}
		case .needsGateway:
			GatewayPromptView()
		case .needsFolders:
			NavigationStack {
				ChooseFoldersView()
					.toolbar {
						ToolbarItem(placement: .primaryAction) {
							NavigationLink {
								RemoteFoldersSettingsView()
							} label: {
								Label("Gateways", systemImage: "externaldrive.connected.to.line.below")
							}
						}
					}
			}
		case .ready:
			TabView {
				Tab("Folders", systemImage: "folder") {
					FoldersView()
				}
				Tab("Settings", systemImage: "gear") {
					SettingsView()
				}
				Tab(role: .search) {
					SearchView()
				}
			}
			.tabViewStyle(.sidebarAdaptable)
		}
	}
}

/// The first thing a new user sees: Synctrain shows the folders of a gateway, so it needs one
private struct GatewayPromptView: View {
	@State private var showPairing = false

	var body: some View {
		ContentUnavailableView {
			Label("Pair with a Gateway", systemImage: "externaldrive.connected.to.line.below")
		} description: {
			Text(
				"Synctrain opens your folders through a gateway: a Mac running Synctrain, or a server running synctrain-gateway."
			)
		} actions: {
			Button("Pair with a Gateway…") {
				showPairing = true
			}
			.buttonStyle(.borderedProminent)
		}
		.sheet(isPresented: $showPairing) {
			RemotePairingView()
		}
	}
}
