// Copyright (C) 2024-2026 Tommy van der Vorst
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this file,
// You can obtain one at https://mozilla.org/MPL/2.0/.
import Foundation
import SwiftUI
import SushitrainCore
import SynctrainHostKit
import ServiceManagement

struct ConfigurationSettingsView: View {
	@Environment(AppState.self) private var appState
	@State private var showHomeDirectorySelector = false
	@State private var currentPath: URL? = nil
	@State private var showRestartAlert: Bool = false
	@State private var containerFolders = ContainerFolders()
	@State private var showSyncthingTakeover = false

	var body: some View {
		Form {
			Section {
				HStack {
					if let p = currentPath {
						Text(p.path(percentEncoded: false))
							.frame(maxWidth: .infinity, alignment: .leading)
							.multilineTextAlignment(.leading)
						Button(
							openInFilesAppLabel,
							systemImage: "arrow.up.forward.app",
							action: {
								openURLInSystemFilesApp(url: p)
							}
						).labelStyle(.iconOnly)
					}
					else {
						Text("(Default location)")
							.frame(maxWidth: .infinity, alignment: .leading)
							.multilineTextAlignment(.leading)
						Button(
							openInFilesAppLabel,
							systemImage: "arrow.up.forward.app",
							action: {
								let url = URL(
									fileURLWithPath: self.appState.client
										.currentConfigDirectory())
								openURLInSystemFilesApp(url: url)
							}
						).labelStyle(.iconOnly)
					}
				}

				Button("Select configuration folder...") {
					self.showHomeDirectorySelector = true
				}
				.buttonStyle(.link)

				if currentPath != nil {
					Button("Use default configuration location") {
						self.choose(nil)
					}
					.buttonStyle(.link)
				}
			} header: {
				Text("Configuration folder location")
			} footer: {
				Text(
					"The configuration folder contains the settings for the app, as well as the keys to communicate with other devices and bookkeeping of synchronized folders. By default, the configuration folder is managed by the app."
				)
			}

			ContainerFoldersSection(model: containerFolders)

			// Not after a takeover: the configuration is Syncthing's already, or a copy with its identity
			if SyncthingInstallation.hasConfiguration && EngineSetup.choice != .importSyncthing
				&& appState.configDirectory.standardizedFileURL.path
					!= SyncthingInstallation.home.standardizedFileURL.path
			{
				Section {
					Button("Take over Syncthing's setup...") {
						showSyncthingTakeover = true
					}
					.buttonStyle(.link)
				} header: {
					Text("Syncthing")
				} footer: {
					Text(
						"Synctrain can use the configuration of Syncthing.app on this Mac, keeping its device ID, folders and devices."
					)
				}
			}
		}
		.sheet(isPresented: $showSyncthingTakeover) {
			NavigationStack {
				SyncthingTakeoverView(decision: .offerTakeover, fromSettings: true)
					.toolbar {
						ToolbarItem(placement: .confirmationAction) {
							Button("Done") {
								showSyncthingTakeover = false
							}
						}
					}
			}
			.frame(minWidth: 520, minHeight: 560)
		}
		.formStyle(.grouped)
		.navigationTitle("Configuration settings")
		.fileImporter(isPresented: $showHomeDirectorySelector, allowedContentTypes: [.directory]) {
			result in
			switch result {
			case .success(let url):
				self.choose(url)
			case .failure(let err):
				Log.info("Failed to select home dir: \(err.localizedDescription)")
			}
		}
		.alert(
			"Configuration folder changed", isPresented: $showRestartAlert,
			actions: {
				if AppRelauncher.canRelaunch {
					Button("Restart the app") {
						AppRelauncher.relaunch()
					}
				}
				else {
					Button("Close the app") {
						AppRelauncher.quit()
					}
				}
			},
			message: {
				if AppRelauncher.canRelaunch {
					Text("The configuration folder was changed. The app restarts to use it.")
				}
				else {
					Text(
						"The path to the configuration folder was changed and will be used when the app is restarted. The app will now close."
					)
				}
			}
		)
		.task {
			self.updatePath()
			await containerFolders.update(appState)
		}
	}

