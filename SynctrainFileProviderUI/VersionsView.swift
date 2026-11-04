// Copyright (C) 2026 Gordon
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this file,
// You can obtain one at https://mozilla.org/MPL/2.0/.
import QuickLook
import SwiftUI
import SynctrainFileProviderKit

/// An error the Files app asks the extension to explain: a gateway that no longer accepts this device
struct ErrorView: View {
	let message: String
	let done: () -> Void

	var body: some View {
		NavigationStack {
			ContentUnavailableView {
				Label("Device Not Accepted", systemImage: "exclamationmark.triangle")
			} description: {
				Text(message)
			}
			#if os(macOS)
				.safeAreaInset(edge: .bottom) {
					HStack {
						Spacer()
						Button("Done", action: done)
						.keyboardShortcut(.defaultAction)
					}
					.padding()
				}
			#else
				.toolbar {
					ToolbarItem(placement: .confirmationAction) {
						Button("Done", action: done)
					}
				}
			#endif
		}
	}
}
