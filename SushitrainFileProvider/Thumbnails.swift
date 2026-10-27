// Copyright (C) 2026 Gordon
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this file,
// You can obtain one at https://mozilla.org/MPL/2.0/.
@preconcurrency import FileProvider
import ImageIO
import SynctrainFileProviderKit
import UniformTypeIdentifiers

/// Thumbnails for images on this device, made from the file in the folder, so the system does not need to download
/// them into its own copy first. Other items get none (the system shows an icon for them).
extension FileProviderExtension: NSFileProviderThumbnailing {
	private static let defaultThumbnailSize: CGFloat = 256

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

		// One at a time: the extension has little memory to spare
		Task.detached(priority: .utility) {
			for identifier in itemIdentifiers {
				if progress.isCancelled {
					break
				}
				let data = autoreleasepool {
					Self.thumbnail(for: identifier, context: context, maximumPixels: maximumPixels)
				}
				perThumbnail.call(identifier, data, nil)
				progress.completedUnitCount += 1
			}
			completion.call(progress.isCancelled ? CocoaError(.userCancelled) : nil)
		}
		return progress
	}

	private static func thumbnail(
		for identifier: NSFileProviderItemIdentifier, context: ExtensionContext, maximumPixels: CGFloat
	) -> Data? {
		guard let id = identifier.itemID, let item = try? context.store.item(id: id), item.kind == .file,
			UTType(filenameExtension: (item.name as NSString).pathExtension)?.conforms(to: .image) == true,
			context.isOnDevice(item)
		else {
			return nil
		}
		let url = context.localURL(for: item)
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
