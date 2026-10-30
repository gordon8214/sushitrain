// Copyright (C) 2026 Gordon
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this file,
// You can obtain one at https://mozilla.org/MPL/2.0/.
import SushitrainCore
import SwiftUI

/// Every setting of the engine, as the web GUI's Advanced dialog has them, for what has no screen of its own. The engine
/// says which fields there are, so that new ones show up here without a change to the app.
struct AdvancedConfigurationView: View {
	@Environment(AppState.self) private var appState
	@State private var folders: [SushitrainFolder] = []
	@State private var peers: [SushitrainPeer] = []

	var body: some View {
		Form {
			Section {
				Label(
					"Be careful! These are expert settings. Incorrect values may damage your folder contents and stop the app from synchronizing.",
					systemImage: "exclamationmark.triangle.fill"
				)
				.foregroundStyle(.red)
			}

			AdvancedRestartSection()

			Section {
				self.link(SushitrainAdvancedSectionOptions, title: Text("Options"))
				#if os(macOS)
					self.link(SushitrainAdvancedSectionGUI, title: Text("Web GUI"))
					self.link(SushitrainAdvancedSectionLDAP, title: Text(verbatim: "LDAP"))
				#endif
			}

			Section("Folders") {
				ForEach(folders) { folder in
					self.link(SushitrainAdvancedSectionFolderPrefix + folder.folderID, title: Text(folder.displayName))
				}
			}

			Section("Devices") {
				ForEach(peers) { peer in
					self.link(SushitrainAdvancedSectionDevicePrefix + peer.deviceID(), title: Text(peer.displayName))
				}
			}

			Section("Defaults") {
				self.link(SushitrainAdvancedSectionDefaultFolder, title: Text("Default folder"))
				self.link(SushitrainAdvancedSectionDefaultDevice, title: Text("Default device"))
				self.link(SushitrainAdvancedSectionDefaultIgnores, title: Text("Default ignore patterns"))
			}
		}
		.navigationTitle("Advanced configuration")
		#if os(macOS)
			.formStyle(.grouped)
		#endif
		#if os(iOS)
			.navigationBarTitleDisplayMode(.inline)
		#endif
		// Folders and devices also come and go through the web GUI and other devices
		.task(id: appState.eventCounter) {
			self.folders = await appState.folders().sorted()
			self.peers = await appState.peers().sorted()
		}
	}

	private func link(_ section: String, title: Text) -> some View {
		NavigationLink {
			AdvancedSectionView(section: section, title: title)
		} label: {
			title
		}
	}
}

/// Says when saved settings wait for the engine to restart, and offers to do that
struct AdvancedRestartSection: View {
	@Environment(AppState.self) private var appState

	var body: some View {
		// Reads the engine's state for each change it reports, as it says nothing of this by itself
		let _ = appState.eventCounter
		if appState.client.isEngineRestartRequired() {
			Section {
				Button("Restart the engine") {
					appState.restartEngine()
				}
				.disabled(appState.isEngineRestarting)
			} footer: {
				Text("Some saved settings take effect when the engine restarts.")
			}
		}
	}
}

/// One section of the configuration, whose fields are saved together: each change can make the engine restart a folder
/// or a service, and the web GUI's fields depend on each other.
private struct AdvancedSectionView: View {
	let section: String
	let title: Text

	@Environment(AppState.self) private var appState
	/// The fields as they were read, which save the changes in `values`
	@State private var loaded: SushitrainAdvancedSection? = nil
	@State private var fields: [SushitrainAdvancedField] = []
	/// The text of each field, by its path
	@State private var values: [String: String] = [:]
	@State private var loadError: String? = nil
	@State private var isSaving = false
	@State private var errorMessage: ErrorMessage? = nil