	/// Remembers the chosen folder (nil for the default location): outside the sandbox by its path, in the sandbox
	/// with a security-scoped bookmark
	private func choose(_ url: URL?) {
		if FolderAccess.usesSecurityScope {
			var bookmark: Data? = nil
			if let url {
				_ = url.startAccessingSecurityScopedResource()
				guard let data = try? url.bookmarkData(options: [.withSecurityScope]) else {
					return
				}
				bookmark = data
				self.keepProxy(in: url)
			}
			UserDefaults.standard.setValue(bookmark, forKey: ConfigDirectorySetting.bookmarkKey)
		}
		else {
			self.keepProxy(in: url ?? EngineLocations.current.configDirectory)
			ConfigDirectorySetting.choose(url)
		}
		self.updatePath()
		self.showRestartAlert = true
	}

	/// The proxy is kept with the configuration, and must not end because the configuration moved
	private func keepProxy(in folder: URL) {
		do {
			try appState.client.copyProxySettings(folder.path(percentEncoded: false))
		}
		catch {
			Log.warn("Could not copy the proxy settings to \(folder): \(error.localizedDescription)")
		}
	}

	private func updatePath() {
		if !FolderAccess.usesSecurityScope {
			self.currentPath = ConfigDirectorySetting.path.map { URL(fileURLWithPath: $0, isDirectory: true) }
			return
		}
		var isStale: Bool = false
		if let bd = UserDefaults.standard.data(forKey: ConfigDirectorySetting.bookmarkKey),
			let url = try? URL(
				resolvingBookmarkData: bd, options: [.withSecurityScope, .withoutUI],
				bookmarkDataIsStale: &isStale), !isStale
		{
			self.currentPath = url
		}
		else {
			self.currentPath = nil
		}
	}
}

struct AdvancedSettingsView: View {
	@Environment(AppState.self) private var appState
	@ObservedObject var userSettings: AppUserSettings
	@State private var diskCacheSizeBytes: UInt? = nil
	@State private var showListeningAddresses = false
	@State private var showDiscoveryAddresses = false
	@State private var showSTUNAddresses = false
	@State private var folders: [SushitrainFolder] = []
	@State private var confirmClearThumbnailCache = false

	@State private var updateCounter = 0  // Used to force update after binding set... ugly

	@State private var showConfigurationSettings = false
	@State private var showTroubleshooting = false

