// Copyright (C) 2026 Gordon
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this file,
// You can obtain one at https://mozilla.org/MPL/2.0/.
@preconcurrency import FileProvider
import ImageIO
import SynctrainFileProviderKit
import UniformTypeIdentifiers

/// Thumbnails made without the system downloading files into its own copy first: the gateway's, when it renders the
/// type, otherwise decoded here from an image this device holds. Items without a thumbnail get the icon for their type.
extension FileProviderExtension: NSFileProviderThumbnailing {
	private static let defaultThumbnailSize: CGFloat = 256
	/// Larger images are left to the system's thumbnail service, which runs in its own process: decoding them here
	/// would take much of the memory the extension may use
	private static let maximumDecodedPixels = 20_000_000
	/// How long a gateway's thumbnail may take. A gateway that does not answer in time is not asked again for a while.
	private static let gatewayTimeout: Duration = .seconds(5)

	func fetchThumbnails(
		for itemIdentifiers: [NSFileProviderItemIdentifier], requestedSize size: CGSize,
		perThumbnailCompletionHandler: @escaping (NSFileProviderItemIdentifier, Data?, (any Error)?) -> Void,
		completionHandler: @escaping ((any Error)?) -> Void
	) -> Progress {
		let progress = Progress(totalUnitCount: Int64(itemIdentifiers.count))
		guard let context, let remoteProvider = remote else {
			completionHandler(nil)
			return progress
		}
		let maximumPixels = max(size.width, size.height) > 0 ? max(size.width, size.height) : Self.defaultThumbnailSize
		let perThumbnail = Completion(call: perThumbnailCompletionHandler)
		let completion = Completion(call: completionHandler)

		let task = Task.detached(priority: .utility) {
			for identifier in itemIdentifiers {
				if progress.isCancelled {
					break
				}
				var data: Data? = nil
				var error: (any Error)? = nil
				do {
					if let id = identifier.itemID, let item = try context.store.item(id: id), item.kind == .file,
						let binding = try context.remoteBinding(folderID: item.folderID)
					{
						let type = UTType(filenameExtension: (item.name as NSString).pathExtension) ?? .data
						// Waiting for the gateway takes no memory: only what is decoded here waits for the queue
						data = try await Self.remoteThumbnail(
							of: item, binding: binding, provider: remoteProvider, type: type, maximumPixels: maximumPixels)
					}
				}
				catch let failure {
					// Not "no thumbnail" (which the system keeps until the item changes): it may work later
					error = CocoaError(.fileReadUnknown, userInfo: [NSUnderlyingErrorKey: failure as NSError])
				}
				perThumbnail.call(identifier, data, error)
				progress.completedUnitCount += 1
			}
			trace("made \(progress.completedUnitCount) thumbnails \(memoryFootprint())")
			completion.call(progress.isCancelled ? CocoaError(.userCancelled) : nil)
		}
		progress.cancellationHandler = { task.cancel() }
		return progress
	}

	/// The gateway's thumbnail of a file, when it renders the type (for the
	/// exact revision), else one decoded here from an image this device holds. Videos are never streamed and files are
	/// never downloaded for it.
	private static func remoteThumbnail(
		of item: IndexItem, binding: RemoteFolderBinding, provider: RemoteProvider, type: UTType, maximumPixels: CGFloat
	) async throws -> Data? {
		let decodable = type.conforms(to: .image) && !type.conforms(to: .rawImage)
		do {
			let data = try await withTimeout(gatewayTimeout) {
				try await provider.gatewayThumbnail(of: item, binding: binding, type: type, maximumPixels: maximumPixels)
			}
			if let data {
				return data
			}
		}
		catch {
			if error is CancellationError, !Task.isCancelled {
				// Too slow: the next thumbnails do not wait for it again
				provider.noteUnresponsive(serviceID: binding.serviceID)
			}
			// Unreachable, or too slow: an image this device holds still does
			guard decodable, try provider.localCopy(of: item) != nil else {
				throw error
			}
		}
		guard decodable, let local = try provider.localCopy(of: item) else {
			return nil
		}
		return try await oneAtATime { autoreleasepool { imageThumbnail(of: local, maximumPixels: maximumPixels) } }
	}

	/// Runs the work while no other thumbnail is made in the extension (decoding takes much of its memory)
	private static func oneAtATime<T>(_ work: () async throws -> T) async throws -> T {
		await ThumbnailQueue.shared.acquire()
		do {
			let result = try await work()
			await ThumbnailQueue.shared.release()
			return result
		}
		catch {
			await ThumbnailQueue.shared.release()
			throw error
		}
	}

	/// Runs the work, cancelling it when it takes longer than the timeout
	private static func withTimeout<T: Sendable>(
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
