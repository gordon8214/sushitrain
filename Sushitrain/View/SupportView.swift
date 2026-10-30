// Copyright (C) 2024-2025 Tommy van der Vorst
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this file,
// You can obtain one at https://mozilla.org/MPL/2.0/.
import SwiftUI
@preconcurrency import SushitrainCore
import SynctrainHostKit

private struct AppSupportBundle: Encodable {
	enum PrefValue: Encodable {
		case string(String)
		case int(Int)
		case double(Double)
		case boolean(Bool)
		case list([PrefValue])
		case dictionary([String: PrefValue])
		case null
		case data(Data)

		init(_ v: Any) {
			if let b = v as? Bool {
				self = .boolean(b)
			}
			else if let s = v as? String {
				self = .string(s)
			}
			else if let d = v as? Double {
				self = .double(d)
			}
			else if let n = v as? Int {
				self = .int(n)
			}
			else if let b = v as? [Any] {
				self = .list(b.map(PrefValue.init))
			}
			else if let d = v as? [String: Any] {
				self = .dictionary(PrefValue.from(d))
			}
			else if let d = v as? Data {
				self = .data(d)
			}
			else {
				self = .null
			}
		}

		func encode(to encoder: any Encoder) throws {
			switch self {
			case .string(let s): try s.encode(to: encoder)
			case .int(let s): try s.encode(to: encoder)
			case .double(let s): try s.encode(to: encoder)
			case .boolean(let s): try s.encode(to: encoder)
			case .list(let s): try s.encode(to: encoder)
			case .dictionary(let s): try s.encode(to: encoder)
			case .data(let s):
				if let utf = String(data: s, encoding: .utf8) {
					try utf.encode(to: encoder)
				}
				else {
					try s.base64EncodedString().encode(to: encoder)
				}
			case .null: return
			}
		}

		static func from(_ values: [String: Any]) -> [String: PrefValue] {
			var out: [String: PrefValue] = [:]
			for (k, v) in values {
				out[k] = PrefValue(v)
			}
			return out
		}
	}

	var appVersion: String?
	var bundleIdentifier: String?
	var bundlePath: String?
	var userSettings: [String: PrefValue]
	var secondsSinceLaunch: Double?
	var isUsingCustomConfiguration: Bool
	var osName: String
	var osVersion: String
}

extension AppState {
	fileprivate func generateAppSupportBundle() -> AppSupportBundle {
		let mainBundle = Bundle.main

		UserDefaults.standard.dictionaryRepresentation()
		return AppSupportBundle(
			appVersion: mainBundle.buildVersionNumber,
			bundleIdentifier: mainBundle.bundleIdentifier,
			bundlePath: mainBundle.bundlePath,
			userSettings: AppSupportBundle.PrefValue.from(
				UserDefaults.standard.persistentDomain(forName: mainBundle.bundleIdentifier!) ?? [:]),
			secondsSinceLaunch: Date.now.timeIntervalSince(self.launchedAt),
			isUsingCustomConfiguration: self.client.isUsingCustomConfiguration,
			osName: self.osName,
			osVersion: self.osVersion
		)
	}

	private var osVersion: String {
		#if os(iOS)
			return UIDevice.current.systemVersion
		#else
			let ver = ProcessInfo.processInfo.operatingSystemVersion
			return "\(ver.majorVersion).\(ver.minorVersion).\(ver.patchVersion)"
		#endif
	}

	private var osName: String {
		#if os(iOS)
			return UIDevice.current.systemName
		#else
			return "macOS"
		#endif
	}
}

/// A log level, as the core reports it (slog's numbers)
private enum LogLevel: Int, CaseIterable, Identifiable {
	case debug = -4
	case info = 0
	case warning = 4
	case error = 8

	var id: Int { self.rawValue }

	/// The named level that includes any other number, as Syncthing labels it
	init(slogLevel: Int) {
		switch slogLevel {
		case ..<0: self = .debug
		case ..<4: self = .info
		case ..<8: self = .warning
		default: self = .error
		}
	}