	var body: some View {
		Form {
			Section {
				Toggle(
					"Listen for incoming connections",
					isOn: Binding(
						get: {
							return appState.client.isListening()
						},
						set: { listening in
							try? appState.client.setListening(listening)
							self.updateCounter += 1
						}))

				// Listening addresses popup sheet
				Button("Listening addresses...", systemImage: "envelope.front") {
					showListeningAddresses = true
				}
				.disabled(!appState.client.isListening())
				.buttonStyle(.link)
				.sheet(isPresented: $showListeningAddresses) {
					NavigationStack {
						AsyncAddressesView(addressType: .listening)
							.navigationTitle("Listening addresses")
							.toolbar {
								SheetButton(role: .save) {
									showListeningAddresses = false
								}
							}
					}
				}
			} header: {
				Text("Connectivity")
			} footer: {
				if appState.client.isListening() {
					Text(
						"Added devices can connect to this device on their initiative, while the app is running. This may cause additional battery drain. It is advisable to enable this only if you experience difficulty connecting to other devices."
					)
				}
				else {
					Text(
						"Connections to other devices can only be initiated by this device, not by the other added devices."
					)
				}
			}

			EngineAccessSection()

			Section {
				Toggle(
					"One connection is enough",
					isOn: Binding(
						get: {
							return appState.client.getEnoughConnections() == 1
						},
						set: { enough in
							// Fails while the engine restarts
							do {
								try appState.client.setEnoughConnections(enough ? 1 : 0)
							}
							catch {
								Log.warn("Could not change the connection setting: \(error.localizedDescription)")
							}
						}))
			} footer: {
				Text(
					"When this setting is enabled, the app will not attempt to connect to more devices after one connection has been established."
				)
			}

			Section("Discovery") {
				Toggle(
					"Announce on local networks",
					isOn: Binding(
						get: {
							return appState.client.isLocalAnnounceEnabled()
						},
						set: { nv in
							try? appState.client.setLocalAnnounceEnabled(nv)
							self.updateCounter += 1
						}))

				Toggle(
					"Announce LAN addresses",
					isOn: Binding(
						get: {
							return appState.client.isAnnounceLANAddressesEnabled()
						},
						set: { nv in
							try? appState.client.setAnnounceLANAddresses(nv)
							self.updateCounter += 1
						}))

				Toggle(
					"Announce globally",
					isOn: Binding(
						get: {
							return appState.client.isGlobalAnnounceEnabled()
						},
						set: { nv in
							try? appState.client.setGlobalAnnounceEnabled(nv)
							self.updateCounter += 1
						}))

				// Global announce addresses popup sheet
				Button("Global announce servers...", systemImage: "megaphone.fill") {
					showDiscoveryAddresses = true
				}
				.sheet(isPresented: $showDiscoveryAddresses) {
					NavigationStack {
						AsyncAddressesView(addressType: .discovery)
							.navigationTitle("Global announce servers")
							.toolbar {
								SheetButton(role: .save) {
									showDiscoveryAddresses = false
								}
							}
					}
				}
				.disabled(!appState.client.isGlobalAnnounceEnabled())
				.buttonStyle(.link)
			}

			Section("Network traversal") {
				Toggle(
					"Enable relaying",
					isOn: Binding(
						get: {
							return appState.client.isRelaysEnabled()
						},
						set: { nv in
							try? appState.client.setRelaysEnabled(nv)
							self.updateCounter += 1
						}))

				Toggle(
					"Enable NAT-PMP / UPnP",
					isOn: Binding(
						get: {
							return appState.client.isNATEnabled()
						},
						set: { nv in
							try? appState.client.setNATEnabled(nv)
							self.updateCounter += 1
						}))

				Toggle(
					"Enable STUN",
					isOn: Binding(
						get: {
							return appState.client.isSTUNEnabled()
						},
						set: { nv in
							try? appState.client.setSTUNEnabled(nv)
							self.updateCounter += 1
						}))

				// STUN server addresses popup sheet
				Button("STUN Servers...", systemImage: "arrow.trianglehead.swap") {
					showSTUNAddresses = true
				}
				.sheet(isPresented: $showSTUNAddresses) {
					NavigationStack {
						AsyncAddressesView(addressType: .stun)
							.navigationTitle("STUN servers")
							.toolbar {
								SheetButton(role: .save) {
									showSTUNAddresses = false
								}
							}
					}
				}
				.disabled(!appState.client.isSTUNEnabled())
				.buttonStyle(.link)
			}

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

			Section {
				NavigationLink("Engine options") {
					EngineOptionsView()
				}
			} footer: {
				Text("Disk space, performance, the audit log, and usage and crash reporting.")
			}

			Section {
				NavigationLink("Connection options") {
					ConnectionOptionsView()
				}
			} footer: {
				Text("Limits, priorities, reconnecting, local discovery and port mapping.")
			}

			Section {
				NavigationLink("Proxy") {
					ProxySettingsView()
				}
			} footer: {
				Text("A SOCKS5 or HTTP proxy for the engine's outgoing connections.")
			}

			Section {
				NavigationLink("Defaults") {
					DefaultsSettingsView()
				}
			} footer: {
				Text("What new folders and devices start with.")
			}

			Section {
				NavigationLink("Ignored devices") {
					IgnoredDevicesView()
				}
			} footer: {
				Text("Devices whose requests to connect you turned away.")
			}

			Section {
				NavigationLink("Advanced configuration") {
					AdvancedConfigurationView()
				}
			} footer: {
				Text("Every setting of the engine, for experts.")
			}

			Section {
				Button("Troubleshooting") {
					self.showTroubleshooting = true
				}
			}.buttonStyle(.link)
				.sheet(isPresented: $showTroubleshooting) {
					NavigationStack {
						TroubleshootingView(userSettings: userSettings)
							.toolbar {
								SheetButton(role: .done) {
									showTroubleshooting = false
								}
							}
					}
				}

			Section {
				Button("Configuration settings") {
					self.showConfigurationSettings = true
				}
			}.buttonStyle(.link)
				.sheet(isPresented: $showConfigurationSettings) {
					ConfigurationSettingsView()
						.toolbar {
							SheetButton(role: .done) {
								showConfigurationSettings = false
							}
						}
				}
		}
		.task {
			await self.update()
		}
		.onDisappear {
			appState.applySettings()
		}
		// This needs to be here to make sure the view refreshes after changing something
		.onChange(of: self.updateCounter) { _, _ in
			Log.info("Update counter changed")
		}
		.navigationTitle("Advanced settings")
		.formStyle(.grouped)
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
		var text = Text(
			"When the cache is enabled, thumbnails will load quicker and use less data when viewed more than once."
		)

		if let bytes = self.diskCacheSizeBytes {
			text =
				text
				+ Text(
					"Currently the thumbnail cache is using \(formatter.string(fromByteCount: Int64(bytes))) of disk space."
				)
		}

		if appState.userSettings.cacheThumbnailsToFolderID == "" {
			text =
				text + Text(" ")
				+ Text(
					"When disk space is scarce, the system may decide to remove some thumbnails in order to free up space."
				)
		}

		return text
	}
}

