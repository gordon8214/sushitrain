// Copyright (C) 2026 Gordon
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this file,
// You can obtain one at https://mozilla.org/MPL/2.0/.
@preconcurrency import AVFoundation
import SynctrainFileProviderKit
import os

/// Feeds a player from a gateway. `AVPlayer` cannot send the credential or check the gateway's pinned key itself, and
/// credentials never go into URLs, so it gets a URL of a scheme of ours and every read goes through the gateway client.
final class GatewayMediaLoader: NSObject, AVAssetResourceLoaderDelegate, Sendable {
	private static let scheme = "synctrain-media"

	private let stream: RemoteContentStream
	private let size: Int64
	private let contentType: String
	private let queue = DispatchQueue(label: "synctrain.media-loader")
	/// The reads in progress, by the request they serve, and whether playback ended (after which none start)
	private struct Reads: Sendable {
		var tasks: [ObjectIdentifier: Task<Void, Never>] = [:]
		var closed = false
	}

	private let reads = OSAllocatedUnfairLock(initialState: Reads())

	init(stream: RemoteContentStream, size: Int64, contentType: String) {
		self.stream = stream
		self.size = size
		self.contentType = contentType
	}

	/// An asset that plays through this loader. Keep the loader for as long as the asset is used.
	func makeAsset() -> AVURLAsset? {
		guard let url = URL(string: "\(Self.scheme)://item/media") else {
			return nil
		}
		let asset = AVURLAsset(url: url)
		asset.resourceLoader.setDelegate(self, queue: queue)
		return asset
	}

	func resourceLoader(
		_ resourceLoader: AVAssetResourceLoader,
		shouldWaitForLoadingOfRequestedResource loadingRequest: AVAssetResourceLoadingRequest
	) -> Bool {
		if let information = loadingRequest.contentInformationRequest {
			information.contentType = contentType
			information.contentLength = size
			information.isByteRangeAccessSupported = true
		}
		guard let dataRequest = loadingRequest.dataRequest else {
			loadingRequest.finishLoading()
			return true
		}
		let start = dataRequest.requestedOffset
		let end =
			dataRequest.requestsAllDataToEndOfResource ? size : min(start + Int64(dataRequest.requestedLength), size)
		let request = PendingRequest(loading: loadingRequest, data: dataRequest)
		let id = ObjectIdentifier(loadingRequest)
		return reads.withLock { reads in
			guard !reads.closed else {
				return false
			}
			// Started under the lock, so that its own removal can't come before it is recorded, nor `close()` miss it
			reads.tasks[id] = Task { [self] in
				await serve(request, from: start, to: end)
				self.reads.withLock { _ = $0.tasks.removeValue(forKey: id) }
			}
			return true
		}
	}

	func resourceLoader(
		_ resourceLoader: AVAssetResourceLoader, didCancel loadingRequest: AVAssetResourceLoadingRequest
	) {
		reads.withLock { $0.tasks.removeValue(forKey: ObjectIdentifier(loadingRequest)) }?.cancel()
	}

	/// Stops every read and starts no more, as playback ended. The player cancels reads it no longer needs, but may not
	/// as it goes away, and a read to the end of a file would then download all of it.
	func close() {
		let tasks = reads.withLock { reads in
			reads.closed = true
			defer { reads.tasks = [:] }
			return Array(reads.tasks.values)
		}
		for task in tasks {
			task.cancel()
		}
	}

	/// The requests of the player, which it hands over to the queue of this loader and accepts answers to from anywhere
	private struct PendingRequest: @unchecked Sendable {
		let loading: AVAssetResourceLoadingRequest
		let data: AVAssetResourceLoadingDataRequest
	}

	/// Answers the request with the bytes as they arrive, in few requests to the gateway; a request the player canceled
	/// stops its stream at the next chunk
	private func serve(_ request: PendingRequest, from start: Int64, to end: Int64) async {
		do throws(RemoteError) {
			try await stream.stream(from: start, to: end) { chunk in
				guard !request.loading.isCancelled else {
					return false
				}
				request.data.respond(with: chunk)
				return true
			}
		}
		catch {
			if error != .canceled, !Task.isCancelled {
				request.loading.finishLoading(
					with: NSError(
						domain: NSURLErrorDomain, code: NSURLErrorResourceUnavailable,
						userInfo: [NSLocalizedDescriptionKey: error.userDescription]))
			}
			return
		}
		if !Task.isCancelled {
			request.loading.finishLoading()
		}
	}
}
