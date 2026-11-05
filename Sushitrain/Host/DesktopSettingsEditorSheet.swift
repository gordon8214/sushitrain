// Copyright (C) 2026 Gordon. SPDX-License-Identifier: MPL-2.0
#if os(macOS)
	import SwiftUI

	/// Explicit Cancel owns discarding a draft; implicit dismissal and native Back cannot discard it.
	struct DesktopSettingsEditorSheet: View {
		let action: String
		let appState: AppState?
		var body: some View {
			DesktopSettingsPanel { DesktopSettingsEditor(action: action, appState: appState) }
		}
	}
#endif
