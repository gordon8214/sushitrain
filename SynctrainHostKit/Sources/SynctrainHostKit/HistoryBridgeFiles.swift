// Copyright (C) 2026 Gordon
// SPDX-License-Identifier: MPL-2.0
#if os(macOS)
	import Foundation
	import CryptoKit
	import Darwin

	public enum HistoryBridgeFiles {
		public static func copyVerified(from source: URL, to destination: URL) throws {
			let info = try source.resourceValues(forKeys: [.isRegularFileKey, .isSymbolicLinkKey])
			guard info.isRegularFile == true, info.isSymbolicLink != true else {
				throw HistoryBridgeError.message("The archived copy is no longer a regular file.")
			}
			guard
				copyfile(source.path, destination.path, nil, copyfile_flags_t(COPYFILE_ALL | COPYFILE_EXCL | COPYFILE_NOFOLLOW_SRC))
					== 0
			else { throw POSIXError(POSIXErrorCode(rawValue: errno) ?? .EIO) }
			do {
				try Task.checkCancellation()
				guard try signature(source) == signature(destination) else {
					throw HistoryBridgeError.message("The destination did not preserve the version's contents and metadata.")
				}
			}
			catch {
				try? FileManager.default.removeItem(at: destination)
				throw error
			}
		}
		public static func signature(_ url: URL) throws -> [String: Data] {
			var result = ["content": try digest(url)]
			let count = listxattr(url.path, nil, 0, XATTR_NOFOLLOW)
			guard count >= 0 else { throw POSIXError(POSIXErrorCode(rawValue: errno) ?? .EIO) }
			var names = [CChar](repeating: 0, count: count)
			guard listxattr(url.path, &names, names.count, XATTR_NOFOLLOW) == count else {
				throw HistoryBridgeError.message("The file's attributes changed while being copied.")
			}
			for bytes in names.split(separator: 0) {
				let name = String(decoding: bytes.map { UInt8(bitPattern: $0) }, as: UTF8.self)
				if name == "com.apple.ResourceFork" {
					result["xattr:" + name] = try digest(url.appending(path: "..namedfork/rsrc"))
				}
				else {
					let size = getxattr(url.path, name, nil, 0, 0, XATTR_NOFOLLOW)
					guard size >= 0 else { throw HistoryBridgeError.message("An archived attribute could not be read.") }
					var value = Data(count: size)
					let read = value.withUnsafeMutableBytes { getxattr(url.path, name, $0.baseAddress, size, 0, XATTR_NOFOLLOW) }
					guard read == size else { throw HistoryBridgeError.message("An archived attribute changed while being copied.") }
					result["xattr:" + name] = Data(SHA256.hash(data: value))
				}
			}
			return result
		}
		private static func digest(_ url: URL) throws -> Data {
			let file = try FileHandle(forReadingFrom: url)
			defer { try? file.close() }
			var hash = SHA256()
			while let data = try file.read(upToCount: 1 << 20), !data.isEmpty {
				try Task.checkCancellation()
				hash.update(data: data)
			}
			return Data(hash.finalize())
		}
	}
#endif
