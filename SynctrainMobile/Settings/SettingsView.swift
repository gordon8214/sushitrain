// Copyright (C) 2026 Gordon
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this file,
// You can obtain one at https://mozilla.org/MPL/2.0/.
import SwiftUI

struct SettingsView: View {
	private var coordinator = RemoteCoordinator.shared
	@AppStorage(DeviceName.storageKey) private var deviceName = ""
	@AppStorage(CacheSettings.storageKey) private var cacheLimit = CacheSettings.defaultLimit
	@AppStorage("viewerSort") private var sort = ViewerSort.name
	@AppStorage("hideDotFiles") private var hideDotFiles = true
	@State private var cacheSize: Int64? = nil

	var body: some View {
		NavigationStack {
			Form {
				Section {
					NavigationLink(destination: RemoteFoldersSettingsView()) {
						Text("Gateways").badge(coordinator.gateways.count)
					}
					NavigationLink(destination: ChooseFoldersView()) {
						Text("Folders").badge(coordinator.folders.count)
					}
					LabeledContent("Device name") {
						TextField(DeviceName.systemName, text: $deviceName)
							.multilineTextAlignment(.trailing)
					}
				} footer: {
					Text("Gateways list this device under its name. It applies the next time you pair.")
				}

				Section {
					Picker("Keep up to", selection: $cacheLimit) {
						ForEach(CacheSettings.choices, id: \.self) { limit in
							Text(Int64(limit).formatted(.byteCount(style: .memory))).tag(limit)
						}
					}
					LabeledContent("In use") {
						if let cacheSize {
							Text(cacheSize.formatted(.byteCount(style: .file)))
						}
						else {
							ProgressView()
						}
					}
					Button("Clear Downloaded Files", role: .destructive) {
						trim(to: 0)
					}
				} header: {
					Text("Downloaded files")
				} footer: {
					Text(
						"Files you open are kept on this device for a while, up to this size. Files you chose to keep in the Files app are not affected."
					)
				}

				Section("Display") {
					Picker("Sort by", selection: $sort) {
						ForEach(ViewerSort.allCases) { Text($0.label).tag($0) }
					}
					Toggle("Hide Files Starting with a Dot", isOn: $hideDotFiles)
				}

				Section {
					NavigationLink("About Synctrain", destination: AboutView())
				}
			}
			.navigationTitle("Settings")
			.task {
				await measureCache()
			}
			.onChange(of: cacheLimit) { _, limit in
				trim(to: Int64(limit))
			}
		}
	}

	private func measureCache() async {
		guard let storage = RemoteEnvironment.shared?.storage else {
			return
		}
		cacheSize = await Task.detached(priority: .utility) { storage.contentByteCount }.value
	}

	private func trim(to limit: Int64) {
		guard let storage = RemoteEnvironment.shared?.storage else {
			return
		}
		Task {
			await Task.detached(priority: .userInitiated) { storage.trimContent(toBytes: limit) }.value
			await measureCache()
		}
	}
}