private struct BandwidthSettingsView: View {
	@Environment(AppState.self) private var appState
	@ObservedObject var userSettings: AppUserSettings

	@State private var downLimitMbitsPerSec: Int = 0
	@State private var upLimitMbitsPerSec: Int = 0
	@State private var isNetworkTrafficLowPriority = false

	private func update() {
		self.downLimitMbitsPerSec = appState.client.getBandwidthLimitDownMbitsPerSec()
		self.upLimitMbitsPerSec = appState.client.getBandwidthLimitUpMbitsPerSec()
		self.isNetworkTrafficLowPriority = appState.client.isNetworkTrafficLowPriority()
	}

	private func save() {
		// Fails while the engine restarts
		do {
			try appState.client.setBandwidthLimitsMbitsPerSec(downLimitMbitsPerSec, up: upLimitMbitsPerSec)
			try appState.client.setNetworkTrafficLowPriority(self.isNetworkTrafficLowPriority)
		}
		catch {
			Log.warn("Could not save the bandwidth settings: \(error.localizedDescription)")
		}
	}

	var body: some View {
		Form {
			Section("Limit file transfer bandwidth") {
				// Global down
				Toggle(
					"Limit receiving bandwidth",
					isOn: Binding(
						get: {
							return self.downLimitMbitsPerSec > 0
						},
						set: { self.downLimitMbitsPerSec = $0 ? 10 : 0 }
					))

				if downLimitMbitsPerSec > 0 {
					Stepper(
						"\(downLimitMbitsPerSec) Mbit/s",
						value: $downLimitMbitsPerSec, in: 1...1000)
				}

				// Global up
				Toggle(
					"Limit sending bandwidth",
					isOn: Binding(
						get: {
							return self.upLimitMbitsPerSec > 0
						},
						set: { self.upLimitMbitsPerSec = $0 ? 10 : 0 }
					))

				if upLimitMbitsPerSec > 0 {
					Stepper(
						"\(upLimitMbitsPerSec) Mbit/s",
						value: $upLimitMbitsPerSec, in: 1...1000)
				}

				// LAN bandwidth limit
				if upLimitMbitsPerSec > 0 || downLimitMbitsPerSec > 0 {
					Toggle(
						"Also limit in local networks",
						isOn: Binding(
							get: {
								return appState.client.isBandwidthLimitedInLAN()
							},
							set: { nv in
								try? appState.client.setBandwidthLimitedInLAN(nv)
							}))
				}
			}.task {
				self.update()
			}
			.onChange(of: [self.downLimitMbitsPerSec, self.upLimitMbitsPerSec]) { _, _ in
				self.save()
			}

			Section("Limit streaming") {
				Toggle(
					"Limit streaming bandwidth",
					isOn: Binding(
						get: {
							userSettings.streamingLimitMbitsPerSec > 0
						},
						set: { nv in
							if nv {
								userSettings.streamingLimitMbitsPerSec = 15
							}
							else {
								userSettings.streamingLimitMbitsPerSec = 0
							}
						}))

				if userSettings.streamingLimitMbitsPerSec > 0 {
					Stepper(
						"\(userSettings.streamingLimitMbitsPerSec) Mbit/s",
						value: userSettings.$streamingLimitMbitsPerSec, in: 1...100)
				}
			}

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

			Section {
				if deviceHasCellularCapability() {
					Toggle("On cellular networks", isOn: userSettings.$disableDevicesOnCellular)
				}

				Toggle("On metered networks", isOn: userSettings.$disableDevicesOnMetered)
				Toggle("In low power mode", isOn: userSettings.$disableDevicesInLowPowerMode)
			} header: {
				Text("Limit connectivity")
			} footer: {
				Text(
					"When the app detects that the system's internet connection is of the selected types, it will temporarily disable network connections to other devices. This will cause synchronization to be paused, and files that are not on this device yet cannot be streamed."
				)
			}

			Section {
				Toggle("Low priority traffic", isOn: $isNetworkTrafficLowPriority).onChange(of: self.isNetworkTrafficLowPriority) {
					_, _ in
					self.save()
				}
			} footer: {
				Text(
					"When this is enabled, other network traffic may be prioritized over this app's traffic by supported networks. This may help on congested networks."
				)
			}
		}
		.navigationTitle("Bandwidth limitations")
		.formStyle(.grouped)
	}
}

