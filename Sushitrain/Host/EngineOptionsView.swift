// Copyright (C) 2026 Gordon
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this file,
// You can obtain one at https://mozilla.org/MPL/2.0/.
#if os(macOS)
	import SushitrainCore
	import SwiftUI

	/// The engine's general and advanced options that have no setting of their own. They are saved together, and the engine
	/// checks them then: numbers stay text until that, as a number field only commits its value when it loses focus, which
	/// clicking Save doesn't do.
	struct EngineOptionsView: View {
		private typealias Draft = SettingsDraft<SushitrainEngineOptions>

		@Environment(AppState.self) private var appState
		@State private var draft: Draft? = nil
		@State private var saved: SushitrainEngineOptions? = nil
		@State private var auditStatus = ""
		@State private var isSaving = false
		@State private var errorMessage: ErrorMessage? = nil

		var body: some View {
			Group {
				if let draft = Binding($draft) {
					Form {
						AdvancedRestartSection()
						self.diskSection(draft)
						self.performanceSection(draft)
						self.indexSection(draft)
						self.auditSection(draft)
						self.featureFlagsSection(draft)
						self.reportingSection(draft)
					}
					.autocorrectionDisabled()
					// The engine reads the options while it saves them
					.disabled(isSaving)
				}
				else {
					ProgressView()
				}
			}
			.formStyle(.grouped)
			.navigationTitle("Engine options")
			.toolbar {
				// Says what it does, where the system would call a button of this role Done
				ToolbarItem(placement: .confirmationAction) {
					Button("Save") {
						self.save()
					}
					.disabled(!self.canSave)
				}
			}
			// Again after the engine restarted, and when something else saved the configuration
			.task(id: appState.isEngineRestarting) {
				self.refresh()
			}
			.onChange(of: appState.eventCounter) {
				self.refresh()
			}
			.errorAlert($errorMessage)
		}

		private func diskSection(_ draft: Binding<Draft>) -> some View {
			Section {
				LabeledContent("Minimum free disk space") {
					HStack {
						TextField("Minimum free disk space", text: draft.minHomeDiskFree)
							.labelsHidden()
						Picker("Unit", selection: draft.minHomeDiskFreeUnit) {
							ForEach(draft.wrappedValue.settings.minHomeDiskFreeUnits()?.asArray() ?? [], id: \.self) { unit in
								Text(verbatim: unit).tag(unit)
							}
						}
						.labelsHidden()
						.fixedSize()
					}
				}
				TextField("Keep temporary files (hours)", text: draft.keepTemporariesH)
			} header: {
				Text("Disk space")
			} footer: {
				Text(
					"While the disk with the app's database has less free space than this, folders stop with an error; 0 turns this check off. Temporary files of interrupted transfers are kept for this many hours, so that their data needn't be transferred again; with 0 they are removed at the next scan."
				)
			}
		}

		private func performanceSection(_ draft: Binding<Draft>) -> some View {
			Section {
				EngineLimitField(
					title: "Folders working at once", automaticTitle: "One per CPU core", valueTitle: "Number of folders",
					customValue: "4", text: draft.maxFolderConcurrency)
				EngineLimitField(
					title: "Data for incoming requests", automaticTitle: "256 MiB", valueTitle: "Limit (KiB)",
					customValue: "524288", text: draft.maxIncomingRequestKiB)
				Toggle("Run at low priority", isOn: draft.lowPriority)
			} header: {
				Text("Performance")
			} footer: {
				Text(
					"How many folders can scan or synchronize at the same time, and how much data this device handles at once for requests from other devices. Low priority sets the Unix nice value of the whole app, as the engine runs inside it, which makes little difference on macOS. Turning it on takes effect when the engine restarts, and turning it off when the app restarts."
				)
			}
		}

		private func indexSection(_ draft: Binding<Draft>) -> some View {
			Section {
				TextField("Minimum blocks for a temporary index", text: draft.tempIndexMinBlocks)
				Toggle("Send the full index after an upgrade", isOn: draft.sendFullIndexOnUpgrade)
			} header: {
				Text("Indexes")
			} footer: {
				Text(
					"While downloading a file of more than this many blocks, this device tells others which blocks it already has, so that they can fetch those from it. After an upgrade of the engine, the full index can be sent to other devices again. That takes time, but makes up for any differences between versions."
				)
			}
		}

		private func auditSection(_ draft: Binding<Draft>) -> some View {
			Section {
				Toggle("Keep an audit log", isOn: draft.auditEnabled)
				TextField("Audit log file", text: draft.auditFile, prompt: Text("A new file for each start"))
			} header: {
				Text("Audit log")
			} footer: {
				VStack(alignment: .leading) {
					Text(
						"The audit log records every event of the engine, as lines of JSON. That includes file names, device addresses, and the configuration with its API key and encryption passwords, so keep it private. It can't be in a synchronized folder. Without a file, each start of the engine writes a new one in the configuration folder, and each is removed after a week. Changes take effect when the engine restarts."
					)
					if !auditStatus.isEmpty {
						Text(auditStatus)
					}
				}
			}
		}

		private func featureFlagsSection(_ draft: Binding<Draft>) -> some View {
			Section {
				TextEditor(text: draft.featureFlags)
					.font(.body.monospaced())
					.frame(minHeight: 66)
					.accessibilityLabel(Text("Feature flags"))
			} header: {
				Text("Feature flags")
			} footer: {
				Text(
					"Experimental features of the engine to turn on, one name per line. Leave this empty unless the engine's release notes name one."
				)
			}
		}

		private func reportingSection(_ draft: Binding<Draft>) -> some View {
			Section {
				Toggle("Send anonymous usage reports", isOn: draft.usageReporting)
				Toggle("Send crash reports", isOn: draft.crashReporting)
			} header: {
				Text("Reporting")
			} footer: {
				Text(
					"Both are off unless you turn them on, and go to the Syncthing project. A usage report goes out when you turn it on, about 30 minutes after each start of the engine, and then once a day, under a random ID: anonymous statistics such as the platform, the number of folders and devices, the amount of data, and the features in use. While it is on, failures inside the engine are also sent as they happen, to the crash report address. A crash report goes out at the first start after a crash, with where in the code the engine stopped; its error message can include a file path. The addresses are UR URL and CR URL under Options in the advanced configuration."
				)
			}
		}

		private var canSave: Bool {
			guard let draft, let saved, !isSaving, !appState.isEngineRestarting else {
				return false
			}
			return !draft.settings.equal(saved)
		}

		private func save() {
			guard let options = draft?.settings else {
				return
			}
			let client = appState.client
			isSaving = true
			Task {
				do {
					try await goBlocking { try client.setEngineOptions(options) }
					self.load()
				}
				catch {
					errorMessage = ErrorMessage(error)
				}
				isSaving = false
			}
		}

		/// Follows the configuration, unless that would throw away what is being edited
		private func refresh() {
			auditStatus = appState.client.auditStatusMessage()
			if let draft, let saved {
				guard draft.settings.equal(saved), appState.client.engineOptions()?.equal(saved) == false else {
					return
				}
			}
			self.load()
		}

		private func load() {
			let client = appState.client
			saved = client.engineOptions()
			draft = client.engineOptions().map(Draft.init)
		}
	}

	/// A limit that the engine takes as a number, with 0 for its default and below 0 for no limit
	private struct EngineLimitField: View {
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
