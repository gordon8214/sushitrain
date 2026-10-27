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

		// One at a time: the extension has little memory to spare
		Task.detached(priority: .utility) {
			var remote: RemoteFiles? = nil
			for identifier in itemIdentifiers {
				if progress.isCancelled {
					break
				}
				var data: Data? = nil
				if let id = identifier.itemID, let item = try? context.store.item(id: id), item.kind == .file {
					let type = UTType(filenameExtension: (item.name as NSString).pathExtension) ?? .data
					if context.isOnDevice(item) {
						data = await Self.thumbnail(of: context.localURL(for: item), type: type, maximumPixels: maximumPixels)
					}
					else if let fetcher {
						if remote == nil {
							remote = await RemoteFiles(fetcher: fetcher)
						}
						data = await remote?.thumbnail(of: item, type: type, maximumPixels: maximumPixels)
					}
				}
				perThumbnail.call(identifier, data, nil)
				progress.completedUnitCount += 1
			}
			trace("made \(progress.completedUnitCount) thumbnails \(memoryFootprint())")
			completion.call(progress.isCancelled ? CocoaError(.userCancelled) : nil)
		}
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

		func thumbnail(of item: IndexItem, type: UTType, maximumPixels: CGFloat) async -> Data? {
			guard let heartbeat else {
				return nil
			}
			if type.conforms(to: .audiovisualContent) {
				return await FileProviderExtension.videoThumbnail(
					of: fetcher.streamingURL(for: item, from: heartbeat), type: type, maximumPixels: maximumPixels)
			}
			guard item.size <= FileProviderExtension.maximumRemoteFileSize else {
				return nil
			}
			// Keep the name: the thumbnail service recognizes files by their extension
			let directory = FileManager.default.temporaryDirectory.appending(path: UUID().uuidString)
			defer { try? FileManager.default.removeItem(at: directory) }
			let temporary = directory.appending(path: item.name)
			do {
				try FileManager.default.createDirectory(at: directory, withIntermediateDirectories: true)
				try await fetcher.download(item, from: heartbeat, to: temporary, progress: Progress())
			}
			catch {
				return nil
			}
			return await FileProviderExtension.thumbnail(of: temporary, type: type, maximumPixels: maximumPixels)
		}
	}

	/// A thumbnail of a file on disk: images are decoded here (cheap), other types by the system's thumbnail service
	fileprivate static func thumbnail(of url: URL, type: UTType, maximumPixels: CGFloat) async -> Data? {
		if type.conforms(to: .image) {
			return autoreleasepool { imageThumbnail(of: url, maximumPixels: maximumPixels) }
		}
		let request = QLThumbnailGenerator.Request(
			fileAt: url, size: CGSize(width: maximumPixels, height: maximumPixels), scale: 1,
			representationTypes: .thumbnail)
		guard let representation = try? await QLThumbnailGenerator.shared.generateBestRepresentation(for: request)
		else {
			return nil
		}
		return jpeg(representation.cgImage)
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
		return jpeg(image)
	}

	private static func imageThumbnail(of url: URL, maximumPixels: CGFloat) -> Data? {
		guard let source = CGImageSourceCreateWithURL(url as CFURL, [kCGImageSourceShouldCache: false] as CFDictionary)
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
		return jpeg(image)
	}

	private static func jpeg(_ image: CGImage) -> Data? {
		let data = NSMutableData()
		guard let destination = CGImageDestinationCreateWithData(data, UTType.jpeg.identifier as CFString, 1, nil) else {
			return nil
		}
		CGImageDestinationAddImage(
			destination, image, [kCGImageDestinationLossyCompressionQuality: 0.8] as CFDictionary)
		return CGImageDestinationFinalize(destination) ? data as Data : nil
	}
}
