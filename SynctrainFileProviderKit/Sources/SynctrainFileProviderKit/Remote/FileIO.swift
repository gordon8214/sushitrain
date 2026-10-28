// Copyright (C) 2026 Gordon
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this file,
// You can obtain one at https://mozilla.org/MPL/2.0/.
import CryptoKit
import Darwin
import Foundation

/// File access with bounded buffers: content is never read into memory as a whole
struct FileDescriptor: ~Copyable {
	let raw: Int32

	init(_ url: URL, flags: Int32, mode: mode_t = 0o600) throws(RemoteError) {
		let fd = Darwin.open(url.path(percentEncoded: false), flags | O_CLOEXEC, mode)
		guard fd >= 0 else {
			throw .fileSystem("could not open \(url.lastPathComponent): \(String(cString: strerror(errno)))")
		}
		raw = fd
	}

	deinit {
		Darwin.close(raw)
	}

	var size: Int64 {
		var st = stat()
		return fstat(raw, &st) == 0 ? Int64(st.st_size) : 0
	}

	/// Reads up to `count` bytes at the offset; fewer only at the end of the file
	func read(into buffer: UnsafeMutableRawBufferPointer, count: Int, at offset: Int64) throws(RemoteError) -> Int {
		var done = 0
		while done < count {
			let result = pread(raw, buffer.baseAddress! + done, count - done, off_t(offset) + off_t(done))
			if result < 0 {
				if errno == EINTR {
					continue
				}
				throw .fileSystem("could not read: \(String(cString: strerror(errno)))")
			}
			if result == 0 {
				break
			}
			done += result
		}
		return done
	}

	func write(_ buffer: UnsafeRawBufferPointer, at offset: Int64) throws(RemoteError) {
		var done = 0
		while done < buffer.count {
			let result = pwrite(raw, buffer.baseAddress! + done, buffer.count - done, off_t(offset) + off_t(done))
			if result < 0 {
				if errno == EINTR {
					continue
				}
				throw .fileSystem("could not write: \(String(cString: strerror(errno)))")
			}
			done += result
		}
	}

	func write(_ bytes: [UInt8], at offset: Int64) throws(RemoteError) {
		var failure: RemoteError?
		bytes.withUnsafeBytes { buffer in
			do throws(RemoteError) {
				try write(buffer, at: offset)
			}
			catch {
				failure = error
			}
		}
		if let failure {
			throw failure
		}
	}

	func truncate(to size: Int64) throws(RemoteError) {
		guard ftruncate(raw, off_t(size)) == 0 else {
			throw .fileSystem("could not size a file: \(String(cString: strerror(errno)))")
		}
	}

	func sync() throws(RemoteError) {
		guard fsync(raw) == 0 else {
			throw .fileSystem("could not sync a file: \(String(cString: strerror(errno)))")
		}
	}
}

enum FileIO {
	/// The size of the buffers used to stream files
	static let bufferSize = 256 << 10

	/// The SHA-256 of a part of a file (all of it by default), read with a bounded buffer
	static func sha256(of url: URL, offset: Int64 = 0, length: Int64? = nil) throws(RemoteError) -> SHA256.Digest {
		let file = try FileDescriptor(url, flags: O_RDONLY)
		let end = length.map { offset + $0 } ?? file.size
		var hasher = SHA256()
		let buffer = UnsafeMutableRawBufferPointer.allocate(byteCount: bufferSize, alignment: 16)
		defer { buffer.deallocate() }
		var position = offset
		while position < end {
			let count = try file.read(into: buffer, count: Int(min(Int64(bufferSize), end - position)), at: position)
			if count == 0 {
				throw .fileSystem("\(url.lastPathComponent) is shorter than expected")
			}
			hasher.update(bufferPointer: UnsafeRawBufferPointer(rebasing: buffer[0..<count]))
			position += Int64(count)
		}
		return hasher.finalize()
	}

	/// Copies a part of a file into a new file, with a bounded buffer; returns the standard base64 SHA-256 of the bytes
	@discardableResult
	static func copy(from source: URL, offset: Int64, length: Int64, to destination: URL) throws(RemoteError) -> String {
		let input = try FileDescriptor(source, flags: O_RDONLY)
		let output = try FileDescriptor(destination, flags: O_WRONLY | O_CREAT | O_TRUNC)
		var hasher = SHA256()
		let buffer = UnsafeMutableRawBufferPointer.allocate(byteCount: bufferSize, alignment: 16)
		defer { buffer.deallocate() }
		var done: Int64 = 0
		while done < length {
			let count = try input.read(into: buffer, count: Int(min(Int64(bufferSize), length - done)), at: offset + done)
			if count == 0 {
				throw .fileSystem("\(source.lastPathComponent) is shorter than expected")
			}
			let chunk = UnsafeRawBufferPointer(rebasing: buffer[0..<count])
			hasher.update(bufferPointer: chunk)
			try output.write(chunk, at: done)
			done += Int64(count)
		}
		try output.sync()
		return Data(hasher.finalize()).base64EncodedString()
	}

	/// The available capacity of the volume holding the URL, for important use
	static func availableCapacity(at url: URL) -> Int64? {
		(try? url.resourceValues(forKeys: [.volumeAvailableCapacityForImportantUsageKey]))?
			.volumeAvailableCapacityForImportantUsage
	}
}
