// Copyright (C) 2026 Gordon
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this file,
// You can obtain one at https://mozilla.org/MPL/2.0/.
@preconcurrency import AVFoundation
@preconcurrency import FileProvider
import ImageIO
@preconcurrency import QuickLookThumbnailing
import SynctrainFileProviderKit
import UniformTypeIdentifiers

/// Thumbnails made without the system downloading files into its own copy first. Files on this device are read where
/// they are in the folder. Files that are only on other devices are fetched through the app while it is awake: videos
/// are streamed (only the frames needed are transferred), other files are downloaded up to a size. Items without a
/// thumbnail get the icon for their type.
extension FileProviderExtension: NSFileProviderThumbnailing {
	private static let defaultThumbnailSize: CGFloat = 256
	/// Larger files (other than videos) that are only on other devices get no thumbnail (fetching them takes too long)
	private static let maximumRemoteFileSize: Int64 = 16 * 1_048_576
	/// Larger images are left to the system's thumbnail service, which runs in its own process: decoding them here
	/// would take much of the memory the extension may use
	private static let maximumDecodedPixels = 20_000_000
	/// How long a thumbnail of a file on another device may take
	private static let remoteTimeout: Duration = .seconds(20)

	func fetchThumbnails(
		for itemIdentifiers: [NSFileProviderItemIdentifier], requestedSize size: CGSize,
		perThumbnailCompletionHandler: @escaping (NSFileProviderItemIdentifier, Data?, (any Error)?) -> Void,
		completionHandler: @escaping ((any Error)?) -> Void
	) -> Progress {
		let progress = Progress(totalUnitCount: Int64(itemIdentifiers.count))
		guard let context else {
			completionHandler(nil)
			return progress
		}
		let maximumPixels = max(size.width, size.height) > 0 ? max(size.width, size.height) : Self.defaultThumbnailSize
		let perThumbnail = Completion(call: perThumbnailCompletionHandler)
		let completion = Completion(call: completionHandler)
		let fetcher = self.fetcher

		let task = Task.detached(priority: .utility) {
			var remote: RemoteFiles? = nil
			for identifier in itemIdentifiers {
				if progress.isCancelled {
					break
				}
				// One at a time, also across requests: the extension has little memory to spare
				await ThumbnailQueue.shared.acquire()
				var data: Data? = nil
				var error: (any Error)? = nil
				do {
					if let id = identifier.itemID, let item = try context.store.item(id: id), item.kind == .file {
						let type = UTType(filenameExtension: (item.name as NSString).pathExtension) ?? .data
						if context.isOnDevice(item) {
							data = await Self.thumbnail(
								of: context.localURL(for: item), type: type, maximumPixels: maximumPixels)
						}
						else if let fetcher {
							if remote == nil {
								remote = await RemoteFiles(fetcher: fetcher)
							}
							data = try await remote?.thumbnail(of: item, type: type, maximumPixels: maximumPixels)
						}
					}
				}
				catch let failure {
					// Not "no thumbnail" (which the system keeps until the item changes): it may work later
					error = CocoaError(.fileReadUnknown, userInfo: [NSUnderlyingErrorKey: failure as NSError])
				}
				await ThumbnailQueue.shared.release()
				perThumbnail.call(identifier, data, error)
				progress.completedUnitCount += 1
			}
			trace("made \(progress.completedUnitCount) thumbnails \(memoryFootprint())")
			completion.call(progress.isCancelled ? CocoaError(.userCancelled) : nil)
		}
		progress.cancellationHandler = { task.cancel() }
		return progress
	}

	/// Fetches files that are only on other devices through the app, when it is awake
	private struct RemoteFiles {
		let fetcher: ContentFetcher
		let heartbeat: Heartbeat?

		init(fetcher: ContentFetcher) async {
			self.fetcher = fetcher
			if let heartbeat = fetcher.awakeApp(), await fetcher.ping(heartbeat) {
				self.heartbeat = heartbeat
			}
			else {
				self.heartbeat = nil
			}
		}

		/// Throws when the file cannot be fetched now (such as while the app is not running)
		func thumbnail(of item: IndexItem, type: UTType, maximumPixels: CGFloat) async throws -> Data? {
			guard let heartbeat else {
				throw FetchError.appNotRunning
			}
			let fetcher = self.fetcher
			if type.conforms(to: .movie) {
				return try await FileProviderExtension.withTimeout(FileProviderExtension.remoteTimeout) {
					await FileProviderExtension.videoThumbnail(
						of: fetcher.streamingURL(for: item, from: heartbeat), type: type, maximumPixels: maximumPixels)
				}
			}
			guard item.size <= FileProviderExtension.maximumRemoteFileSize else {
				return nil
			}
			// Keep the name: the thumbnail service recognizes files by their extension
			let directory = FileManager.default.temporaryDirectory.appending(path: UUID().uuidString)
			defer { try? FileManager.default.removeItem(at: directory) }
			let temporary = directory.appending(path: item.name)
			try FileManager.default.createDirectory(at: directory, withIntermediateDirectories: true)
			try await FileProviderExtension.withTimeout(FileProviderExtension.remoteTimeout) {
				_ = try await fetcher.download(item, from: heartbeat, to: temporary, progress: Progress())
			}
			return await FileProviderExtension.thumbnail(of: temporary, type: type, maximumPixels: maximumPixels)
		}
	}

