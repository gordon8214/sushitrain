// Copyright (C) 2026 Gordon
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this file,
// You can obtain one at https://mozilla.org/MPL/2.0/.
@preconcurrency import FileProvider
import Foundation
import Observation
import SynctrainFileProviderKit

/// The previous versions of one file of a gateway folder, and what can be done with them
@Observable @MainActor final class VersionsModel {
	enum Phase: Equatable {
		case loading
		case loaded
		case failed(Problem)
	}

	/// What went wrong, as it is shown
	struct Problem: Equatable, Identifiable {
		var title: String
		var message: String
		/// Whether trying again may help
		var canRetry: Bool

		var id: String { title + message }
	}

	private(set) var phase = Phase.loading
	private(set) var fileName = ""
	private(set) var currentModified: Date?
	private(set) var currentSize: Int64 = 0
	/// The versions shown, newest first
	private(set) var versions: [RemoteVersion] = []
	/// How many versions are hidden because their contents are those of a newer one
	private(set) var hiddenCount = 0
	private(set) var versioning = RemoteVersioning.none
	private(set) var gatewayName = ""
	private(set) var canWrite = false
	/// Whether the file has changes on this device that the gateway has not confirmed yet
	private(set) var hasPendingChange = false
	/// The version being downloaded for a preview
	private(set) var downloadingID: String?
	/// What is being done to the file (restoring or copying), shown over the list
	private(set) var activity: String?
	/// The downloaded version being previewed
	var previewURL: URL?
	/// The version the user is asked to confirm restoring
	var restoreCandidate: RemoteVersion?
	/// A failed action
	var actionProblem: Problem?

	/// Where versions are downloaded for previews, removed when the sheet closes
	private nonisolated static let downloadsPrefix = "Versions-"

	@ObservationIgnored private let itemIdentifier: NSFileProviderItemIdentifier?
	/// The file's index row, once its identifier is resolved
	@ObservationIgnored private var itemID: Int64?
	@ObservationIgnored private let domainIdentifier: NSFileProviderDomainIdentifier
	@ObservationIgnored private var browser: RemoteVersionBrowser?
	@ObservationIgnored private var started = false
	/// Downloads and writes in progress, canceled when the sheet closes
	@ObservationIgnored private var tasks: [Task<Void, Never>] = []
	@ObservationIgnored private let directory = FileManager.default.temporaryDirectory.appending(
		path: downloadsPrefix + UUID().uuidString, directoryHint: .isDirectory)
	@ObservationIgnored private let complete: @MainActor () -> Void

	init(
		itemIdentifier: NSFileProviderItemIdentifier?, domainIdentifier: NSFileProviderDomainIdentifier?,
		complete: @escaping @MainActor () -> Void
	) {
		self.itemIdentifier = itemIdentifier
		self.domainIdentifier = domainIdentifier ?? NSFileProviderDomainIdentifier(SharedContainer.domainIdentifier)
		self.complete = complete
	}

	// MARK: - Loading

	/// Loads the versions the first time the sheet appears (not again when it reappears after a preview)
	func loadIfNeeded() async {
		guard !started else {
			return
		}
		started = true
		await load()
	}

	func load() async {
		phase = .loading
		if itemID == nil, let itemIdentifier {
			itemID = await Self.itemID(of: itemIdentifier, in: domainIdentifier)
		}
		do {
			let (browser, initial) = try await Self.open(itemID: itemID, current: browser).get()
			self.browser = browser
			let endpoint = browser.client.endpoint
			gatewayName = endpoint.displayName.isEmpty ? endpoint.host : endpoint.displayName
			canWrite = browser.canWrite
			var file = initial
			let list = try await browser.versions()
			if list.revision != file.remote.revision {
				// This device hasn't seen the file's latest change yet: catch up, so that a restore is based on it
				try? await browser.refresh()
				file = try await Self.readFile(browser)
			}
			fileName = file.item.name
			currentModified = file.item.modificationDate
			currentSize = file.item.size
			hasPendingChange = file.item.pending != .none
			versioning = list.versioning
			versions = RemoteVersionBrowser.distinct(
				list.versions, currentModifiedNs: file.remote.modifiedNs, currentSize: file.remote.size)
			hiddenCount = list.versions.count - versions.count
			phase = .loaded
		}
		catch {
			log.warning("Could not list versions: \(String(describing: error), privacy: .private)")
			phase = .failed(problem(error, forVersion: false))
		}
	}

	/// The index row of the file. The Files app names it by its document ID (`__fp/fs/docID(…)`) rather than by the
	/// identifier the File Provider gave it, so that identifier is looked up through the file's location.
	@concurrent private nonisolated static func itemID(
		of identifier: NSFileProviderItemIdentifier, in domainIdentifier: NSFileProviderDomainIdentifier
	) async -> Int64? {
		if let itemID = ItemIdentifiers.itemID(from: identifier.rawValue) {
			return itemID
		}
		do {
			guard
				let domain = try await NSFileProviderManager.domains().first(where: { $0.identifier == domainIdentifier }),
				let manager = NSFileProviderManager(for: domain)
			else {
				return nil
			}
			let url = try await manager.getUserVisibleURL(for: identifier)
			let (resolved, _) = try await NSFileProviderManager.identifierForUserVisibleFile(at: url)
			return ItemIdentifiers.itemID(from: resolved.rawValue)
		}
		catch {
			log.warning(
				"Could not resolve \(identifier.rawValue, privacy: .public): \(error.localizedDescription, privacy: .public)")
			return nil
		}
	}

