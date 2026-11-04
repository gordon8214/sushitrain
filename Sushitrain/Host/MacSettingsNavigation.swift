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
		case general, filesAndPreviews, network, bandwidth, engine, defaults
		case remoteFileService, engineAccess, configuration, troubleshooting, advancedConfiguration
		case gateways, finder, pendingEdits

		var id: String { rawValue }

		var title: LocalizedStringResource {
			switch self {
			case .general: "General"
			case .filesAndPreviews: "Files & Previews"
			case .network: "Network"
			case .bandwidth: "Bandwidth"
			case .engine: "Engine"
			case .defaults: "Defaults"
			case .remoteFileService: "Remote File Service"
			case .engineAccess: "Engine Access"
			case .configuration: "Configuration"
			case .troubleshooting: "Troubleshooting"
			case .advancedConfiguration: "Advanced Configuration"
			case .gateways: "Gateways"
			case .finder: "Finder"
			case .pendingEdits: "Pending Edits"
			}
		}

		var symbol: String {
			switch self {
			case .general: "gearshape"
			case .filesAndPreviews: "doc.richtext"
			case .network: "network"
			case .bandwidth: "speedometer"
			case .engine: "cpu"
			case .defaults: "slider.horizontal.3"
			case .remoteFileService: "externaldrive.connected.to.line.below"
			case .engineAccess: "server.rack"
			case .configuration: "folder.badge.gearshape"
			case .troubleshooting: "stethoscope"
			case .advancedConfiguration: "gearshape.2"
			case .gateways: "externaldrive.badge.wifi"
			case .finder: "folder"
			case .pendingEdits: "arrow.up.doc"
			}
		}
	}

	enum MacSettingsMode: String {
		case synchronize, filesInFinder

		var categories: [MacSettingsCategory] {
			switch self {
			case .synchronize:
				[
					.general, .filesAndPreviews, .network, .bandwidth, .engine, .defaults,
					.remoteFileService, .engineAccess, .configuration, .troubleshooting, .advancedConfiguration,
				]
			case .filesInFinder:
				[.general, .gateways, .finder, .pendingEdits]
			}
		}

		var defaultCategory: MacSettingsCategory { self == .synchronize ? .general : .gateways }
		var selectionKey: String { "macSettingsSelectedCategory.\(rawValue)" }
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
		@ObservationIgnored private let selectionKey: String

		init(mode: MacSettingsMode) {
			selectionKey = mode.selectionKey
			policy = SettingsNavigationPolicy(
				savedSelection: UserDefaults.standard.string(forKey: mode.selectionKey).flatMap(MacSettingsCategory.init),
				categories: Set(mode.categories), defaultSelection: mode.defaultCategory)
		}

		var selection: MacSettingsCategory { policy.selection }
		var isSaving: Bool { editors.values.contains(where: \.isSaving) }

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
