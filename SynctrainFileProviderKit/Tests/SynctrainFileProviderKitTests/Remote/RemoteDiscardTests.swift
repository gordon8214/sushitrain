// Copyright (C) 2026 Gordon
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this file,
// You can obtain one at https://mozilla.org/MPL/2.0/.
import Foundation
import Testing

@testable import SynctrainFileProviderKit

/// Discarded changes (which the system may make again), who made the rows of pending creations, and the system's
/// identifiers of the items it created
@Suite struct RemoteDiscardTests {
	/// Submits a claimed operation with all of its bytes uploaded
	private func submit(_ store: IndexStore, operationID: String, upload: String) throws -> OutboxClaim {
		var claim = try #require(try store.claimMutation(owner: "app", duration: 60, operationID: operationID))
		claim = try #require(
			try store.recordUploadCreated(
				claim,
				upload: UploadInfo(upload: upload, token: "stu_\(upload)", offset: 6, length: 6, expires: .distantFuture),
				tokenReference: nil))
		claim = try #require(try store.recordUploadOffset(claim, offset: 6))
		guard case .ready(let submitted, _, _) = try store.prepareSubmission(claim, authorityGeneration: 3) else {
			throw IndexError.notAllowed("not submitted")
		}
		return submitted
	}

	private func root(_ store: IndexStore) throws -> Int64 {
		try #require(try store.folder(id: "docs")).rootItemID
	}

	// MARK: - Discards

	/// The discarded contents are remembered with the discard; the system's attempt to make the edit again takes them
	@Test func discardedEditIsRememberedByItsContents() throws {
		let store = try makeRemoteStore()
		try store.snapshot([remoteRoot, remoteFile("itm_a", "a.txt")])
		let a = try #require(try store.item(folderID: "docs", path: "a.txt"))
		let edit = try store.recordRemoteContentChange(itemID: a.id, modifiedNanoseconds: 2, stagedFile: staged("e1"))
		let operationID = try #require(edit.mutation?.operationID)
		let result = try store.discardMutation(operationID: operationID)
		#expect(result.releasedStagedFiles == ["Staged/e1"])
		#expect(try store.item(id: a.id)?.pending == PendingChange.none, "shows the gateway's version again")
		let remembered = try store.remoteDiscards(itemID: a.id)
		#expect(remembered.map(\.kind) == [.replace] && remembered.first?.digest == staged("e1").digest)
		#expect(remembered.first?.operationID == operationID && remembered.first?.templateIdentifier == nil)

		#expect(try store.consumeDiscardedEdit(itemID: a.id, digest: staged("e2").digest) == nil, "another edit")
		#expect(try store.consumeDiscardedEdit(itemID: a.id + 1, digest: staged("e1").digest) == nil)
		#expect(try store.consumeDiscardedEdit(itemID: a.id, digest: staged("e1").digest)?.operationID == operationID)
		#expect(try store.consumeDiscardedEdit(itemID: a.id, digest: staged("e1").digest) == nil, "taken once")
		#expect(try store.remoteDiscards().isEmpty)
	}

	/// A rejected edit the user reviewed and discarded is remembered like an unsent one
	@Test func discardedRejectedEditIsRemembered() throws {
		let store = try makeRemoteStore()
		try store.snapshot([remoteRoot, remoteFile("itm_a", "a.txt")])
		let a = try #require(try store.item(folderID: "docs", path: "a.txt"))
		let edit = try store.recordRemoteContentChange(itemID: a.id, modifiedNanoseconds: 2, stagedFile: staged("e1"))
		let operationID = try #require(edit.mutation?.operationID)
		let submitted = try submit(store, operationID: operationID, upload: "upl_e1")
		// Not while it may be in progress on the gateway: then nothing is remembered either
		#expect(throws: IndexError.self) { try store.discardMutation(operationID: operationID) }
		#expect(try store.remoteDiscards().isEmpty)

		_ = try store.recordRejection(submitted, error: "forbidden: read-only", currentItem: nil)
		_ = try store.discardMutation(operationID: operationID)
		#expect(try store.consumeDiscardedEdit(itemID: a.id, digest: staged("e1").digest)?.operationID == operationID)
	}

	/// A creation the system made is remembered by the identifier the system gave its item: it is that item, wherever
	/// the system creates it again, as long as its contents are the discarded ones
	@Test func discardedCreationIsRememberedByTheSystemsIdentifier() throws {
		let store = try makeRemoteStore()
		try store.snapshot([remoteRoot])
		let root = try root(store)
		let created = try store.recordRemoteCreation(
			folderID: "docs", parentItemID: root, name: "new.txt", kind: .file, modifiedNanoseconds: 1,
			stagedFile: staged("n"), templateIdentifier: "template-1")
		let item = try #require(created.item)
		let operationID = try #require(created.mutation?.operationID)
		#expect(created.mutation?.templateIdentifier == "template-1")
		#expect(try store.remoteCreation(templateIdentifier: "template-1")?.operationID == operationID)

		_ = try store.discardMutation(operationID: operationID)
		#expect(try store.item(id: item.id) == nil)
		#expect(try store.remoteCreation(templateIdentifier: "template-1") == nil, "its row is gone")

		let digest = staged("n").digest
		#expect(
			try store.consumeDiscardedCreation(
				templateIdentifier: "template-2", parentItemID: root, name: "new.txt", kind: .file, digest: digest) == nil,
			"another item of the system, although it has the same name and contents")
		#expect(
			try store.consumeDiscardedCreation(
				templateIdentifier: "template-1", parentItemID: root, name: "new.txt", kind: .file,
				digest: staged("n2").digest) == nil, "edited since")
		#expect(
			try store.consumeDiscardedCreation(
				templateIdentifier: "template-1", parentItemID: root, name: "new.txt", kind: .directory, digest: nil)
				== nil)
		let discard = try #require(
			try store.consumeDiscardedCreation(
				templateIdentifier: "template-1", parentItemID: 9999, name: "renamed.txt", kind: .file, digest: digest))
		#expect(discard.kind == .create && discard.templateIdentifier == "template-1" && discard.itemID == item.id)
		#expect(
			try store.consumeDiscardedCreation(
				templateIdentifier: "template-1", parentItemID: root, name: "new.txt", kind: .file, digest: digest) == nil)
	}

	@Test func discardedDirectoryCreationIsRememberedWithoutContents() throws {
		let store = try makeRemoteStore()
		try store.snapshot([remoteRoot])
		let root = try root(store)
		let created = try store.recordRemoteCreation(
			folderID: "docs", parentItemID: root, name: "new", kind: .directory, modifiedNanoseconds: 1,
			stagedFile: nil, templateIdentifier: "template-d")
		let child = try store.recordRemoteCreation(
			folderID: "docs", parentItemID: try #require(created.item?.id), name: "c.txt", kind: .file,
			modifiedNanoseconds: 1, stagedFile: staged("c"), templateIdentifier: "template-c")
		_ = try store.discardMutation(operationID: try #require(created.mutation?.operationID))
		#expect(try store.mutation(operationID: try #require(child.mutation?.operationID))?.state == .canceled)
		#expect(try store.remoteDiscards().map(\.templateIdentifier) == ["template-d"], "only what the user discarded")
		#expect(
			try store.consumeDiscardedCreation(
				templateIdentifier: "template-d", parentItemID: root, name: "new", kind: .directory, digest: nil)?.digest
				== nil)
	}

	/// The edit of a creation the gateway never committed (here: rejected, with the edit after it) is the system's
	/// pending creation still, once both are discarded and the row is gone
	@Test func discardedEditOfAnUncommittedCreationIsRememberedAsTheCreation() throws {
		let store = try makeRemoteStore()
		try store.snapshot([remoteRoot])
		let root = try root(store)
		let created = try store.recordRemoteCreation(
			folderID: "docs", parentItemID: root, name: "new.txt", kind: .file, modifiedNanoseconds: 1,
			stagedFile: staged("n"), templateIdentifier: "template-1")
		let item = try #require(created.item)
		let creation = try #require(created.mutation?.operationID)
		let submitted = try submit(store, operationID: creation, upload: "upl_n")
		let edit = try store.recordRemoteContentChange(itemID: item.id, modifiedNanoseconds: 2, stagedFile: staged("n2"))
		let editID = try #require(edit.mutation?.operationID)
		#expect(edit.mutation?.dependencies == [creation])
		_ = try store.recordRejection(submitted, error: "forbidden: read-only", currentItem: nil)
		#expect(try store.mutation(operationID: editID)?.state == .failed)

		_ = try store.discardMutation(operationID: creation)
		#expect(try store.item(id: item.id) != nil, "the rejected edit still shows")
		#expect(try store.remoteDiscards().isEmpty)
		_ = try store.discardMutation(operationID: editID)
		#expect(try store.item(id: item.id) == nil)
		#expect(try store.consumeDiscardedEdit(itemID: item.id, digest: staged("n2").digest) == nil)
		let discard = try #require(
			try store.consumeDiscardedCreation(
				templateIdentifier: "template-1", parentItemID: root, name: "new.txt", kind: .file,
				digest: staged("n2").digest))
		#expect(discard.kind == .create && discard.operationID == editID)
	}

	/// After the creation was committed, a discarded edit is one of the item, also when the system asks to create its
	/// item again (it learns the identifier from that call)
	@Test func discardedEditOfACommittedCreationIsAnEdit() throws {
		let store = try makeRemoteStore()
		try store.snapshot([remoteRoot])
		let root = try root(store)
		let created = try store.recordRemoteCreation(
			folderID: "docs", parentItemID: root, name: "new.txt", kind: .file, modifiedNanoseconds: 1,
			stagedFile: staged("n"), templateIdentifier: "template-1")
		let item = try #require(created.item)
		let submitted = try submit(store, operationID: try #require(created.mutation?.operationID), upload: "upl_n")
		_ = try store.recordOperation(
			submitted, operation(submitted.record, state: .committed, item: remoteFile("itm_new", "new.txt", size: 6)))
		#expect(try store.remoteCreation(templateIdentifier: "template-1")?.itemID == item.id, "found once committed")

		let edit = try store.recordRemoteContentChange(itemID: item.id, modifiedNanoseconds: 2, stagedFile: staged("n2"))
		_ = try store.discardMutation(operationID: try #require(edit.mutation?.operationID))
		#expect(try store.item(id: item.id) != nil)
		#expect(
			try store.consumeDiscardedCreation(
				templateIdentifier: "template-1", parentItemID: root, name: "new.txt", kind: .file,
				digest: staged("n2").digest) == nil)
		#expect(try store.consumeDiscardedEdit(itemID: item.id, digest: staged("n2").digest) != nil)
	}

	@Test func discardsExpire() throws {
		let store = try makeRemoteStore()
		try store.snapshot([remoteRoot, remoteFile("itm_a", "a.txt")])
		let a = try #require(try store.item(folderID: "docs", path: "a.txt"))
		let edit = try store.recordRemoteContentChange(itemID: a.id, modifiedNanoseconds: 2, stagedFile: staged("e1"))
		_ = try store.discardMutation(operationID: try #require(edit.mutation?.operationID))
		let later = Date().addingTimeInterval(IndexStore.discardLifetime + 60)
		#expect(try store.remoteDiscards(now: later).isEmpty)
		#expect(try store.consumeDiscardedEdit(itemID: a.id, digest: staged("e1").digest, now: later) == nil)
		#expect(try store.remoteDiscards().count == 1, "not expired yet")
	}

	/// What the extension gives up in a call of the system is not remembered (the system makes the change differently),
	/// nor are the creations Synctrain made itself
	@Test func onlyWhatTheSystemMayMakeAgainIsRemembered() throws {
		let store = try makeRemoteStore()
		try store.snapshot([remoteRoot, remoteFile("itm_a", "a.txt")])
		let a = try #require(try store.item(folderID: "docs", path: "a.txt"))
		let edit = try store.recordRemoteContentChange(itemID: a.id, modifiedNanoseconds: 2, stagedFile: staged("e1"))
		_ = try store.discardMutation(operationID: try #require(edit.mutation?.operationID), remembering: false)
		let copy = try store.recordRemoteCreation(
			folderID: "docs", parentItemID: try root(store), name: "a.sync-conflict-20261027-120000-ABCDEFG.txt",
			kind: .file, modifiedNanoseconds: 1, stagedFile: staged("e2"), origin: .provider)
		_ = try store.discardMutation(operationID: try #require(copy.mutation?.operationID))
		let move = try store.recordRemoteMove(itemID: a.id, toParentItemID: try root(store), name: "b.txt")
		_ = try store.discardMutation(operationID: try #require(move.mutation?.operationID))
		#expect(try store.remoteDiscards().isEmpty)
	}

	@Test func rebindingForgetsTheFoldersDiscards() throws {
		let store = try makeRemoteStore()
		try store.snapshot([remoteRoot, remoteFile("itm_a", "a.txt")])
		let a = try #require(try store.item(folderID: "docs", path: "a.txt"))
		let edit = try store.recordRemoteContentChange(itemID: a.id, modifiedNanoseconds: 2, stagedFile: staged("e1"))
		_ = try store.discardMutation(operationID: try #require(edit.mutation?.operationID))
		try store.bindRemoteFolder(
			RemoteFolderBinding(
				folderID: "docs", accessMode: .syncthing, serviceID: nil, remoteFolderID: nil,
				permissions: RemotePermissions(read: true, write: true)))
		#expect(try store.remoteDiscards().isEmpty)
	}

	// MARK: - Origins

	/// Rows the system created are hidden while their creation is pending (the system shows its own item); conflict
	/// copies Synctrain made are listed. The origin stays with the row through later unsent changes.
	@Test func pendingCreationsKeepWhoMadeThem() throws {
		let store = try makeRemoteStore()
		try store.snapshot([remoteRoot, remoteDirectory("itm_d", "d"), remoteFile("itm_a", "a.txt")])
		let root = try root(store)
		let d = try #require(try store.item(folderID: "docs", path: "d"))
		let a = try #require(try store.item(folderID: "docs", path: "a.txt"))
		let system = try #require(
			try store.recordRemoteCreation(
				folderID: "docs", parentItemID: root, name: "new.txt", kind: .file, modifiedNanoseconds: 1,
				stagedFile: staged("n"), templateIdentifier: "template-1"
			).item)
		let copy = try #require(
			try store.recordRemoteCreation(
				folderID: "docs", parentItemID: root, name: "a.sync-conflict-20261027-120000-ABCDEFG.txt", kind: .file,
				modifiedNanoseconds: 1, stagedFile: staged("c"), origin: .provider
			).item)
		_ = try store.recordRemoteContentChange(itemID: a.id, modifiedNanoseconds: 2, stagedFile: staged("e1"))
		#expect(try store.remoteOverlay(itemID: system.id)?.origin == .system)
		#expect(try store.remoteOverlay(itemID: copy.id)?.origin == .provider)
		#expect(try store.remoteOverlay(itemID: a.id)?.origin == nil, "existed before")

		// Edited and moved before anything was sent: still the same creation, made by the same
		_ = try store.recordRemoteContentChange(itemID: copy.id, modifiedNanoseconds: 3, stagedFile: staged("c2"))
		_ = try store.recordRemoteMove(itemID: copy.id, toParentItemID: d.id, name: "copy.txt")
		let content = try store.remoteLocalContent(of: [system.id, copy.id, a.id])
		#expect(content[system.id]?.overlayOrigin == .system)
		#expect(content[copy.id]?.overlayOrigin == .provider && content[copy.id]?.overlayStagedPath == "Staged/c2")
		#expect(content[a.id]?.overlayOrigin == nil && content[a.id]?.overlayStagedPath == "Staged/e1")
		#expect(try store.mutations(states: [.staged]).count == 3)

		// Deleted after its creation was sent: the deletion's overlay keeps the origin until the two are resolved
		let creation = try #require(try store.remoteOverlay(itemID: system.id)).operationID
		_ = try submit(store, operationID: creation, upload: "upl_n")
		let deletion = try store.recordRemoteDeletion(itemID: system.id)
		#expect(deletion.mutation?.dependencies == [creation])
		#expect(try store.remoteOverlay(itemID: system.id)?.origin == .system)
	}

	// MARK: - The system's identifiers

	/// The system renames its pending item before the creation was sent: the creation goes to the new place, and is
	/// still found by the system's identifier
	@Test func unsentCreationFollowsItsItem() throws {
		let store = try makeRemoteStore()
		try store.snapshot([remoteRoot, remoteDirectory("itm_d", "d")])
		let root = try root(store)
		let d = try #require(try store.item(folderID: "docs", path: "d"))
		let created = try store.recordRemoteCreation(
			folderID: "docs", parentItemID: root, name: "new.txt", kind: .file, modifiedNanoseconds: 1,
			stagedFile: staged("n"), templateIdentifier: "template-1")
		let item = try #require(created.item)
		let operationID = try #require(created.mutation?.operationID)

		let moved = try store.recordRemoteMove(itemID: item.id, toParentItemID: d.id, name: "renamed.txt")
		#expect(moved.mutation?.operationID == operationID, "the creation goes there instead")
		#expect(moved.item?.path == "d/renamed.txt" && moved.item?.pending == .create)
		let creation = try #require(try store.remoteCreation(templateIdentifier: "template-1"))
		#expect(creation.operationID == operationID && creation.targetName == "renamed.txt")
		#expect(creation.targetParentItemID == d.id && creation.templateIdentifier == "template-1")
		#expect(try store.mutations().count == 1, "no second creation")
	}

	/// After the creation was sent, the item moves after it
	@Test func sentCreationIsFollowedByAMove() throws {
		let store = try makeRemoteStore()
		try store.snapshot([remoteRoot, remoteDirectory("itm_d", "d")])
		let root = try root(store)
		let d = try #require(try store.item(folderID: "docs", path: "d"))
		let created = try store.recordRemoteCreation(
			folderID: "docs", parentItemID: root, name: "new.txt", kind: .file, modifiedNanoseconds: 1,
			stagedFile: staged("n"), templateIdentifier: "template-1")
		let item = try #require(created.item)
		let operationID = try #require(created.mutation?.operationID)
		let submitted = try submit(store, operationID: operationID, upload: "upl_n")

		let moved = try store.recordRemoteMove(itemID: item.id, toParentItemID: d.id, name: "renamed.txt")
		let move = try #require(moved.mutation)
		#expect(move.kind == .move && move.dependencies == [operationID])
		#expect(try store.remoteCreation(templateIdentifier: "template-1")?.operationID == operationID)

		_ = try store.recordOperation(
			submitted, operation(submitted.record, state: .committed, item: remoteFile("itm_new", "new.txt", size: 6)))
		let claim = try #require(try store.claimMutation(owner: "app", duration: 60, operationID: move.operationID))
		guard case .ready(_, let request, _) = try store.prepareSubmission(claim, authorityGeneration: 3) else {
			Issue.record("not submitted")
			return
		}
		#expect(request.kind == .move && request.itemID == "itm_new" && request.parentID == "itm_d")
		#expect(request.name == "renamed.txt")
	}
}
