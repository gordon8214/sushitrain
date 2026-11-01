// Copyright (C) 2026 Gordon
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this file,
// You can obtain one at https://mozilla.org/MPL/2.0/.
#if os(macOS)
	import SushitrainCore
	import SwiftUI

	/// How a folder synchronizes. The sections are shared with the defaults for new folders.
	struct FolderSyncSections: View {
		@Binding var draft: SettingsDraft<SushitrainFolderSettings>
		var isDefaults = false

		var body: some View {
			self.filesSection()
			self.diskSection()
			self.fileSystemSection()
			self.scanSection()
			self.transferSection()
		}

		private func filesSection() -> some View {
			Section {
				Picker("File pull order", selection: $draft.order) {
					Text("Random").tag("random")
					Text("Alphabetic").tag("alphabetic")
					Text("Smallest first").tag("smallestFirst")
					Text("Largest first").tag("largestFirst")
					Text("Oldest first").tag("oldestFirst")
					Text("Newest first").tag("newestFirst")
				}
				Toggle("Ignore deletions from other devices", isOn: $draft.ignoreDelete)
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

		private func diskSection() -> some View {
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

		private func fileSystemSection() -> some View {
			Section {
				Toggle("Case-sensitive file names", isOn: $draft.caseSensitiveFS)
				if draft.settings.caseSensitiveFS {
					SettingsWarning(
						text: "On a disk that isn't case-sensitive, files whose names differ only by case can overwrite each other.")
				}
				Toggle("Normalize file names", isOn: $draft.autoNormalize)
				if !draft.settings.autoNormalize {
					SettingsWarning(text: "Files with wrongly encoded names will show as failed instead of being corrected.")
				}
				Toggle("No sparse files", isOn: $draft.disableSparseFiles)
				Toggle("Don't wait for writes to reach the disk", isOn: $draft.disableFsync)
				if draft.settings.disableFsync {
					SettingsWarning(text: "A crash or power failure can damage files that were being written.")
				}
				SettingsNumberField(title: "Modification time window", unit: "seconds", text: $draft.modTimeWindowS)
				if !isDefaults {
					TextField("Folder marker", text: $draft.markerName, prompt: Text(verbatim: ".stfolder"))
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

		private func scanSection() -> some View {
			Section {
				SettingsNumberField(title: "Watcher timeout", unit: "seconds", text: $draft.fsWatcherTimeoutS)
				SettingsNumberField(title: "Scan progress interval", unit: "seconds", text: $draft.scanProgressIntervalS)
			} header: {
				Text("Scanning")
			} footer: {
				Text(
					"How long changes that keep coming can hold up a scan, where 0 lets the engine go by the watcher's delay. How often a scan reports its progress, where 0 is every 2 seconds and a negative number turns this off."
				)
			}
		}

		private func transferSection() -> some View {
			Section {
				SettingsNumberField(title: "Copiers", unit: "per file", text: $draft.copiers)
				SettingsNumberField(title: "Hashers", unit: "at once", text: $draft.hashers)
				SettingsNumberField(title: "Files written at once", unit: "files", text: $draft.maxConcurrentWrites)
				SettingsNumberField(title: "Pending data", unit: "KiB", text: $draft.pullerMaxPendingKiB)
				SettingsNumberField(title: "Delay before pulling", unit: "seconds", text: $draft.pullerDelayS)
				SettingsNumberField(title: "Pause after a failure", unit: "seconds", text: $draft.pullerPauseS)
				Picker("Block pull order", selection: $draft.blockPullOrder) {
					Text("Standard").tag("standard")
					Text("Random").tag("random")
					Text("In order").tag("inOrder")
				}
				Picker("Copy method", selection: $draft.blockCopyMethod) {
					ForEach(draft.settings.blockCopyMethods()?.asArray() ?? [], id: \.self) { method in
						Text(Self.copyMethodTitle(method)).tag(method)
					}
				}
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

	/// What goes along with a file besides its contents. The sections are shared with the defaults for new folders.
	struct FolderMetadataSections: View {
		@Binding var draft: SettingsDraft<SushitrainFolderSettings>

		private var type: String { draft.settings.type }
		private var cantApply: Bool { type == "sendonly" || type == "receiveencrypted" }
		private var cantSend: Bool { type == "receiveonly" || type == "receiveencrypted" }

		var body: some View {
			self.xattrSection()
			self.xattrFilterSection()
			self.ownershipSection()
			self.permissionsSection()
		}

		private func xattrSection() -> some View {
			Section {
				Toggle("Synchronize extended attributes", isOn: $draft.syncXattrs)
					.disabled(cantApply)
				Toggle("Send extended attributes", isOn: self.sendBinding(\.sendXattrs, syncing: draft.settings.syncXattrs))
					.disabled(cantSend || draft.settings.syncXattrs)
				Picker("Attributes on macOS", selection: $draft.xattrMode) {
					Text("Those that belong to the file").tag("syncable")
					Text("All").tag("all")
				}
			} header: {
				Text("Extended attributes")
			} footer: {
				Text(
					"Extended attributes are metadata such as Finder tags and resource forks. Synchronizing sends them to other devices and applies theirs here; sending does only the first, and is always on while synchronizing. Sending costs performance, as every file has to be looked at. macOS marks the attributes that belong to the file and not to this Mac, such as quarantine and caches, which stay here unless All is chosen. Rules below take the place of this choice."
				)
			}
		}

		private func xattrFilterSection() -> some View {
			Section {
				XattrFilterEditor(text: $draft.xattrFilterEntries)
				SettingsNumberField(title: "Largest attribute", unit: "bytes", text: $draft.xattrMaxSingleEntrySize)
				SettingsNumberField(title: "Attributes per file", unit: "bytes", text: $draft.xattrMaxTotalSize)
			} header: {
				Text("Attribute filter")
			} footer: {
				Text(
					"Rules are tried in order, and the first whose pattern matches an attribute's name decides, with * for any text. With any rule here, whatever none of them matches is left out, and the choice for macOS above doesn't apply: end with Allow * to change that. Attributes over the size limits are left out too; 0 means no limit."
				)
			}
		}

		private func ownershipSection() -> some View {
			Section {
				Toggle("Synchronize ownership", isOn: $draft.syncOwnership)
					.disabled(cantApply)
				Toggle("Send ownership", isOn: self.sendBinding(\.sendOwnership, syncing: draft.settings.syncOwnership))
					.disabled(cantSend || draft.settings.syncOwnership)
				Toggle("Copy ownership from the parent directory", isOn: $draft.copyOwnershipFromParent)
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

		private func permissionsSection() -> some View {
			Section {
				Toggle("Ignore permissions", isOn: $draft.ignorePerms)
					.disabled(type == "receiveencrypted")
			} header: {
				Text("Permissions")
			} footer: {
				Text(
					"Doesn't compare or synchronize file permissions, for disks that don't have them, such as FAT and exFAT. Always on for receive-encrypted folders."
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

	/// The rules of an extended attribute filter, which the engine takes as lines of + or - and a pattern
	private struct XattrFilterEditor: View {
		private static let permit = "+"
		private static let deny = "-"

		@Binding var text: String

		private var lines: [String] {
			text.isEmpty ? [] : text.components(separatedBy: "\n")
		}

		var body: some View {
			ForEach(Array(lines.enumerated()), id: \.offset) { index, line in
				HStack {
					Picker("Action", selection: self.actionBinding(index, line)) {
						Text("Allow").tag(Self.permit)
						Text("Deny").tag(Self.deny)
					}
					.labelsHidden()
					.fixedSize()
					TextField("Attribute name", text: self.patternBinding(index, line), prompt: Text(verbatim: "com.apple.*"))
						.labelsHidden()
						.font(.body.monospaced())
					Button("Move up", systemImage: "arrow.up") {
						self.move(index, by: -1)
					}
					.disabled(index == 0)
					Button("Move down", systemImage: "arrow.down") {
						self.move(index, by: 1)
					}
					.disabled(index == lines.count - 1)
					Button("Remove", systemImage: "minus.circle") {
						var lines = self.lines
						lines.remove(at: index)
						text = lines.joined(separator: "\n")
					}
				}
				.labelStyle(.iconOnly)
				.buttonStyle(.borderless)
			}
			Button("Add a rule", systemImage: "plus.circle") {
				text = (lines + [Self.permit]).joined(separator: "\n")
			}
			.buttonStyle(.link)
		}

		private func actionBinding(_ index: Int, _ line: String) -> Binding<String> {
			Binding(
				get: { line.hasPrefix(Self.deny) ? Self.deny : Self.permit },
				set: { self.replace(index, with: $0 + line.dropFirst()) }
			)
		}

		private func patternBinding(_ index: Int, _ line: String) -> Binding<String> {
			Binding(
				get: { String(line.dropFirst()) },
				set: { self.replace(index, with: line.prefix(1) + $0) }
			)
		}

		private func replace(_ index: Int, with line: String) {
			var lines = self.lines
			lines[index] = line
			text = lines.joined(separator: "\n")
		}

		private func move(_ index: Int, by offset: Int) {
			var lines = self.lines
			lines.swapAt(index, index + offset)
			text = lines.joined(separator: "\n")
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
				}
				.pickerStyle(.menu)
			} footer: {
				Text(self.typeDescription)
			}

			if type == SushitrainVersioningTypeSimple {
				Section {
					SettingsNumberField(title: "Versions to keep", unit: "per file", text: $draft.keep)
					SettingsNumberField(title: "Maximum age", unit: "days", text: $draft.cleanoutDays)
				} footer: {
					Text("Set maximum age to 0 to keep versions regardless of age.")
				}
			}
			else if type == SushitrainVersioningTypeTrashcan {
				Section {
					SettingsNumberField(title: "Maximum age", unit: "days", text: $draft.cleanoutDays)
				} footer: {
					Text("Set maximum age to 0 to keep deleted files until you remove them manually.")
				}
			}
			else if type == SushitrainVersioningTypeStaggered {
				Section {
					SettingsNumberField(title: "Maximum age", unit: "days", text: $draft.maxAgeDays)
				} footer: {
					Text("Set maximum age to 0 to keep versions regardless of age.")
				}
			}
			else if type == SushitrainVersioningTypeExternal {
				Section {
					TextField(
						"Command", text: $draft.command, prompt: Text(verbatim: "/usr/local/bin/archive %FOLDER_PATH% %FILE_PATH%")
					)
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
				SettingsNumberField(title: "Cleanup interval", unit: "minutes", text: $draft.cleanupIntervalMinutes)
				// New folders would all keep their versions in the same place
				if !isDefaults {
					LabeledContent("Versions path") {
						HStack {
							TextField("Versions path", text: $draft.path, prompt: Text(verbatim: ".stversions"))
								.labelsHidden()
							Button("Choose...") {
								self.chooseVersionsPath()
							}
						}
					}
				}
				if type == SushitrainVersioningTypeStaggered && !draft.settings.hasCustomPath() {
					Toggle("Keep platform history", isOn: $draft.keepPlatformHistory)
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

	struct FolderVersioningSettingsView: View {
		let folder: SushitrainFolder

		var body: some View {
			SettingsPage(
				title: "File versioning", isSheet: true, load: { folder.versioningSettings() }, isEqual: { $0.equal($1) },
				save: { [folder] in try folder.setVersioningSettings($0) },
				confirmation: { settings in
					guard settings.losesPlatformHistory() else {
						return nil
					}
					return String(
						localized:
							"This ends the platform history of this folder. Versions that have it stay, but it isn't protected from cleaning up anymore, and restoring one may fail."
					)
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
