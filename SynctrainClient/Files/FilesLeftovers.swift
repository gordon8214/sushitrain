// Copyright (C) 2026 Gordon
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this file,
// You can obtain one at https://mozilla.org/MPL/2.0/.
#if os(macOS)
	import Foundation
	import SynctrainFileProviderKit

	/// What the Synctrain location cannot synchronize and keeps on this Mac: names macOS keeps for itself (`._name` and
	/// the like), names an on-demand folder's `.stignore` cannot select (`*`, control characters, a space at the edge),
	/// and paths a folder's ignore patterns exclude. The extension refuses those with `excludedFromSync`, so they stay
	/// in the system's copy alone and the index never has them (`SystemCopy.unknownPaths` finds them).
	///
	/// The system cannot remove the directory of such a file, so a folder that leaves Finder leaves its directory at
	/// the top level of the location, with those files inside. The user is told where it is, and `moveAside` renames it
	/// when the folder comes back, as a folder whose name is taken shows nothing of its own. Nothing is ever removed:
	/// these can be files of the user's that never reached another device.
	enum FilesLeftovers {
		/// How many paths are looked for at most
		static let limit = 100
		/// How many folders of the location `moveAside` looks at; it gives up rather than take one of them for a
		/// leftover
		private static let folderLimit = 1000
		/// How many names `moveAside` tries before it gives up
		private static let attempts = 100

		/// What the location keeps on this Mac, for the settings to tell the user once
		struct Report: Sendable, Equatable {
			/// The folder as the location shows it
			let folder: String
			/// What to show in Finder: the directory that stays, or the one it was moved to
			let url: URL
			let kind: Kind

			enum Kind: Sendable, Equatable {
				/// How many files of a folder that leaves were taken out of the location, with one of their paths
				/// relative to the folder's root and where they are now. `isComplete` is false when the folder's copy
				/// was not looked at in full, so that there can be more than `count`.
				case movedOut(count: Int, example: String, isComplete: Bool, place: String)
				/// The same, for files that could not be taken out and stay in the system's copy
				case staying(count: Int, example: String, isComplete: Bool)
				/// The name the directory of an earlier round has now, next to the folder that came back
				case movedAside(String)
			}
		}

		/// Takes what the location cannot synchronize out of a folder that is leaving it, into a directory of this Mac,
		/// keeping the paths the files have in the folder. macOS removes its copy of a folder with everything in it,
		/// including files it keeps on this device alone, so a file that is nowhere else would be gone. Returns how
		/// many were moved; the rest stay where they are.
		static func moveOut(_ paths: [String], from root: URL, to destination: URL) -> Int {
			let fileManager = FileManager.default
			var moved = 0
			for path in paths {
				let source = root.appending(path: path, directoryHint: .inferFromPath)
				let target = destination.appending(path: path, directoryHint: .inferFromPath)
				do {
					try fileManager.createDirectory(
						at: target.deletingLastPathComponent(), withIntermediateDirectories: true)
					try fileManager.moveItem(at: source, to: target)
					moved += 1
				}
				catch {
					Log.warn(
						"Could not take a file the Synctrain location cannot synchronize out of it: \(error.localizedDescription)")
				}
			}
			return moved
		}

		/// What `moveAside` did with the directory a folder of the label left behind: its new name, or what the
		/// coordinator makes of it
		enum Aside<Moved: Sendable>: Sendable {
			case nothingInTheWay
			case moved(Moved)
			/// The directory is there and could not be renamed, so the folder would show it instead of its own files
			case failed
		}

		/// Renames the directory that a folder of this label left at the top level of the location, so that the folder
		/// shows its own files again. A directory of one of the location's folders is left alone: that one is the
		/// system's own. A folder whose label another folder has is shown under "label (folder ID)"
		/// (`IndexWriter.refreshRootNames`), and a directory it left is not recognized here.
		static func moveAside(label: String, in location: URL, store: IndexStore) -> Aside<String> {
			let fileManager = FileManager.default
			let key = FileNames.collisionKey(label)
			guard let folders = try? store.children(of: nil, limit: folderLimit), folders.count < folderLimit else {
				Log.warn("Could not tell the Synctrain location's folders from what it keeps")
				return .nothingInTheWay
			}
			guard !folders.contains(where: { FileNames.collisionKey($0.name) == key }),
				let left = SystemCopy.names(in: location).first(where: { FileNames.collisionKey($0) == key })
			else {
				return .nothingInTheWay
			}
			// The name is one the file system gave, so it is a name and not a path, as `appending` would take it
			let url = location.appending(path: left, directoryHint: .isDirectory)
			let base = String(localized: "\(label) (not synchronized)")
			guard FileNames.isValid(base) else {
				Log.warn("A folder's name cannot be a name in the Synctrain location")
				return .failed
			}
			for attempt in 1...attempts {
				let name = attempt == 1 ? base : "\(base) \(attempt)"
				let destination = location.appending(path: name, directoryHint: .isDirectory)
				do {
					try fileManager.moveItem(at: url, to: destination)
					Log.info("Moved a directory the Synctrain location cannot synchronize aside")
					return .moved(name)
				}
				catch {
					// Most likely a name that is taken, which the next attempt works around
					if attempt == attempts {
						Log.warn(
							"Could not move a directory the Synctrain location cannot synchronize aside: "
								+ error.localizedDescription)
					}
				}
			}
			return .failed
		}
	}
#endif
