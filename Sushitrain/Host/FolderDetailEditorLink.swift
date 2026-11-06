// Copyright (C) 2026 Gordon. SPDX-License-Identifier: MPL-2.0
#if os(macOS)
	import SwiftUI
	import SushitrainCore

	struct FolderDetailEditorLink: View {
		enum Editor { case synchronization, versioning, ignores }
		let folder: SushitrainFolder
		let editor: Editor
		var body: some View {
			Section {
				GuardedSettingsLink(title: Text(title)) {
					switch editor {
					case .synchronization: FolderSyncSettingsView(folder: folder)
					case .versioning: FolderVersioningSettingsView(folder: folder)
					case .ignores: IgnoresView(folder: folder)
					}
				}
			}
		}
		private var title: LocalizedStringKey {
			switch editor {
			case .synchronization: "Synchronization"
			case .versioning: "Versioning"
			case .ignores: "Ignore Patterns"
			}
		}
	}
#endif