	var label: LocalizedStringKey {
		switch self {
		case .debug: return "Debug"
		case .info: return "Info"
		case .warning: return "Warning"
		case .error: return "Error"
		}
	}

	/// As Syncthing writes it in log lines
	var abbreviation: String {
		switch self {
		case .debug: return "DBG"
		case .info: return "INF"
		case .warning: return "WRN"
		case .error: return "ERR"
		}
	}
}

/// A package whose log level can be set on its own, as `SushitrainClient.logFacilitiesJSON` returns it
private struct LogFacility: Decodable, Identifiable {
	let name: String
	let description: String
	let level: Int

	var id: String { self.name }
}

/// A logged message, as `SushitrainClient.logLinesJSON` returns it
private struct LogLine: Decodable {
	let when: Int64  // nanoseconds since 1970
	let level: Int
	let facility: String
	let message: String
	/// The line as the log shows it, formatted once
	let formatted: String

	private enum CodingKeys: String, CodingKey {
		case when, level, facility, message
	}

	private static let timeFormatter: DateFormatter = {
		let formatter = DateFormatter()
		formatter.locale = Locale(identifier: "en_US_POSIX")
		formatter.dateFormat = "HH:mm:ss.SSS"
		return formatter
	}()

	init(from decoder: any Decoder) throws {
		let container = try decoder.container(keyedBy: CodingKeys.self)
		self.when = try container.decode(Int64.self, forKey: .when)
		self.level = try container.decode(Int.self, forKey: .level)
		self.facility = try container.decode(String.self, forKey: .facility)
		self.message = try container.decode(String.self, forKey: .message)
		let date = Date(timeIntervalSince1970: Double(self.when) / 1_000_000_000.0)
		self.formatted =
			"\(Self.timeFormatter.string(from: date)) \(LogLevel(slogLevel: self.level).abbreviation) \(self.message)"
	}
}

extension SushitrainClient {
	fileprivate func logFacilities() -> [LogFacility] {
		do {
			return try JSONDecoder().decode([LogFacility].self, from: self.logFacilitiesJSON())
		}
		catch {
			Log.warn("Cannot read log facilities: \(error.localizedDescription)")
			return []
		}
	}

	/// The lines logged after the given time (in nanoseconds since 1970, or 0 for all of them)
	fileprivate func logLines(after: Int64) -> [LogLine] {
		do {
			return try JSONDecoder().decode([LogLine].self, from: self.logLinesJSON(after))
		}
		catch {
			Log.warn("Cannot read log lines: \(error.localizedDescription)")
			return []
		}
	}
}

private struct LogView: View {
	/// A line as the view lists it
	private struct Row: Identifiable {
		let id: Int
		let line: LogLine
	}

	/// As many lines as the core keeps
	private static let maxRows = 1000

	@Environment(AppState.self) private var appState
	@State private var rows: [Row] = []
	@State private var hasLoaded = false
	@State private var isPaused = false
	@State private var facilityNames: [String] = []
	@State private var facility: String? = nil

	private var shownRows: [Row] {
		guard let facility = self.facility else {
			return self.rows
		}
		return self.rows.filter { $0.line.facility == facility }
	}

	private static var controlsPlacement: ToolbarItemPlacement {
		#if os(iOS)
			return .bottomBar
		#else
			return .automatic
		#endif
	}

	var body: some View {
		let shownRows = self.shownRows
		ScrollView {
			LazyVStack(alignment: .leading, spacing: 0) {
				ForEach(shownRows) { row in
					Text(row.line.formatted)
						.frame(maxWidth: .infinity, alignment: .leading)
				}
			}
			.textSelection(.enabled)
			.monospaced()
			.padding()
		}
		.defaultScrollAnchor(.bottom)
		.overlay {
			if self.hasLoaded && shownRows.isEmpty {
				ContentUnavailableView("No log messages", systemImage: "heart.text.clipboard")
			}
		}
		.navigationTitle("Log")
		#if os(iOS)
			.navigationBarTitleDisplayMode(.inline)
		#endif
		.toolbar {
			ToolbarItem {
				Picker("Facility", selection: $facility) {
					Text("All facilities").tag(String?.none)
					ForEach(self.facilityNames, id: \.self) { name in
						Text(name).tag(String?.some(name))
					}
				}
				.pickerStyle(.menu)
			}

			ToolbarItemGroup(placement: Self.controlsPlacement) {
				Button(self.isPaused ? "Resume" : "Pause", systemImage: self.isPaused ? "play" : "pause") {
					self.isPaused.toggle()
				}

				Button("Copy", systemImage: "doc.on.doc") {
					writeTextToPasteboard(shownRows.map(\.line.formatted).joined(separator: "\n"))
				}
				.disabled(shownRows.isEmpty)
			}
		}
		.task {
			await self.follow()
		}
	}

