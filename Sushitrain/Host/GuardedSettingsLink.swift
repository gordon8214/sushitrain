// Copyright (C) 2026 Gordon. SPDX-License-Identifier: MPL-2.0
#if os(macOS)
	import SwiftUI

	/// Staged editors have an explicit Cancel/Save boundary instead of an unguarded stack pop.
	struct GuardedSettingsLink<Content: View>: View {
		let title: Text
		@ViewBuilder let content: () -> Content
		@State private var presented = false
		init(_ title: LocalizedStringKey, @ViewBuilder content: @escaping () -> Content) {
			self.title = Text(title)
			self.content = content
		}
		init(title: Text, @ViewBuilder content: @escaping () -> Content) {
			self.title = title
			self.content = content
		}
		var body: some View {
			Button {
				presented = true
			} label: {
				title
			}
			.sheet(isPresented: $presented) { DesktopSettingsPanel { content() } }
		}
	}

	struct DesktopSettingsPanel<Content: View>: View {
		@ViewBuilder let content: () -> Content
		@Environment(\.dismiss) private var dismiss
		@Environment(\.settingsNavigation) private var navigation
		var body: some View {
			NavigationStack {
				content().toolbar {
					ToolbarItem(placement: .cancellationAction) {
						Button(navigation?.hasChanges == true ? "Cancel" : "Done") { dismiss() }
							.disabled(navigation?.isSaving == true)
					}
				}
			}
			.frame(minWidth: 620, minHeight: 520)
			.interactiveDismissDisabled()
		}
	}
#endif
