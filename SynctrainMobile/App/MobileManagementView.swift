// Copyright (C) 2026 Gordon. SPDX-License-Identifier: MPL-2.0
import SwiftUI
import SynctrainHostKit

struct MobileManagementView: View {
	@State private var browser = MobileBrowserNavigation()
	@State private var paths: [String: NavigationPath] = [:]
	@State private var selection: String? = "overview"
	@State private var column = NavigationSplitViewColumn.detail
	@State private var search = SettingsSearch(catalog: .desktop, platform: "ios")
	private let groups = DesktopUICatalog.desktop.navigation(platform: "ios", mode: "files")

	var body: some View {
		@Bindable var search = search
		NavigationSplitView(preferredCompactColumn: $column) {
			List(selection: $selection) {
				if search.query.trimmingCharacters(in: .whitespacesAndNewlines).isEmpty {
					ForEach(groups) { group in
						Section(group.localizedTitle) {
							ForEach(group.items) { item in
								NavigationLink(value: item.id) { Label(item.localizedTitle, systemImage: item.symbol) }
									.accessibilityIdentifier("navigation.\(item.id)")
							}
						}
					}
				}
				else {
					if search.results.isEmpty { ContentUnavailableView.search(text: search.query) }
					ForEach(search.results) { entry in
						NavigationLink(value: "search:" + entry.id) { SettingsSearchResultLabel(entry: entry) }
					}
				}
			}
			.navigationTitle("Synctrain")
			.searchable(text: $search.query, prompt: "Search Settings")
		} detail: {
			NavigationStack(
				path: Binding(
					get: { paths[selection ?? "overview"] ?? NavigationPath() }, set: { paths[selection ?? "overview"] = $0 })
			) {
				Group {
					if let entry = search.entries.first(where: { "search:" + $0.id == selection }) {
						MobileSettingsSearchDestination(entry: entry)
					}
					else {
						MobileManagementPage(destination: selection ?? "overview", browser: browser)
					}
				}
				.id(selection ?? "overview")
				.navigationTitle(
					search.entries.first(where: { "search:" + $0.id == selection })?.localizedTitle ?? groups.flatMap(\.items).first(
						where: { $0.id == selection })?.title ?? "Overview")
			}
		}
		.task { search.mode = "files" }
		.onReceive(NotificationCenter.default.publisher(for: UIApplication.didReceiveMemoryWarningNotification)) { _ in
			browser.releaseListings()
		}
	}
}

private struct MobileManagementPage: View {
	let destination: String
	let browser: MobileBrowserNavigation
	@State private var chooseHistoryFile = false
	var body: some View {
		if destination == "files" {
			MobileFolderList(navigation: browser)
		}
		else {
			Group {
				switch destination {
				case "overview": MobileOverview()
				case "folders": ChooseFoldersView().navigationTitle("Folders")
				case "remoteAccess": Form { RemoteGatewaysSection() }.navigationTitle("Remote Access")
				case "activity":
					Form {
						RemoteTransfersSection(store: RemoteEnvironment.shared?.store)
						if RemoteCoordinator.shared.operations.isEmpty {
							Section("Pending Changes") { Text("No pending edits.") }
						}
						RemoteOperationsSection()
					}.navigationTitle("Activity")
				case "history":
					Form {
						Button("Choose a File") { chooseHistoryFile = true }
						NavigationLink("History Sources") { HistorySourcesView() }
					}.navigationTitle("History")
				case "about": AboutView()
				default:
					MobileSettingsForm(category: destination)
						.navigationTitle(DesktopUICatalog.desktop.settings.first { $0.id == destination }?.title ?? "General")
				}
			}
			.sheet(isPresented: $chooseHistoryFile) {
				FoldersView(isPresentedModally: true, selectHistory: true)
			}
		}
	}
}

private struct MobileOverview: View {
	private var coordinator = RemoteCoordinator.shared
	@State private var locationError: String?
	var body: some View {
		Form {
			Section("Status") {
				Text("\(coordinator.folders.count) folders from \(coordinator.gateways.count) gateways")
				Button("Open in Files", systemImage: "folder") {
					Task {
						if let url = await FilesDomain.shared.locationURL() {
							ClientLifecycle.show(url)
						}
						else {
							locationError = "Turn on Synctrain under Locations in the Files app to see your folders."
						}
					}
				}.buttonStyle(.borderedProminent)
			}
			Section("Needs Attention") {
				if coordinator.operations.isEmpty { Text("No pending edits.") }
				RemoteOperationsSection()
				ForEach(coordinator.gateways) { gateway in
					if let problem = gateway.problem { Label(problem, systemImage: "exclamationmark.triangle") }
				}
			}
			RemoteGatewaysSection()
			Section("Folders") {
				NavigationLink("Manage Folders") { ChooseFoldersView() }
			}
		}
		.navigationTitle("Overview")
		.refreshable { await coordinator.refresh() }
		.alert(
			"Files location unavailable",
			isPresented: Binding(get: { locationError != nil }, set: { if !$0 { locationError = nil } })
		) {
			Button("OK") { locationError = nil }
		} message: {
			Text(locationError ?? "")
		}
	}
}
