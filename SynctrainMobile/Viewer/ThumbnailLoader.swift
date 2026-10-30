// Copyright (C) 2026 Gordon
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this file,
// You can obtain one at https://mozilla.org/MPL/2.0/.
import ImageIO
import SynctrainFileProviderKit
import UIKit
import UniformTypeIdentifiers

/// Thumbnails of files: the gateway's, kept on disk by content so that they stay available offline, or decoded from an
/// image this device holds. Files without one get the icon of their type.
actor ThumbnailLoader {
	static let shared = ThumbnailLoader()

	/// The sizes asked for, so that a few cached thumbnails serve all views
	private static let sizes = [64, 128, 256, 512, 1024]
	/// How long a gateway that failed is left alone
	private static let backoff: TimeInterval = 60

	private let memory = NSCache<NSString, UIImage>()
	/// Thumbnails the gateway does not have (by key), which it is not asked for again
	private var missing: Set<String> = []
	private var failedAt: [String: Date] = [:]

	private var directory: URL {
		URL.cachesDirectory.appending(path: "Thumbnails", directoryHint: .isDirectory)
	}

	/// The thumbnail of the file, in about the given size in pixels, or nil when there is none
	func thumbnail(of item: IndexItem, pixels: Int) async -> UIImage? {
		guard let environment = RemoteEnvironment.shared, item.kind == .file, let hash = item.blocksHash,
			let name = environment.storage.cachedContentURL(ContentIdentity(hash))?.lastPathComponent
		else {
			return nil
		}
		let size = Self.sizes.first { $0 >= pixels } ?? Self.sizes[Self.sizes.count - 1]
		let key = "\(name)-\(size)"
		if let image = memory.object(forKey: key as NSString) {
			return image
		}
		let file = directory.appending(path: "\(key).jpg", directoryHint: .notDirectory)
		if let image = UIImage(contentsOfFile: file.path(percentEncoded: false)) {
			memory.setObject(image, forKey: key as NSString)
			return image
		}
		let type = UTType(filenameExtension: (item.name as NSString).pathExtension) ?? .data
		if !missing.contains(key), let data = await gatewayThumbnail(of: item, type: type, size: size, key: key) {
			try? FileManager.default.createDirectory(at: directory, withIntermediateDirectories: true)
			try? data.write(to: file, options: .atomic)
			if let image = UIImage(data: data) {
				memory.setObject(image, forKey: key as NSString)
				return image
			}
		}
		guard type.conforms(to: .image), let image = await decoded(item, environment: environment, size: size) else {
			return nil
		}
		memory.setObject(image, forKey: key as NSString)
		return image
	}

	private func gatewayThumbnail(of item: IndexItem, type: UTType, size: Int, key: String) async -> Data? {
		guard item.pending == .none, let feature = Capabilities.thumbnailFeature(for: type),
			let environment = RemoteEnvironment.shared,
			let binding = try? environment.store.remoteBinding(folderID: item.folderID),
			let state = try? environment.store.remoteItem(itemID: item.id), !state.isDeleted,
			state.contentID?.rawValue == item.blocksHash
		else {
			return nil
		}
		let serviceID = binding.serviceID
		if let failed = failedAt[serviceID], Date().timeIntervalSince(failed) < Self.backoff {
			return nil
		}
		let status = await MainActor.run { RemoteCoordinator.shared.status(serviceID: serviceID) }
		guard let status, status.isUsable, status.capabilities?.supports(feature) == true,
			let client = environment.client(serviceID: serviceID)
		else {
			return nil
		}
		let destination = URL.temporaryDirectory.appending(path: "thumbnail-\(UUID().uuidString)")
		defer { try? FileManager.default.removeItem(at: destination) }
		do throws(RemoteError) {
			try await environment.scheduler.withSlot(.thumbnail) { () async throws(RemoteError) in
				try await client.thumbnail(itemID: state.gatewayID, revision: state.revision, size: size, to: destination)
			}
		}
		catch .notFound {
			missing.insert(key)
			return nil
		}
		catch .unavailable, .canceled, .revisionMismatch {
			// The gateway is busy making it, or the file changed
			return nil
		}
		catch {
			failedAt[serviceID] = Date()
			return nil
		}
		return try? Data(contentsOf: destination)
	}

	/// A reduced version of an image this device holds
	private func decoded(_ item: IndexItem, environment: RemoteEnvironment, size: Int) async -> UIImage? {
		guard let hash = item.blocksHash, let url = environment.storage.cachedContentURL(ContentIdentity(hash)),
			let source = CGImageSourceCreateWithURL(url as CFURL, [kCGImageSourceShouldCache: false] as CFDictionary)
		else {
			return nil
		}
		let options: [CFString: Any] = [
			kCGImageSourceCreateThumbnailFromImageAlways: true,
			kCGImageSourceCreateThumbnailWithTransform: true,
			kCGImageSourceThumbnailMaxPixelSize: size,
		]
		return CGImageSourceCreateThumbnailAtIndex(source, 0, options as CFDictionary).map(UIImage.init(cgImage:))
	}
}
