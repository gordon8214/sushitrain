// Copyright (C) 2026 Gordon
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this file,
// You can obtain one at https://mozilla.org/MPL/2.0/.
import Foundation
import Testing
import UniformTypeIdentifiers

@testable import SynctrainFileProviderKit

@Suite struct RemoteContentTests {
	@Test func aRowShowsTheGatewaysVersion() throws {
		let store = try makeRemoteStore()
		let storage = RemoteStorage(container: SharedContainer(baseURL: try makeTemporaryDirectory()))
		try store.snapshot([remoteFile("itm_a", "a.txt")])
		let item = try #require(try store.item(folderID: "docs", path: "a.txt"))

		guard case .remote(let state) = try store.contentSource(of: item, storage: storage) else {
			Issue.record("not the gateway's version")
			return
		}
		#expect(state.gatewayID == "itm_a")
	}

	@Test func anUnsentEditWinsOverTheGatewaysVersion() throws {
		let store = try makeRemoteStore()
		let storage = RemoteStorage(container: SharedContainer(baseURL: try makeTemporaryDirectory()))
		try storage.createDirectories()
		try store.snapshot([remoteFile("itm_a", "a.txt")])
		let item = try #require(try store.item(folderID: "docs", path: "a.txt"))
		let url = storage.url(relativePath: "Staged/edit")
		try Data("edit".utf8).write(to: url)
		let staged = StagedFile(relativePath: "Staged/edit", size: 4, digest: "digest", modifiedNs: 1)
		_ = try store.recordRemoteContentChange(itemID: item.id, modifiedNanoseconds: 1, stagedFile: staged)

		let edited = try #require(try store.item(id: item.id))
		#expect(try store.contentSource(of: edited, storage: storage) == .staged(url))

		// Without its staged file, what is left is the gateway's version
		try FileManager.default.removeItem(at: url)
		guard case .remote = try store.contentSource(of: edited, storage: storage) else {
			Issue.record("not the gateway's version")
			return
		}
	}

	@Test func anUnknownVersionIsNotReportedAsGone() throws {
		let store = try makeRemoteStore()
		let storage = RemoteStorage(container: SharedContainer(baseURL: try makeTemporaryDirectory()))
		try store.snapshot([remoteFile("itm_a", "a.txt")])
		let item = try #require(try store.item(folderID: "docs", path: "a.txt"))
		try store.setRaw("blocks_hash = 'c1.other.1'", id: item.id)
		let stale = try #require(try store.item(id: item.id))
		#expect(try store.contentSource(of: stale, storage: storage) == nil)
	}

	@Test func searchMatchesEveryTermInNames() throws {
		let store = try makeRemoteStore()
		try store.snapshot([
			remoteFile("itm_a", "Holiday Photo.JPG"), remoteFile("itm_b", "photo of a cat.png"),
			remoteDirectory("itm_d", "Photos"), remoteFile("itm_c", "Photos/notes.txt", parent: "itm_d"),
		])
		func names(_ terms: String...) throws -> [String] {
			try store.searchItems(terms: terms).map(\.name)
		}
		#expect(try names("photo") == ["Holiday Photo.JPG", "photo of a cat.png", "Photos"])
		#expect(try names("PHOTO", "cat") == ["photo of a cat.png"])
		#expect(try names("notes") == ["notes.txt"], "by name, in any directory")
		#expect(try names("Documents").isEmpty, "the folder root is not a result")
		#expect(try names().isEmpty && names("").isEmpty)
		#expect(try store.searchItems(terms: ["photo"], folderID: "other").isEmpty)
		#expect(try store.searchItems(terms: ["photo"], limit: 2).count == 2)
	}

	@Test func searchTakesWildcardsLiterally() throws {
		let store = try makeRemoteStore()
		try store.snapshot([remoteFile("itm_a", "100%.txt"), remoteFile("itm_b", "a_b.txt"), remoteFile("itm_c", "axb.txt")])
		#expect(try store.searchItems(terms: ["%"]).map(\.name) == ["100%.txt"])
		#expect(try store.searchItems(terms: ["a_b"]).map(\.name) == ["a_b.txt"])
	}

	@Test func deletedItemsAreNotFound() throws {
		let store = try makeRemoteStore()
		try store.snapshot([remoteFile("itm_a", "a.txt")])
		try store.snapshot([], cursor: "g2.1")
		#expect(try store.searchItems(terms: ["a.txt"]).isEmpty)
	}

	@Test func thumbnailFeaturesFollowTheType() {
		#expect(Capabilities.thumbnailFeature(for: .jpeg) == .imageThumbnails)
		#expect(Capabilities.thumbnailFeature(for: .mpeg4Movie) == .videoThumbnails)
		#expect(Capabilities.thumbnailFeature(for: .pdf) == .pdfThumbnails)
		#expect(Capabilities.thumbnailFeature(for: .plainText) == nil)
	}

	/// Three cached files of 8 KiB each, the oldest first
	private func cachedFiles() throws -> (RemoteStorage, [URL]) {
		let storage = RemoteStorage(container: SharedContainer(baseURL: try makeTemporaryDirectory()))
		try storage.createDirectories()
		let urls = try (0..<3).map { index in
			let url = storage.contentURL.appending(path: "file-\(index)")
			try Data(repeating: 1, count: 8192).write(to: url)
			try FileManager.default.setAttributes(
				[.modificationDate: Date(timeIntervalSince1970: Double(1000 + index))],
				ofItemAtPath: url.path(percentEncoded: false))
			return url
		}
		return (storage, urls)
	}

	private func exists(_ url: URL) -> Bool {
		FileManager.default.fileExists(atPath: url.path(percentEncoded: false))
	}

	@Test func trimmingRemovesWhatWasUsedLongestAgo() throws {
		let (storage, urls) = try cachedFiles()
		let total = storage.contentByteCount
		#expect(total >= 3 * 8192)

		storage.trimContent(toBytes: total)
		#expect(urls.allSatisfy(exists), "within the limit")

		storage.trimContent(toBytes: total - 1)
		#expect(!exists(urls[0]) && exists(urls[1]) && exists(urls[2]))
		#expect(storage.contentByteCount < total)
	}

	@Test func usingAFileKeepsItLonger() throws {
		let (storage, urls) = try cachedFiles()
		storage.markContentUsed(urls[0])
		storage.trimContent(toBytes: storage.contentByteCount - 1)
		#expect(exists(urls[0]) && !exists(urls[1]) && exists(urls[2]))
	}

	@Test func trimmingAnEmptyCacheDoesNothing() throws {
		let storage = RemoteStorage(container: SharedContainer(baseURL: try makeTemporaryDirectory()))
		storage.trimContent(toBytes: 0)
		#expect(storage.contentByteCount == 0)
	}
}