struct TabbedSettingsView: View {
	@Environment(AppState.self) private var appState
	@Binding var presence: AppPresence
	@State private var selection: String = "general"

	var body: some View {
		TabView(selection: $selection) {
			if appState.startupState == .started {
				Tab(
					value: "general",
					content: {
						GeneralSettingsView(userSettings: appState.userSettings, presence: $presence)
					},
					label: {
						Label("General", systemImage: "app.badge.checkmark.fill")
					})

				Tab(
					value: "bandwidth",
					content: {
						BandwidthSettingsView(userSettings: appState.userSettings)
					},
					label: {
						Label("Bandwidth", systemImage: "tachometer")
					})

				Tab(
					value: "gateway",
					content: {
						GatewaySettingsView()
					},
					label: {
						Label("Remote file service", systemImage: "externaldrive.connected.to.line.below")
					})

				Tab(
					value: "photo",
					content: {
						PhotoBackupSettingsView(photoBackup: appState.photoBackup)
					},
					label: {
						Label("Photo back-up", systemImage: "photo")
					})

				Tab(
					value: "advanced",
					content: {
						AdvancedSettingsView(userSettings: appState.userSettings)
					},
					label: {
						Label("Advanced", systemImage: "gear")
					})
			}
		}
		.frame(minWidth: 500, minHeight: 450)
		.windowResizeBehavior(.automatic)
		.formStyle(.grouped)
	}
}

