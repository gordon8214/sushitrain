// Copyright (C) 2024-2026 Tommy van der Vorst
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this file,
// You can obtain one at https://mozilla.org/MPL/2.0/.
import Foundation
import SwiftUI
import SushitrainCore
import SynctrainHostKit

struct FilesPreviewSettingsView: View {
	@Environment(AppState.self) private var appState
	@ObservedObject var userSettings: AppUserSettings

	var body: some View {
		Form {
			BrowsingSettingsSections(userSettings: userSettings)
			PreviewSettingsSections(userSettings: userSettings)
			ThumbnailCacheSettingsSection(userSettings: userSettings)
		}
		.formStyle(.grouped)
		.onDisappear { appState.applySettings() }
	}
}

private struct PreviewSettingsSections: View {
	@ObservedObject var userSettings: AppUserSettings

	var body: some View {
		Group {
			// Thumbnail settings
			Section("Previews") {
				Toggle(
					"Show image previews",
					isOn: Binding(
						get: {
							return userSettings.maxBytesForPreview > 0
						},
						set: { nv in
							if nv {
								userSettings.maxBytesForPreview = 3 * 1024 * 1024  // 3 MiB
							}
							else {
								userSettings.maxBytesForPreview = 0
							}
						}))

				if userSettings.maxBytesForPreview > 0 {
					Stepper(
						"\(userSettings.maxBytesForPreview / 1024 / 1024) MB",
						value: Binding(
							get: {
								userSettings.maxBytesForPreview / 1024 / 1024
							},
							set: { nv in
								userSettings.maxBytesForPreview = nv * 1024 * 1024
							}), in: 1...100)
				}
			}

			Section {
				Toggle("Show video previews", isOn: userSettings.$previewVideos)
			}

		}
	}
}

private struct BrowsingSettingsSections: View {
	@ObservedObject var userSettings: AppUserSettings

	var body: some View {
		Group {
			Section {
				Toggle("Preview files on tap", isOn: userSettings.$tapFileToPreview)
			}

			Section {
				Toggle("Automatically switch to grid view", isOn: userSettings.$automaticallySwitchViewStyle)
			} footer: {
				Text("When a folder contains only images, it will automatically be shown as a grid instead of a list.")
			}

			Section {
				Toggle("Automatically display web pages", isOn: userSettings.$automaticallyShowWebpages)
			} footer: {
				Text("When a folder contains a file named 'index.html', it will automatically be shown as a web page.")
			}

			Section {
				Toggle("Hide dotfiles", isOn: userSettings.$dotFilesHidden)
			} footer: {
				Text(
					"When enabled, files and directories whose name start with a dot will not be shown when browsing a folder. These files and directories will remain visible in search results."
				)
			}
		}
	}
}

private struct ThumbnailCacheSettingsSection: View {
	@Environment(AppState.self) private var appState
	@ObservedObject var userSettings: AppUserSettings
	@State private var diskCacheSizeBytes: UInt? = nil
	@State private var folders: [SushitrainFolder] = []
	@State private var confirmClearThumbnailCache = false

	var body: some View {
		Section {
			Toggle("Cache thumbnails on disk", isOn: userSettings.$cacheThumbnailsToDisk)

			// Select thumbnail folder
			Picker("Cache location", selection: userSettings.$cacheThumbnailsToFolderID) {
				Text("On this device").tag("")
				ForEach(folders, id: \.folderID) { folder in
					Text(folder.displayName).tag(folder.folderID)
				}
				if !folders.contains(where: {
					$0.folderID == userSettings.cacheThumbnailsToFolderID
				}) {
					Text(userSettings.cacheThumbnailsToFolderID).disabled(true)
				}
			}
			.pickerStyle(.menu).disabled(!userSettings.cacheThumbnailsToDisk)

			// Clear thumbnail cache button
			if userSettings.cacheThumbnailsToFolderID == "" {
				Button("Clear thumbnail cache") {
					confirmClearThumbnailCache = true

				}.confirmationDialog(
					"Are you sure you want to clear the thumbnail cache?",
					isPresented: $confirmClearThumbnailCache, titleVisibility: .visible
				) {
					Button("Clear thumbnail cache", role: .destructive) {
						Task {
							await ImageCache.shared.clear()
							self.diskCacheSizeBytes = nil
						}
					}
				}
			}
		} header: {
			Text("File previews")
		} footer: {
			self.cacheText
		}

		.task { await self.update() }
	}

	private func update() async {
		self.folders = await appState.folders().sorted()
		Task {
			do {
				self.diskCacheSizeBytes = try await ImageCache.shared.diskCacheSizeBytes()
			}
			catch {
				Log.warn("Could not determine thumbnail cache size: \(error.localizedDescription)")
			}
		}
	}

	private var cacheText: Text {
		let formatter = ByteCountFormatter()
		var sentences = [
			String(
				localized:
					"When the cache is enabled, thumbnails will load quicker and use less data when viewed more than once.")
		]

		if let bytes = self.diskCacheSizeBytes {
			sentences.append(
				String(
					localized:
						"Currently the thumbnail cache is using \(formatter.string(fromByteCount: Int64(bytes))) of disk space."
				))
		}

		if appState.userSettings.cacheThumbnailsToFolderID == "" {
			sentences.append(
				String(
					localized:
						"When disk space is scarce, the system may decide to remove some thumbnails in order to free up space."
				))
		}

		return Text(sentences.joined(separator: " "))
	}
}