	/// Adds lines as they are logged, until the view disappears. The core is called off the main actor, and returns only
	/// the lines after the last one shown.
	private func follow() async {
		let client = appState.client
		let registered = await Task.detached { client.logFacilities().map(\.name) }.value
		self.addFacilityNames(registered)
		while !Task.isCancelled {
			if !self.isPaused {
				let after = self.rows.last?.line.when ?? 0
				let lines = await Task.detached { client.logLines(after: after) }.value
				self.append(lines)
				self.hasLoaded = true
			}
			try? await Task.sleep(for: .seconds(2))
		}
	}

	private func append(_ lines: [LogLine]) {
		guard !lines.isEmpty else {
			return
		}
		var rows = self.rows
		var nextID = (rows.last?.id ?? -1) + 1
		for line in lines {
			rows.append(Row(id: nextID, line: line))
			nextID += 1
		}
		if rows.count > Self.maxRows {
			rows.removeFirst(rows.count - Self.maxRows)
		}
		self.rows = rows
		self.addFacilityNames(lines.map(\.facility))
	}

	/// Adds facilities to the filter, which keeps those whose lines are gone, so that the one shown stays listed
	private func addFacilityNames(_ names: [String]) {
		var all = Set(self.facilityNames)
		all.formUnion(names.filter { !$0.isEmpty })
		let sorted = all.sorted()
		if sorted != self.facilityNames {
			self.facilityNames = sorted
		}
	}
}

/// Shows the level of each log facility, which the user can change while the app runs
private struct LogLevelsView: View {
	@ObservedObject var userSettings: AppUserSettings
	@Environment(AppState.self) private var appState
	@State private var facilities: [LogFacility] = []

	private var hasChangedLevels: Bool {
		let defaultLevel = appState.client.defaultLogLevel()
		return self.facilities.contains { $0.level != defaultLevel }
	}

	var body: some View {
		Form {
			Section {
				ForEach(self.facilities) { facility in
					Picker(selection: self.levelBinding(facility)) {
						ForEach(LogLevel.allCases) { level in
							Text(level.label).tag(level)
						}
					} label: {
						VStack(alignment: .leading) {
							Text(facility.name)
							Text(facility.description).font(.caption).foregroundStyle(.secondary)
						}
					}
				}
			} footer: {
				Text(
					"Each facility logs messages at its level and above. A change takes effect at once, and the messages show in the log. Debug messages slow down the app and use more battery."
				)
			}

			Section {
				Toggle("Keep levels after restarting", isOn: userSettings.$keepLogLevels)

				Button("Reset to defaults") {
					appState.client.resetLogLevels()
					self.update()
				}
				.disabled(!self.hasChangedLevels)
				#if os(macOS)
					.buttonStyle(.link)
				#endif
			} footer: {
				Text("Unless you keep them, levels return to their defaults when the app restarts.")
			}
		}
		#if os(macOS)
			.formStyle(.grouped)
		#endif
		.navigationTitle("Log levels")
		#if os(iOS)
			.navigationBarTitleDisplayMode(.inline)
		#endif
		.task {
			self.update()
		}
		.onChange(of: userSettings.keepLogLevels) { _, _ in
			self.saveKeptLevels()
		}
	}

	private func levelBinding(_ facility: LogFacility) -> Binding<LogLevel> {
		return Binding(
			get: { LogLevel(slogLevel: facility.level) },
			set: { level in
				appState.client.setLogLevel(facility.name, level: level.rawValue)
				self.update()
			})
	}