struct GeneralSettingsView: View {
	@Environment(AppState.self) private var appState
	@ObservedObject var userSettings: AppUserSettings
	@Binding var presence: AppPresence

	var body: some View {
		Form {
			Section {
				TextField(
					"Device name",
					text: Binding(
						get: {
							var err: NSError? = nil
							return appState.client.getName(&err)
						},
						set: { nn in
							try? appState.client.setName(nn)
						}))
			}

			Section {
				AppPresenceToggles(presence: $presence)
			}

			Section {
				Picker("Folder access from menu", selection: userSettings.$menuFolderAction) {
					Text("Do not show folders").tag(MenuFolderAction.hide)
					Text("Open in Finder").tag(MenuFolderAction.finder)
					Text("Open in the app").tag(MenuFolderAction.browser)
				}.disabled(!presence.inMenuBar)
			}

			Section {
				AutoStartToggleView()
			}

			Section("View settings") {
				ViewSettingsView(userSettings: appState.userSettings)
			}

			Section {
				Button("Switch to Files in Finder…") {
					ModeSwitch.askToSwitchToFilesInFinder()
				}
			} footer: {
				Text(
					"This Mac then runs no engine. Finder shows the folders of a gateway on another system, and keeps what you open."
				)
			}
		}
	}
}

struct ViewSettingsView: View {
	@ObservedObject var userSettings: AppUserSettings

	var body: some View {
		Form {
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
		.navigationTitle("View settings")
	}
}

private struct AsyncAddressesView: View {
	@Environment(AppState.self) var appState

	let addressType: AddressType

	@State private var loading = true
	@State private var addresses: [String] = []

	var body: some View {
		ZStack {
			if self.loading {
				ProgressView()
			}
			else {
				AddressesView(
					addresses: addresses,
					onChange: {
						if !self.loading {
							self.addresses = $0
							self.write()
						}
					},
					addressType: self.addressType
				)
			}
		}
		.task {
			await self.update()
		}
		.onDisappear {
			self.write()
		}
	}

	private func update() async {
		self.loading = true
		let client = self.appState.client
		let addressType = self.addressType
		self.addresses = await Task.detached {
			switch addressType {
			case .discovery:
				return client.discoveryAddresses()?.asArray() ?? []
			case .listening:
				return client.listenAddresses()?.asArray() ?? []
			case .device:
				// not supported
				return []
			case .stun:
				return client.stunAddresses()?.asArray() ?? []
			}
		}.value
		self.addresses.sort()
		Log.info("Loaded addresses \(self.addresses)")
		self.loading = false
	}

	private func write() {
		let addresses = self.addresses
		let appState = self.appState
		Task.detached {
			do {
				switch addressType {
				case .discovery:
					try appState.client.setDiscoveryAddresses(
						SushitrainListOfStrings.from(addresses))
				case .listening:
					Log.info("Writing listening addresses: \(addresses)")
					try appState.client.setListenAddresses(SushitrainListOfStrings.from(addresses))
				case .device:
					// not supported
					abort()
				case .stun:
					try appState.client.setStunAddresses(
						SushitrainListOfStrings.from(addresses))
				}
			}
			catch {
				Log.warn("Failed to write \(self.addressType) addresses: \(error)")
			}
		}
	}
}

struct AutoStartToggleView: View {
	@State private var autoStart: Bool = false

	var body: some View {
		Toggle(isOn: $autoStart) {
			Label("Launch at login", systemImage: "autostartstop")
		}
		.onChange(of: autoStart) { _, nv in
			if nv {
				try? SMAppService.mainApp.register()
			}
			else {
				try? SMAppService.mainApp.unregister()
			}
			self.update()
		}
		.onAppear {
			self.update()
		}
	}

	private func update() {
		self.autoStart = SMAppService.mainApp.status == .enabled
	}
}
