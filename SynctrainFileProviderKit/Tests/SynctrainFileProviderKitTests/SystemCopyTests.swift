// Copyright (C) 2026 Gordon
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this file,
// You can obtain one at https://mozilla.org/MPL/2.0/.
import Darwin
import Foundation
import Testing

@testable import SynctrainFileProviderKit

/// What the system keeps in its copy of a folder that the folder itself does not: the files the location cannot
/// synchronize, which stay on the device and keep their directories from being removed
@Suite struct SystemCopyTests {
	/// Writes the paths below a new directory, directories being the ones that end in "/". Names go to the file system
	/// as they are given, so a precomposed name stays precomposed.
	private func makeTree(_ paths: [String]) throws -> URL {
		let root = try makeTemporaryDirectory()
		for path in paths {
			let location = root.path(percentEncoded: false) + "/" + path
			let components = path.split(separator: "/").map(String.init)
			var directory = root.path(percentEncoded: false)
			for component in components.dropLast() {
				directory += "/" + component
				mkdir(directory, 0o755)
			}
			if path.hasSuffix("/") {
				#expect(mkdir(String(location.dropLast()), 0o755) == 0, "\(path)")
			}
			else {
				let descriptor = open(location, O_CREAT | O_WRONLY, 0o644)
				#expect(descriptor >= 0, "\(path)")
				close(descriptor)
			}
		}
		return root
	}

	@Test func unknownPathsFindsWhatTheFolderDoesNotHave() throws {
		let store = try makeRemoteStore()
		try store.snapshot([
			remoteDirectory("itm_docs", "docs"), remoteFile("itm_a", "docs/a.txt", parent: "itm_docs"),
			remoteDirectory("itm_deep", "docs/deep", parent: "itm_docs"),
		])
		let root = try makeTree([
			"docs/a.txt", "docs/._a.txt", "docs/deep/", "docs/deep/.DS_Store", "docs/deep/with a space ",
			".DS_Store", "Icon\r",
		])

		let found = try SystemCopy.unknownPaths(folderID: "docs", root: root, store: store)
		#expect(found.paths == ["Icon\r", "docs/._a.txt", "docs/deep/with a space "])
		#expect(found.isComplete)
	}