	private func update() {
		self.facilities = appState.client.logFacilities()
		self.saveKeptLevels()
	}

	private func saveKeptLevels() {
		let kept = userSettings.keepLogLevels ? appState.client.changedLogLevels() : ""
		if kept != userSettings.keptLogLevels {
			userSettings.keptLogLevels = kept
		}
	}
}

private struct SupportBundleView: View {
	@Environment(AppState.self) private var appState
	@State private var writingSupportBundle: Bool = false
	@State private var supportBundle: URL? = nil
	@State private var supportBundleDirectory: URL? = nil

	#if os(macOS)
		@State private var showSaveSupportBundle: Bool = false
	#endif

	var body: some View {
		Section {
			Text(
				"To allow others to assist you in troubleshooting, you can send them a support bundle. This bundle contains technical information about the app. **Note: while the app will redact user names, IP addresses and device IDs in the bundle, it may still contain personally identifiable information, such as folder names. Be sure to review the contents of the bundle before sending it to others.**"
			)
			.fixedSize(horizontal: false, vertical: true)
			.onDisappear {
				if let directory = self.supportBundleDirectory {
					try? FileManager.default.removeItem(at: directory)
				}
				self.supportBundleDirectory = nil
				self.supportBundle = nil
			}
			#if os(macOS)
				.fileImporter(isPresented: $showSaveSupportBundle, allowedContentTypes: [.directory]) { res in
					switch res {
					case .failure(let e):
						Log.warn("error selecting path: \(e)")
					case .success(let url):
						if !url.startAccessingSecurityScopedResource() {
							Log.warn("Could not start accessing folder to export support bundle to")
						}
						defer { url.stopAccessingSecurityScopedResource() }

						if let sbURL = self.supportBundle {
							let targetURL = url.appendingPathComponent(sbURL.lastPathComponent, isDirectory: false)
							Log.info("Copying support bundle  to URL \(targetURL)")
							try? FileManager.default.copyItem(at: sbURL, to: targetURL)
						}
					}
				}
			#endif

			if let s = supportBundle {
				ShareLink(item: s, subject: Text("Support bundle"), message: Text("Support bundle"), preview: self.sharePreview()) {
					Label("Share information bundle for support", systemImage: "text.page.badge.magnifyingglass")
				}
				#if os(macOS)
					.buttonStyle(.link)
				#endif

				#if os(macOS)
					Button("Save support bundle", systemImage: "text.page.badge.magnifyingglass") {
						showSaveSupportBundle = true
					}
					.buttonStyle(.link)
				#endif
			}
			else {
				Button("Generate support bundle", systemImage: "text.page.badge.magnifyingglass") {
					Task.detached {
						await self.generateSupportBundle()
					}
				}.disabled(writingSupportBundle)
					#if os(macOS)
						.buttonStyle(.link)
					#endif
			}
		}
	}

	private func generateSupportBundle() async {
		if self.supportBundle == nil && !self.writingSupportBundle {
			self.writingSupportBundle = true
			var tempDir: URL? = nil
			do {
				// Generate app-side bundle
				let appBundle = appState.generateAppSupportBundle()
				let encoder = JSONEncoder()
				encoder.outputFormatting = [.prettyPrinted, .withoutEscapingSlashes]
				let appBundleJSON = try encoder.encode(appBundle)

				let directory = try AppLocations.current.makeTemporaryDirectory(prefix: "SupportBundle-")
				tempDir = directory
				let tempPath = directory.appending(
					component: "Synctrain-support-bundle-\(ProcessInfo().globallyUniqueString).zip")
				try self.appState.client.writeSupportBundle(tempPath.path(percentEncoded: false), appInfo: appBundleJSON)

				DispatchQueue.main.async {
					self.writingSupportBundle = false
					self.supportBundle = tempPath
					self.supportBundleDirectory = directory
				}
			}
			catch {
				Log.warn("cannot write support bundle: \(error.localizedDescription)")
				if let tempDir {
					try? FileManager.default.removeItem(at: tempDir)
				}
				DispatchQueue.main.async {
					self.writingSupportBundle = false
				}
			}
		}
	}

