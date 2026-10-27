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

	@Test func heartbeat() throws {
		let url = try temporaryDirectory().appending(path: "heartbeat.json")
		#expect(Heartbeat.read(from: url) == nil)

		let now = Date()
		let heartbeat = Heartbeat(awakeUntil: now.addingTimeInterval(5), port: 1234, token: "secret")
		try heartbeat.write(to: url)
		let read = try #require(Heartbeat.read(from: url))
		#expect(read.port == 1234 && read.token == "secret")
		#expect(read.isFresh(at: now) && !read.isFresh(at: now.addingTimeInterval(6)))
		#expect(!Heartbeat(awakeUntil: now.addingTimeInterval(5), port: 0, token: "x").isFresh(at: now))

		Heartbeat.clear(at: url)
		#expect(Heartbeat.read(from: url) == nil)
	}

	@Test func fingerprint() throws {
		let url = try temporaryDirectory().appending(path: "file.bin")
		try Data(repeating: 7, count: 10).write(to: url)
		let path = url.path(percentEncoded: false)
		var times = [
			timespec(tv_sec: 1_700_000_000, tv_nsec: 123_456_789), timespec(tv_sec: 1_700_000_000, tv_nsec: 123_456_789),
		]
		#expect(utimensat(AT_FDCWD, path, &times, 0) == 0)

		let fingerprint = try #require(FileFingerprint(path: path))
		#expect(fingerprint.isRegularFile && fingerprint.size == 10)
		#expect(fingerprint.matches(size: 10, modifiedNanoseconds: 1_700_000_000_123_456_789))
		#expect(!fingerprint.matches(size: 10, modifiedNanoseconds: 1_700_000_000_123_456_788))
		#expect(
			fingerprint.matches(size: 10, modifiedNanoseconds: 1_700_000_000_000_000_000, windowNanoseconds: 1_000_000_000))
		#expect(!fingerprint.matches(size: 11, modifiedNanoseconds: 1_700_000_000_123_456_789))

		let fd = open(path, O_RDONLY)
		defer { close(fd) }
		#expect(FileFingerprint(fileDescriptor: fd) == fingerprint)
		#expect(FileFingerprint(path: path + ".missing") == nil)

		#expect(fingerprint.inode != 0 && !fingerprint.isDirectory && fingerprint.birthNanoseconds > 0)
		let directory = try #require(FileFingerprint(path: url.deletingLastPathComponent().path(percentEncoded: false)))
		#expect(directory.isDirectory && !directory.isRegularFile)
	}

	/// APFS keeps times before 1677 (it clamps them to about -9.2e9 seconds), which do not fit in nanoseconds
	@Test func fingerprintOfAncientFiles() throws {
		var st = stat()
		st.st_mode = S_IFREG | 0o644
		st.st_mtimespec = timespec(tv_sec: -11_644_473_600, tv_nsec: 0)  // 1601, the zero time of Windows
		st.st_birthtimespec = timespec(tv_sec: -9_223_372_037, tv_nsec: 0)
		var ancient = FileFingerprint(st)
		#expect(ancient.modifiedNanoseconds == .min && ancient.birthNanoseconds == .min)
		#expect(!ancient.matches(size: 0, modifiedNanoseconds: 1_700_000_000_000_000_000, windowNanoseconds: 1))
		#expect(ancient.matches(size: 0, modifiedNanoseconds: .min))

		st.st_mtimespec = timespec(tv_sec: 9_300_000_000, tv_nsec: 999_999_999)
		ancient = FileFingerprint(st)
		#expect(ancient.modifiedNanoseconds == .max)
		#expect(!ancient.matches(size: 0, modifiedNanoseconds: -1, windowNanoseconds: .max))
		#expect(!ancient.matches(size: 0, modifiedNanoseconds: .max, windowNanoseconds: -1))
		#expect(FileFingerprint.nanoseconds(timespec(tv_sec: -9_223_372_036, tv_nsec: -854_775_808)) == .min)
		#expect(FileFingerprint.nanoseconds(timespec(tv_sec: -1, tv_nsec: 500_000_000)) == -500_000_000)

		// The same through the file system
		let url = try temporaryDirectory().appending(path: "old.bin")
		try Data([1]).write(to: url)
		var times = [timespec(tv_sec: -11_644_473_600, tv_nsec: 0), timespec(tv_sec: -11_644_473_600, tv_nsec: 0)]
		#expect(utimensat(AT_FDCWD, url.path(percentEncoded: false), &times, 0) == 0)
		let onDisk = try #require(FileFingerprint(path: url.path(percentEncoded: false)))
		#expect(onDisk.modifiedNanoseconds == .min && onDisk.size == 1)
	}

	@Test func sharedContainerLayout() {
		let container = SharedContainer(baseURL: URL(filePath: "/group", directoryHint: .isDirectory))
		#expect(container.folderURL(folderID: "abc")?.path(percentEncoded: false) == "/group/Synctrain/Folders/abc/")
		#expect(
			container.fileURL(folderID: "abc", path: "d/e.txt").path(percentEncoded: false)
				== "/group/Synctrain/Folders/abc/d/e.txt")
		#expect(container.indexURL.path(percentEncoded: false) == "/group/FileProvider/index.sqlite")
		#expect(
			container.validatedFileURL(folderID: "abc", path: "")?.path(percentEncoded: false)
				== "/group/Synctrain/Folders/abc/")
	}

	/// Folder IDs come from other devices: none may place a folder outside the folders directory
	@Test func unsafeFolderIDs() {
		let container = SharedContainer(baseURL: URL(filePath: "/group", directoryHint: .isDirectory))
		for id in ["..", "../x", "a/b", ".", "", "a\0b", "../../FileProvider"] {
			#expect(!SharedContainer.isValidFolderID(id))
			#expect(container.folderURL(folderID: id) == nil)
			#expect(container.validatedFileURL(folderID: id, path: "x") == nil)
			#expect(container.fileURL(folderID: id, path: "x") == SharedContainer.unusableURL)
			#expect(container.fileURL(folderID: id, path: "") == SharedContainer.unusableURL)
		}
		for path in ["..", "../other/x", "d/../../x", "/etc"] {
			#expect(container.validatedFileURL(folderID: "abc", path: path) == nil)
			#expect(container.fileURL(folderID: "abc", path: path) == SharedContainer.unusableURL)
		}
		#expect(SharedContainer.isValidFolderID("abcde-12345") && SharedContainer.isValidFolderID("..."))
		#expect(!SharedContainer.unusableURL.path(percentEncoded: false).hasPrefix("/group"))
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
