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
			NavigationSplitView {
				MacSettingsSidebar(mode: mode, navigation: navigation)
			} detail: {
				NavigationStack {
					detail(navigation.selection)
						.navigationTitle(Text(navigation.selection.title))
				}
				.id(navigation.selection)
			}
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
				if appState.startupState == .started || appState.isEngineRestarting {
					SynchronizeSettingsDetail(category: category, presence: $presence)
				}
				else {
					ContentUnavailableView(
						"Settings unavailable", systemImage: "gearshape", description: Text("Start the engine to change its settings."))
				}
			}
		}
	}

	private struct SynchronizeSettingsDetail: View {
		let category: MacSettingsCategory
		@Binding var presence: AppPresence
		@Environment(AppState.self) private var appState

		var body: some View {
			switch category {
			case .historySources: HistorySourcesView()
			case .general: GeneralSettingsView(userSettings: appState.userSettings, presence: $presence)
			case .filesAndPreviews: FilesPreviewSettingsView(userSettings: appState.userSettings)
			case .network: NetworkSettingsView(userSettings: appState.userSettings)
			case .bandwidth: BandwidthSettingsView(userSettings: appState.userSettings)
			case .engine: EngineOptionsView()
			case .defaults: DefaultsSettingsView()
			case .remoteFileService: GatewaySettingsView()
			case .engineAccess: Form { EngineAccessSection() }
			case .configuration: ConfigurationSettingsView()
			case .troubleshooting: TroubleshootingView(userSettings: appState.userSettings)
			case .advancedConfiguration: AdvancedConfigurationView()
			default: EmptyView()
			}
		}
	}
#endif
