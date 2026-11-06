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
			.searchContext { entry in
				DesktopSettingsSearchDestination(entry: entry, mode: .filesInFinder, presence: $presence, appState: nil)
			}
			.task { await RemoteCoordinator.shared.refresh() }
		}
	}

	private struct FilesSettingsDetail: View {
		let category: MacSettingsCategory
		@Binding var presence: AppPresence
		var body: some View {
			DesktopSettingsCategoryView(category: category, mode: .filesInFinder, presence: $presence, appState: nil)
		}
	}
#endif
