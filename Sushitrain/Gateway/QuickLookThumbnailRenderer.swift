// Copyright (C) 2026 Gordon
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this file,
// You can obtain one at https://mozilla.org/MPL/2.0/.
#if os(macOS)
	import Foundation
	import ImageIO
	import os
	import QuickLookThumbnailing
	@preconcurrency import SushitrainCore
	import SynctrainHostKit
	import UniformTypeIdentifiers

	/// Renders thumbnails of images, videos and PDFs for the remote file service with Quick Look. The core calls it on
	/// its own threads (one thumbnail at a time), so it blocks until the thumbnail is ready or the time is up.
	final class QuickLookThumbnailRenderer: NSObject, SushitrainRemoteThumbnailRendererProtocol, Sendable {
		enum RenderError: LocalizedError {
			case invalidPath
			case unknownType
			case timedOut
			case encodingFailed

			var errorDescription: String? {
				switch self {
				case .invalidPath: return "invalid path"
				case .unknownType: return "the file type is unknown"
				case .timedOut: return "Quick Look did not render the thumbnail in time"
				case .encodingFailed: return "the thumbnail could not be encoded"
				}
			}
		}

		/// Matches the thumbnail features the core announces (images, videos and PDFs)
		private static let supportedTypes: [UTType] = [.image, .movie, .pdf]
		private static let timeout: TimeInterval = 20
		private static let jpegQuality = 0.8

		func supportsThumbnail(_ name: String?) -> Bool {
			guard let name, let type = UTType(filenameExtension: (name as NSString).pathExtension) else {
				return false
			}
			return Self.supportedTypes.contains { type.conforms(to: $0) }
		}

		func renderThumbnail(_ path: String?, size: Int) throws -> Data {
			guard let path, (path as NSString).isAbsolutePath, size > 0 else {
				throw RenderError.invalidPath
			}
			let url = URL(filePath: path)
			if !url.pathExtension.isEmpty {
				return try render(url, size: size)
			}

			// Files fetched from other devices for the thumbnail have no extension; Quick Look needs one
			guard let type = Self.sniffType(url), let ext = type.preferredFilenameExtension else {
				throw RenderError.unknownType
			}
			let directory = try AppLocations.current.makeTemporaryDirectory(prefix: "thumbnail-")
			defer {
				try? FileManager.default.removeItem(at: directory)
			}
			let typed = directory.appending(path: "file." + ext, directoryHint: .notDirectory)
			do {
				try FileManager.default.linkItem(at: url, to: typed)
			}
			catch {
				try FileManager.default.copyItem(at: url, to: typed)
			}
			return try render(typed, size: size)
		}

		private func render(_ url: URL, size: Int) throws -> Data {
			let request = QLThumbnailGenerator.Request(
				fileAt: url, size: CGSize(width: size, height: size), scale: 1, representationTypes: .thumbnail)
			let result = OSAllocatedUnfairLock<Result<Data, any Error>?>(initialState: nil)
			let done = DispatchSemaphore(value: 0)
			QLThumbnailGenerator.shared.generateBestRepresentation(for: request) { representation, error in
				let outcome: Result<Data, any Error>
				if let representation {
					outcome = Result { try Self.jpeg(representation.cgImage, maxPixels: size) }
				}
				else {
					outcome = .failure(error ?? RenderError.encodingFailed)
				}
				result.withLock { $0 = outcome }
				done.signal()
			}
			if done.wait(timeout: .now() + Self.timeout) == .timedOut {
				QLThumbnailGenerator.shared.cancel(request)
				throw RenderError.timedOut
			}
			guard let outcome = result.withLock({ $0 }) else {
				throw RenderError.timedOut
			}
			return try outcome.get()
		}

		/// Encodes an image as JPEG on white (JPEG has no transparency), with its longer side at most maxPixels
		private static func jpeg(_ image: CGImage, maxPixels: Int) throws -> Data {
			let scale = min(1, Double(maxPixels) / Double(max(image.width, image.height)))
			let width = max(1, Int((Double(image.width) * scale).rounded()))
			let height = max(1, Int((Double(image.height) * scale).rounded()))
			guard
				let context = CGContext(
					data: nil, width: width, height: height, bitsPerComponent: 8, bytesPerRow: 0,
					space: CGColorSpace(name: CGColorSpace.sRGB) ?? CGColorSpaceCreateDeviceRGB(),
					bitmapInfo: CGImageAlphaInfo.noneSkipLast.rawValue)
			else {
				throw RenderError.encodingFailed
			}
			let rect = CGRect(x: 0, y: 0, width: width, height: height)
			context.setFillColor(CGColor(gray: 1, alpha: 1))
			context.fill(rect)
			context.interpolationQuality = .high
			context.draw(image, in: rect)
			let data = NSMutableData()
			guard let flattened = context.makeImage(),
				let destination = CGImageDestinationCreateWithData(data, UTType.jpeg.identifier as CFString, 1, nil)
			else {
				throw RenderError.encodingFailed
			}
			let options = [kCGImageDestinationLossyCompressionQuality: jpegQuality] as CFDictionary
			CGImageDestinationAddImage(destination, flattened, options)
			guard CGImageDestinationFinalize(destination) else {
				throw RenderError.encodingFailed
			}
			return data as Data
		}

		/// Recognizes images and PDFs by their contents
		private static func sniffType(_ url: URL) -> UTType? {
			if let source = CGImageSourceCreateWithURL(url as CFURL, nil), let identifier = CGImageSourceGetType(source),
				let type = UTType(identifier as String), type.conforms(to: .image)
			{
				return type
			}
			guard let handle = try? FileHandle(forReadingFrom: url) else {
				return nil
			}
			defer {
				try? handle.close()
			}
			let header = (try? handle.read(upToCount: 5)) ?? Data()
			return header == Data("%PDF-".utf8) ? .pdf : nil
		}
	}
#endif