	private func sharePreview() -> SharePreview<Never, Never> {
		return SharePreview("Support bundle")
	}
}

struct SupportView: View {
	@Environment(AppState.self) private var appState

	var body: some View {
		Form {
			Section {
				Text(
					"If you have a question about Synctrain or are experiencing issues, you may find a solution on our 'frequently asked questions' page."
				)
				.fixedSize(horizontal: false, vertical: true)

				Link(destination: URL(string: "https://t-shaped.nl/synctrain-support")!) {
					Label("View frequently asked questions", systemImage: "link")
				}
			}

			Section {
				Text(
					"If your issue remains unresolved, you may invoke the help of our community through our discussions page. Remember that this app is free software. While we do care deeply about your feedback and want to ensure the app is working correctly, we do not have the resources to provide support with your specific issue or usage of Synctrain. You can greatly increase the chance of fixing your issue by providing detailed descriptions of your issue, your configuration and usage, and what you already did to try and resolve the issue yourself. Please provide as much information as possible, including relevant settings, screenshots and error messages."
				)
				.fixedSize(horizontal: false, vertical: true)

				Link(destination: URL(string: "https://github.com/pixelspark/sushitrain/discussions/categories/q-a")!) {
					Label("Ask a question", systemImage: "link")
				}
				Link(destination: URL(string: "https://github.com/pixelspark/sushitrain/discussions/categories/ideas")!) {
					Label("Propose an idea", systemImage: "link")
				}
			}

			SupportBundleView()

			Section {
				Text(
					"Synctrain is based on Syncthing. If you have any questions about Syncthing itself, or are experiencing issues with devices running other versions of Syncthing, you may find helpful answers and discussions on the Syncthing forum."
				)
				.fixedSize(horizontal: false, vertical: true)

				Link(destination: URL(string: "https://forum.syncthing.net")!) {
					Label("Visit the Syncthing forum", systemImage: "link")
				}
			}

			Section {
				Text(
					"To diagnose any issue you may be experiencing, you may want to make use of the troubleshooting options provided in this app."
				)
				.fixedSize(horizontal: false, vertical: true)

				NavigationLink(destination: TroubleshootingView(userSettings: appState.userSettings)) {
					Label("Troubleshooting options", systemImage: "book.and.wrench")
				}
			}
		}
		#if os(macOS)
			.formStyle(.grouped)
		#endif
		.navigationTitle("Questions, support & feedback")
		#if os(iOS)
			.navigationBarTitleDisplayMode(.inline)
		#endif
	}
}

/// This view performs database maintenance when it appears, and disappears automatically.
/// When an error occurs, it shows the message before dismissing.
struct MaintenanceView: View {
	@Environment(AppState.self) private var appState
	@Environment(\.dismiss) private var dismiss
	@State private var errorMessage: ErrorMessage? = nil

	struct ErrorMessage: Identifiable {
		let message: String
		var id: String {
			return self.message
		}
	}

	var body: some View {
		VStack {
			HStack {
				Spacer()
				ContentUnavailableView {
					ProgressView().controlSize(.extraLarge)
				} description: {
					Text("Performing database maintenance...")
				}
				Spacer()
			}
		}
		.alert(item: $errorMessage) { err in
			Alert(
				title: Text("An error occurred"), message: Text(err.message),
				dismissButton: .default(Text("OK")) {
					errorMessage = nil
					dismiss()
				})
		}
		.task {
			await self.performDatabaseMaintenance()
		}
		#if os(iOS)
			.onAppear {
				Log.info("Asserting idle timer disable")
				UIApplication.shared.isIdleTimerDisabled = true
			}
			.onDisappear {
				Log.info("Deasserting idle timer disable")
				UIApplication.shared.isIdleTimerDisabled = false
			}
		#endif
	}

