// Copyright (C) 2026 Gordon
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this file,
// You can obtain one at https://mozilla.org/MPL/2.0/.
import Darwin
@preconcurrency import FileProvider
import Foundation
import SynctrainFileProviderKit

enum FetchError: LocalizedError {
	case appNotRunning
	case noDeviceAvailable
	case serverError(Int)

	var errorDescription: String? {
		switch self {
		case .appNotRunning:
			return String(localized: "Open Synctrain to download this file from your other devices.")
		case .noDeviceAvailable:
			return String(localized: "None of the devices that have this file are connected.")
		case .serverError(let status):
			return String(localized: "Synctrain could not provide this file (error \(status)).")
		}
	}

	/// The error to report to the File Provider framework, carrying our explanation
	var fileProviderError: Error {
		NSError(
			domain: NSFileProviderErrorDomain, code: NSFileProviderError.serverUnreachable.rawValue,
			userInfo: [NSLocalizedDescriptionKey: errorDescription ?? "", NSUnderlyingErrorKey: self as NSError])
	}
}

/// Obtains file contents: from the folder on this device when the current version is there, otherwise from other
/// devices through the app, as long as it is running.
final class ContentFetcher: Sendable {
	private let context: ExtensionContext
	private let session: URLSession

	private static let pingTimeout: TimeInterval = 1.5
	/// How long a download may go without receiving data
	private static let idleTimeout: TimeInterval = 10

	init(context: ExtensionContext) {
		self.context = context
		let configuration = URLSessionConfiguration.ephemeral
		configuration.urlCache = nil
		configuration.requestCachePolicy = .reloadIgnoringLocalCacheData
		configuration.httpMaximumConnectionsPerHost = 2
		configuration.timeoutIntervalForRequest = Self.idleTimeout
		configuration.waitsForConnectivity = false
		self.session = URLSession(configuration: configuration)
	}

	func invalidate() {
		session.invalidateAndCancel()
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

	private func request(_ heartbeat: Heartbeat, path: String, query: [URLQueryItem] = []) -> URLRequest {
		var components = URLComponents()
		components.scheme = "http"
		components.host = "127.0.0.1"
		components.port = heartbeat.port
		components.path = path
		components.queryItems = query.isEmpty ? nil : query
		var request = URLRequest(url: components.url!)
		request.setValue("Bearer \(heartbeat.token)", forHTTPHeaderField: "Authorization")
		return request
	}

	/// Whether the app answers. A suspended app still accepts connections but never responds, hence the short timeout.
	func ping(_ heartbeat: Heartbeat) async -> Bool {
		var request = request(heartbeat, path: "/ping")
		request.timeoutInterval = Self.pingTimeout
		guard let (_, response) = try? await session.data(for: request) else {
			return false
		}
		return (response as? HTTPURLResponse)?.statusCode == 200
	}

	/// Downloads the file through the app into the destination, reporting progress
	func download(_ item: IndexItem, from heartbeat: Heartbeat, to destination: URL, progress: Progress) async throws {
		let request = request(
			heartbeat, path: "/file",
			query: [URLQueryItem(name: "folder", value: item.folderID), URLQueryItem(name: "path", value: item.path)])

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
