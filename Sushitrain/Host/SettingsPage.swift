// Copyright (C) 2026 Gordon
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this file,
// You can obtain one at https://mozilla.org/MPL/2.0/.
#if os(macOS)
	import SwiftUI

	extension EnvironmentValues {
		@Entry var settingsSaveFailure = 0
	}

	/// A page of engine settings that are saved together, as EngineOptionsView is. The engine checks them on saving: numbers
	/// stay text until then, as a number field only commits its value when it loses focus, which clicking Save doesn't do.
	struct SettingsPage<Settings: AnyObject & Sendable, Content: View>: View {
		typealias Draft = SettingsDraft<Settings>

		let title: LocalizedStringKey
		/// For a page in a sheet: saving closes it, next to a button that closes it without saving
		var isSheet = false
		let load: () -> Settings?
		/// A separate pending preview; load remains the saved baseline for dirty checking.
		var loadPreview: (() -> Settings?)? = nil
		let isEqual: (Settings, Settings) -> Bool
		let save: @Sendable (Settings) throws -> Void
		/// What to ask before saving these settings, when there is something to ask
		var confirmation: (Settings) -> String? = { _ in nil }
		@ViewBuilder let content: (Binding<Draft>) -> Content

		@Environment(AppState.self) private var appState
		@Environment(\.dismiss) private var dismiss
		@State private var draft: Draft? = nil
		@State private var saved: Settings? = nil
		@State private var isSaving = false
		@State private var errorMessage: ErrorMessage? = nil
		@State private var question: String? = nil
		@State private var saveFailure = 0

		var body: some View {
			Group {
				if let draft = Binding($draft) {
					Form {
						self.content(draft)
							.environment(\.settingsSaveFailure, saveFailure)
					}
					.autocorrectionDisabled()
					// The engine reads the settings while it saves them
					.disabled(isSaving)
				}
				else {
					ProgressView()
				}
			}
			.formStyle(.grouped)
			.navigationTitle(title)
			.settingsEditor(hasChanges: self.hasChanges, isSaving: isSaving)
			.toolbar {
				if isSheet {
					// A save can take as long as the folder takes to restart, and its error has to have a view to show on
					SheetButton(role: .cancel, isDisabled: isSaving) {
						dismiss()
					}
				}
				// Says what it does, where the system would call a button of this role Done
				ToolbarItem(placement: .confirmationAction) {
					Button("Save") {
						self.requestSave()
					}
					.disabled(!self.canSave)
				}
			}
			.frame(minWidth: isSheet ? 520 : nil, minHeight: isSheet ? 460 : nil)
			// Again after the engine restarted, and when something else saved the configuration
			.task(id: appState.isEngineRestarting) {
				self.refresh()
			}
			.onChange(of: appState.eventCounter) {
				self.refresh()
			}
			.confirmationDialog(
				question ?? "", isPresented: Binding(get: { question != nil }, set: { if !$0 { question = nil } }),
				titleVisibility: .visible
			) {
				Button("Save", role: .destructive) {
					self.performSave()
				}
			}
			.errorAlert($errorMessage)
		}

		private var hasChanges: Bool {
			guard let draft, let saved else { return false }
			return !isEqual(draft.settings, saved)
		}

		private var canSave: Bool {
			guard !isSaving, !appState.isEngineRestarting else {
				return false
			}
			return self.hasChanges
		}

		private func requestSave() {
			guard let settings = draft?.settings else {
				return
			}
			if let confirmation = self.confirmation(settings) {
				question = confirmation
			}
			else {
				self.performSave()
			}
		}

		private func performSave() {
			guard let settings = draft?.settings else {
				return
			}
			let save = self.save
			isSaving = true
			Task {
				do {
					try await goBlocking { try save(settings) }
					if isSheet {
						dismiss()
					}
					else {
						self.reload()
					}
				}
				catch {
					saveFailure += 1
					errorMessage = ErrorMessage(error)
				}
				isSaving = false
			}
		}

		/// Follows the configuration, unless that would throw away what is being edited
		private func refresh() {
			if let draft, let saved {
				guard isEqual(draft.settings, saved), let current = load(), !isEqual(current, saved) else {
					return
				}
			}
			self.reload()
		}

		private func reload() {
			saved = load()
			if let loadPreview {
				draft = loadPreview().map(Draft.init)
			}
			else {
				draft = load().map(Draft.init)
			}
		}
	}

	/// A number with its unit
	struct SettingsNumberField: View {
		let title: LocalizedStringKey
		let unit: LocalizedStringKey
		@Binding var text: String

		var body: some View {
			LabeledContent(title) {
				HStack {
					TextField(title, text: $text)
						.labelsHidden()
						.multilineTextAlignment(.trailing)
					Text(unit)
						.foregroundStyle(.secondary)
				}
			}
		}
	}

	/// A minimum of free disk space: a number, and a unit of the ones the engine offers
	struct SettingsDiskSpaceField: View {
		let title: LocalizedStringKey
		@Binding var value: String
		@Binding var unit: String
		let units: [String]

		var body: some View {
			LabeledContent(title) {
				HStack {
					TextField(title, text: $value)
						.labelsHidden()
					Picker("Unit", selection: $unit) {
						ForEach(units, id: \.self) { unit in
							Text(verbatim: unit).tag(unit)
						}
					}
					.labelsHidden()
					.fixedSize()
				}
			}
		}
	}

	/// A list of values, one per line
	struct SettingsLinesEditor: View {
		let title: LocalizedStringKey
		@Binding var text: String

		var body: some View {
			TextEditor(text: $text)
				.font(.body.monospaced())
				.frame(minHeight: 66)
				.accessibilityLabel(Text(title))
		}
	}

	/// What a setting risks, right below it, for as long as it is set that way
	struct SettingsWarning: View {
		let text: LocalizedStringKey

		var body: some View {
			Label(text, systemImage: "exclamationmark.triangle.fill")
				.foregroundStyle(.orange)
		}
	}

	/// A limit that the engine takes as a number, with 0 for its default and below 0 for no limit
	struct EngineLimitField: View {
		private enum Mode {
			case automatic, unlimited, custom
		}

		let title: LocalizedStringKey
		let automaticTitle: LocalizedStringKey
		let valueTitle: LocalizedStringKey
		/// The value to start from when choosing one
		let customValue: String
		@Binding var text: String
		/// Keeps the field while typing, which can pass through a number that means another mode
		@FocusState private var isEditing: Bool

		var body: some View {
			Picker(title, selection: Binding(get: { self.mode }, set: { self.choose($0) })) {
				Text(automaticTitle).tag(Mode.automatic)
				Text("No limit").tag(Mode.unlimited)
				Text("Custom").tag(Mode.custom)
			}
			if self.mode == .custom || isEditing {
				TextField(valueTitle, text: $text)
					.focused($isEditing)
			}
		}

		private var mode: Mode {
			guard let limit = Int(text.trimmingCharacters(in: .whitespaces)), limit <= 0 else {
				return .custom
			}
			return limit == 0 ? .automatic : .unlimited
		}

		private func choose(_ mode: Mode) {
			// The menu also calls this for the choice that is already made
			guard mode != self.mode else {
				return
			}
			isEditing = false
			switch mode {
			case .automatic: text = "0"
			case .unlimited: text = "-1"
			case .custom: text = customValue
			}
		}
	}
#endif
