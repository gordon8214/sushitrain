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
		@State private var search = SettingsSearch(catalog: .desktop, platform: "macOS")
		var appState: AppState?
		var searchDestination: ((SettingSearchEntry) -> AnyView)?
		private var searchRevision: String {
			"\(appState?.finderConfigurationGeneration ?? 0).\(appState?.startupState == .started).\(appState?.isEngineRestarting ?? false)"
		}

		init(mode: MacSettingsMode, @ViewBuilder detail: @escaping (MacSettingsCategory) -> Detail) {
			self.mode = mode
			self.detail = detail
			_navigation = State(initialValue: MacSettingsNavigation(mode: mode))
		}

		var body: some View {
			@Bindable var navigation = navigation
			let path = navigation.path
			NavigationSplitView {
				MacSettingsSidebar(mode: mode, navigation: navigation, search: search)
			} detail: {
				NavigationStack(path: Binding(get: { path }, set: { navigation.requestPath($0) })) {
					Group {
						if let target = navigation.searchTarget, let searchDestination {
							SettingsSearchReveal(anchor: target.anchor) { searchDestination(target) }
								.toolbar {
									ToolbarItem(placement: .navigation) {
										Button("Back", systemImage: "chevron.backward") { navigation.requestBack() }
											.keyboardShortcut("[", modifiers: .command).disabled(navigation.isSaving)
									}
								}
						}
						else {
							detail(navigation.selection)
						}
					}
					.environment(\.settingsPageID, navigation.rootPageID)
					.navigationTitle(navigation.searchTarget?.localizedTitle ?? String(localized: navigation.selection.title))
					.navigationDestination(for: MacSettingsRoute.self) { route in navigation.page(route) }
				}
				.environment(\.settingsStack, navigation)
				.environment(\.settingsUsesStack, true)
				.id("\(navigation.selection.rawValue).\(navigation.editorRevision)")
			}
			.background(SettingsWindowCloseGuard(navigation: navigation))
			.environment(\.settingsNavigation, navigation)
			.task(id: searchRevision) {
				guard let appState, appState.startupState == .started, !appState.isEngineRestarting else {
					search.resetEngine()
					return
				}
				let client = appState.client
				await search.refresh {
					let data = try await goBlocking { try client.settingsSearchIndex() }
					return try JSONDecoder().decode([SettingSearchEntry].self, from: data)
				}
			}
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
		@Bindable var search: SettingsSearch
		@State private var selectedSearchID: String?

		var body: some View {
			Group {
				if !search.query.trimmingCharacters(in: .whitespacesAndNewlines).isEmpty {
					List(
						selection: Binding<String?>(
							get: { selectedSearchID },
							set: { id in
								selectedSearchID = id
								if let entry = search.results.first(where: { $0.id == id }) { navigation.requestSearch(entry) }
							}
						)
					) {
						if let error = search.error { Text(error).font(.callout).foregroundStyle(.secondary) }
						if search.results.isEmpty { Text("No Results") }
						ForEach(search.results) { entry in
							SettingsSearchResultLabel(entry: entry)
								.tag(entry.id)
								.accessibilityIdentifier("settings.result.\(entry.id)")
						}
					}
				}
				else {
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
				}
			}
			.searchable(text: $search.query, placement: .sidebar, prompt: "Search Settings")
			.onSubmit(of: .search) { if let entry = search.results.first { navigation.requestSearch(entry) } }
			.onChange(of: navigation.searchTarget?.id) { _, id in selectedSearchID = id }
			.onChange(of: navigation.isConfirmingDiscard) { _, confirming in
				if !confirming { selectedSearchID = navigation.searchTarget?.id }
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
			.searchContext(appState: appState) { entry in
				DesktopSettingsSearchDestination(entry: entry, mode: .synchronize, presence: $presence, appState: appState)
			}
			.task { MCPIntegration.shared.start(appState) }
		}
	}

	extension MacSettingsView {
		func searchContext<Content: View>(
			appState: AppState? = nil, @ViewBuilder destination: @escaping (SettingSearchEntry) -> Content
		) -> Self {
			var view = self
			view.appState = appState
			view.searchDestination = { AnyView(destination($0)) }
			return view
		}
	}

#endif
