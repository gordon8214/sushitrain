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
			.id(section.action)
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
							TextField("Device name", text: $name).settingsAnchor("name")
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

	struct DesktopSettingsSearchDestination: View {
		let entry: SettingSearchEntry
		let mode: MacSettingsMode
		@Binding var presence: AppPresence
		let appState: AppState?
		private var destination: SettingSearchEntry.MacDestination {
			entry.macDestination(
				usesLDAP: entry.action == "gui" && appState?.client.guiSettings()?.authMode == "ldap",
				hasTransferLimit: entry.field != "limitBandwidthInLan"
					|| (appState?.client.getBandwidthLimitDownMbitsPerSec() ?? 0) > 0
					|| (appState?.client.getBandwidthLimitUpMbitsPerSec() ?? 0) > 0)
		}
		var body: some View {
			Group {
				if !entry.isAvailable(mode: mode.capabilityMode) {
					ContentUnavailableView(
						entry.localizedTitle, systemImage: "gearshape",
						description: Text("Available in \(entry.capability) mode. Change the operating mode in General."))
				}
				else if let section = entry.section, destination == .expertEditor {
					AdvancedSectionView(section: section, title: Text(entry.localizedTitle))
						.environment(\.settingsSearchAnchor, entry.field)
				}
				else if destination == .guiEditor {
					GUISettingsView()
				}
				else if destination == .filesLocation {
					Form { FilesLocationSettingsView() }
				}
				else if let section = entry.section, let appState {
					if section.hasPrefix("folder/"), !entry.action.hasPrefix("advanced/") {
						let folder = appState.client.folder(withID: String(section.dropFirst(7)))
						if let folder, folder.exists() {
							if [
								"syncXattrs", "sendXattrs", "syncOwnership", "sendOwnership", "copyOwnershipFromParent", "ignorePerms",
								"xattrFilter",
							].contains(where: { entry.field?.hasPrefix($0) == true }) {
								FolderMetadataSettingsView(folder: folder)
							}
							else {
								FolderSyncSettingsView(folder: folder)
							}
						}
						else {
							ContentUnavailableView("This folder no longer exists", systemImage: "folder")
						}
					}
					else if section.hasPrefix("device/"), !entry.action.hasPrefix("advanced/") {
						let id = String(section.dropFirst(7))
						if let device = appState.client.peer(withID: id), appState.client.peers()?.asArray().contains(id) == true {
							DeviceSettingsView(device: device)
						}
						else {
							ContentUnavailableView("This device no longer exists", systemImage: "externaldrive")
						}
					}
					else if section == "defaults/folder",
						[
							"syncXattrs", "sendXattrs", "syncOwnership", "sendOwnership", "copyOwnershipFromParent", "ignorePerms",
							"xattrFilter",
						].contains(where: { entry.field?.hasPrefix($0) == true })
					{
						DefaultFolderMetadataView()
					}
					else {
						DesktopSettingsEditor(action: entry.action, appState: appState)
					}
				}
				else if entry.action == "remoteService" {
					GatewaySettingsView()
				}
				else if entry.action == "gateways" {
					RemoteFoldersSettingsView()
				}
				else if entry.action == "historySources" {
					HistorySourcesView()
				}
				else if let category = MacSettingsCategory(rawValue: entry.category) {
					if [
						"previews", "storage", "basic/options", "connections", "proxy", "bandwidth", "policy", "resources", "engine",
						"gui", "configuration", "advanced", "logs", "mcp",
					].contains(entry.action)
						|| entry.action.hasPrefix("defaults/") || entry.action.hasPrefix("reset/")
					{
						if entry.action == "mcp" {
							MCPSettingsView()
						}
						else {
							DesktopSettingsEditor(action: entry.action, appState: appState)
						}
					}
					else {
						DesktopSettingsCategoryView(category: category, mode: mode, presence: $presence, appState: appState)
					}
				}
			}
			.navigationTitle(entry.localizedTitle)
		}
	}

	private struct DesktopMenuAccessView: View {
		@ObservedObject var userSettings: AppUserSettings
		var body: some View {
			Picker("Folder access from menu", selection: userSettings.$menuFolderAction) {
				Text("Do not show folders").tag(MenuFolderAction.hide)
				Text("Open in Finder").tag(MenuFolderAction.finder)
				Text("Open in the app").tag(MenuFolderAction.browser)
			}.settingsAnchor("menuFolderAction")
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
				Toggle("On metered networks", isOn: userSettings.$disableDevicesOnMetered).settingsAnchor("disableDevicesOnMetered")
				if deviceHasCellularCapability() {
					Toggle("On cellular networks", isOn: userSettings.$disableDevicesOnCellular).settingsAnchor(
						"disableDevicesOnCellular")
				}
				Toggle("In low power mode", isOn: userSettings.$disableDevicesInLowPowerMode).settingsAnchor(
					"disableDevicesInLowPowerMode")
			}
			.navigationTitle("Automatic Pausing")
		}
	}

	struct DesktopFilesPreferencesView: View {
		@AppStorage("hideDotFiles") private var hideDotFiles = true
		@AppStorage(CacheSettings.storageKey) private var limit = CacheSettings.defaultLimit
		var body: some View {
			Form {
				Section("Browsing") { Toggle("Hide dotfiles", isOn: $hideDotFiles).settingsAnchor("hideDotFiles") }
				Section("Previews and Thumbnails") {
					Text(
						"Finder and Quick Look provide previews and thumbnails using the system’s file types. Previewing a file can download its contents."
					)
				}
				Section("Download Cache") {
					Picker("Cache budget", selection: $limit) {
						ForEach(CacheSettings.choices, id: \.self) { Text(Int64($0), format: .byteCount(style: .file)).tag($0) }
					}.settingsAnchor("cacheLimit")
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