	private func performDatabaseMaintenance() async {
		do {
			try await self.appState.maintenanceManager.performDatabaseMaintenance()
			self.dismiss()
		}
		catch {
			Log.warn("Database maintenance failed: \(error)")
			self.errorMessage = ErrorMessage(message: error.localizedDescription)
		}
	}
}

struct TroubleshootingView: View {
	@ObservedObject var userSettings: AppUserSettings

	@Environment(AppState.self) private var appState
	@Environment(\.dismiss) private var dismiss
	@State private var hasMigratedLegacyDatabase = false
	@State private var hasLegacyDatabase = false
	@State private var performingDatabaseMaintenance = false
	@State private var databaseSize: Int64? = nil
	@State private var cacheSize: Int64? = nil
	@State private var tempSize: Int64? = nil
	@State private var showIgnoredDiscoveredReset = false
	@State private var showV1BackupRemoved = false
	@State private var showCacheCleared = false
	@State private var showTempCleared = false
	@State private var showClearFailed = false
	@State private var showLog = false
	@State private var showLogLevels = false
	@State private var showAllBookmarksRemoved = false
	@State private var showResetDeviceIdentity = false
	@State private var showMaintenance = false
	@State private var showBlockIndexDisabledForAllFolders = false
	@State private var errorMessage: ErrorMessage? = nil

	private static let formatter = ByteCountFormatter()

