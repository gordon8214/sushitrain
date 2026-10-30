// Copyright (C) 2026 Gordon
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this file,
// You can obtain one at https://mozilla.org/MPL/2.0/.
import CryptoKit
import Foundation
import Testing

@testable import SynctrainFileProviderKit

let testServiceID = "svc_nfawJqjGwm48Os0jcKFukw"
let testRemoteFolder = "Documents"
let testRootGatewayID = "itm_SBNJTRN-FjG7owHVrKtuew"
let testEndpoint = RemoteEndpoint(serviceID: testServiceID, host: "gateway.test", port: 20920, displayName: "truenas")

/// The fixtures shared with the gateway (Docs/remote-api/fixtures)
let fixturesURL = URL(filePath: #filePath).deletingLastPathComponent().deletingLastPathComponent()
	.deletingLastPathComponent().deletingLastPathComponent().deletingLastPathComponent()
	.appending(path: "Docs/remote-api/fixtures", directoryHint: .isDirectory)

func fixture<T: Decodable>(_ name: String, as type: T.Type = T.self) throws -> T {
	try RemoteJSON.decoder().decode(T.self, from: Data(contentsOf: fixturesURL.appending(path: name)))
}

func makeTemporaryDirectory() throws -> URL {
	let dir = FileManager.default.temporaryDirectory.appending(path: "kit-remote-\(UUID().uuidString)")
	try FileManager.default.createDirectory(at: dir, withIntermediateDirectories: true)
	return dir
}

/// A store with the local folder "docs" bound to the gateway's "Documents"
func makeRemoteStore(write: Bool = true) throws -> IndexStore {
	let store = try makeStore()
	try store.saveRemoteEndpoint(testEndpoint)
	try store.bindRemoteFolder(
		RemoteFolderBinding(
			folderID: "docs", serviceID: testServiceID, remoteFolderID: testRemoteFolder,
			permissions: RemotePermissions(read: true, write: write)),
		folder: FolderDescription(folderID: "docs", label: "Documents"),
		rootGatewayID: testRootGatewayID)
	return store
}

func revision(_ seed: String) -> RemoteRevision {
	RemoteRevision("r1." + Data(SHA256.hash(data: Data(seed.utf8))).base64URLEncodedString())
}

func contentID(_ bytes: Data) -> ContentIdentity {
	ContentIdentity(blocksHash: Data(SHA256.hash(data: Data(SHA256.hash(data: bytes)))), size: Int64(bytes.count))
}

func remoteFile(
	_ id: String, _ path: String, parent: String = testRootGatewayID, rev: String? = nil, size: Int64 = 3,
	content: String = "abc", deleted: Bool = false
) -> RemoteItem {
	RemoteItem(
		id: id, folderID: testRemoteFolder, parentID: parent, name: FileNames.lastComponent(path), path: path,
		kind: .file, size: size, modifiedNs: 1_000, mode: 420, writable: true, revision: revision(rev ?? id + path),
		contentID: contentID(Data(content.utf8)), availability: .local, deleted: deleted ? true : nil)
}

func remoteDirectory(_ id: String, _ path: String, parent: String = testRootGatewayID, generation: Int64 = 1)
	-> RemoteItem
{
	RemoteItem(
		id: id, folderID: testRemoteFolder, parentID: parent, name: FileNames.lastComponent(path), path: path,
		kind: .directory, size: 0, modifiedNs: 1_000, mode: 493, writable: true, revision: revision(id + path),
		subtreeGeneration: generation, availability: .local)
}

let remoteRoot = RemoteItem(
	id: testRootGatewayID, folderID: testRemoteFolder, parentID: nil, name: "", path: "", kind: .directory, size: 0,
	modifiedNs: 1, mode: 493, writable: true, revision: revision("root"), subtreeGeneration: 1)

extension IndexStore {
	/// Takes a complete snapshot of the items (one page, no later changes)
	@discardableResult
	func snapshot(_ items: [RemoteItem], folderID: String = "docs", cursor: String = "g1.10") throws
		-> RemoteApplyResult
	{
		try beginRemoteSnapshot(folderID: folderID)
		let page = SnapshotPage(snapshot: "snp_\(UUID().uuidString.prefix(8))", cursor: cursor, items: items, next: nil)
		let first = try applyRemoteSnapshotPage(page, folderID: folderID, requestedPage: nil)
		var result = try applyRemoteChangesPage(
			ChangesPage(changes: [], cursor: cursor, more: false), folderID: folderID, after: cursor)
		result.changed += first.changed
		return result
	}

	/// Applies changes after the folder's current cursor
	@discardableResult
	func changes(_ items: [RemoteItem], folderID: String = "docs") throws -> RemoteApplyResult {
		let state = try remoteSyncState(folderID: folderID)
		let cursor = try #require(state.cursor)
		let parts = cursor.split(separator: ".")
		let next = "\(parts[0]).\((Int(parts[1]) ?? 0) + 1)"
		return try applyRemoteChangesPage(
			ChangesPage(changes: items, cursor: next, more: false), folderID: folderID, after: cursor)
	}

	func row(_ id: Int64) throws -> IndexItem? {
		try withConnection { connection throws(IndexError) in
			try connection.queryOne(
				"SELECT \(Self.itemColumns) FROM items WHERE id = ?", [id], Self.item)
		}
	}
}

func staged(_ name: String, size: Int64 = 6) -> StagedFile {
	StagedFile(
		relativePath: "Staged/\(name)", size: size,
		digest: Data(SHA256.hash(data: Data(name.utf8))).base64URLEncodedString())
}

/// An operation result as the gateway reports it
func operation(
	_ record: MutationRecord, state: RemoteOperationState, item: RemoteItem?, conflictItem: RemoteItem? = nil,
	content: ContentIdentity? = nil, generation: Int64 = 3
) -> RemoteOperation {
	RemoteOperation(
		id: record.operationID, operation: "op_\(record.operationID.prefix(8))",
		kind: record.kind.remoteKind ?? .create, state: state, item: item, conflictItem: conflictItem,
		contentID: content, error: nil, authorityGeneration: generation)
}
