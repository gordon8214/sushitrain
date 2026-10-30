// Copyright (C) 2026 Gordon
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this file,
// You can obtain one at https://mozilla.org/MPL/2.0/.
import Foundation
import Testing

@testable import SynctrainHostKit

/// A fake system layout: shared caches and temporary directories that also hold other apps' files.
private final class Fixture {
	let root: URL
	let systemCaches: URL
	let systemTemporary: URL
	let groupCaches: URL
	let locations: AppLocations

	init(withGroup: Bool = true) throws {
		self.root = FileManager.default.temporaryDirectory.appending(path: "hostkit-tests-\(UUID().uuidString)")
		self.systemCaches = root.appending(path: "Caches")
		self.systemTemporary = root.appending(path: "tmp")
		self.groupCaches = root.appending(path: "Group/Library/Caches")
		for directory in [systemCaches, systemTemporary, groupCaches] {
			try FileManager.default.createDirectory(at: directory, withIntermediateDirectories: true)
		}
		self.locations = AppLocations(
			bundleIdentifier: "test.Synctrain", systemCaches: systemCaches, systemTemporary: systemTemporary,
			groupCaches: withGroup ? groupCaches : nil)
	}

	deinit {
		try? FileManager.default.removeItem(at: root)
	}

	/// Writes a file, or a folder holding one when `folder` is set, as the app's temporary items are
	func write(_ url: URL, folder: Bool = false, modified: Date? = nil) throws {
		let file = folder ? url.appending(path: "content") : url
		try FileManager.default.createDirectory(at: file.deletingLastPathComponent(), withIntermediateDirectories: true)
		try Data("x".utf8).write(to: file)
		if let modified {
			try FileManager.default.setAttributes([.modificationDate: modified], ofItemAtPath: url.path)
		}
	}

	func exists(_ url: URL) -> Bool {
		FileManager.default.fileExists(atPath: url.path)
	}
}

private let old = Date().addingTimeInterval(-3 * 86400)
private let dayAgo = Date().addingTimeInterval(-86400)

@Test func locationsAreSubdirectoriesNamedAfterTheBundle() throws {
	let f = try Fixture()
	#expect(f.locations.caches.path == f.systemCaches.appending(path: "test.Synctrain").path)
	#expect(f.locations.temporary.path == f.systemTemporary.appending(path: "test.Synctrain").path)
	#expect(f.locations.thumbnails.path == f.systemCaches.appending(path: "test.Synctrain/thumbnails").path)
}

@Test func bundleIdentifiersMustBeARealPathComponent() {
	#expect(AppLocations.isValidBundleIdentifier("gordonbyrnes.Sushitrain"))
	#expect(AppLocations.isValidBundleIdentifier("a-b.c1"))
	for invalid in ["", ".", "..", "...", "a/b", "a b", "../x"] {
		#expect(!AppLocations.isValidBundleIdentifier(invalid), "\(invalid)")
	}
}

@Test func clearingKeepsOtherAppsFiles() throws {
	let f = try Fixture()
	try f.locations.prepare()
	let otherCache = f.systemCaches.appending(path: "com.example.Other")
	let otherTemp = f.systemTemporary.appending(path: "other-app-folder")
	let otherGroupItem = f.groupCaches.appending(path: "Unrelated")
	let ownCache = f.locations.thumbnails.appending(path: "a")
	let ownTemp = f.locations.temporary.appending(path: "Downloads-1")
	let handoff = f.groupCaches.appending(path: "SynctrainShare-1")
	for url in [otherCache, otherTemp, otherGroupItem, ownCache, ownTemp, handoff] {
		try f.write(url, folder: true)
	}

	#expect(f.locations.clearCaches().isEmpty)
	#expect(f.locations.clearTemporaryFiles().isEmpty)

	#expect(!f.exists(ownCache))
	#expect(!f.exists(ownTemp))
	#expect(!f.exists(handoff))
	#expect(f.exists(otherCache))
	#expect(f.exists(otherTemp))
	#expect(f.exists(otherGroupItem))
	#expect(f.exists(f.locations.caches))
	#expect(f.exists(f.locations.temporary))
}

