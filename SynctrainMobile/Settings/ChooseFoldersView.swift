// Copyright (C) 2026 Gordon
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this file,
// You can obtain one at https://mozilla.org/MPL/2.0/.
import SwiftUI

/// Chooses the folders of the paired gateways that this device opens
struct ChooseFoldersView: View {
	private var coordinator = RemoteCoordinator.shared

	var body: some View {
		Form {
			ForEach(coordinator.gateways) { status in
				RemoteGatewayFoldersSection(serviceID: status.id)
			}
		}
		.navigationTitle("Choose Folders")
		.refreshable {
			await coordinator.refresh()
		}
		.task {
			await coordinator.refresh()
		}
	}
}
