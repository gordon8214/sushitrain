// Copyright (C) 2026 Gordon
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this file,
// You can obtain one at https://mozilla.org/MPL/2.0/.
#if os(macOS)
	import SwiftUI
	import SynctrainHostKit

	struct MacSettingsView<Detail: View>: View {
		let mode: MacSettingsMode
		@ViewBuilder let detail: (MacSettingsCategory) -> Detail
		@State private var navigation: MacSettingsNavigation

		init(mode: MacSettingsMode, @ViewBuilder detail: @escaping (MacSettingsCategory) -> Detail) {
			self.mode = mode
			self.detail = detail
			_navigation = State(initialValue: MacSettingsNavigation(mode: mode))
		}

		var body: some View {
			@Bindable var navigation = navigation
			let path = navigation.path
			NavigationSplitView {
				MacSettingsSidebar(mode: mode, navigation: navigation)
			} detail: {
				NavigationStack(path: Binding(get: { path }, set: { navigation.requestPath($0) })) {
					detail(navigation.selection)
						.environment(\.settingsPageID, navigation.rootPageID)
						.navigationTitle(Text(navigation.selection.title))
						.navigationDestination(for: MacSettingsRoute.self) { route in navigation.page(route) }
				}
				.environment(\.settingsStack, navigation)
				.environment(\.settingsUsesStack, true)
				.id("\(navigation.selection.rawValue).\(navigation.editorRevision)")
			}
			.background(SettingsWindowCloseGuard(navigation: navigation))
			.environment(\.settingsNavigation, navigation)
			.formStyle(.grouped)
			.frame(minWidth: 800, minHeight: 560)
			.windowResizeBehavior(.enabled)
			#if DEBUG
				.preferredColorScheme(SettingsFixtureApp.colorScheme)
			#endif
			.alert("Discard unsaved changes?", isPresented: $navigation.isConfirmingDiscard) {
				Button("Keep Editing", role: .cancel) { navigation.keepEditing() }
				Button("Discard Changes", role: .destructive) { navigation.discardChanges() }
			} message: {
				Text("The changes on this page have not been saved.")
			}
		}
	}

	private struct MacSettingsSidebar: View {
		let mode: MacSettingsMode
		let navigation: MacSettingsNavigation

		var body: some View {
			List(
				selection: Binding<MacSettingsCategory?>(
					get: { navigation.selection },
					set: { category in
						if let category { navigation.request(category) }
					})
			) {
				ForEach(mode.categories) { category in
					Label {
						Text(category.title)
					} icon: {
						Image(systemName: category.symbol)
					}
					.tag(category)
					.accessibilityIdentifier("settings.category.\(category.rawValue)")
				}
			}
			.listStyle(.sidebar)
			.navigationTitle("Settings")
			.navigationSplitViewColumnWidth(min: 220, ideal: 240, max: 280)
			.disabled(navigation.isSaving)
			.accessibilityLabel("Settings categories")
		}
	}

	struct SynchronizeSettingsView: View {
		@Binding var presence: AppPresence
		@Environment(AppState.self) private var appState

		var body: some View {
			MacSettingsView(mode: .synchronize) { category in
				DesktopSettingsCategoryView(category: category, mode: .synchronize, presence: $presence, appState: appState)
			}
			.task { MCPIntegration.shared.start(appState) }
		}
	}

#endif
