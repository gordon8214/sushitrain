// Copyright (C) 2026 Gordon. SPDX-License-Identifier: MPL-2.0
#if os(macOS)
	import SwiftUI
	import SynctrainHostKit
	import SynctrainFileProviderKit
	@preconcurrency import SushitrainCore

	struct DesktopEngineSidebar: View {
		@Binding var selection: Route?
		@Environment(AppState.self) private var appState
		@State private var folders: [SushitrainFolder] = []
		@SceneStorage("desktop.files.expanded") private var expanded = true

		var body: some View {
			List(selection: $selection) {
				ForEach(DesktopUICatalog.desktop.destinations.filter { $0.id != "settings" }) { item in
					DesktopEngineSidebarRow(item: item, folders: folders, expanded: $expanded, userSettings: appState.userSettings)
				}
			}
			.listStyle(.sidebar)
			.safeAreaInset(edge: .bottom) { DesktopSettingsButton() }
			.navigationSplitViewColumnWidth(min: 200, ideal: 230, max: 300)
			.task(id: appState.eventCounter) {
				folders = await appState.folders().sorted {
					$0.displayName.lowercased() == $1.displayName.lowercased()
						? $0.folderID < $1.folderID : $0.displayName.lowercased() < $1.displayName.lowercased()
				}
			}
		}
	}

	private struct DesktopEngineSidebarRow: View {
		let item: DesktopUICatalog.Destination
		let folders: [SushitrainFolder]
		@Binding var expanded: Bool
		@ObservedObject var userSettings: AppUserSettings
		private var route: Route {
			switch item.id {
			case "overview": .start
			case "folders": .folders
			case "devices": .devices
			default: .desktop(item.id)
			}
		}
		var body: some View {
			Group {
				if item.id == "files" {
					DisclosureGroup(isExpanded: $expanded) {
						ForEach(folders.filter { !userSettings.hideHiddenFolders || $0.isHidden == false }, id: \.folderID) { folder in
							NavigationLink(value: Route.folder(folderID: folder.folderID, prefix: nil)) {
								HStack {
									Label(folder.displayName, systemImage: "folder")
									Spacer()
									FolderMetricView(metric: userSettings.viewMetric, folder: folder)
								}
							}
						}
					} label: {
						NavigationLink(value: route) { Label(item.title, systemImage: item.symbol) }
					}
				}
				else {
					NavigationLink(value: route) { Label(item.title, systemImage: item.symbol) }
				}
			}
			.accessibilityIdentifier("desktop.destination.\(item.id)")
		}
	}

	struct DesktopSettingsButton: View {
		@Environment(\.openWindow) private var openWindow
		var body: some View {
			Button("Settings", systemImage: "gearshape") {
				AppWindowPresentation.open { openWindow(id: "appSettings") }
			}
			.buttonStyle(.plain)
			.frame(maxWidth: .infinity, alignment: .leading)
			.padding()
			.accessibilityIdentifier("desktop.destination.settings")
		}
	}

	struct DesktopSynchronizePage: View {
		let destination: String
		@Environment(AppState.self) private var appState
		var body: some View {
			Group {
				switch destination {
				case "remoteAccess": GatewaySettingsView()
				case "files": FoldersView()
				case "history":
					Form {
						Section("Previous Versions") {
							NavigationLink("Choose a File") { FoldersView() }
							Text("Choose Previous Versions from a file’s actions. Deleted files are available in its folder’s history.")
						}
						Section("History Sources") { NavigationLink("Manage History Sources") { HistorySourcesView() } }
					}
				case "activity":
					Form {
						Section("Transfers") {
							NavigationLink("Downloads") { DownloadsView() }
							NavigationLink("Uploads") { UploadsView() }
						}
						Section("Pending Changes and Errors") {
							NavigationLink("Recent Changes") { ChangesView(userSettings: appState.userSettings) }
						}
						Section("Recovery") {
							Text(
								"File-provider recovery is available in Files mode. Synchronization conflicts are kept beside their original files."
							)
						}
						Section("Activity Log") { NavigationLink("Activity Log") { LogView() } }
					}
				default: FoldersView()
				}
			}
			.formStyle(.grouped)
			.navigationTitle(DesktopUICatalog.desktop.destinations.first { $0.id == destination }?.title ?? "Overview")
		}
	}

	struct DesktopFilesWindow: View {
		@SceneStorage("desktop.files.selection") private var selection = "overview"
		@State private var navigation: DesktopFilesNavigation
		init(environment: RemoteEnvironment? = .shared) {
			_navigation = State(initialValue: DesktopFilesNavigation(environment: environment))
		}
		@SceneStorage("desktop.files.shortcutsExpanded") private var expanded = true
		private var coordinator = RemoteCoordinator.shared
		var body: some View {
			NavigationSplitView {
				List(selection: $selection) {
					ForEach(DesktopUICatalog.desktop.destinations.filter { $0.id != "settings" }) { item in
						Group {
							if item.id == "files" {
								DisclosureGroup(isExpanded: $expanded) {
									ForEach(
										coordinator.folders.sorted {
											$0.label.lowercased() == $1.label.lowercased()
												? $0.folderID < $1.folderID : $0.label.lowercased() < $1.label.lowercased()
										},
										id: \.folderID
									) { folder in
										Button {
											selection = "folder:" + folder.folderID
										} label: {
											Label(folder.label, systemImage: "folder")
										}.buttonStyle(.plain).tag("folder:" + folder.folderID)
									}
								} label: {
									Label(item.title, systemImage: item.symbol).tag(item.id)
								}
							}
							else {
								Label(item.title, systemImage: item.symbol).tag(item.id)
							}
						}
						.tag(item.id)
						.accessibilityIdentifier("desktop.destination.\(item.id)")
					}
				}
				.listStyle(.sidebar)
				.safeAreaInset(edge: .bottom) { DesktopSettingsButton() }
				.navigationSplitViewColumnWidth(min: 200, ideal: 230, max: 300)
			} detail: {
				NavigationStack(
					path: Binding(get: { navigation.paths[selection] ?? [] }, set: { navigation.paths[selection] = $0 })
				) {
					DesktopFilesPage(destination: selection)
						.navigationDestination(for: DesktopFilesDirectory.self) { directory in
							if let folder = coordinator.folders.first(where: { $0.folderID == directory.folderID }) {
								DesktopFilesBrowser(folder: folder, parentID: directory.parentID, title: directory.title)
							}
							else {
								ContentUnavailableView("Folder unavailable", systemImage: "folder")
							}
						}
				}.id(selection)
			}
			.environment(navigation)
			.frame(minWidth: 800, minHeight: 560)
		}
	}

	private struct DesktopFilesPage: View {
		let destination: String
		private var coordinator = RemoteCoordinator.shared
		var body: some View {
			Group {
				if destination.hasPrefix("folder:"),
					let folder = coordinator.folders.first(where: { "folder:" + $0.folderID == destination })
				{
					DesktopFilesBrowser(folder: folder, parentID: folder.rootItemID, title: folder.label)
				}
				else {
					switch destination {
					case "folders", "files": DesktopRemoteFolderList()
					case "remoteAccess": Form { RemoteGatewaysSection() }
					case "history":
						Form {
							Section("Previous Versions") { NavigationLink("Choose a File") { DesktopRemoteFolderList() } }
							Section("History Sources") { NavigationLink("Manage History Sources") { HistorySourcesView() } }
						}
					case "activity": DesktopFilesActivity()
					case "devices":
						ContentUnavailableView(
							"Devices are managed by each gateway", systemImage: "externaldrive",
							description: Text("Switch to Synchronize mode in General to manage local synchronization devices."))
					default:
						Form {
							Section("Status") {
								Text("Files mode")
								Text("\(coordinator.folders.count) folders from \(coordinator.gateways.count) gateways")
							}
							Section("Needs Attention") {
								Text("\(coordinator.operations.count) pending edits")
								CrashNoticeSection()
							}
							RemoteGatewaysSection()
							Section("Storage") { NavigationLink("Cache and Offline Storage") { DesktopFilesPreferencesView() } }
						}
					}
				}
			}
			.formStyle(.grouped)
			.navigationTitle(DesktopUICatalog.desktop.destinations.first { $0.id == destination }?.title ?? "Files")
			.refreshable { await coordinator.refresh() }
		}
	}

	private struct DesktopRemoteFolderList: View {
		private var coordinator = RemoteCoordinator.shared
		var body: some View {
			List {
				ForEach(
					coordinator.folders.sorted {
						$0.label.lowercased() == $1.label.lowercased()
							? $0.folderID < $1.folderID : $0.label.lowercased() < $1.label.lowercased()
					}, id: \.folderID
				) { folder in
					NavigationLink(
						value: DesktopFilesDirectory(folderID: folder.folderID, parentID: folder.rootItemID, title: folder.label)
					) {
						Label(folder.label, systemImage: "folder")
					}
				}
				NavigationLink("Choose Folders…") { Form { RemoteGatewaysSection() } }
			}
			.navigationTitle("Folders")
		}
	}
#endif
