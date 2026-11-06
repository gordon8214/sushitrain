// Copyright (C) 2026 Gordon. SPDX-License-Identifier: MPL-2.0
#if os(macOS)
	import SwiftUI
	import SushitrainCore
	import SynctrainHostKit

	struct DesktopSettingsCategoryView: View {
		let category: MacSettingsCategory
		let mode: MacSettingsMode
		@Binding var presence: AppPresence
		let appState: AppState?

		var body: some View {
			Form {
				if [.filesAndStorage, .network, .synchronization, .defaults, .advanced].contains(category) {
					Section {
						ForEach(category.descriptor.sections) { section in
							DesktopSettingsSection(section: section, mode: mode, presence: $presence, appState: appState, grouped: true)
						}
					}
				}
				else {
					ForEach(category.descriptor.sections) { section in
						DesktopSettingsSection(section: section, mode: mode, presence: $presence, appState: appState)
					}
				}
			}
			.formStyle(.grouped)
			.navigationTitle(Text(category.title))
		}
	}

	private struct DesktopSettingsSection: View {
		let section: DesktopUICatalog.Section
		let mode: MacSettingsMode
		@Binding var presence: AppPresence
		let appState: AppState?
		var grouped = false
		@State private var name = ""
		@State private var savedName = ""
		@State private var error: ErrorMessage?

		var body: some View {
			Group {
				if grouped {
					entry
				}
				else {
					Section {
						entry
					} header: {
						if !["mcp", "support", "licenses"].contains(section.action) { Text(section.title) }
					}
				}
			}
			.settingsEditor(hasChanges: section.action == "name" && name != savedName)
			.errorAlert($error)
		}

		@ViewBuilder private var entry: some View {
			if section.capability == "synchronize", let appState,
				appState.startupState != .started && !appState.isEngineRestarting
			{
				unavailable("Start the engine to change these settings.")
			}
			else if !section.isAvailable(mode: mode.capabilityMode) {
				unavailable(
					section.capability == "files"
						? "Available in Files mode. Change the operating mode in General."
						: "Available in Synchronize mode. Change the operating mode in General.")
			}
			else {
				switch section.action {
				case "name":
					if let appState {
						HStack {
							TextField("Device name", text: $name)
							Button("Save") {
								do {
									try appState.client.setName(name)
									savedName = name
								}
								catch { self.error = ErrorMessage(error) }
							}
						}
						.task {
							var problem: NSError?
							name = appState.client.getName(&problem)
							savedName = name
						}
					}
				case "mode":
					LabeledContent("Current mode", value: mode == .synchronize ? "Synchronize" : "Files")
					if mode == .synchronize {
						Button("Switch to Files…") { ModeSwitch.askToSwitchToFilesInFinder() }
					}
					else {
						SwitchToSynchronizeButton()
					}
				case "startup": AutoStartToggleView()
				case "presence": AppPresenceToggles(presence: $presence)
				case "fileManager":
					FinderIntegrationSection()
					if mode == .filesInFinder {
						GuardedSettingsLink("Finder Location") { Form { FilesLocationSettingsView() } }
					}
				case "menuAccess":
					if let appState {
						DesktopMenuAccessView(userSettings: appState.userSettings)
					}
					else {
						Text("Open Synctrain or Show in Finder from the menu bar.")
					}
				case "mcp": GuardedSettingsLink("MCP") { MCPSettingsView() }
				case "versions":
					LabeledContent(
						"App version", value: Bundle.main.object(forInfoDictionaryKey: "CFBundleShortVersionString") as? String ?? "")
					if appState != nil { Text("Engine: \(SushitrainVersion())") }
				case "support", "licenses": GuardedSettingsLink(title: Text(section.title)) { AboutView(showsIntroduction: false) }
				default:
					GuardedSettingsLink(title: Text(section.title)) {
						DesktopSettingsEditor(action: section.action, appState: appState)
					}
				}
			}
		}

		private func unavailable(_ reason: LocalizedStringKey) -> some View {
			VStack(alignment: .leading, spacing: 4) {
				Text(section.title)
				Text(reason).font(.callout).foregroundStyle(.secondary)
					.fixedSize(horizontal: false, vertical: true)
			}
			.accessibilityElement(children: .combine)
		}
	}

	private struct DesktopMenuAccessView: View {
		@ObservedObject var userSettings: AppUserSettings
		var body: some View {
			Picker("Folder access from menu", selection: userSettings.$menuFolderAction) {
				Text("Do not show folders").tag(MenuFolderAction.hide)
				Text("Open in Finder").tag(MenuFolderAction.finder)
				Text("Open in the app").tag(MenuFolderAction.browser)
			}
		}
	}

	struct DesktopSettingsEditor: View {
		let action: String
		let appState: AppState?
		var body: some View {
			Group {
				if let appState {
					switch action {
					case "previews": FilesPreviewSettingsView(userSettings: appState.userSettings)
					case "locations": FoldersView()
					case "basic/options": NetworkSettingsView(userSettings: appState.userSettings)
					case "connections": ConnectionOptionsView()
					case "proxy": ProxySettingsView()
					case "bandwidth": BandwidthSettingsView(userSettings: appState.userSettings)
					case "policy": DesktopPauseSettingsView(userSettings: appState.userSettings)
					case "resources": Form { ScanningPrioritySection() }
					case "engine": EngineOptionsView()
					case "defaults/folder": DefaultFolderSettingsView()
					case "reset/defaults/folder": FolderSyncResetSettingsView()
					case "defaults/versioning": DefaultVersioningSettingsView()
					case "defaults/device": DefaultDeviceSettingsView()
					case "defaults/ignores": DefaultIgnoresView()
					case "gui": Form { EngineAccessSection() }
					case "configuration": ConfigurationSettingsView()
					case "advanced": AdvancedConfigurationView()
					case "logs": TroubleshootingView(userSettings: appState.userSettings)
					default: Text("This setting is unavailable while the engine is stopped.")
					}
				}
				else {
					switch action {
					case "previews", "storage": DesktopFilesPreferencesView()
					case "locations": Form { FilesLocationSettingsView() }
					default: Text("Available in Synchronize mode.")
					}
				}
			}
		}
	}

	private struct DesktopPauseSettingsView: View {
		@ObservedObject var userSettings: AppUserSettings
		var body: some View {
			Form {
				Toggle("On metered networks", isOn: userSettings.$disableDevicesOnMetered)
				if deviceHasCellularCapability() { Toggle("On cellular networks", isOn: userSettings.$disableDevicesOnCellular) }
				Toggle("In low power mode", isOn: userSettings.$disableDevicesInLowPowerMode)
			}
			.navigationTitle("Automatic Pausing")
		}
	}

	struct DesktopFilesPreferencesView: View {
		@AppStorage("hideDotFiles") private var hideDotFiles = true
		@AppStorage(CacheSettings.storageKey) private var limit = CacheSettings.defaultLimit
		var body: some View {
			Form {
				Section("Browsing") { Toggle("Hide dotfiles", isOn: $hideDotFiles) }
				Section("Previews and Thumbnails") {
					Text(
						"Finder and Quick Look provide previews and thumbnails using the system’s file types. Previewing a file can download its contents."
					)
				}
				Section("Download Cache") {
					Picker("Cache budget", selection: $limit) {
						ForEach(CacheSettings.choices, id: \.self) { Text(Int64($0), format: .byteCount(style: .file)).tag($0) }
					}
					Text("Kept files and pending edits are retained independently of the disposable cache.")
				}
			}
			.onChange(of: limit) { _, limit in
				guard let storage = RemoteEnvironment.shared?.storage else { return }
				Task.detached(priority: .utility) { storage.trimContent(toBytes: Int64(limit)) }
			}
			.navigationTitle("Files & Storage")
		}
	}
#endif