	var body: some View {
		Form {
			Section {
				Toggle("Save log to a file", isOn: userSettings.$loggingToFileEnabled)
				#if os(macOS)
					if appState.isLoggingToFile {
						Button("Show in Finder", systemImage: "folder") {
							NSWorkspace.shared.open(EngineLocations.current.logs)
						}
					}
				#endif
			} header: {
				Text("Logging")
			} footer: {
				if appState.userSettings.loggingToFileEnabled {
					#if os(macOS)
						if appState.isLoggingToFile {
							Text("The app is logging to a file, which you can share with the developers.")
						}
						else {
							Text(
								"After restarting the app, the app will write a log file, which you can then share with the developers."
							)
						}
					#else
						if appState.isLoggingToFile {
							Text(
								"The app is logging to a file in the application folder, which you can share with the developers."
							)
						}
						else {
							Text(
								"After restarting the app, the app will write a log file in the application folder, which you can then share with the developers."
							)
						}
					#endif
				}
				else {
					if appState.isLoggingToFile {
						Text("Restart the app to stop logging.")
					}
					else {
						Text(
							"Logging slows down the app and uses more battery. Only enable it if you are experiencing problems."
						)
					}
				}
			}

			Section {
				Button("View log messages", systemImage: "heart.text.clipboard") {
					showLog = true
				}.sheet(isPresented: $showLog) {
					NavigationStack {
						LogView()
							.toolbar {
								SheetButton(role: .done) {
									showLog = false
								}
							}
					}
					#if os(macOS)
						.frame(minWidth: 600, minHeight: 450)
					#endif
				}
				#if os(macOS)
					.buttonStyle(.link)
				#endif

				Button("Log levels", systemImage: "slider.horizontal.3") {
					showLogLevels = true
				}.sheet(isPresented: $showLogLevels) {
					NavigationStack {
						LogLevelsView(userSettings: userSettings)
							.toolbar {
								SheetButton(role: .done) {
									showLogLevels = false
								}
							}
					}
					#if os(macOS)
						.frame(minWidth: 500, minHeight: 450)
					#endif
				}
				#if os(macOS)
					.buttonStyle(.link)
				#endif
			}

			Section {
				LabeledContent("Database type") {
					if hasLegacyDatabase {
						// This shouldn't happen because either the migration fails or the app runs, but if it does happen
						// we want to know (and therefore indicate it in the UI).
						Text("v1").foregroundStyle(.red)
					}
					else {
						Text("v2")
					}
				}

				LabeledContent("Database size") {
					if let size = self.databaseSize {
						Text(Self.formatter.string(fromByteCount: size))
					}
					else {
						Text("Unknown")
					}
				}

				if appState.userSettings.migratedToV2At > 0.0 {
					LabeledContent("Upgraded at") {
						Text(
							Date(timeIntervalSinceReferenceDate: appState.userSettings.migratedToV2At).formatted(
								date: .abbreviated, time: .shortened))
					}
				}

				LabeledContent("Last maintenance") {
					if let time = appState.client.lastMaintenanceTime()?.date() {
						Text(time.formatted(date: .abbreviated, time: .shortened))
					}
					else {
						Text("Unknown")
					}
				}

				Button("Perform database maintenance now") {
					showMaintenance = true
				}

			} header: {
				Text("Database maintenance")
			}

			if hasLegacyDatabase {
				Section {
					Button("Restart app to remove v1 database") {
						UserDefaults.standard.set(true, forKey: "clearV1Index")
						exit(0)
					}
					#if os(macOS)
						.buttonStyle(.link)
					#endif
				} footer: {
					Text(
						"A legacy database is still present. If the app is functioning correctly, it is safe to manually delete this database. In order to do this, the app needs to be restarted."
					)
				}.disabled(performingDatabaseMaintenance)
			}

			if hasMigratedLegacyDatabase {
				Section {
					Button("Remove v1 database back-up") {
						self.clearMigratedLegacyDatabase()
					}
					#if os(macOS)
						.buttonStyle(.link)
					#endif
					.alert("Removed v1 database back-up", isPresented: $showV1BackupRemoved) {
						Button("OK") {}
					}
				} footer: {
					Text(
						"After a database upgrade, a copy of the old version is retained for a while. This copy may take up a significant amount of storage space. If everything is working as expected, it is safe to remove this back-up."
					)
				}.disabled(performingDatabaseMaintenance)
			}

			Section {
				Button("Reset ignored discovered devices", role: .destructive) {
					appState.userSettings.ignoreDiscoveredDevices.removeAll()
					showIgnoredDiscoveredReset = true
				}
				.alert("Ignored discovered devices reset", isPresented: $showIgnoredDiscoveredReset) {
					Button("OK") {}
				}

				Button("Disable block index for all folders", role: .destructive) {
					Task {
						do {
							for folder in await appState.folders() {
								try folder.setBlockIndexingEnabled(false)
							}
							showBlockIndexDisabledForAllFolders = true
						}
						catch {
							self.errorMessage = ErrorMessage(error)
						}
					}
				}
				.alert("Block index disabled for all folders", isPresented: $showBlockIndexDisabledForAllFolders) {
					Button("OK") {}
				}
				.errorAlert($errorMessage)
			}

			Section {
				Button("Restart Syncthing", systemImage: "arrow.clockwise") {
					appState.restartEngine()
				}
				.disabled(
					(appState.startupState != .started && !appState.engineCanRestart) || appState.isEngineRestarting)
			} footer: {
				Text(
					"Stops synchronization and starts it again, with the configuration as it is saved. The app stays open."
				)
			}

			Section("Cache directory") {
				LabeledContent("Cache directory size") {
					if let size = self.cacheSize {
						Text(Self.formatter.string(fromByteCount: size))
					}
					else {
						Text("Unknown")
					}
				}

				Button("Clear cache directory", role: .destructive) {
					self.clearCacheDirectory()
				}
				.alert("Cache directory cleared", isPresented: $showCacheCleared) {
					Button("OK") {}
				}
			}

			Section("Temporary files") {
				LabeledContent("Temporary files size") {
					if let size = self.tempSize {
						Text(Self.formatter.string(fromByteCount: size))
					}
					else {
						Text("Unknown")
					}
				}

				Button("Remove temporary files", role: .destructive) {
					self.clearTempDirectory()
				}
				.alert("Temporary files removed", isPresented: $showTempCleared) {
					Button("OK") {}
				}
				.alert("Some files could not be removed", isPresented: $showClearFailed) {
					Button("OK") {}
				}
			}

			Section {
				Button("Remove all bookmarks", role: .destructive) {
					self.removeAllBookmarks()
				}
				.alert("All bookmarks have been removed", isPresented: $showAllBookmarksRemoved) {
					Button("OK") {}
				}
			}

			Section {
				Button("Reset device identity", role: .destructive) {
					self.showResetDeviceIdentity = true
				}
				.alert("Read this carefully", isPresented: $showResetDeviceIdentity) {
					Button("Reset device identity", role: .destructive) {
						do {
							try self.appState.client.clearIdentity()
							exit(0)
						}
						catch {
							self.errorMessage = ErrorMessage(error)
						}
					}
				} message: {
					Text(
						"If you reset the device identity, the device ID will change, and you will have to re-add this device to all other devices. Any files on this device will not be removed, but synchronization will not work until the other devices have re-accepted the new identity of this device again. Resetting the device identity should only be necessary if you have migrated from another device. The app will close to reset the device identity. This cannot be undone!"
					)
				}
			}
		}
		#if os(macOS)
			.formStyle(.grouped)
			.navigationBarBackButtonHidden(true)
			.toolbar {
				ToolbarItem(placement: .navigation) {
					Button(action: {
						dismiss()
					}) {
						Label("Back", systemImage: "arrow.left.circle")
					}
				}
			}
		#endif
		.task {
			await self.updateDatabaseInfo()
		}
		.navigationTitle("Troubleshooting")
		#if os(iOS)
			.navigationBarTitleDisplayMode(.inline)
		#endif
		.sheet(isPresented: $showMaintenance) {
			MaintenanceView()
		}
	}

