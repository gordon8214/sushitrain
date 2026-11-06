// Copyright (C) 2026 Gordon. SPDX-License-Identifier: MPL-2.0
#if os(macOS)
	import SwiftUI
	import SynctrainHostKit
	@preconcurrency import SushitrainCore

	@MainActor enum MacManagementCommands {
		static let request = Notification.Name("SynctrainManagementDestination")
		private static var pending: String?
		private static var explanation: String?

		static func show(_ destination: String, openWindow: OpenWindowAction) {
			AppWindowPresentation.open {
				if AppMode.resolve() == .synchronize {
					QuickActionService.shared.action = .desktop(destination)
				}
				else {
					queue(destination)
					openWindow(id: "files", value: "files")
				}
			}
		}

		static func queue(_ destination: String) {
			let raw = destination.hasPrefix("settings:") ? String(destination.dropFirst(9)) : destination
			let id = DesktopUICatalog.desktop.aliases[raw] ?? raw
			let visible = DesktopUICatalog.desktop.navigation(platform: "macOS", mode: "files").flatMap(\.items).contains {
				$0.id == id
			}
			pending = visible ? id : "overview"
			explanation =
				visible ? nil : String(localized: "The requested page is unavailable in Files mode. Overview is shown instead.")
			NotificationCenter.default.post(name: request, object: pending)
		}
		static func takeExplanation() -> String? {
			defer { explanation = nil }
			return explanation
		}

		static func takePending() -> String? {
			defer { pending = nil }
			return pending
		}
	}

	struct ManagementSearchResults: View {
		let search: SettingsSearch
		let navigation: MacSettingsNavigation
		var body: some View {
			if let error = search.error { Text(error).foregroundStyle(.secondary) }
			if search.results.isEmpty { ContentUnavailableView.search(text: search.query) }
			ForEach(search.results) { entry in
				Button {
					navigation.requestSearch(entry)
				} label: {
					SettingsSearchResultLabel(entry: entry)
				}
				.buttonStyle(.plain)
				.accessibilityIdentifier("settings.result.\(entry.id)")
			}
		}
	}

	struct MacManagementDetail: View {
		let destination: String
		let mode: MacSettingsMode
		let navigation: MacSettingsNavigation
		@Binding var presence: AppPresence
		var appState: AppState?

		var body: some View {
			NavigationStack(path: Binding(get: { navigation.path }, set: { navigation.requestPath($0) })) {
				Group {
					if let entry = navigation.searchTarget {
						SettingsSearchReveal(anchor: entry.anchor) {
							DesktopSettingsSearchDestination(entry: entry, mode: mode, presence: $presence, appState: appState)
						}
						.navigationTitle(entry.localizedTitle)
						.toolbar {
							ToolbarItem(placement: .navigation) {
								Button("Back", systemImage: "chevron.backward") { navigation.requestBack() }
									.keyboardShortcut("[", modifiers: .command).disabled(navigation.isSaving)
							}
						}
					}
					else if let category = MacSettingsCategory(rawValue: destination) {
						DesktopSettingsCategoryView(category: category, mode: mode, presence: $presence, appState: appState)
					}
				}
				.environment(\.settingsPageID, navigation.rootPageID)
				.navigationDestination(for: MacSettingsRoute.self) { route in navigation.page(route) }
			}
			.environment(\.settingsNavigation, navigation)
			.environment(\.settingsStack, navigation)
			.environment(\.settingsUsesStack, true)
			.id("\(destination).\(navigation.editorRevision)")
		}
	}

	private struct ManagementDiscardAlert: ViewModifier {
		@Bindable var navigation: MacSettingsNavigation
		func body(content: Content) -> some View {
			content.alert("Discard unsaved changes?", isPresented: $navigation.isConfirmingDiscard) {
				Button("Keep Editing", role: .cancel) { navigation.keepEditing() }
				Button("Discard Changes", role: .destructive) { navigation.discardChanges() }
			} message: {
				Text("The changes on this page have not been saved.")
			}
		}
	}

	extension View {
		func managementDiscardAlert(navigation: MacSettingsNavigation) -> some View {
			modifier(ManagementDiscardAlert(navigation: navigation))
		}
	}

	struct DesktopManagedFolders: View {
		@Environment(AppState.self) private var appState
		@Environment(\.navigateTo) private var navigateTo
		@State private var folders: [SushitrainFolder] = []
		@State private var adding = false
		@State private var draft = AddFolder()
		var body: some View {
			List {
				ForEach(folders, id: \.folderID) { folder in
					Section(folder.displayName) {
						if let url = folder.localNativeURL {
							Button("Show in Finder", systemImage: "folder") { openURLInSystemFilesApp(url: url) }
						}
						GuardedSettingsLink("Synchronization") { FolderSyncSettingsView(folder: folder) }
						GuardedSettingsLink("Versioning") { FolderVersioningSettingsView(folder: folder) }
						Button("Browse in Synctrain") { navigateTo(.folder(folderID: folder.folderID, prefix: nil)) }
					}
				}
				Button("Add Folder…", systemImage: "plus") { adding = true }
			}
			.navigationTitle("Folders")
			.task(id: appState.eventCounter) {
				folders = await appState.folders().sorted {
					$0.displayName.localizedStandardCompare($1.displayName) == .orderedAscending
				}
			}
			.sheet(isPresented: $adding) { NavigationStack { AddFolderView(adding: draft, shown: $adding) } }
		}
	}
#endif
