// Copyright (C) 2026 Gordon
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this file,
// You can obtain one at https://mozilla.org/MPL/2.0/.
@preconcurrency import FileProvider
import ImageIO
import SynctrainFileProviderKit
import UniformTypeIdentifiers

/// Thumbnails for images, made from the file in the folder when it is on this device, so the system does not need to
/// download it into its own copy first. Images that are only on other devices are fetched through the app while it is
/// awake (up to a size). Other items get none (the system shows an icon for them).
extension FileProviderExtension: NSFileProviderThumbnailing {
	private static let defaultThumbnailSize: CGFloat = 256
	/// Larger images that are only on other devices get no thumbnail (fetching them would take too long)
	private static let maximumRemoteImageSize: Int64 = 16 * 1_048_576

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
			var remote: RemoteImages? = nil
			for identifier in itemIdentifiers {
				if progress.isCancelled {
					break
				}
				var data: Data? = nil
				if let id = identifier.itemID, let item = try? context.store.item(id: id), item.kind == .file,
					UTType(filenameExtension: (item.name as NSString).pathExtension)?.conforms(to: .image) == true
				{
					if context.isOnDevice(item) {
						data = autoreleasepool { Self.thumbnail(of: context.localURL(for: item), maximumPixels: maximumPixels) }
					}
					else if item.size <= Self.maximumRemoteImageSize, let fetcher {
						if remote == nil {
							remote = await RemoteImages(fetcher: fetcher)
						}
						data = await remote?.thumbnail(of: item, maximumPixels: maximumPixels)
					}
				}
				perThumbnail.call(identifier, data, nil)
				progress.completedUnitCount += 1
			}
			completion.call(progress.isCancelled ? CocoaError(.userCancelled) : nil)
		}
		return progress
	}

	/// Fetches images that are only on other devices through the app, when it is awake
	private struct RemoteImages {
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

		func thumbnail(of item: IndexItem, maximumPixels: CGFloat) async -> Data? {
			guard let heartbeat else {
				return nil
			}
			let temporary = FileManager.default.temporaryDirectory.appending(path: UUID().uuidString)
			defer { try? FileManager.default.removeItem(at: temporary) }
			do {
				try await fetcher.download(item, from: heartbeat, to: temporary, progress: Progress())
			}
			catch {
				return nil
			}
			return autoreleasepool { FileProviderExtension.thumbnail(of: temporary, maximumPixels: maximumPixels) }
		}
	}

	fileprivate static func thumbnail(of url: URL, maximumPixels: CGFloat) -> Data? {
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
		let data = NSMutableData()
		guard let destination = CGImageDestinationCreateWithData(data, UTType.jpeg.identifier as CFString, 1, nil) else {
			return nil
		}
		CGImageDestinationAddImage(
			destination, image, [kCGImageDestinationLossyCompressionQuality: 0.8] as CFDictionary)
		return CGImageDestinationFinalize(destination) ? data as Data : nil
	}
}