	/// Runs the work, cancelling it when it takes longer than the timeout
	fileprivate static func withTimeout<T: Sendable>(
		_ timeout: Duration, _ work: @escaping @Sendable () async throws -> T
	) async throws -> T {
		try await withThrowingTaskGroup(of: T.self) { group in
			group.addTask { try await work() }
			group.addTask {
				try await Task.sleep(for: timeout)
				throw CancellationError()
			}
			defer { group.cancelAll() }
			guard let result = try await group.next() else {
				throw CancellationError()
			}
			return result
		}
	}

	/// A thumbnail of a file on disk: images are decoded here (cheap), other types by the system's thumbnail service
	fileprivate static func thumbnail(of url: URL, type: UTType, maximumPixels: CGFloat) async -> Data? {
		if type.conforms(to: .image) && !type.conforms(to: .rawImage),
			let data = autoreleasepool(invoking: { imageThumbnail(of: url, maximumPixels: maximumPixels) })
		{
			return data
		}
		let request = QLThumbnailGenerator.Request(
			fileAt: url, size: CGSize(width: maximumPixels, height: maximumPixels), scale: 1,
			representationTypes: .thumbnail)
		guard let representation = try? await QLThumbnailGenerator.shared.generateBestRepresentation(for: request)
		else {
			return nil
		}
		return encoded(representation.cgImage)
	}

	/// A frame from the start of a video. Over HTTP, only the parts of the file needed for it are transferred.
	fileprivate static func videoThumbnail(of url: URL, type: UTType, maximumPixels: CGFloat) async -> Data? {
		var options: [String: Any] = [:]
		if let mimeType = type.preferredMIMEType {
			options[AVURLAssetOverrideMIMETypeKey] = mimeType
		}
		let generator = AVAssetImageGenerator(asset: AVURLAsset(url: url, options: options))
		generator.appliesPreferredTrackTransform = true
		generator.maximumSize = CGSize(width: maximumPixels, height: maximumPixels)
		guard let (image, _) = try? await generator.image(at: .zero) else {
			return nil
		}
		return encoded(image)
	}

	/// A thumbnail decoded here, for images that are not too large (others return nil)
	private static func imageThumbnail(of url: URL, maximumPixels: CGFloat) -> Data? {
		guard let source = CGImageSourceCreateWithURL(url as CFURL, [kCGImageSourceShouldCache: false] as CFDictionary),
			let properties = CGImageSourceCopyPropertiesAtIndex(source, 0, nil) as? [CFString: Any],
			let width = properties[kCGImagePropertyPixelWidth] as? Int,
			let height = properties[kCGImagePropertyPixelHeight] as? Int,
			width.multipliedReportingOverflow(by: height).partialValue <= maximumDecodedPixels
		else {
			return nil
		}
		// Decodes a reduced version of the image rather than the whole of it
		let options: [CFString: Any] = [
			kCGImageSourceCreateThumbnailFromImageAlways: true,
			kCGImageSourceCreateThumbnailWithTransform: true,
			kCGImageSourceThumbnailMaxPixelSize: maximumPixels,
		]
		guard let image = CGImageSourceCreateThumbnailAtIndex(source, 0, options as CFDictionary) else {
			return nil
		}
		return encoded(image)
	}

	/// JPEG, or PNG for images with transparency (which JPEG would turn black)
	private static func encoded(_ image: CGImage) -> Data? {
		let hasAlpha = ![.none, .noneSkipFirst, .noneSkipLast].contains(image.alphaInfo)
		let data = NSMutableData()
		let type = hasAlpha ? UTType.png : UTType.jpeg
		guard let destination = CGImageDestinationCreateWithData(data, type.identifier as CFString, 1, nil) else {
			return nil
		}
		CGImageDestinationAddImage(
			destination, image, [kCGImageDestinationLossyCompressionQuality: 0.8] as CFDictionary)
		return CGImageDestinationFinalize(destination) ? data as Data : nil
	}
}

/// Lets one thumbnail be made at a time in the whole extension
private actor ThumbnailQueue {
	static let shared = ThumbnailQueue()
	private var isBusy = false
	private var waiting: [CheckedContinuation<Void, Never>] = []

	func acquire() async {
		guard isBusy else {
			isBusy = true
			return
		}
		await withCheckedContinuation { waiting.append($0) }
	}

	func release() {
		if waiting.isEmpty {
			isBusy = false
		}
		else {
			waiting.removeFirst().resume()
		}
	}
}
