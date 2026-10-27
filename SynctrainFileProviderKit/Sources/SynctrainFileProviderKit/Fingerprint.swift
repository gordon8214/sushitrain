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
	public var modifiedNanoseconds: Int64
	public var inode: UInt64
	public var isRegularFile: Bool

	init(_ st: stat) {
		size = Int64(st.st_size)
		modifiedNanoseconds = Int64(st.st_mtimespec.tv_sec) * 1_000_000_000 + Int64(st.st_mtimespec.tv_nsec)
		inode = UInt64(st.st_ino)
		isRegularFile = (st.st_mode & S_IFMT) == S_IFREG
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

	/// Whether this is a regular file with the given size and a modification time within the window
	public func matches(size: Int64, modifiedNanoseconds: Int64, windowNanoseconds: Int64 = 0) -> Bool {
		isRegularFile && self.size == size && abs(self.modifiedNanoseconds - modifiedNanoseconds) <= windowNanoseconds
	}
}
