// Copyright (C) 2026 Gordon
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this file,
// You can obtain one at https://mozilla.org/MPL/2.0/.
import Darwin
import Foundation

/// The system's copy of a folder of the Files location, as the Files app or Finder keeps it on this device. The items
/// of the index are there once the system has asked for them, and what the extension refused with `excludedFromSync`
/// (names macOS keeps for itself, names an on-demand folder cannot select, paths a folder's ignore patterns exclude)
/// is there and nowhere else, as the system keeps those on the device without synchronizing them.
public enum SystemCopy {
	/// What the system's copy of a folder holds that the folder itself does not
	public struct Unknown: Sendable, Equatable {
		/// The paths, relative to the folder's root, shallowest first
		public var paths: [String] = []
		/// Whether all of the copy was looked at. False when the walk stopped at its limit or its budget, was
		/// cancelled, or could not read a directory, so that there can be more than `paths`.
		public var isComplete = true

		public init(paths: [String] = [], isComplete: Bool = true) {
			self.paths = paths
			self.isComplete = isComplete
		}
	}

	/// Where a directory of the copy is, and what its path in the folder is
	private struct Entry {
		var location: String
		var path: String
	}

	/// What the folder has no live item at below its own directory in the system's copy. Only the top of such a
	/// subtree is listed: what is inside it goes with it. Finder's `.DS_Store` is left out, as the system removes its
	/// own with the directory it is in.
	///
	/// The walk stops at `limit` paths or after `budget`, as folders can be large and this only tells the user where
	/// their files are. It reads directory listings, so nothing of the folder is downloaded, and it only walks into
	/// directories the folder has, so it stays inside what the index describes. Names are matched as the index stores
	/// them, which is how the system writes its copy; a name that differs from the index only in case (which the
	/// volume cannot tell apart) is reported as unknown.
	public static func unknownPaths(
		folderID: String, root: URL, store: IndexStore, limit: Int = 100, budget: TimeInterval = 10
	) throws(IndexError) -> Unknown {
		let deadline = ContinuousClock.now + .seconds(budget)
		var found = Unknown()
		var location = root.path(percentEncoded: false)
		if location.count > 1, location.hasSuffix("/") {
			location.removeLast()
		}
		var level = [Entry(location: location, path: "")]
		while !level.isEmpty {
			guard found.paths.count < limit, ContinuousClock.now < deadline, !Task.isCancelled else {
				found.isComplete = false
				break
			}
			var next: [Entry] = []
			var batch: [Entry] = []
			for directory in level {
				guard found.paths.count < limit, ContinuousClock.now < deadline, !Task.isCancelled else {
					found.isComplete = false
					break
				}
				guard let names = names(at: directory.location) else {
					// A directory that cannot be read may hold more
					found.isComplete = false
					continue
				}
				var stopped = false
				for name in names where name != ".DS_Store" {
					batch.append(
						Entry(
							location: directory.location + "/" + name,
							path: FileNames.join(directory.path, FileNames.normalized(name))))
					if batch.count >= IndexStore.maximumPathsPerQuery {
						try sort(&batch, folderID: folderID, store: store, into: &found, descending: &next)
						if found.paths.count >= limit || ContinuousClock.now >= deadline || Task.isCancelled {
							found.isComplete = false
							stopped = true
							break
						}
					}
				}
				if stopped {
					break
				}
			}
			try sort(&batch, folderID: folderID, store: store, into: &found, descending: &next)
			level = next
		}
		if found.paths.count > limit {
			found.isComplete = false
			found.paths = Array(found.paths.prefix(limit))
		}
		return found
	}

	/// Asks the index about a batch of entries at once: those it has no item at are what stays, and the directories it
	/// has are what the walk goes on with
	private static func sort(
		_ batch: inout [Entry], folderID: String, store: IndexStore, into found: inout Unknown,
		descending next: inout [Entry]
	) throws(IndexError) {
		guard !batch.isEmpty else {
			return
		}
		let known = try store.liveKinds(folderID: folderID, at: batch.map(\.path))
		found.paths += batch.filter { known[$0.path] == nil }.map(\.path).sorted()
		next += batch.filter { known[$0.path]?.isContainer == true }
		batch.removeAll(keepingCapacity: true)
	}

	/// The names in a directory, as the file system has them; empty when it cannot be read
	public static func names(in directory: URL) -> [String] {
		names(at: directory.path(percentEncoded: false)) ?? []
	}

	/// `FileManager` leaves out the `._name` files that macOS keeps a file's metadata in, which are exactly what the
	/// location cannot synchronize, so the directory itself is read. Nil when it cannot be opened; names that are not
	/// valid UTF-8 are left out, as nothing could be done with them.
	private static func names(at location: String) -> [String]? {
		guard let handle = opendir(location) else {
			return nil
		}
		defer { closedir(handle) }
		var names: [String] = []
		while let entry = readdir(handle) {
			let name = withUnsafeBytes(of: entry.pointee.d_name) { bytes in
				String(bytes: bytes.prefix(Int(entry.pointee.d_namlen)), encoding: .utf8)
			}
			if let name, name != ".", name != ".." {
				names.append(name)
			}
		}
		return names
	}
}
