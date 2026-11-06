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
		case .needsGateway, .needsFolders, .ready:
			MobileManagementView()
		}
	}
}
