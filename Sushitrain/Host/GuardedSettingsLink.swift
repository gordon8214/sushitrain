// Copyright (C) 2026 Gordon. SPDX-License-Identifier: MPL-2.0
#if os(macOS)
	import SwiftUI

	/// Presents a native navigation row; the owning stack protects departures from staged editors.
	struct GuardedSettingsLink<Content: View>: View {
		let title: Text
		@ViewBuilder let content: () -> Content
		@Environment(\.settingsStack) private var navigation
		@State private var route = MacSettingsRoute()
		init(_ title: LocalizedStringKey, @ViewBuilder content: @escaping () -> Content) {
			self.title = Text(title)
			self.content = content
		}
		init(title: Text, @ViewBuilder content: @escaping () -> Content) {
			self.title = title
			self.content = content
		}
		var body: some View {
			if let navigation {
				NavigationLink(value: route) { title }
					.onAppear { navigation.register(route, title: title, content: content()) }
			}
			else {
				NavigationLink {
					StandaloneSettingsPage(title: title, content: content)
				} label: {
					title
				}
			}
		}
	}

	struct SettingsStackPage<Content: View>: View {
		let navigation: MacSettingsNavigation
		let pageID: UUID
		let title: Text
		@ViewBuilder let content: () -> Content
		var body: some View {
			content()
				.environment(\.settingsPageID, pageID)
				.navigationTitle(title)
				.navigationBarBackButtonHidden()
				.toolbar {
					ToolbarItem(placement: .navigation) {
						Button("Back", systemImage: "chevron.backward") { navigation.requestBack() }
							.keyboardShortcut("[", modifiers: .command)
							.disabled(navigation.isSaving)
					}
				}
		}
	}

	/// Folder editors can also use a stack in the main app window.
	private struct StandaloneSettingsPage<Content: View>: View {
		let title: Text
		@ViewBuilder let content: () -> Content
		@Environment(\.dismiss) private var dismiss
		@Environment(\.settingsNavigation) private var navigation
		@State private var confirmingDiscard = false
		var body: some View {
			content()
				.environment(\.settingsUsesStack, true)
				.navigationTitle(title)
				.navigationBarBackButtonHidden()
				.toolbar {
					ToolbarItem(placement: .navigation) {
						Button("Back", systemImage: "chevron.backward") {
							if navigation?.hasChanges == true {
								confirmingDiscard = true
							}
							else {
								dismiss()
							}
						}
						.keyboardShortcut("[", modifiers: .command)
						.disabled(navigation?.isSaving == true)
					}
				}
				.alert("Discard unsaved changes?", isPresented: $confirmingDiscard) {
					Button("Keep Editing", role: .cancel) {}
					Button("Discard Changes", role: .destructive) { dismiss() }
				} message: {
					Text("The changes on this page have not been saved.")
				}
		}
	}
#endif