@Test func clearingKeepsItemsInUse() throws {
	let f = try Fixture()
	try f.locations.prepare()
	let pendingShare = f.groupCaches.appending(path: "SynctrainShare-pending")
	let importedShare = f.groupCaches.appending(path: "SynctrainShare-done")
	let download = f.locations.temporary.appending(path: "Downloads-1")
	for url in [pendingShare, importedShare, download] {
		try f.write(url, folder: true)
	}

	#expect(f.locations.clearTemporaryFiles(keeping: [pendingShare.appending(path: "content")]).isEmpty)

	#expect(f.exists(pendingShare.appending(path: "content")))
	#expect(!f.exists(importedShare))
	#expect(!f.exists(download))
}

@Test func clearingRefusesADirectoryWithAnotherName() throws {
	let f = try Fixture()
	let other = f.systemCaches.appending(path: "other-file")
	try f.write(other)
	// A wrong path, such as the shared caches root itself, is never emptied.
	#expect(!AppLocations.removeContents(of: f.systemCaches, named: "test.Synctrain").isEmpty)
	#expect(f.exists(other))
}

@Test func clearingFollowsNoSymbolicLinks() throws {
	let f = try Fixture()
	let elsewhere = f.root.appending(path: "Elsewhere")
	let target = elsewhere.appending(path: "keep")
	try f.write(target)

	// The app's own folder is a link to another directory: nothing is removed
	try FileManager.default.createSymbolicLink(at: f.locations.caches, withDestinationURL: elsewhere)
	#expect(!f.locations.clearCaches().isEmpty)
	#expect(f.exists(target))

	// A link inside the app's folder is removed, its target is not
	try FileManager.default.createDirectory(at: f.locations.temporary, withIntermediateDirectories: true)
	let link = f.locations.temporary.appending(path: "link")
	try FileManager.default.createSymbolicLink(at: link, withDestinationURL: elsewhere)
	#expect(f.locations.clearTemporaryFiles().isEmpty)
	#expect((try? FileManager.default.destinationOfSymbolicLink(atPath: link.path)) == nil)
	#expect(f.exists(target))
}

@Test func clearingMissingDirectoriesIsNotAnError() throws {
	let f = try Fixture()
	#expect(f.locations.clearCaches().isEmpty)
	#expect(f.locations.clearTemporaryFiles().isEmpty)
	#expect(f.locations.pruneTemporaryFiles(modifiedBefore: dayAgo, includingLegacyItems: true).isEmpty)
}

@Test func withoutAnAppGroupOnlyTheTemporaryDirectoryIsCleared() throws {
	let f = try Fixture(withGroup: false)
	try f.locations.prepare()
	let handoff = f.groupCaches.appending(path: "SynctrainShare-1")
	try f.write(handoff, folder: true, modified: old)
	#expect(f.locations.clearTemporaryFiles().isEmpty)
	#expect(f.locations.pruneTemporaryFiles(modifiedBefore: dayAgo, includingLegacyItems: false).isEmpty)
	#expect(f.exists(handoff))
	#expect(f.locations.temporaryDirectories.map(\.lastPathComponent) == ["test.Synctrain"])
	#expect(f.locations.shareHandoffDirectory(containing: handoff.appending(path: "content")) == nil)
}

@Test func pruningRemovesOnlyOldItems() throws {
	let f = try Fixture()
	try f.locations.prepare()
	let oldTemp = f.locations.temporary.appending(path: "old-folder")
	let newTemp = f.locations.temporary.appending(path: "new-folder")
	let oldHandoff = f.groupCaches.appending(path: "SynctrainShare-old")
	let newHandoff = f.groupCaches.appending(path: "SynctrainShare-new")
	let oldGroupItem = f.groupCaches.appending(path: "Unrelated")
	let oldOther = f.systemTemporary.appending(path: "other-old-file")
	try f.write(oldTemp, folder: true, modified: old)
	try f.write(newTemp, folder: true)
	try f.write(oldHandoff, folder: true, modified: old)
	try f.write(newHandoff, folder: true)
	try f.write(oldGroupItem, folder: true, modified: old)
	try f.write(oldOther, modified: old)

	#expect(f.locations.pruneTemporaryFiles(modifiedBefore: dayAgo, includingLegacyItems: true).isEmpty)

	#expect(!f.exists(oldTemp))
	#expect(f.exists(newTemp))
	#expect(!f.exists(oldHandoff))
	#expect(f.exists(newHandoff))
	#expect(f.exists(oldGroupItem))
	#expect(f.exists(oldOther))
}

