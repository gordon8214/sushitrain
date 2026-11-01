// Copyright (C) 2026 Gordon
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this file,
// You can obtain one at https://mozilla.org/MPL/2.0/.
#if os(macOS)
	import SushitrainCore
	import SwiftUI

	/// A device's settings. The sections are shared with the defaults for new devices.
	struct DeviceSettingsSections: View {
		@Binding var draft: SettingsDraft<SushitrainDeviceSettings>

		var body: some View {
			self.transferSection()
			self.foldersSection()
			self.networksSection()
			self.advancedSection()
		}

		private func transferSection() -> some View {
			Section {
				Picker("Compression", selection: $draft.compression) {
					Text("Metadata only").tag("metadata")
					Text("All data").tag("always")
					Text("None").tag("never")
				}
				SettingsNumberField(title: "Upload limit", unit: "Mbit/s", text: $draft.maxSendMbits)
				SettingsNumberField(title: "Download limit", unit: "Mbit/s", text: $draft.maxRecvMbits)
			} header: {
				Text("Transfers")
			} footer: {
				Text(
					"Compressing all data saves bandwidth at the cost of CPU time. A limit of 0 means none, apart from the limits for all devices together."
				)
			}
		}

		private func foldersSection() -> some View {
			Section {
				Toggle("Accept folders automatically", isOn: $draft.autoAcceptFolders)
					.disabled(draft.settings.untrusted)
				Toggle("Keep what an introducer stops sharing", isOn: $draft.skipIntroductionRemovals)
			} header: {
				Text("Folders")
			} footer: {
				Text(
					"Folders this device offers are then added and shared without asking, in the default place for new folders. That isn't possible for an untrusted device. When it is an introducer, devices and folders it introduced normally go when it stops listing them; this keeps them."
				)
			}
		}

		private func networksSection() -> some View {
			Section {
				SettingsLinesEditor(title: "Allowed networks", text: $draft.allowedNetworks)
			} header: {
				Text("Allowed networks")
			} footer: {
				Text(
					"Connect with this device only from these networks, one per line, such as 192.168.0.0/16. The first that matches decides, and ! in front of one refuses it. With none, it can connect from anywhere."
				)
			}
		}

		private func advancedSection() -> some View {
			Section {
				SettingsNumberField(title: "Connections", unit: "at once", text: $draft.numConnections)
				SettingsNumberField(title: "Request size", unit: "KiB", text: $draft.maxRequestKiB)
				TextField("Certificate name", text: $draft.certName, prompt: Text("Default"))
				TextField("Web GUI port", text: $draft.remoteGUIPort)
			} header: {
				Text("Advanced")
			} footer: {
				Text(
					"How many connections to keep with this device, and how much data it can ask for at a time; 0 lets the engine decide. The certificate name is what this device's certificate has to say, if it isn't the usual one. The port is where the web GUI of this device can be reached; 0 means none."
				)
			}
		}
	}

	/// The device's settings as a sheet
	struct DeviceSettingsView: View {
		let device: SushitrainPeer

		var body: some View {
			SettingsPage(
				title: "Connection options", isSheet: true, load: { device.settings() }, isEqual: { $0.equal($1) },
				save: { [device] in try device.setSettings($0) },
				content: { draft in
					DeviceSettingsSections(draft: draft)
				}
			)
		}
	}

	/// What a device offered that was ignored, which can be offered again. The caller keeps the list, as a view with nothing
	/// to show has nothing to run a task on.
	struct DeviceIgnoredFoldersSection: View {
		let device: SushitrainPeer
		let folders: [SushitrainIgnoredFolder]

		@State private var errorMessage: ErrorMessage? = nil

		var body: some View {
			Section {
				ForEach(folders, id: \.folderID) { folder in
					LabeledContent {
						Button("Stop ignoring") {
							self.unignore(folder)
						}
					} label: {
						Text(folder.label.isEmpty ? folder.folderID : folder.label)
						Text(verbatim: folder.folderID).font(.caption).foregroundStyle(.secondary)
					}
				}
				.errorAlert($errorMessage)
			} header: {
				Text("Ignored folders")
			} footer: {
				Text(
					"This device offered these folders, and they were ignored. Once you stop ignoring one, it can offer it again."
				)
			}
		}

		private func unignore(_ folder: SushitrainIgnoredFolder) {
			let folderID = folder.folderID
			Task {
				do {
					try await goBlocking { [device] in try device.unignoreFolder(folderID) }
				}
				catch {
					errorMessage = ErrorMessage(error)
				}
			}
		}
	}
#endif
