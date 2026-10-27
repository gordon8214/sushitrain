// Copyright (C) 2026 Gordon
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this file,
// You can obtain one at https://mozilla.org/MPL/2.0/.
import Foundation

public enum FileNames {
	/// A key under which names that cannot coexist in one directory collide: canonically composed (NFC) and case folded.
	/// Two entries whose names share a key would refer to the same file on most Apple file systems.
	public static func collisionKey(_ name: String) -> String {
		name.precomposedStringWithCanonicalMapping.folding(options: [.caseInsensitive], locale: nil)
	}

	/// The form in which names are written to disk (canonically composed, as Syncthing normalizes them on Apple file
	/// systems)
	public static func normalized(_ name: String) -> String {
		name.precomposedStringWithCanonicalMapping
	}

	/// Whether the name can be used for a file or directory
	public static func isValid(_ name: String) -> Bool {
		!name.isEmpty && name != "." && name != ".." && !name.contains("/") && !name.contains("\0")
	}

	/// Names of files that belong to Syncthing itself and must never be exposed or created
	public static func isReserved(_ name: String, markerName: String = ".stfolder") -> Bool {
		name == markerName || name == ".stignore" || name == ".stversions" || name.hasPrefix(".syncthing.")
			|| name.hasPrefix("~syncthing~")
	}

	/// Whether any component of the path is reserved
	public static func isReservedPath(_ path: String, markerName: String = ".stfolder") -> Bool {
		path.split(separator: "/").contains { isReserved(String($0), markerName: markerName) }
	}

	/// The last component of a path relative to the folder root ("" for the root itself)
	public static func lastComponent(_ path: String) -> String {
		guard let slash = path.lastIndex(of: "/") else {
			return path
		}
		return String(path[path.index(after: slash)...])
	}

	/// The parent of a path relative to the folder root, or "" when the path is directly below the root
	public static func parentPath(_ path: String) -> String {
		guard let slash = path.lastIndex(of: "/") else {
			return ""
		}
		return String(path[..<slash])
	}

	public static func join(_ parent: String, _ name: String) -> String {
		parent.isEmpty ? name : parent + "/" + name
	}

	/// Whether `path` is `ancestor` or lies below it
	public static func isSelfOrDescendant(_ path: String, of ancestor: String) -> Bool {
		ancestor.isEmpty || path == ancestor || path.hasPrefix(ancestor + "/")
	}

	/// Whether the file name looks like a conflict copy created by Syncthing
	public static func isConflictCopy(_ name: String) -> Bool {
		name.contains(".sync-conflict-")
	}

	/// The name of a conflict copy in the format used by Syncthing: `name.sync-conflict-YYYYMMDD-HHMMSS-SHORTID.ext`
	public static func conflictCopyName(for name: String, date: Date, shortDeviceID: String) -> String {
		var calendar = Calendar(identifier: .gregorian)
		calendar.timeZone = .current
		let c = calendar.dateComponents([.year, .month, .day, .hour, .minute, .second], from: date)
		let stamp = String(
			format: "%04d%02d%02d-%02d%02d%02d", c.year ?? 0, c.month ?? 0, c.day ?? 0, c.hour ?? 0, c.minute ?? 0,
			c.second ?? 0)

		// Same as Go's filepath.Ext: the suffix from the last dot, unless that dot starts the name
		let ext: String
		if let dot = name.lastIndex(of: "."), dot != name.startIndex {
			ext = String(name[dot...])
		}
		else {
			ext = ""
		}
		let base = String(name.dropLast(ext.count))
		return "\(base).sync-conflict-\(stamp)-\(shortDeviceID)\(ext)"
	}
}