	var body: some View {
		self.content
			#if os(iOS)
				.navigationBarTitleDisplayMode(.inline)
			#endif
			.navigationTitle(title)
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

	@ViewBuilder private var content: some View {
		if loaded != nil {
			self.form
		}
		else if let loadError {
			ContentUnavailableView(loadError, systemImage: "exclamationmark.triangle")
		}
		else {
			ProgressView()
		}
	}

	private var form: some View {
		Form {
			AdvancedRestartSection()
			#if os(macOS)
				self.guiWarning
			#endif
			ForEach(self.groups, id: \.self) { group in
				self.fieldSection(group)
			}
		}
		.autocorrectionDisabled()
		.disabled(isSaving)
		#if os(macOS)
			.formStyle(.grouped)
		#endif
	}

	#if os(macOS)
		/// The GUI's settings with what is being edited, for the warnings that go with them
		private var editedGUISettings: SushitrainGUISettings? {
			guard section == SushitrainAdvancedSectionGUI, let settings = appState.client.guiSettings() else {
				return nil
			}
			settings.address = values["address"] ?? settings.address
			settings.authMode = values["authMode"] ?? settings.authMode
			settings.user = values["user"] ?? settings.user
			settings.password = values["password"] ?? ""
			settings.insecureSkipHostCheck = values["insecureSkipHostcheck"] == "true"
			return settings
		}

		/// The warnings of the GUI's settings page, which this section can get the GUI past just the same
		@ViewBuilder private var guiWarning: some View {
			if let settings = self.editedGUISettings {
				if settings.isExposed() {
					Section {
						Label(
							"Others can reach the web GUI and REST API without signing in, and control this device's synchronization.",
							systemImage: "exclamationmark.triangle.fill"
						)
						.foregroundStyle(.red)
					}
				}
				else if settings.isUnprotected() {
					Section {
						Label(
							"Without signing in, any user or program on this Mac can use the web GUI and REST API, and control this device's synchronization.",
							systemImage: "exclamationmark.triangle.fill"
						)
						.foregroundStyle(.orange)
					}
				}
			}
		}
	#endif

	private func fieldSection(_ group: String) -> some View {
		Section {
			ForEach(fields.filter { $0.group == group }, id: \.path) { field in
				AdvancedFieldView(field: field, value: self.value(of: field))
			}
		} header: {
			if !group.isEmpty {
				Text(verbatim: group)
			}
		}
	}

	private func value(of field: SushitrainAdvancedField) -> Binding<String> {
		Binding(
			get: { values[field.path] ?? "" },
			set: { values[field.path] = $0 })
	}

	/// The groups of fields, in the order the engine lists them
	private var groups: [String] {
		var seen = Set<String>()
		return fields.map(\.group).filter { seen.insert($0).inserted }
	}

	private var hasChanges: Bool {
		fields.contains { values[$0.path] != $0.value }
	}

	private var canSave: Bool {
		loaded != nil && !isSaving && !appState.isEngineRestarting && self.hasChanges
	}

	private func save() {
		guard let loaded else {
			return
		}
		let edited = values
		for field in fields {
			field.value = edited[field.path] ?? field.value
		}
		let client = appState.client
		isSaving = true
		Task {
			do {
				try await goBlocking { try client.save(loaded) }
				self.load()
			}
			catch {
				errorMessage = ErrorMessage(error)
				// The fields start over from the configuration, as they were changed above, and the edits stay
				self.load()
				values = edited
			}
			isSaving = false
		}
	}

	/// Follows the configuration, unless that would throw away what is being edited
	private func refresh() {
		guard !isSaving, !self.hasChanges else {
			return
		}
		if let loaded, let current = try? appState.client.advancedSection(section), current.equal(loaded) {
			return
		}
		self.load()
	}

	private func load() {
		do {
			let read = try appState.client.advancedSection(section)
			let fields = (0..<read.fieldCount()).compactMap { read.field(at: $0) }
			self.loaded = read
			self.fields = fields
			self.values = Dictionary(fields.map { ($0.path, $0.value) }, uniquingKeysWith: { first, _ in first })
			self.loadError = nil
		}
		catch {
			self.loaded = nil
			self.fields = []
			self.loadError = error.localizedDescription
		}
	}
}

/// The control for a field, for its kind of value
private struct AdvancedFieldView: View {
	let field: SushitrainAdvancedField
	@Binding var value: String

	var body: some View {
		Group {
			switch field.kind {
			case SushitrainAdvancedKindBool:
				Toggle(
					isOn: Binding(
						get: { value == "true" },
						set: { value = $0 ? "true" : "false" })
				) {
					Text(verbatim: field.label)
				}
				.disabled(field.isReadOnly)

			case SushitrainAdvancedKindEnum:
				Picker(selection: $value) {
					ForEach(field.options()?.asArray() ?? [], id: \.self) { option in
						Text(verbatim: option).tag(option)
					}
				} label: {
					Text(verbatim: field.label)
				}
				.disabled(field.isReadOnly)

			case SushitrainAdvancedKindList, SushitrainAdvancedKindJSON:
				VStack(alignment: .leading) {
					Text(verbatim: field.label)
					TextEditor(text: $value)
						.font(.body.monospaced())
						.frame(minHeight: 88)
						.accessibilityLabel(Text(verbatim: field.label))
						.disabled(field.isReadOnly)
						#if os(iOS)
							.textInputAutocapitalization(.never)
						#endif
				}

			default:
				LabeledContent {
					if field.isReadOnly {
						Text(verbatim: value)
							.textSelection(.enabled)
					}
					else {
						self.textField
							.labelsHidden()
							.multilineTextAlignment(.trailing)
							#if os(iOS)
								.textInputAutocapitalization(.never)
								.keyboardType(self.keyboardType)
							#endif
					}
				} label: {
					Text(verbatim: field.label)
				}
			}
		}
	}

	@ViewBuilder private var textField: some View {
		if field.isSecret {
			SecureField(text: $value, prompt: Text("Unchanged")) {
				Text(verbatim: field.label)
			}
		}
		else {
			TextField(text: $value) {
				Text(verbatim: field.label)
			}
		}
	}

	#if os(iOS)
		// Numbers can be negative, which the number pads have no key for
		private var keyboardType: UIKeyboardType {
			switch field.kind {
			case SushitrainAdvancedKindInt, SushitrainAdvancedKindFloat: .numbersAndPunctuation
			default: .default
			}
		}
	#endif
}
