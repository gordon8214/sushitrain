// Copyright (C) 2026 Gordon
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this file,
// You can obtain one at https://mozilla.org/MPL/2.0/.
import Darwin
@preconcurrency import FileProvider
import Foundation
import os
import SynctrainFileProviderKit

enum FetchError: LocalizedError {
	case appNotRunning
	case noDeviceAvailable
	case serverError(Int)
	/// The app served a newer version than the one in the index (the index has not caught up yet)
	case versionChanged

	var errorDescription: String? {
		switch self {
		case .appNotRunning:
			return String(localized: "Open Synctrain to download this file from your other devices.")
		case .noDeviceAvailable:
			return String(localized: "None of the devices that have this file are connected.")
		case .serverError(let status):
			return String(localized: "Synctrain could not provide this file (error \(status)).")
		case .versionChanged:
			return String(localized: "This file just changed on another device. Try again in a moment.")
		}
	}

	/// The error to report to the File Provider framework, carrying our explanation. Reporting the server as unreachable
	/// makes the system show the whole location as paused until the app signals that this is resolved, so that is
	/// only done when the user asked for a file and the app has to be opened to get it.
	func fileProviderError(isSystemRequest: Bool) -> Error {
		let userInfo: [String: Any] = [
			NSLocalizedDescriptionKey: errorDescription ?? "", NSUnderlyingErrorKey: self as NSError,
		]
		if case .appNotRunning = self, !isSystemRequest {
			return NSError(
				domain: NSFileProviderErrorDomain, code: NSFileProviderError.serverUnreachable.rawValue,
				userInfo: userInfo)
		}
		return NSError(domain: NSCocoaErrorDomain, code: NSFileReadUnknownError, userInfo: userInfo)
	}
}

/// Obtains file contents: from the folder on this device when the current version is there, otherwise from other
/// devices through the app, as long as it is running.
final class ContentFetcher: Sendable {
	private let context: ExtensionContext
	private let session: URLSession
	/// Pings get their own connection, so that they do not wait behind downloads
	private let pingSession: URLSession
	private let isInvalidated = OSAllocatedUnfairLock(initialState: false)

	private static let pingTimeout: TimeInterval = 1.5
	/// How long a download may go without receiving data. The app sends nothing while it fetches a block from another
	/// device, which over a slow relay can take minutes (the app gives the largest blocks 8.5); whether the
	/// app still runs is checked separately (`checkAlive`).
	private static let idleTimeout: TimeInterval = 600
	/// How often a download that makes no progress checks that the app still answers
	private static let livenessInterval: Duration = .seconds(5)

	init(context: ExtensionContext) {
		self.context = context
		session = URLSession(configuration: Self.configuration(maximumConnections: 4, timeout: Self.idleTimeout))
		pingSession = URLSession(configuration: Self.configuration(maximumConnections: 1, timeout: Self.pingTimeout))
	}

	private static func configuration(maximumConnections: Int, timeout: TimeInterval) -> URLSessionConfiguration {
		let configuration = URLSessionConfiguration.ephemeral
		configuration.urlCache = nil
		configuration.requestCachePolicy = .reloadIgnoringLocalCacheData
		configuration.httpMaximumConnectionsPerHost = maximumConnections
		configuration.timeoutIntervalForRequest = timeout
		configuration.waitsForConnectivity = false
		return configuration
	}

	/// Cancels transfers. The sessions are not invalidated: a task created on an invalidated session raises an
	/// exception, and work that started before this call may still create one.
	func invalidate() {
		isInvalidated.withLock { $0 = true }
		for session in [session, pingSession] {
			session.getAllTasks { tasks in
				for task in tasks {
					task.cancel()
				}
			}
		}
	}

	/// Clones the local file into the destination when it is the version in the index. Returns false otherwise.
	func cloneLocal(_ item: IndexItem, to destination: URL) -> Bool {
		let source = context.localURL(for: item).path(percentEncoded: false)
		let fd = open(source, O_RDONLY | O_NOFOLLOW)
		guard fd >= 0 else {
			return false
		}
		defer { close(fd) }

		// The puller replaces files by renaming, so the file we opened stays the version we checked
		guard let fingerprint = FileFingerprint(fileDescriptor: fd),
			fingerprint.matches(size: item.size, modifiedNanoseconds: item.modifiedNanoseconds)
		else {
			return false
		}

		let target = destination.path(percentEncoded: false)
		if fclonefileat(fd, AT_FDCWD, target, 0) == 0 {
			return true
		}

		// Not on the same volume (or the file system cannot clone): copy the data
		let out = open(target, O_WRONLY | O_CREAT | O_EXCL, 0o600)
		guard out >= 0 else {
			return false
		}
		defer { close(out) }
		if fcopyfile(fd, out, nil, copyfile_flags_t(COPYFILE_DATA)) != 0 {
			unlink(target)
			return false
		}
		return true
	}

	/// The app's heartbeat, when it says the app is running
	func awakeApp() -> Heartbeat? {
		guard let heartbeat = Heartbeat.read(from: context.container.heartbeatURL), heartbeat.isFresh() else {
			return nil
		}
		return heartbeat
	}

