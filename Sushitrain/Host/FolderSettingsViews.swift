// Copyright (C) 2026 Gordon
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this file,
// You can obtain one at https://mozilla.org/MPL/2.0/.
#if os(macOS)
	import SushitrainCore
	import SwiftUI

	/// Expansion belongs to the editor, while every value stays in its shared draft.
	struct FolderAdvancedSection<Content: View>: View {
		@Environment(\.settingsSaveFailure) private var saveFailure
		@Environment(\.settingsSearchAnchor) private var searchAnchor
		@State private var isExpanded = false
		@ViewBuilder let content: () -> Content

		var body: some View {
			Section {
				DisclosureGroup("Advanced", isExpanded: $isExpanded) {
					self.content()
				}
			}
			.onChange(of: searchAnchor, initial: true) { _, anchor in if anchor != nil { isExpanded = true } }
			.onChange(of: saveFailure) {
				isExpanded = true
			}
		}
	}

	/// How a folder synchronizes. The same sections serve new-folder defaults.
	struct FolderSyncSections: View {
		@Binding var draft: SettingsDraft<SushitrainFolderSettings>
		var isDefaults = false

		var body: some View {
			FolderDiskSection(draft: $draft)
			FolderAdvancedSection {
				if isDefaults {
					FolderDefaultTuningSection(draft: $draft)
				}
				FolderFilesSection(draft: $draft)
				FolderFileSystemSection(draft: $draft, isDefaults: isDefaults)
				FolderScanSection(draft: $draft)
				FolderTransferSection(draft: $draft)
			}
		}
	}

	private struct FolderDefaultTuningSection: View {
		@Binding var draft: SettingsDraft<SushitrainFolderSettings>

		var body: some View {
			Section {
				SettingsNumberField(title: "Watcher delay", unit: "seconds", text: $draft.fsWatcherDelayS).settingsAnchor(
					"fsWatcherDelayS")
				TextField("Maximum conflict copies", text: $draft.maxConflicts).settingsAnchor("maxConflicts")
				Toggle("Block indexing", isOn: $draft.blockIndexing).settingsAnchor("blockIndexing")
			} footer: {
				Text("For conflict copies, -1 keeps every copy and 0 keeps none.")
			}
		}
	}

	private struct FolderFilesSection: View {
		@Binding var draft: SettingsDraft<SushitrainFolderSettings>

		var body: some View {
			Section {
				Picker("File pull order", selection: $draft.order) {
					Text("Random").tag("random")
					Text("Alphabetic").tag("alphabetic")
					Text("Smallest first").tag("smallestFirst")
					Text("Largest first").tag("largestFirst")
					Text("Oldest first").tag("oldestFirst")
					Text("Newest first").tag("newestFirst")
				}.settingsAnchor("order")
				Toggle("Ignore deletions from other devices", isOn: $draft.ignoreDelete).settingsAnchor("ignoreDelete")
				if draft.settings.ignoreDelete {
					SettingsWarning(text: "With this on, this folder never stays in step with other devices.")
				}
			} header: {
				Text("Files")
			} footer: {
				Text(
					"The order in which files that are needed are fetched. Ignoring deletions keeps files here that other devices have deleted. This is for special cases only: this device stays out of sync, and it shows as such on the others."
				)
			}
		}

	}

	private struct FolderDiskSection: View {
		@Binding var draft: SettingsDraft<SushitrainFolderSettings>

		var body: some View {
			Section {
				SettingsDiskSpaceField(
					title: "Minimum free disk space", value: $draft.minDiskFree, unit: $draft.minDiskFreeUnit,
					units: draft.settings.minDiskFreeUnits()?.asArray() ?? [])
			} header: {
				Text("Disk space")
			} footer: {
				Text(
					"While the disk with this folder has less free space than this, it doesn't fetch files anymore. 0 turns this check off. Percentages are of the size of the disk."
				)
			}
		}

	}

	private struct FolderFileSystemSection: View {
		@Binding var draft: SettingsDraft<SushitrainFolderSettings>
		var isDefaults = false

		var body: some View {
			Section {
				Toggle("Case-sensitive file names", isOn: $draft.caseSensitiveFS).settingsAnchor("caseSensitiveFS")
				if draft.settings.caseSensitiveFS {
					SettingsWarning(
						text: "On a disk that isn't case-sensitive, files whose names differ only by case can overwrite each other.")
				}
				Toggle("Normalize file names", isOn: $draft.autoNormalize).settingsAnchor("autoNormalize")
				if !draft.settings.autoNormalize {
					SettingsWarning(text: "Files with wrongly encoded names will show as failed instead of being corrected.")
				}
				Toggle("No sparse files", isOn: $draft.disableSparseFiles).settingsAnchor("disableSparseFiles")
				Toggle("Don't wait for writes to reach the disk", isOn: $draft.disableFsync).settingsAnchor("disableFsync")
				if draft.settings.disableFsync {
					SettingsWarning(text: "A crash or power failure can damage files that were being written.")
				}
				SettingsNumberField(title: "Modification time window", unit: "seconds", text: $draft.modTimeWindowS).settingsAnchor(
					"modTimeWindowS")
				if !isDefaults {
					TextField("Folder marker", text: $draft.markerName, prompt: Text(verbatim: ".stfolder")).settingsAnchor(
						"markerName")
				}
			} header: {
				Text("File system")
			} footer: {
				VStack(alignment: .leading) {
					Text(
						"Case-sensitive names turn off the check for files that differ only by case; macOS disks normally aren't case-sensitive. Normalizing corrects the Unicode encoding of file names. Sparse files save space by not writing empty blocks. Modification times that are this close count as equal, for disks that round them; 0 lets the engine go by the disk."
					)
					if !isDefaults {
						Text(
							"The marker is what tells the engine that the folder is there: a file or directory by that name has to exist in the folder, or it stops."
						)
					}
				}
			}
		}

	}

	private struct FolderScanSection: View {
		@Binding var draft: SettingsDraft<SushitrainFolderSettings>

		var body: some View {
			Section {
				SettingsNumberField(title: "Watcher timeout", unit: "seconds", text: $draft.fsWatcherTimeoutS).settingsAnchor(
					"fsWatcherTimeoutS")
				SettingsNumberField(title: "Scan progress interval", unit: "seconds", text: $draft.scanProgressIntervalS)
					.settingsAnchor("scanProgressIntervalS")
			} header: {
				Text("Scanning")
			} footer: {
				Text(
					"How long changes that keep coming can hold up a scan, where 0 lets the engine go by the watcher's delay. How often a scan reports its progress, where 0 is every 2 seconds and a negative number turns this off."
				)
			}
		}

	}

	private struct FolderTransferSection: View {
		@Binding var draft: SettingsDraft<SushitrainFolderSettings>

		var body: some View {
			Section {
				SettingsNumberField(title: "Copiers", unit: "per file", text: $draft.copiers).settingsAnchor("copiers")
				SettingsNumberField(title: "Hashers", unit: "at once", text: $draft.hashers).settingsAnchor("hashers")
				SettingsNumberField(title: "Files written at once", unit: "files", text: $draft.maxConcurrentWrites).settingsAnchor(
					"maxConcurrentWrites")
				SettingsNumberField(title: "Pending data", unit: "KiB", text: $draft.pullerMaxPendingKiB).settingsAnchor(
					"pullerMaxPendingKiB")
				SettingsNumberField(title: "Delay before pulling", unit: "seconds", text: $draft.pullerDelayS).settingsAnchor(
					"pullerDelayS")
				SettingsNumberField(title: "Pause after a failure", unit: "seconds", text: $draft.pullerPauseS).settingsAnchor(
					"pullerPauseS")
				Picker("Block pull order", selection: $draft.blockPullOrder) {
					Text("Standard").tag("standard")
					Text("Random").tag("random")
					Text("In order").tag("inOrder")
				}.settingsAnchor("blockPullOrder")
				Picker("Copy method", selection: $draft.blockCopyMethod) {
					ForEach(draft.settings.blockCopyMethods()?.asArray() ?? [], id: \.self) { method in
						Text(Self.copyMethodTitle(method)).tag(method)
					}
				}.settingsAnchor("blockCopyMethod")
			} header: {
				Text("Transfers")
			} footer: {
				Text(
					"For experts: leave these as they are unless you know why not, as 0 lets the engine decide for most of them. Pending data is how much is requested from other devices at once for a file. The block pull order decides which device serves which part of a file. The copy method is how blocks that exist elsewhere in the folder are reused."
				)
			}
		}

		private static func copyMethodTitle(_ method: String) -> String {
			switch method {
			case "standard": return String(localized: "Standard")
			case "all": return String(localized: "Fastest available")
			default: return method
			}
		}
	}
	/// Metadata controls shared with the defaults for new folders.
	struct FolderMetadataSections: View {
		@Binding var draft: SettingsDraft<SushitrainFolderSettings>

		var body: some View {
			FolderXattrSection(draft: $draft)
			FolderOwnershipSection(draft: $draft)
			FolderPermissionsSection(draft: $draft)
			FolderAdvancedSection {
				FolderXattrFilterSection(draft: $draft)
			}
		}
	}

	private struct FolderXattrSection: View {
		@Binding var draft: SettingsDraft<SushitrainFolderSettings>

		private var type: String { draft.settings.type }
		private var cantApply: Bool { type == "sendonly" || type == "receiveencrypted" }
		private var cantSend: Bool { type == "receiveonly" || type == "receiveencrypted" }

		var body: some View {
			Section {
				Toggle("Synchronize extended attributes", isOn: $draft.syncXattrs).settingsAnchor("syncXattrs")
					.disabled(cantApply)
				Toggle("Send extended attributes", isOn: self.sendBinding(\.sendXattrs, syncing: draft.settings.syncXattrs))
					.settingsAnchor("settings")
					.disabled(cantSend || draft.settings.syncXattrs)

			} header: {
				Text("Extended attributes")
			} footer: {
				Text(
					"Extended attributes are metadata such as Finder tags and resource forks. Synchronizing sends them to other devices and applies theirs here; sending does only the first, and is always on while synchronizing. Sending costs performance, as every file has to be looked at."
				)
			}
		}

		/// Sending shows as on while synchronizing, which includes it
		private func sendBinding(
			_ keyPath: ReferenceWritableKeyPath<SushitrainFolderSettings, Bool>, syncing: Bool
		) -> Binding<Bool> {
			Binding(
				get: { syncing || draft.settings[keyPath: keyPath] },
				set: { draft[dynamicMember: keyPath] = $0 }
			)
		}
	}

	private struct FolderXattrFilterSection: View {
		@Binding var draft: SettingsDraft<SushitrainFolderSettings>

		var body: some View {
			Section {
				Picker("Attributes on macOS", selection: $draft.xattrMode) {
					Text("Those that belong to the file").tag("syncable")
					Text("All").tag("all")
				}.settingsAnchor("xattrMode")
				XattrFilterEditor(text: $draft.xattrFilterEntries)
				SettingsNumberField(title: "Largest attribute", unit: "bytes", text: $draft.xattrMaxSingleEntrySize).settingsAnchor(
					"xattrMaxSingleEntrySize")
				SettingsNumberField(title: "Attributes per file", unit: "bytes", text: $draft.xattrMaxTotalSize).settingsAnchor(
					"xattrMaxTotalSize")
			} header: {
				Text("Attribute filter")
			} footer: {
				Text(
					"The macOS attribute mode selects file metadata or all attributes, including quarantine and caches. Custom rules override that mode. Rules are tried in order, and the first whose pattern matches an attribute's name decides, with * for any text. With any rule here, whatever none of them matches is left out, and the choice for macOS above doesn't apply: end with Allow * to change that. Attributes over the size limits are left out too; 0 means no limit."
				)
			}
		}

	}

	private struct FolderOwnershipSection: View {
		@Binding var draft: SettingsDraft<SushitrainFolderSettings>

		private var type: String { draft.settings.type }
		private var cantApply: Bool { type == "sendonly" || type == "receiveencrypted" }
		private var cantSend: Bool { type == "receiveonly" || type == "receiveencrypted" }

		var body: some View {
			Section {
				Toggle("Synchronize ownership", isOn: $draft.syncOwnership).settingsAnchor("syncOwnership")
					.disabled(cantApply)
				Toggle("Send ownership", isOn: self.sendBinding(\.sendOwnership, syncing: draft.settings.syncOwnership))
					.settingsAnchor("settings")
					.disabled(cantSend || draft.settings.syncOwnership)
				Toggle("Copy ownership from the parent directory", isOn: $draft.copyOwnershipFromParent).settingsAnchor(
					"copyOwnershipFromParent"
				)
				// It has to stay possible to turn this off, as both together can't be saved
				.disabled(draft.settings.syncOwnership && !draft.settings.copyOwnershipFromParent)
			} header: {
				Text("Ownership")
			} footer: {
				Text(
					"Applying ownership from other devices typically takes running with elevated privileges, which this app doesn't do. Sending costs performance. Copying from the parent directory gives new files the owner of the directory they arrive in, and can't go together with synchronizing ownership."
				)
			}
		}

		/// Sending shows as on while synchronizing, which includes it
		private func sendBinding(
			_ keyPath: ReferenceWritableKeyPath<SushitrainFolderSettings, Bool>, syncing: Bool
		) -> Binding<Bool> {
			Binding(
				get: { syncing || draft.settings[keyPath: keyPath] },
				set: { draft[dynamicMember: keyPath] = $0 }
			)
		}
	}

	private struct FolderPermissionsSection: View {
		@Binding var draft: SettingsDraft<SushitrainFolderSettings>

		private var type: String { draft.settings.type }

		var body: some View {
			Section {
				Toggle("Ignore permissions", isOn: $draft.ignorePerms).settingsAnchor("ignorePerms")
					.disabled(type == "receiveencrypted")
			} header: {
				Text("Permissions")
			} footer: {
				Text(
					"Doesn't compare or synchronize file permissions, for disks that don't have them, such as FAT and exFAT. Always on for receive-encrypted folders."
				)
			}
		}

	}
	/// The rules of an extended attribute filter, which the engine takes as lines of + or - and a pattern
	private struct XattrFilterEditor: View {
		private struct Rule: Identifiable {
			let id = UUID()
			var line: String
		}

		private static let permit = "+"
		private static let deny = "-"
		@Binding var text: String
		@State private var rules: [Rule]
		@FocusState private var focusedRule: UUID?

		init(text: Binding<String>) {
			self._text = text
			self._rules = State(initialValue: Self.read(text.wrappedValue))
		}

		var body: some View {
			ForEach(rules.enumerated(), id: \.element.id) { index, rule in
				HStack {
					Picker("Action", selection: self.actionBinding(rule)) {
						Text("Allow").tag(Self.permit)
						Text("Deny").tag(Self.deny)
					}
					.labelsHidden()
					.fixedSize()
					TextField("Attribute name", text: self.patternBinding(rule), prompt: Text(verbatim: "com.apple.*"))
						.labelsHidden()
						.font(.body.monospaced())
						.focused($focusedRule, equals: rule.id)
					Button("Move up", systemImage: "arrow.up") {
						self.move(rule.id, by: -1)
					}
					.disabled(index == 0)
					Button("Move down", systemImage: "arrow.down") {
						self.move(rule.id, by: 1)
					}
					.disabled(index == rules.count - 1)
					Button("Remove", systemImage: "minus.circle") {
						rules.removeAll { $0.id == rule.id }
						self.write()
					}
				}
				.labelStyle(.iconOnly)
				.buttonStyle(.borderless)
			}
			Button("Add a rule", systemImage: "plus.circle") {
				let rule = Rule(line: Self.permit)
				rules.append(rule)
				self.write()
				focusedRule = rule.id
			}
			.buttonStyle(.link)
			.onChange(of: text) {
				// A reload from the engine replaces the draft; our own edits keep each row's identity.
				if text != rules.map(\.line).joined(separator: "\n") {
					rules = Self.read(text)
				}
			}
		}

		private static func read(_ text: String) -> [Rule] {
			text.isEmpty ? [] : text.components(separatedBy: "\n").map { Rule(line: $0) }
		}

		private func actionBinding(_ rule: Rule) -> Binding<String> {
			Binding(
				get: { rule.line.hasPrefix(Self.deny) ? Self.deny : Self.permit },
				set: { self.replace(rule.id, with: $0 + rule.line.dropFirst()) }
			)
		}

		private func patternBinding(_ rule: Rule) -> Binding<String> {
			Binding(
				get: { String(rule.line.dropFirst()) },
				set: { value in self.replace(rule.id, with: String(rule.line.prefix(1)) + value) }
			)
		}

		private func replace(_ id: UUID, with line: String) {
			// AppKit may commit a text field after its row has been removed.
			guard let index = rules.firstIndex(where: { $0.id == id }) else { return }
			rules[index].line = line
			self.write()
		}

		private func move(_ id: UUID, by offset: Int) {
			guard let index = rules.firstIndex(where: { $0.id == id }), rules.indices.contains(index + offset) else { return }
			rules.swapAt(index, index + offset)
			self.write()
			focusedRule = id
		}

		private func write() {
			text = rules.map(\.line).joined(separator: "\n")
		}
	}

	/// How a folder keeps old versions. The sections are shared with the defaults for new folders.
	struct VersioningSections: View {
		@Binding var draft: SettingsDraft<SushitrainVersioningSettings>
		var isDefaults = false

		private var type: String { draft.settings.type }

		var body: some View {
			Section {
				Text(
					"Versioning applies to changes *received from other devices*. When versioning is enabled, older versions of files that are changed by other devices are saved locally for a while. Versioning does **not** keep older versions of files that are modified locally on this device. See [the Syncthing documentation on versioning](https://docs.syncthing.net/users/versioning.html) for further information."
				)
			} header: {
				Label("About versioning", systemImage: "questionmark.circle")
			}

			Section {
				Picker("Versioning", selection: $draft.type) {
					Text("Disabled").tag(SushitrainVersioningTypeNone)
					Text("Simple").tag(SushitrainVersioningTypeSimple)
					Text("Trash can").tag(SushitrainVersioningTypeTrashcan)
					Text("Staggered").tag(SushitrainVersioningTypeStaggered)
					Text("External").tag(SushitrainVersioningTypeExternal)
				}.settingsAnchor("type")
					.pickerStyle(.menu)
			} footer: {
				Text(self.typeDescription)
			}

			if type == SushitrainVersioningTypeSimple {
				Section {
					SettingsNumberField(title: "Versions to keep", unit: "per file", text: $draft.keep).settingsAnchor("keep")
					SettingsNumberField(title: "Maximum age", unit: "days", text: $draft.cleanoutDays).settingsAnchor("cleanoutDays")
				} footer: {
					Text("Set maximum age to 0 to keep versions regardless of age.")
				}
			}
			else if type == SushitrainVersioningTypeTrashcan {
				Section {
					SettingsNumberField(title: "Maximum age", unit: "days", text: $draft.cleanoutDays).settingsAnchor("cleanoutDays")
				} footer: {
					Text("Set maximum age to 0 to keep deleted files until you remove them manually.")
				}
			}
			else if type == SushitrainVersioningTypeStaggered {
				Section {
					SettingsNumberField(title: "Maximum age", unit: "days", text: $draft.maxAgeDays).settingsAnchor("maxAgeDays")
				} footer: {
					Text("Set maximum age to 0 to keep versions regardless of age.")
				}
			}
			else if type == SushitrainVersioningTypeExternal {
				Section {
					TextField(
						"Command", text: $draft.command, prompt: Text(verbatim: "/usr/local/bin/archive %FOLDER_PATH% %FILE_PATH%")
					).settingsAnchor("command")
						.font(.body.monospaced())
				} footer: {
					Text(
						"Runs this command for each file about to be replaced or deleted, which is the command's to move elsewhere. %FOLDER_PATH% and %FILE_PATH% stand for the folder and the file within it. The engine doesn't know where it puts files, so this app can't list or restore them."
					)
				}
			}

			if type != SushitrainVersioningTypeNone && type != SushitrainVersioningTypeExternal {
				self.storageSection()
			}
		}

		private func storageSection() -> some View {
			Section {
				SettingsNumberField(title: "Cleanup interval", unit: "minutes", text: $draft.cleanupIntervalMinutes).settingsAnchor(
					"cleanupIntervalMinutes")
				// New folders would all keep their versions in the same place
				if !isDefaults {
					LabeledContent("Versions path") {
						HStack {
							TextField("Versions path", text: $draft.path, prompt: Text(verbatim: ".stversions")).settingsAnchor("path")
								.labelsHidden()
							Button("Choose...") {
								self.chooseVersionsPath()
							}
						}
					}
				}
				if type == SushitrainVersioningTypeStaggered && !draft.settings.hasCustomPath() {
					Toggle("Keep platform history", isOn: $draft.keepPlatformHistory).settingsAnchor("keepPlatformHistory")
				}
			} footer: {
				Text(
					"With a cleanup interval of 0, versions are only cleaned up when a file is archived. Versions go in .stversions in the folder unless you enter a path, which can't be in or around a synchronized folder, or where another folder keeps its versions. Platform history keeps the metadata of a file together with its versions, so that restoring brings it back as well; it needs Staggered versioning in the default place."
				)
			}
		}

		private var typeDescription: String {
			switch type {
			case SushitrainVersioningTypeNone:
				return String(localized: "Synctrain will not keep old copies of files for this folder.")
			case SushitrainVersioningTypeSimple:
				return String(
					localized:
						"Synctrain moves files replaced or deleted by remote changes into the versions folder and keeps the configured number of old versions per file."
				)
			case SushitrainVersioningTypeTrashcan:
				return String(
					localized:
						"Synctrain moves files replaced or deleted by remote changes into the versions folder, replacing any older version with the same name."
				)
			case SushitrainVersioningTypeStaggered:
				return String(
					localized:
						"Synctrain moves files replaced or deleted by remote changes into the versions folder, then keeps fewer versions as they age."
				)
			default:
				return String(localized: "A command of your choice handles files replaced or deleted by remote changes.")
			}
		}

		private func chooseVersionsPath() {
			let panel = NSOpenPanel()
			panel.canChooseDirectories = true
			panel.canChooseFiles = false
			panel.canCreateDirectories = true
			panel.allowsMultipleSelection = false
			if panel.runModal() == .OK, let url = panel.url {
				draft.path = url.path(percentEncoded: false)
			}
		}
	}

	/// The sections above for a folder of the app, as sheets of the folder's settings
	struct FolderSyncSettingsView: View {
		let folder: SushitrainFolder

		var body: some View {
			SettingsPage(
				title: "Synchronization options", isSheet: true, load: { folder.settings() }, isEqual: { $0.equal($1) },
				save: { [folder] in try folder.setSettings($0) },
				content: { draft in
					AdvancedFolderRestartNotice()
					FolderSyncSections(draft: draft)
				}
			)
		}
	}

	struct FolderMetadataSettingsView: View {
		let folder: SushitrainFolder

		var body: some View {
			SettingsPage(
				title: "Metadata", isSheet: true, load: { folder.settings() }, isEqual: { $0.equal($1) },
				save: { [folder] in try folder.setSettings($0) },
				content: { draft in
					AdvancedFolderRestartNotice()
					FolderMetadataSections(draft: draft)
				}
			)
		}
	}

	/// A whole-folder reset is reviewed separately from the normal partial editors.
	struct FolderSyncResetSettingsView: View {
		@Environment(AppState.self) private var appState
		var folder: SushitrainFolder? = nil

		var body: some View {
			SettingsPage(
				title: "Reset Synchronization Settings", isSheet: true,
				load: {
					if let folder { return folder.settings() }
					return appState.client.defaultFolderSettings()
				},
				loadPreview: {
					if let folder { return folder.resetSettingsPreview() }
					return appState.client.defaultFolderResetSettingsPreview()
				},
				isEqual: { $0.equal($1) },
				save: { [folder, client = appState.client] settings in
					if let folder {
						try folder.setSettings(settings)
					}
					else {
						try client.setDefaultFolderSettings(settings)
					}
				},
				content: { draft in
					Section {
						Text(
							"Restore built-in synchronization defaults. Folder location, direction, sharing, ignore patterns, and versioning stay unchanged. Review the settings, then choose Save."
						)
					}
					if folder != nil {
						AdvancedFolderRestartNotice()
					}
					else {
						Text("New Folder Defaults apply only to folders created in the future.")
							.foregroundStyle(.secondary)
					}
					FolderResetScanSection(draft: draft)
					FolderDiskSection(draft: draft)
					FolderXattrSection(draft: draft)
					FolderOwnershipSection(draft: draft)
					FolderPermissionsSection(draft: draft)
					FolderAdvancedSection {
						FolderDefaultTuningSection(draft: draft)
						FolderFilesSection(draft: draft)
						FolderFileSystemSection(draft: draft, isDefaults: true)
						FolderScanSection(draft: draft)
						FolderTransferSection(draft: draft)
						FolderXattrFilterSection(draft: draft)
					}
				}
			)
		}
	}

	private struct FolderResetScanSection: View {
		@Binding var draft: SettingsDraft<SushitrainFolderSettings>

		var body: some View {
			Section("Change detection") {
				SettingsNumberField(title: "Rescan interval", unit: "seconds", text: $draft.rescanIntervalS).settingsAnchor(
					"rescanIntervalS")
				Toggle("Watch for changes", isOn: $draft.fsWatcherEnabled).settingsAnchor("fsWatcherEnabled")
			}
		}
	}

	struct FolderVersioningSettingsView: View {
		let folder: SushitrainFolder

		var body: some View {
			SettingsPage(
				title: "File versioning", isSheet: true, load: { folder.versioningSettings() }, isEqual: { $0.equal($1) },
				save: { [folder] in try folder.setVersioningSettings($0) },
				confirmation: { settings in
					if settings.losesPlatformHistory() {
						return String(
							localized:
								"This ends the platform history of this folder. Versions that have it stay, but it isn't protected from cleaning up anymore, and restoring one may fail."
						)
					}
					if settings.startsPlatformHistory() {
						return String(
							localized:
								"This starts the platform history of this folder. Versions it has kept so far don't have it, so they can't be restored while it is on. They aren't lost: turning it off lets you restore them again."
						)
					}
					return nil
				},
				content: { draft in
					AdvancedFolderRestartNotice()
					VersioningSections(draft: draft)
				}
			)
		}
	}

	/// Saving restarts the folder, which can interrupt a transfer
	private struct AdvancedFolderRestartNotice: View {
		var body: some View {
			Section {
				Text("Saving restarts this folder: what it is transferring continues afterwards.")
					.foregroundStyle(.secondary)
			}
		}
	}
#endif
