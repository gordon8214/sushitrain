// Copyright (C) 2026 Gordon
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this file,
// You can obtain one at https://mozilla.org/MPL/2.0/.
#if os(macOS)
	import Observation
	import SwiftUI
	import SynctrainHostKit

	enum MacSettingsCategory: String, Hashable, Sendable, Identifiable {
		case general, filesAndStorage, network, synchronization, integrations, defaults, advanced, about
		var id: String { rawValue }
		var descriptor: DesktopUICatalog.Category { DesktopUICatalog.desktop.settings.first { $0.id == rawValue }! }
		var title: LocalizedStringResource { LocalizedStringResource(stringLiteral: descriptor.title) }
		var symbol: String { descriptor.symbol }
	}

	enum MacSettingsMode: String {
		case synchronize, filesInFinder
		var categories: [MacSettingsCategory] {
			DesktopUICatalog.desktop.settings.compactMap { MacSettingsCategory(rawValue: $0.id) }
		}
		var defaultCategory: MacSettingsCategory { .general }
		var selectionKey: String { "macSettingsSelectedCategory.\(rawValue)" }
		var capabilityMode: String { self == .synchronize ? "synchronize" : "files" }
	}

	struct SettingsEditorState: Equatable {
		var hasChanges = false
		var isSaving = false
	}

	struct MacSettingsRoute: Hashable, Sendable { let id = UUID() }

	/// Each window owns its navigation and editors. Only accepted selections are remembered between openings.
	@Observable @MainActor final class MacSettingsNavigation {
		private var policy: SettingsNavigationPolicy<MacSettingsCategory>
		private struct Editor {
			let state: SettingsEditorState
			let pageID: UUID?
		}
		private var editors: [UUID: Editor] = [:]
		private var stack = SettingsStackPolicy<MacSettingsRoute>()
		private(set) var rootPageID = UUID()
		@ObservationIgnored private var pages: [MacSettingsRoute: (title: Text, content: AnyView)] = [:]
		var isConfirmingDiscard = false
		var editorRevision = 0
		private(set) var searchTarget: SettingSearchEntry?
		@ObservationIgnored private var pendingSearchNavigation: (() -> Void)?
		@ObservationIgnored private let selectionKey: String

		init(mode: MacSettingsMode) {
			selectionKey = mode.selectionKey
			policy = SettingsNavigationPolicy(
				savedSelection: UserDefaults.standard.string(forKey: mode.selectionKey).flatMap { value in
					MacSettingsCategory(rawValue: DesktopUICatalog.desktop.aliases[value] ?? value)
				},
				categories: Set(mode.categories), defaultSelection: mode.defaultCategory)
		}

		var selection: MacSettingsCategory { policy.selection }
		var isSaving: Bool { editors.values.contains { $0.state.isSaving } }
		var hasChanges: Bool { editors.values.contains { $0.state.hasChanges } }
		func discardForClosing() {
			pendingSearchNavigation = nil
			editors.removeAll()
			resetStack()
			editorRevision += 1
		}

		func requestSearch(_ entry: SettingSearchEntry?) {
			guard entry?.id != searchTarget?.id else { return }
			guardedSearchNavigation {
				if let category = entry.flatMap({ MacSettingsCategory(rawValue: $0.category) }) {
					self.policy.request(category, hasUnsavedChanges: false, isSaving: false)
				}
				self.searchTarget = entry
				self.didNavigate()
				self.editorRevision += 1
			}
		}

		private func guardedSearchNavigation(_ action: @escaping () -> Void) {
			guard !isSaving else { return }
			if hasChanges {
				pendingSearchNavigation = action
				isConfirmingDiscard = true
			}
			else {
				action()
			}
		}

		func request(_ category: MacSettingsCategory) {
			if searchTarget != nil {
				guardedSearchNavigation {
					self.searchTarget = nil
					self.policy.request(category, hasUnsavedChanges: false, isSaving: false)
					self.didNavigate()
					self.editorRevision += 1
				}
				return
			}
			if policy.request(category, hasUnsavedChanges: hasChanges, isSaving: isSaving) {
				self.didNavigate()
			}
			isConfirmingDiscard = policy.pendingSelection != nil || stack.pendingPath != nil
		}

		var path: [MacSettingsRoute] { stack.path }
		func containsPage(_ id: UUID) -> Bool { id == rootPageID || path.contains { $0.id == id } }
		func register<Content: View>(_ route: MacSettingsRoute, title: Text, content: Content) {
			pages[route] = (title, AnyView(content))
		}
		func page(_ route: MacSettingsRoute) -> some View {
			SettingsStackPage(navigation: self, pageID: route.id, title: pages[route]?.title ?? Text("Settings")) {
				if let page = self.pages[route] { page.content }
			}
			.environment(\.settingsStack, self)
			.environment(\.settingsUsesStack, true)
		}
		func requestPath(_ next: [MacSettingsRoute]) {
			let dirty = Set(path.filter { route in editors.values.contains { $0.pageID == route.id && $0.state.hasChanges } })
			if stack.request(next, dirtyPages: dirty, isSaving: isSaving) { removeDepartedEditors() }
			isConfirmingDiscard = stack.pendingPath != nil || policy.pendingSelection != nil
		}
		func requestBack() {
			if !path.isEmpty {
				requestPath(Array(path.dropLast()))
			}
			else if searchTarget != nil {
				requestSearch(nil)
			}
		}

		func discardChanges() {
			if let action = pendingSearchNavigation {
				pendingSearchNavigation = nil
				action()
			}
			else if stack.pendingPath != nil {
				if stack.discardChanges(isSaving: isSaving) { removeDepartedEditors() }
			}
			else if policy.discardChanges(isSaving: isSaving) {
				didNavigate()
			}
			isConfirmingDiscard = false
		}

		func keepEditing() {
			pendingSearchNavigation = nil
			policy.keepEditing()
			stack.keepEditing()
			isConfirmingDiscard = false
		}

		func report(_ state: SettingsEditorState?, for editor: UUID, pageID: UUID? = nil) {
			if let pageID, !containsPage(pageID) {
				editors[editor] = nil
				return
			}
			editors[editor] = state.map { Editor(state: $0, pageID: pageID) }
		}
		private func removeDepartedEditors() {
			editors = editors.filter { $0.value.pageID.map(containsPage) ?? true }
		}
		private func resetStack() {
			stack.reset()
			pages.removeAll()
			rootPageID = UUID()
		}

		private func didNavigate() {
			editors.removeAll()
			resetStack()
			UserDefaults.standard.set(selection.rawValue, forKey: selectionKey)
		}
	}

	extension EnvironmentValues {
		@Entry var settingsNavigation: MacSettingsNavigation? = nil
		@Entry var settingsStack: MacSettingsNavigation? = nil
		@Entry var settingsPageID: UUID? = nil
		@Entry var settingsUsesStack = false
	}

	private struct SettingsEditorModifier: ViewModifier {
		let state: SettingsEditorState
		@Environment(\.settingsNavigation) private var navigation
		@Environment(\.settingsPageID) private var pageID
		@State private var editorID = UUID()

		func body(content: Content) -> some View {
			content
				.onChange(of: state, initial: true) { _, state in navigation?.report(state, for: editorID, pageID: pageID) }
				.onDisappear {
					if pageID.map({ navigation?.containsPage($0) != true }) ?? true {
						navigation?.report(nil, for: editorID)
					}
				}
		}
	}

	extension View {
		/// In a settings detail, protects this editor from a sidebar transition. Elsewhere it has no effect.
		func settingsEditor(hasChanges: Bool, isSaving: Bool = false) -> some View {
			modifier(SettingsEditorModifier(state: SettingsEditorState(hasChanges: hasChanges, isSaving: isSaving)))
		}
	}
#endif