	private static func url(_ heartbeat: Heartbeat, path: String, query: [URLQueryItem]) -> URL {
		var components = URLComponents()
		components.scheme = "http"
		components.host = "127.0.0.1"
		components.port = heartbeat.port
		components.path = path
		components.queryItems = query.isEmpty ? nil : query
		// Not encoded by URLComponents, but taken as a space and as a separator by the server's query parser
		components.percentEncodedQuery = components.percentEncodedQuery?
			.replacingOccurrences(of: "+", with: "%2B").replacingOccurrences(of: ";", with: "%3B")
		return components.url!
	}

	private func request(_ heartbeat: Heartbeat, path: String, query: [URLQueryItem] = []) throws -> URLRequest {
		if isInvalidated.withLock({ $0 }) {
			throw CancellationError()
		}
		var request = URLRequest(url: Self.url(heartbeat, path: path, query: query))
		request.setValue("Bearer \(heartbeat.token)", forHTTPHeaderField: "Authorization")
		return request
	}

	/// The URL of a file on the app's server, for clients that cannot set headers (the token is in the query instead)
	func streamingURL(for item: IndexItem, from heartbeat: Heartbeat) -> URL {
		Self.url(
			heartbeat, path: "/file",
			query: [
				URLQueryItem(name: "folder", value: item.folderID), URLQueryItem(name: "path", value: item.path),
				URLQueryItem(name: "token", value: heartbeat.token),
			])
	}

	/// Whether the app answers. A suspended app still accepts connections but never responds, hence the short timeout.
	func ping(_ heartbeat: Heartbeat) async -> Bool {
		guard var request = try? request(heartbeat, path: "/ping") else {
			return false
		}
		request.timeoutInterval = Self.pingTimeout
		guard let (_, response) = try? await pingSession.data(for: request) else {
			return false
		}
		return (response as? HTTPURLResponse)?.statusCode == 200
	}

	/// Downloads the file through the app into the destination, reporting progress. The app serves the current version,
	/// which is returned when it is not the version of the item (the index has not caught up yet).
	func download(_ item: IndexItem, from heartbeat: Heartbeat, to destination: URL, progress: Progress) async throws
		-> ServedVersion?
	{
		let request = try request(
			heartbeat, path: "/file",
			query: [URLQueryItem(name: "folder", value: item.folderID), URLQueryItem(name: "path", value: item.path)])

		// Whichever ends first: the download, or the app no longer answering (it was suspended)
		return try await withThrowingTaskGroup(of: ServedVersion?.self) { group in
			group.addTask {
				try await self.transfer(request, of: item, to: destination, progress: progress)
			}
			group.addTask {
				try await self.checkAlive(heartbeat, while: progress)
				return nil
			}
			defer { group.cancelAll() }
			guard let result = try await group.next() else {
				throw CancellationError()
			}
			return result
		}
	}

	/// Returns (throwing) only when the app stops answering while the download makes no progress
	private func checkAlive(_ heartbeat: Heartbeat, while progress: Progress) async throws {
		var lastFraction = -1.0
		while true {
			try await Task.sleep(for: Self.livenessInterval)
			let fraction = progress.fractionCompleted
			if fraction == lastFraction, await !ping(heartbeat) {
				throw FetchError.appNotRunning
			}
			lastFraction = fraction
		}
	}

	private func transfer(_ request: URLRequest, of item: IndexItem, to destination: URL, progress: Progress)
		async throws -> ServedVersion?
	{
		let (downloaded, response) = try await session.download(for: request, delegate: ProgressDelegate(progress))
		defer { try? FileManager.default.removeItem(at: downloaded) }

		let status = (response as? HTTPURLResponse)?.statusCode ?? 0
		switch status {
		case 200:
			try FileManager.default.moveItem(at: downloaded, to: destination)
		case 503:
			throw FetchError.noDeviceAvailable
		default:
			throw FetchError.serverError(status)
		}
		guard let response = response as? HTTPURLResponse,
			let size = response.value(forHTTPHeaderField: "X-Synctrain-Size").flatMap({ Int64($0) }),
			let modified = response.value(forHTTPHeaderField: "X-Synctrain-Mod-Ns").flatMap({ Int64($0) })
		else {
			return nil
		}
		let served = ServedVersion(size: size, modifiedNanoseconds: modified)
		return served.isVersion(of: item) ? nil : served
	}
}

/// The version of a file the app served
struct ServedVersion: Sendable {
	let size: Int64
	let modifiedNanoseconds: Int64

	func isVersion(of item: IndexItem) -> Bool {
		size == item.size && modifiedNanoseconds == item.modifiedNanoseconds
	}
}

/// Mirrors the progress of a download task into the progress reported to the system
private final class ProgressDelegate: NSObject, URLSessionTaskDelegate, Sendable {
	private let progress: Progress

	init(_ progress: Progress) {
		self.progress = progress
	}

	func urlSession(_ session: URLSession, didCreateTask task: URLSessionTask) {
		progress.addChild(task.progress, withPendingUnitCount: progress.totalUnitCount)
		progress.cancellationHandler = { task.cancel() }
	}
}
