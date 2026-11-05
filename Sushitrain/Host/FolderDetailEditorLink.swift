// Copyright (C) 2026 Gordon. SPDX-License-Identifier: MPL-2.0
#if os(macOS)
	import SwiftUI
	import SushitrainCore

	struct FolderDetailEditorLink: View {
		enum Editor { case synchronization, versioning, ignores }
		let folder: SushitrainFolder
		let editor: Editor
		@State private var presented = false
		var body: some View {
			Section {
				Button("Configure…") { presented = true }
			}
			.sheet(isPresented: $presented) {
				NavigationStack {
					switch editor {
					case .synchronization: FolderSyncSettingsView(folder: folder)
					case .versioning: FolderVersioningSettingsView(folder: folder)
					case .ignores: IgnoresView(folder: folder)
					}
				}
			}
		}
	}
#endif
