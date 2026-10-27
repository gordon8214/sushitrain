// Copyright (C) 2026 Gordon
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this file,
// You can obtain one at https://mozilla.org/MPL/2.0/.
import Darwin
import Foundation

/// The identity of a file on disk at a moment in time, as Syncthing would compare it (size and modification time)
public struct FileFingerprint: Sendable, Equatable {
	public var size: Int64
	/// The modification time in nanoseconds since 1970, clamped to what fits (about the years 1677 to 2262)
	public var modifiedNanoseconds: Int64
	/// The creation time in nanoseconds since 1970, clamped like the modification time
	public var birthNanoseconds: Int64
	public var inode: UInt64
	public var isRegularFile: Bool
	public var isDirectory: Bool

	init(_ st: stat) {
		size = Int64(st.st_size)
		modifiedNanoseconds = Self.nanoseconds(st.st_mtimespec)
		birthNanoseconds = Self.nanoseconds(st.st_birthtimespec)
		inode = UInt64(st.st_ino)
		isRegularFile = (st.st_mode & S_IFMT) == S_IFREG
		isDirectory = (st.st_mode & S_IFMT) == S_IFDIR
	}

	/// The fingerprint of the file at the path, without following symlinks
	public init?(path: String) {
		var st = stat()
		guard lstat(path, &st) == 0 else {
			return nil
		}
		self.init(st)
	}

	public init?(fileDescriptor: Int32) {
		var st = stat()
		guard fstat(fileDescriptor, &st) == 0 else {
			return nil
		}
		self.init(st)
	}

	/// The time in nanoseconds since 1970. Times that do not fit (APFS stores times before 1677, e.g. the zero time of
	/// Windows in 1601) are clamped instead of trapping.
	static func nanoseconds(_ time: timespec) -> Int64 {
		let seconds = Int64(time.tv_sec)
		let (scaled, overflow) = seconds.multipliedReportingOverflow(by: 1_000_000_000)
		if overflow {
			return seconds < 0 ? .min : .max
		}
		let (total, carry) = scaled.addingReportingOverflow(Int64(time.tv_nsec))
		if carry {
			return time.tv_nsec < 0 ? .min : .max
		}
		return total
	}

	/// Whether this is a regular file with the given size and a modification time within the window. Times too far apart
	/// to subtract never match.
	public func matches(size: Int64, modifiedNanoseconds: Int64, windowNanoseconds: Int64 = 0) -> Bool {
		guard isRegularFile, self.size == size, windowNanoseconds >= 0 else {
			return false
		}
		let (difference, overflow) = self.modifiedNanoseconds.subtractingReportingOverflow(modifiedNanoseconds)
		return !overflow && difference.magnitude <= UInt64(windowNanoseconds)
	}
}
