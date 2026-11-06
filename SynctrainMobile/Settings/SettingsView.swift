// Copyright (C) 2026 Gordon
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this file,
// You can obtain one at https://mozilla.org/MPL/2.0/.
import SwiftUI
import SynctrainHostKit

struct MobileSettingsForm: View {
	private var coordinator = RemoteCoordinator.shared
	@AppStorage(DeviceName.storageKey) private var deviceName = ""
	@AppStorage(CacheSettings.storageKey) private var cacheLimit = CacheSettings.defaultLimit
	@AppStorage("viewerSort") private var sort = ViewerSort.name
	@AppStorage("hideDotFiles") private var hideDotFiles = true
	@State private var cacheSize: Int64? = nil

	var body: some View {
		Form {
			Section {
				NavigationLink(destination: RemoteFoldersSettingsView()) {
					Text("Gateways").badge(coordinator.gateways.count)
				}
				NavigationLink(destination: ChooseFoldersView()) {
					Text("Folders").badge(coordinator.folders.count)
				}
				NavigationLink("History Sources", destination: HistorySourcesView()).settingsAnchor("historySources")
				LabeledContent("Device name") {
					TextField(DeviceName.systemName, text: $deviceName).settingsAnchor("deviceName")
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
				}.settingsAnchor("cacheLimit")
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
				}.settingsAnchor("clearDownloads")
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
				}.settingsAnchor("viewerSort")
				Toggle("Hide Files Starting with a Dot", isOn: $hideDotFiles).settingsAnchor("hideDotFiles")
			}

			Section {
				NavigationLink("About Synctrain", destination: AboutView())
			}
		}
		.task {
			await measureCache()
		}
		.onChange(of: cacheLimit) { _, limit in
			trim(to: Int64(limit))
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

struct SettingsView: View {
	@State private var search = SettingsSearch(catalog: .desktop, platform: "ios")
	var body: some View {
		@Bindable var search = search
		NavigationStack {
			Group {
				if search.query.trimmingCharacters(in: .whitespacesAndNewlines).isEmpty {
					MobileSettingsForm()
				}
				else {
					List {
						if search.results.isEmpty { ContentUnavailableView.search(text: search.query) }
						ForEach(search.results) { entry in
							NavigationLink(value: entry) { SettingsSearchResultLabel(entry: entry) }
								.accessibilityIdentifier("settings.result.\(entry.id)")
						}
					}
				}
			}
			.navigationTitle("Settings")
			.searchable(text: $search.query, prompt: "Search Settings")
			.navigationDestination(for: SettingSearchEntry.self) { entry in
				MobileSettingsSearchDestination(entry: entry)
			}
		}
	}
}

private struct MobileSettingsSearchDestination: View {
	let entry: SettingSearchEntry
	var body: some View {
		SettingsSearchReveal(anchor: entry.anchor) {
			switch entry.action {
			case "gateways": RemoteFoldersSettingsView()
			case "chooseFolders": ChooseFoldersView()
			case "historySources": HistorySourcesView()
			case "fileManager": Form { FilesLocationSettingsView() }
			default: MobileSettingsForm()
			}
		}
		.navigationTitle(entry.localizedTitle)
	}
}