	/// What is inside a directory that stays goes with it, so only the directory is named
	@Test func unknownPathsNamesOnlyTheTopOfASubtree() throws {
		let store = try makeRemoteStore()
		try store.snapshot([remoteDirectory("itm_docs", "docs")])
		let root = try makeTree(["docs/kept/inside/deep.txt", "docs/._a.txt"])

		#expect(
			try SystemCopy.unknownPaths(folderID: "docs", root: root, store: store).paths == [
				"docs/._a.txt", "docs/kept",
			])
	}

	/// A path the index has as a file is not walked into, whatever is there
	@Test func unknownPathsWalksOnlyWhatTheFolderHasAsADirectory() throws {
		let store = try makeRemoteStore()
		try store.snapshot([
			remoteDirectory("itm_docs", "docs"), remoteFile("itm_odd", "docs/odd", parent: "itm_docs"),
		])
		let root = try makeTree(["docs/odd/._inside"])

		#expect(try SystemCopy.unknownPaths(folderID: "docs", root: root, store: store).paths.isEmpty)
	}

	/// An item the folder deleted is a tombstone in the index, not something the copy may keep
	@Test func unknownPathsCountsADeletedItem() throws {
		let store = try makeRemoteStore()
		try store.snapshot([
			remoteDirectory("itm_docs", "docs"), remoteFile("itm_a", "docs/a.txt", parent: "itm_docs"),
		])
		try store.changes([remoteFile("itm_a", "docs/a.txt", parent: "itm_docs", deleted: true)])
		let root = try makeTree(["docs/a.txt"])

		#expect(try SystemCopy.unknownPaths(folderID: "docs", root: root, store: store).paths == ["docs/a.txt"])
	}

	/// The file system has a name as it was written; the index has it decomposed, as Syncthing normalizes it
	@Test func unknownPathsMatchesAPrecomposedNameOnDisk() throws {
		let store = try makeRemoteStore()
		try store.snapshot([
			remoteDirectory("itm_docs", "docs"),
			remoteFile("itm_cafe", FileNames.normalized("docs/Café.txt"), parent: "itm_docs"),
		])
		let root = try makeTree(["docs/Caf\u{e9}.txt"])

		#expect(try SystemCopy.unknownPaths(folderID: "docs", root: root, store: store).paths.isEmpty)
	}

	@Test func unknownPathsStopsAtTheLimitAndSaysSo() throws {
		let store = try makeRemoteStore()
		try store.snapshot([remoteDirectory("itm_docs", "docs")])
		let root = try makeTree((0..<10).map { "docs/._\($0)" })

		let found = try SystemCopy.unknownPaths(folderID: "docs", root: root, store: store, limit: 4)
		#expect(found.paths == ["docs/._0", "docs/._1", "docs/._2", "docs/._3"])
		#expect(!found.isComplete)
	}

	/// The budget cuts a walk short, with what it found so far
	@Test func unknownPathsKeepsWhatItFoundWhenTheBudgetIsGone() throws {
		let store = try makeRemoteStore()
		try store.snapshot([remoteDirectory("itm_docs", "docs")])
		let root = try makeTree(["docs/._a"])

		let found = try SystemCopy.unknownPaths(folderID: "docs", root: root, store: store, budget: 0)
		#expect(found.paths.isEmpty && !found.isComplete)
	}

	/// More entries in one directory than a single statement asks about
	@Test func unknownPathsAsksAboutMoreEntriesThanOneStatementTakes() throws {
		let store = try makeRemoteStore()
		try store.snapshot([remoteDirectory("itm_docs", "docs")])
		let names = (0..<IndexStore.maximumPathsPerQuery + 20).map { "docs/._\(String(format: "%04d", $0))" }
		let root = try makeTree(names)

		let found = try SystemCopy.unknownPaths(folderID: "docs", root: root, store: store, limit: names.count)
		#expect(found.paths.count == names.count && found.isComplete)
	}

	/// Nothing is reported for a folder whose copy holds only what it has
	@Test func unknownPathsFindsNothingWhenEverythingIsSynchronized() throws {
		let store = try makeRemoteStore()
		try store.snapshot([
			remoteDirectory("itm_docs", "docs"), remoteFile("itm_a", "docs/a.txt", parent: "itm_docs"),
		])
		let root = try makeTree(["docs/a.txt", "docs/.DS_Store"])

		let found = try SystemCopy.unknownPaths(folderID: "docs", root: root, store: store)
		#expect(found.paths.isEmpty && found.isComplete)
	}

	/// A directory that cannot be read may hold more than was found
	@Test func unknownPathsSaysWhenADirectoryCannotBeRead() throws {
		let store = try makeRemoteStore()
		try store.snapshot([remoteDirectory("itm_docs", "docs")])
		let root = try makeTree(["docs/"])
		#expect(chmod(root.path(percentEncoded: false) + "/docs", 0) == 0)
		defer { chmod(root.path(percentEncoded: false) + "/docs", 0o755) }

		let found = try SystemCopy.unknownPaths(folderID: "docs", root: root, store: store)
		#expect(found.paths.isEmpty && !found.isComplete)
	}

	@Test func namesReadsWhatFileManagerHides() throws {
		let root = try makeTree(["a.txt", "._a.txt", "deep/"])

		#expect(Set(SystemCopy.names(in: root)) == ["a.txt", "._a.txt", "deep"])
		#expect(SystemCopy.names(in: root.appending(path: "nowhere")).isEmpty)
		let hidden = try FileManager.default.contentsOfDirectory(atPath: root.path(percentEncoded: false))
		#expect(
			!hidden.contains("._a.txt"),
			"FileManager leaves AppleDouble files out, which is why this reads the directory itself")
	}
}