	/// Opens the browser (unless one is open) and reads the file, off the main actor: the index may be busy
	@concurrent private nonisolated static func open(itemID: Int64?, current: RemoteVersionBrowser?) async -> Result<
		(RemoteVersionBrowser, RemoteVersionBrowser.File), RemoteVersionBrowser.Failure
	> {
		do {
			let browser: RemoteVersionBrowser
			if let current {
				browser = current
			}
			else {
				browser = try makeBrowser(itemID: itemID)
			}
			return .success((browser, try browser.file()))
		}
		catch {
			return .failure(error)
		}
	}

	@concurrent private nonisolated static func readFile(_ browser: RemoteVersionBrowser)
		async throws(RemoteVersionBrowser.Failure) -> RemoteVersionBrowser.File
	{
		try browser.file()
	}

	private nonisolated static func makeBrowser(itemID: Int64?) throws(RemoteVersionBrowser.Failure)
		-> RemoteVersionBrowser
	{
		guard let container = SharedContainer.appGroup(), let itemID else {
			throw .notAvailable
		}
		let group = Bundle.main.object(forInfoDictionaryKey: SharedContainer.appGroupInfoKey) as? String
		let credentials = KeychainCredentialStore(accessGroup: group?.isEmpty == false ? group : nil)
		return try RemoteVersionBrowser(store: IndexStore(container: container), itemID: itemID) { endpoint in
			RemoteFileClient(endpoint: endpoint, credentials: credentials)
		}
	}

	// MARK: - Actions

	func preview(_ version: RemoteVersion) {
		guard downloadingID == nil, activity == nil, let browser else {
			return
		}
		downloadingID = version.id
		let directory = directory
		run {
			defer { self.downloadingID = nil }
			do throws(RemoteVersionBrowser.Failure) {
				self.previewURL = try await browser.download(version, into: directory)
			}
			catch {
				await self.failed(error)
			}
		}
	}

	func restore(_ version: RemoteVersion) {
		write(String(localized: "Restoring…", comment: "Shown while a previous version replaces a file")) {
			browser throws(RemoteVersionBrowser.Failure) in
			try await browser.restore(version)
		}
	}

	func saveCopy(_ version: RemoteVersion) {
		write(String(localized: "Saving a copy…", comment: "Shown while a copy of a previous version is made")) {
			browser throws(RemoteVersionBrowser.Failure) in
			try await browser.saveCopy(of: version)
		}
	}

	/// Closes the sheet, stopping what is in progress. A restore or copy the gateway accepted is carried out anyway.
	func finish() {
		for task in tasks {
			task.cancel()
		}
		try? FileManager.default.removeItem(at: directory)
		complete()
	}

	/// Removes downloads that earlier sheets left behind when the Files app closed them
	nonisolated static func removeLeftoverDownloads() {
		let temporary = FileManager.default.temporaryDirectory
		let entries =
			(try? FileManager.default.contentsOfDirectory(
				at: temporary, includingPropertiesForKeys: [.contentModificationDateKey])) ?? []
		let cutoff = Date.now.addingTimeInterval(-3600)
		for entry in entries where entry.lastPathComponent.hasPrefix(downloadsPrefix) {
			let modified = try? entry.resourceValues(forKeys: [.contentModificationDateKey]).contentModificationDate
			if let modified, modified < cutoff {
				try? FileManager.default.removeItem(at: entry)
			}
		}
	}

	private func run(_ body: @escaping @MainActor () async -> Void) {
		tasks.removeAll { $0.isCancelled }
		tasks.append(Task { await body() })
	}

	/// Has the gateway write a version, then tells the Files app and closes the sheet
	private func write(
		_ description: String,
		_ body: @escaping @Sendable (RemoteVersionBrowser) async throws(RemoteVersionBrowser.Failure) -> RemoteOperation
	) {
		guard activity == nil, let browser else {
			return
		}
		activity = description
		run {
			do throws(RemoteVersionBrowser.Failure) {
				_ = try await body(browser)
			}
			catch {
				self.activity = nil
				await self.failed(error)
				return
			}
			self.activity = nil
			await self.signalFiles()
			self.finish()
		}
	}

	/// Shows why an action failed, and the file and its versions as they are now when they changed
	private func failed(_ failure: RemoteVersionBrowser.Failure) async {
		if case .remote(.canceled) = failure {
			return
		}
		log.warning("Could not use a version: \(String(describing: failure), privacy: .private)")
		actionProblem = problem(failure, forVersion: true)
		switch failure {
		case .remote(.revisionMismatch), .remote(.versionNotFound):
			await load()
		default:
			break
		}
	}

