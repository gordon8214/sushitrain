// Copyright (C) 2026 Gordon
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this file,
// You can obtain one at https://mozilla.org/MPL/2.0/.
import Darwin
import Foundation
import Testing

@testable import SynctrainFileProviderKit

@Suite struct FileNamesTests {
	@Test func collisionKeys() {
		#expect(FileNames.collisionKey("Report.TXT") == FileNames.collisionKey("report.txt"))
		#expect(FileNames.collisionKey("Cafe\u{301}") == FileNames.collisionKey("Caf\u{e9}"))
		#expect(FileNames.collisionKey("a") != FileNames.collisionKey("b"))
	}

	@Test func namesAreStoredDecomposed() {
		// Syncthing stores names decomposed on Apple platforms; the index compares paths byte by byte
		#expect(Array(FileNames.normalized("Caf\u{e9}/\u{c5}r.txt").utf8) == Array("Cafe\u{301}/A\u{30a}r.txt".utf8))
		#expect(Array(FileNames.normalized("Cafe\u{301}").utf8) == Array("Cafe\u{301}".utf8))
		#expect(FileNames.collisionKey("Caf\u{e9}") == FileNames.collisionKey(FileNames.normalized("Caf\u{e9}")))
	}

	@Test func validPaths() {
		for path in ["", "a", "a/b.txt", "Cafe\u{301}/x"] {
			#expect(FileNames.isValidPath(path))
		}
		for path in ["..", "../x", "a/../b", "a/./b", "/a", "a/", "a//b", "a\0b/c"] {
			#expect(!FileNames.isValidPath(path), "\(path)")
		}
	}

	@Test func reservedNames() {
		for name in [".stfolder", ".stignore", ".stversions", ".syncthing.file.tmp", "~syncthing~file.tmp"] {
			#expect(FileNames.isReserved(name))
		}
		#expect(!FileNames.isReserved("stfolder") && !FileNames.isReserved(".hidden"))
		#expect(FileNames.isReservedPath("a/.stversions/b"))
		#expect(!FileNames.isReservedPath("a/b/c"))
	}

	@Test func paths() {
		#expect(FileNames.lastComponent("a/b/c.txt") == "c.txt")
		#expect(FileNames.lastComponent("c.txt") == "c.txt")
		#expect(FileNames.parentPath("a/b/c.txt") == "a/b")
		#expect(FileNames.parentPath("c.txt") == "")
		#expect(FileNames.join("", "x") == "x" && FileNames.join("a", "x") == "a/x")
		#expect(FileNames.isSelfOrDescendant("a/b", of: "a") && FileNames.isSelfOrDescendant("a", of: "a"))
		#expect(!FileNames.isSelfOrDescendant("ab", of: "a"))
	}

	@Test func platformMetadataNames() {
		#if os(macOS)
			for name in [".DS_Store", "._photo.jpg", ".Spotlight-V100", ".fseventsd", ".Trashes", ".TemporaryItems", "Icon\r"] {
				#expect(FileNames.isPlatformMetadata(name), "\(name)")
			}
		#endif
		for name in ["DS_Store", ".hidden", "Icon", "_photo.jpg", "notes.txt"] {
			#expect(!FileNames.isPlatformMetadata(name), "\(name)")
		}
	}

	@Test func conflictCopyNames() throws {
		let date = try #require(
			Calendar(identifier: .gregorian).date(
				from: DateComponents(timeZone: .current, year: 2026, month: 3, day: 4, hour: 5, minute: 6, second: 7)))
		#expect(
			FileNames.conflictCopyName(for: "notes.txt", date: date, shortDeviceID: "ABCDEFG")
				== "notes.sync-conflict-20260304-050607-ABCDEFG.txt")
		#expect(
			FileNames.conflictCopyName(for: "archive.tar.gz", date: date, shortDeviceID: "ABCDEFG")
				== "archive.tar.sync-conflict-20260304-050607-ABCDEFG.gz")
		#expect(
			FileNames.conflictCopyName(for: ".profile", date: date, shortDeviceID: "ABCDEFG")
				== ".profile.sync-conflict-20260304-050607-ABCDEFG")
		#expect(FileNames.isConflictCopy("notes.sync-conflict-20260304-050607-ABCDEFG.txt"))
	}
}

@Suite struct SupportTests {
	func temporaryDirectory() throws -> URL {
		let dir = FileManager.default.temporaryDirectory.appending(path: "kit-support-\(UUID().uuidString)")
		try FileManager.default.createDirectory(at: dir, withIntermediateDirectories: true)
		return dir
	}

	@Test func sharedContainerLayout() {
		let container = SharedContainer(baseURL: URL(filePath: "/group", directoryHint: .isDirectory))
		#expect(container.indexURL.path(percentEncoded: false) == "/group/FileProvider/gateway.sqlite")
	}

	@Test func darwinNotifications() async throws {
		let received = AsyncStream<Void>.makeStream()
		let observer = SharedNotificationObserver(.workQueued) { received.continuation.yield() }
		SharedNotification.workQueued.post()
		var iterator = received.stream.makeAsyncIterator()
		let result: Void? = await iterator.next()
		#expect(result != nil)
		_ = observer
	}
}