	private func removeAllBookmarks() {
		userSettings.bookmarkedRoutes.removeAll()
		showAllBookmarksRemoved = true
	}

	private func updateDatabaseInfo() async {
		self.hasLegacyDatabase = appState.client.hasLegacyDatabase()
		self.hasMigratedLegacyDatabase = appState.client.hasMigratedLegacyDatabase()

		let fileManager = FileManager.default
		let path = URL(fileURLWithPath: appState.client.currentConfigDirectory(), isDirectory: true).appending(
			path: "index-v2", directoryHint: .isDirectory)
		do {
			let size = try await fileManager.sizeOfFolder(path: path)
			self.databaseSize = Int64(size)
			Log.info("size of database is \(size) at path \(path)")
		}
		catch {
			Log.warn("could not determine database size at path \(path): \(error.localizedDescription)")
			self.databaseSize = nil
		}

		// Only the app's own cache and temporary directories, which are all the actions below clear
		let locations = AppLocations.current
		self.cacheSize = (try? await fileManager.sizeOfFolder(path: locations.caches)).map { Int64($0) }
		var tempSize: Int64 = 0
		for directory in locations.temporaryDirectories {
			tempSize += Int64((try? await fileManager.sizeOfFolder(path: directory)) ?? 0)
		}
		self.tempSize = tempSize
	}

	private func clearMigratedLegacyDatabase() {
		if self.performingDatabaseMaintenance {
			return
		}
		Task {
			self.performingDatabaseMaintenance = true
			do {
				try appState.client.clearMigratedLegacyDatabase()
			}
			catch {
				print("Cannot clear migrated V1 index: \(error.localizedDescription)")
			}
			await self.updateDatabaseInfo()
			self.performingDatabaseMaintenance = false
			self.showV1BackupRemoved = true
		}
	}

	private func clearTempDirectory() {
		// Files waiting to be imported may be the only copy of what was shared
		let keeping = Array(IncomingFiles.pending)
		Task {
			let errors = await Task.detached(priority: .userInitiated) {
				AppLocations.current.clearTemporaryFiles(keeping: keeping)
			}.value
			for error in errors {
				Log.warn("failed to remove temp file: \(error.localizedDescription)")
			}
			await self.updateDatabaseInfo()
			if errors.isEmpty {
				self.showTempCleared = true
			}
			else {
				self.showClearFailed = true
			}
		}
	}

	private func clearCacheDirectory() {
		Task {
			let errors = await Task.detached(priority: .userInitiated) {
				AppLocations.current.clearCaches()
			}.value
			for error in errors {
				Log.warn("failed to remove cache file: \(error.localizedDescription)")
			}
			await self.updateDatabaseInfo()
			if errors.isEmpty {
				self.showCacheCleared = true
			}
			else {
				self.showClearFailed = true
			}
		}
	}
}