@Test func pruningRemovesOldLegacyItemsOnlyWhenAsked() throws {
	let f = try Fixture()
	let legacy = ["Downloads-1", "Export-1", "thumbnail-1", UUID().uuidString].map {
		f.systemTemporary.appending(path: $0)
	}
	let legacyBundle = f.systemTemporary.appending(path: "Synctrain-support-bundle-1.zip")
	let recentLegacy = f.systemTemporary.appending(path: "Downloads-recent")
	let other = f.systemTemporary.appending(path: "Other-1")
	for url in legacy + [other] {
		try f.write(url, folder: true, modified: old)
	}
	try f.write(legacyBundle, modified: old)
	try f.write(recentLegacy, folder: true)

	#expect(f.locations.pruneTemporaryFiles(modifiedBefore: dayAgo, includingLegacyItems: false).isEmpty)
	#expect((legacy + [legacyBundle]).allSatisfy(f.exists))

	#expect(f.locations.pruneTemporaryFiles(modifiedBefore: dayAgo, includingLegacyItems: true).isEmpty)
	#expect(!(legacy + [legacyBundle]).contains(where: f.exists))
	#expect(f.exists(recentLegacy))
	#expect(f.exists(other))
}

@Test func legacyThumbnailsMoveOrAreRemoved() throws {
	let f = try Fixture()
	let legacy = f.systemCaches.appending(path: "thumbnails/a/thumb.jpg")
	try f.write(legacy)

	try f.locations.moveLegacyThumbnails()
	#expect(!f.exists(legacy))
	#expect(f.exists(f.locations.thumbnails.appending(path: "a/thumb.jpg")))

	// Again, with nothing to move
	try f.locations.moveLegacyThumbnails()
	#expect(f.exists(f.locations.thumbnails.appending(path: "a/thumb.jpg")))

	// After an earlier move failed and new thumbnails were written, the old cache is removed
	let stale = f.systemCaches.appending(path: "thumbnails/b/thumb.jpg")
	try f.write(stale)
	try f.locations.moveLegacyThumbnails()
	#expect(!f.exists(f.systemCaches.appending(path: "thumbnails")))
	#expect(f.exists(f.locations.thumbnails.appending(path: "a/thumb.jpg")))
}

@Test func prepareCanRunAgain() throws {
	let f = try Fixture()
	try f.locations.prepare()
	try f.write(f.locations.temporary.appending(path: "file"))
	try f.locations.prepare()
	#expect(f.exists(f.locations.temporary.appending(path: "file")))
}

@Test func temporaryDirectoriesIncludeStagedShares() throws {
	let f = try Fixture()
	try f.locations.prepare()
	try f.write(f.groupCaches.appending(path: "SynctrainShare-1"), folder: true)
	try f.write(f.groupCaches.appending(path: "Unrelated"), folder: true)
	#expect(Set(f.locations.temporaryDirectories.map(\.lastPathComponent)) == ["test.Synctrain", "SynctrainShare-1"])
}

@Test func shareHandoffsAreFoundFromTheirFiles() throws {
	let f = try Fixture()
	let handoff = try AppLocations.makeShareHandoffDirectory(in: f.groupCaches)
	#expect(handoff.lastPathComponent.hasPrefix(AppLocations.shareHandoffPrefix))
	#expect(f.exists(handoff))
	let file = handoff.appending(path: "shared.txt")
	#expect(f.locations.shareHandoffDirectory(containing: file)?.path == handoff.path)
	#expect(f.locations.shareHandoffDirectory(containing: f.groupCaches.appending(path: "Unrelated/x")) == nil)
	#expect(f.locations.shareHandoffDirectory(containing: f.locations.temporary.appending(path: "x")) == nil)
	#expect(f.locations.shareHandoffDirectory(containing: f.groupCaches) == nil)
}

@Test func makeTemporaryDirectoryCreatesAUniqueDirectoryInside() throws {
	let f = try Fixture()
	let a = try f.locations.makeTemporaryDirectory(prefix: "Downloads-")
	let b = try f.locations.makeTemporaryDirectory(prefix: "Downloads-")
	#expect(a != b)
	#expect(a.deletingLastPathComponent().standardizedFileURL == f.locations.temporary.standardizedFileURL)
	#expect(a.lastPathComponent.hasPrefix("Downloads-"))
	#expect(f.exists(a))
}
