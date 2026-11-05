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

	/// Each window owns its navigation and editors. Only accepted selections are remembered between openings.
	@Observable @MainActor final class MacSettingsNavigation {
		private var policy: SettingsNavigationPolicy<MacSettingsCategory>
		private var editors: [UUID: SettingsEditorState] = [:]
		var isConfirmingDiscard = false
		var editorRevision = 0
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
		var isSaving: Bool { editors.values.contains(where: \.isSaving) }
		var hasChanges: Bool { editors.values.contains(where: \.hasChanges) }
		func discardForClosing() {
			editors.removeAll()
			editorRevision += 1
		}

		func request(_ category: MacSettingsCategory) {
			if policy.request(category, hasUnsavedChanges: editors.values.contains(where: \.hasChanges), isSaving: isSaving) {
				self.didNavigate()
			}
			isConfirmingDiscard = policy.pendingSelection != nil
		}

		func discardChanges() {
			if policy.discardChanges(isSaving: isSaving) { self.didNavigate() }
		}

		func keepEditing() { policy.keepEditing() }

		func report(_ state: SettingsEditorState?, for editor: UUID) {
			editors[editor] = state
		}

		private func didNavigate() {
			editors.removeAll()
			UserDefaults.standard.set(selection.rawValue, forKey: selectionKey)
		}
	}

	extension EnvironmentValues {
		@Entry var settingsNavigation: MacSettingsNavigation? = nil
	}

	private struct SettingsEditorModifier: ViewModifier {
		let state: SettingsEditorState
		@Environment(\.settingsNavigation) private var navigation
		@State private var editorID = UUID()

		func body(content: Content) -> some View {
			content
				.onChange(of: state, initial: true) { _, state in navigation?.report(state, for: editorID) }
				.onDisappear { navigation?.report(nil, for: editorID) }
		}
	}

	extension View {
		/// In a settings detail, protects this editor from a sidebar transition. Elsewhere it has no effect.
		func settingsEditor(hasChanges: Bool, isSaving: Bool = false) -> some View {
			modifier(SettingsEditorModifier(state: SettingsEditorState(hasChanges: hasChanges, isSaving: isSaving)))
		}
	}
#endif