	/// Has the Files app ask the File Provider for changes, which the browser already stored
	private func signalFiles() async {
		guard
			let domain = (try? await NSFileProviderManager.domains())?.first(where: {
				$0.identifier == domainIdentifier
			}),
			let manager = NSFileProviderManager(for: domain)
		else {
			return
		}
		do {
			try await manager.signalEnumerator(for: .workingSet)
		}
		catch {
			log.warning("Could not signal the working set: \(error.localizedDescription, privacy: .public)")
		}
	}

	// MARK: - Problems

	private func problem(_ failure: RemoteVersionBrowser.Failure, forVersion: Bool) -> Problem {
		switch failure {
		case .notAvailable:
			return Problem(
				title: String(localized: "Not Available"),
				message: String(
					localized: "Previous versions can only be shown for files in folders that Synctrain opens through a gateway."),
				canRetry: false)
		case .notPaired:
			return Problem(
				title: String(localized: "Gateway Not Paired"),
				message: String(localized: "Open Synctrain and pair this device with the gateway again."), canRetry: false)
		case .unsupported:
			return Problem(
				title: String(localized: "Gateway Needs an Update"),
				message: String(
					localized: "Update Synctrain on \(gatewayName) (or synctrain-gateway) to see previous versions of files."),
				canRetry: false)
		case .outdated:
			return Problem(
				title: String(localized: "Update Synctrain"),
				message: String(
					localized:
						"\(gatewayName) uses a newer version of Synctrain. Update Synctrain on this device to see previous versions of files."
				),
				canRetry: false)
		case .pendingChange where forVersion:
			return Problem(
				title: String(localized: "Changes Not Uploaded Yet"),
				message: String(localized: "You can restore a version once your changes to this file are uploaded."),
				canRetry: false)
		case .pendingChange:
			return Problem(
				title: String(localized: "Not Uploaded Yet"),
				message: String(localized: "Previous versions are shown once this file is uploaded to the gateway."),
				canRetry: true)
		case .readOnly:
			return Problem(
				title: String(localized: "Read Only"),
				message: String(localized: "This device may only read this folder on \(gatewayName)."), canRetry: false)
		case .outcomeUnknown:
			return Problem(
				title: String(localized: "Still in Progress"),
				message: String(
					localized: "\(gatewayName) is still working on it. The file shows the result in Files when it is done."),
				canRetry: false)
		case .store:
			return Problem(
				title: String(localized: "Something Went Wrong"),
				message: String(localized: "Synctrain could not read what it knows about this file."), canRetry: true)
		case .remote(let error):
			return problem(error, forVersion: forVersion)
		}
	}

	private func problem(_ error: RemoteError, forVersion: Bool) -> Problem {
		switch error {
		case .unreachable(.serverCertificateUntrusted):
			return Problem(
				title: String(localized: "Gateway Not Trusted"),
				message: String(localized: "\(gatewayName) presented a certificate Synctrain does not trust. Pair with it again."),
				canRetry: false)
		case .unreachable:
			return Problem(
				title: String(localized: "Gateway Not Reachable"),
				message: String(localized: "Synctrain could not reach \(gatewayName). Check your connection and try again."),
				canRetry: true)
		case .unauthenticated:
			return Problem(
				title: String(localized: "Device Not Accepted"),
				message: String(
					localized: "\(gatewayName) no longer accepts this device. Open Synctrain and pair it again."),
				canRetry: false)
		case .versionNotFound:
			return Problem(
				title: String(localized: "Version No Longer Available"),
				message: String(localized: "The folder's versioning on \(gatewayName) has removed this version."),
				canRetry: true)
		case .notFound:
			return Problem(
				title: String(localized: "File Not Found"),
				message: String(localized: "The file is no longer on \(gatewayName)."), canRetry: false)
		case .revisionMismatch:
			return Problem(
				title: String(localized: "File Changed"),
				message: String(
					localized: "The file changed on \(gatewayName) since this device last saw it. Nothing was restored."),
				canRetry: true)
		case .nameConflict:
			return Problem(
				title: String(localized: "Name Taken"),
				message: String(localized: "Synctrain could not find a free name for the copy."), canRetry: true)
		case .forbidden where forVersion:
			return Problem(
				title: String(localized: "Not Permitted"),
				message: String(localized: "\(gatewayName) did not allow this change."), canRetry: false)
		case .forbidden:
			return Problem(
				title: String(localized: "Not Permitted"),
				message: String(localized: "\(gatewayName) does not allow reading this file."), canRetry: false)
		case .unavailable, .overloaded:
			return Problem(
				title: String(localized: "Gateway Busy"),
				message: String(localized: "\(gatewayName) can't do this right now. Try again in a moment."),
				canRetry: true)
		case .insufficientStorage:
			return Problem(
				title: String(localized: "Gateway Full"),
				message: String(localized: "\(gatewayName) is out of storage space."), canRetry: false)
		default:
			return Problem(
				title: String(localized: "Something Went Wrong"),
				message: String(localized: "\(gatewayName) could not complete the request."), canRetry: true)
		}
	}
}
