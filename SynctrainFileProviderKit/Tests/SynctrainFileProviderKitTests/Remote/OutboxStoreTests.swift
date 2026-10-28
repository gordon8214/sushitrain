// Copyright (C) 2026 Gordon
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this file,
// You can obtain one at https://mozilla.org/MPL/2.0/.
import Foundation
import Testing

@testable import SynctrainFileProviderKit

@Suite struct OutboxStoreTests {
	/// The app and the extension each open the database themselves
	@Test func claimsAndGenerationsAcrossProcesses() throws {
		let app = try makeRemoteStore()
		let extensionStore = IndexStore(url: app.url)
		try app.snapshot([remoteRoot, remoteFile("itm_a", "a.txt")])
		let a = try #require(try app.item(folderID: "docs", path: "a.txt"))
		let edit = try extensionStore.recordRemoteContentChange(
			itemID: a.id, modifiedNanoseconds: 1, stagedFile: staged("e1"))
		let operationID = try #require(edit.mutation?.operationID)

		let appClaim = try #require(try app.claimMutation(owner: "app", duration: 60))
		#expect(appClaim.operationID == operationID && appClaim.record.baseRevision == revision("itm_aa.txt"))
		#expect(try extensionStore.claimMutation(owner: "extension", duration: 60) == nil, "claimed by the app")

		// The app is suspended past its claim: the extension takes over, with a new generation
		let later = Date().addingTimeInterval(120)
		let extensionClaim = try #require(
			try extensionStore.claimMutation(owner: "extension", duration: 60, now: later))
		#expect(extensionClaim.generation == appClaim.generation + 1)

		// The app's claim no longer works for anything, so it cannot send a request for the operation
		#expect(try app.recordUploadOffset(appClaim, offset: 3) == nil)
		#expect(try app.renewClaim(appClaim, duration: 60) == nil)
		guard case .lost = try app.prepareSubmission(appClaim, authorityGeneration: 3) else {
			Issue.record("the app could submit with a lost claim")
			return
		}
		#expect(try extensionStore.recordUploadOffset(extensionClaim, offset: 3) != nil)
		#expect(try app.mutation(operationID: operationID)?.uploadOffset == 3)
	}

	@Test func newerUnsentEditReplacesTheOperation() throws {
		let store = try makeRemoteStore()
		try store.snapshot([remoteRoot, remoteFile("itm_a", "a.txt")])
		let a = try #require(try store.item(folderID: "docs", path: "a.txt"))
		let first = try store.recordRemoteContentChange(itemID: a.id, modifiedNanoseconds: 1, stagedFile: staged("e1"))
		let claim = try #require(try store.claimMutation(owner: "app", duration: 60))

		let second = try store.recordRemoteContentChange(itemID: a.id, modifiedNanoseconds: 2, stagedFile: staged("e2"))
		#expect(second.mutation?.operationID == first.mutation?.operationID, "the same operation, never sent")
		#expect(second.mutation?.stagedPath == "Staged/e2" && second.releasedStagedFiles == ["Staged/e1"])
		#expect(try store.recordUploadOffset(claim, offset: 1) == nil, "the worker uploading e1 lost its claim")
		#expect(try store.mutations().count == 1)
	}

	@Test func editsAfterASubmittedOneWaitForIt() throws {
		let store = try makeRemoteStore()
		try store.snapshot([remoteRoot, remoteFile("itm_a", "a.txt")])
		let a = try #require(try store.item(folderID: "docs", path: "a.txt"))
		_ = try store.recordRemoteContentChange(itemID: a.id, modifiedNanoseconds: 1, stagedFile: staged("e1"))
		var claim = try #require(try store.claimMutation(owner: "app", duration: 60))
		claim = try #require(
			try store.recordUploadCreated(
				claim,
				upload: UploadInfo(upload: "upl_1", token: "stu_1", offset: 0, length: 6, expires: .distantFuture),
				tokenReference: nil))
		claim = try #require(try store.recordUploadOffset(claim, offset: 6))
		guard case .ready(let submitted, let request, _) = try store.prepareSubmission(claim, authorityGeneration: 3)
		else {
			Issue.record("not submitted")
			return
		}
		#expect(request.kind == .replace && request.itemID == "itm_a" && request.upload == "upl_1")
		#expect(submitted.record.state == .submitted && submitted.record.payload == request)

		// A second edit cannot replace a submitted one: it waits for it, without a base revision yet
		let second = try store.recordRemoteContentChange(itemID: a.id, modifiedNanoseconds: 2, stagedFile: staged("e2"))
		let secondRecord = try #require(second.mutation)
		#expect(secondRecord.operationID != request.id && secondRecord.dependencies == [request.id])
		#expect(secondRecord.baseRevision == nil)
		#expect(try store.claimMutation(owner: "extension", duration: 60) == nil, "waits for the first")

		// Replaying the first sends the stored request unchanged
		guard case .ready(_, let replay, _) = try store.prepareSubmission(submitted, authorityGeneration: 3) else {
			Issue.record("no replay")
			return
		}
		#expect(replay == request)

		let committed = remoteFile("itm_a", "a.txt", rev: "after-e1", size: 6, content: "e1e1e1")
		let content = try #require(committed.contentID)
		_ = try store.recordOperation(
			submitted,
			operation(submitted.record, state: .committed, item: committed, content: content))
		#expect(try store.stagedFile(contentID: content) == "Staged/e1", "the staged copy is a cached version")
		let next = try #require(try store.claimMutation(owner: "extension", duration: 60))
		#expect(next.operationID == secondRecord.operationID && next.record.baseRevision == committed.revision)
		#expect(try store.item(id: a.id)?.pending == .modify, "the second edit is still shown")
	}

	@Test func committedCreationMapsTheGatewayItemToTheNewRow() throws {
		let store = try makeRemoteStore()
		try store.snapshot([remoteRoot])
		let root = try #require(try store.folder(id: "docs")).rootItemID
		let created = try store.recordRemoteCreation(
			folderID: "docs", parentItemID: root, name: "new.txt", kind: .file, modifiedNanoseconds: 7,
			stagedFile: staged("n"))
		let item = try #require(created.item)
		#expect(item.pending == .create)

		// The change feed shows the new item before the operation's answer arrives: it waits for the local row
		let remote = remoteFile("itm_new", "new.txt", size: 6)
		try store.changes([remote])
		#expect(try store.waitingRemoteItems(folderID: "docs").map(\.gatewayID) == ["itm_new"])

		var claim = try #require(try store.claimMutation(owner: "app", duration: 60))
		claim = try #require(
			try store.recordUploadCreated(
				claim,
				upload: UploadInfo(upload: "upl_n", token: "stu_n", offset: 0, length: 6, expires: .distantFuture),
				tokenReference: nil))
		claim = try #require(try store.recordUploadOffset(claim, offset: 6))
		guard case .ready(let submitted, let request, _) = try store.prepareSubmission(claim, authorityGeneration: 3)
		else {
			Issue.record("not submitted")
			return
		}
		#expect(request.kind == .create && request.parentID == testRootGatewayID && request.name == "new.txt")
		_ = try store.recordOperation(submitted, operation(submitted.record, state: .committed, item: remote))
		let shown = try #require(try store.item(id: item.id))
		#expect(shown.pending == .none && shown.path == "new.txt")
		#expect(try store.remoteItem(itemID: item.id)?.gatewayID == "itm_new")
		#expect(try store.remoteOverlay(itemID: item.id) == nil)
		#expect(try store.children(of: root, limit: 10).count == 1, "no second row")
	}

	@Test func conflictKeepsTheEditAsACopy() throws {
		let store = try makeRemoteStore()
		try store.snapshot([remoteRoot, remoteFile("itm_a", "a.txt")])
		let a = try #require(try store.item(folderID: "docs", path: "a.txt"))
		let before = try #require(try store.item(id: a.id))
		_ = try store.recordRemoteContentChange(itemID: a.id, modifiedNanoseconds: 1, stagedFile: staged("e1"))
		var claim = try #require(try store.claimMutation(owner: "app", duration: 60))
		claim = try #require(
			try store.recordUploadCreated(
				claim,
				upload: UploadInfo(upload: "upl_1", token: "stu_1", offset: 6, length: 6, expires: .distantFuture),
				tokenReference: nil))
		claim = try #require(try store.recordUploadOffset(claim, offset: 6))
		guard case .ready(let submitted, _, _) = try store.prepareSubmission(claim, authorityGeneration: 3) else {
			Issue.record("not submitted")
			return
		}
		let copy = remoteFile("itm_copy", "a.sync-conflict-20261027-120000-ABCDEFG.txt", size: 6, content: "e1")
		_ = try store.recordOperation(
			submitted,
			operation(
				submitted.record, state: .conflict, item: remoteFile("itm_a", "a.txt", rev: "theirs"),
				conflictItem: copy, content: copy.contentID))
		#expect(try store.item(id: a.id)?.pending == PendingChange.none)
		#expect(try store.item(id: a.id)?.contentRevision ?? 0 > before.contentRevision)
		#expect(try store.item(folderID: "docs", path: copy.path) != nil)
		#expect(try store.mutations(states: [.conflict]).count == 1)
	}

	@Test func rejectedDeletionBringsTheItemBack() throws {
		let store = try makeRemoteStore()
		try store.snapshot([remoteRoot, remoteDirectory("itm_d", "d"), remoteFile("itm_x", "d/x.txt", parent: "itm_d")])
		let d = try #require(try store.item(folderID: "docs", path: "d"))
		let x = try #require(try store.item(folderID: "docs", path: "d/x.txt"))
		let deletion = try store.recordRemoteDeletion(itemID: d.id)
		#expect(try store.item(id: d.id) == nil && store.item(id: x.id) == nil)
		#expect(deletion.mutation?.subtreeGeneration == 1)

		// A refresh does not bring them back while the deletion is pending
		try store.changes([remoteFile("itm_x", "d/x.txt", parent: "itm_d", rev: "newer")])
		#expect(try store.item(id: x.id) == nil)

		let claim = try #require(try store.claimMutation(owner: "app", duration: 60))
		guard case .ready(let submitted, let request, _) = try store.prepareSubmission(claim, authorityGeneration: 3)
		else {
			Issue.record("not submitted")
			return
		}
		#expect(request.kind == .delete && request.subtreeGeneration == 1)
		_ = try store.recordRejection(
			submitted, error: "revision_mismatch", currentItem: remoteDirectory("itm_d", "d", generation: 2))
		#expect(try store.item(id: d.id)?.path == "d", "the same item comes back")
		#expect(try store.item(id: x.id)?.parentID == d.id)
		#expect(try store.remoteOverlay(itemID: x.id) == nil)
	}

	@Test func deletingAnUnsentCreationCancelsIt() throws {
		let store = try makeRemoteStore()
		try store.snapshot([remoteRoot])
		let root = try #require(try store.folder(id: "docs")).rootItemID
		let dir = try #require(
			try store.recordRemoteCreation(
				folderID: "docs", parentItemID: root, name: "new", kind: .directory, modifiedNanoseconds: 1,
				stagedFile: nil
			).item)
		let child = try store.recordRemoteCreation(
			folderID: "docs", parentItemID: dir.id, name: "c.txt", kind: .file, modifiedNanoseconds: 1,
			stagedFile: staged("c"))
		#expect(child.mutation?.dependencies.count == 1, "waits for its directory")
		let deletion = try store.recordRemoteDeletion(itemID: dir.id)
		#expect(deletion.mutation == nil && deletion.releasedStagedFiles == ["Staged/c"])
		#expect(try store.mutations(states: [.canceled]).count == 2)
		#expect(try store.hasUnresolvedMutations() == false)
		#expect(try store.children(of: root, limit: 10).isEmpty)
	}

	@Test func authorityGenerationChangeMakesUnconfirmedOperationsUnknown() throws {
		let store = try makeRemoteStore()
		try store.snapshot([remoteRoot, remoteFile("itm_a", "a.txt"), remoteFile("itm_b", "b.txt")])
		#expect(try store.recordAuthorityGeneration(3, serviceID: testServiceID) == false)
		let a = try #require(try store.item(folderID: "docs", path: "a.txt"))
		let b = try #require(try store.item(folderID: "docs", path: "b.txt"))
		_ = try store.recordRemoteDeletion(itemID: a.id)
		let claim = try #require(try store.claimMutation(owner: "app", duration: 60))
		guard case .ready = try store.prepareSubmission(claim, authorityGeneration: 3) else {
			Issue.record("not submitted")
			return
		}
		let unsent = try store.recordRemoteDeletion(itemID: b.id)

		#expect(try store.recordAuthorityGeneration(4, serviceID: testServiceID))
		#expect(try store.mutation(operationID: claim.operationID)?.state == .outcomeUnknown)
		#expect(try store.mutation(operationID: try #require(unsent.mutation?.operationID))?.state == .staged)
		#expect(try store.remoteSyncState(folderID: "docs").needsSnapshot)
		#expect(try store.item(id: a.id) == nil, "still shown as deleted until reviewed")
		#expect(try store.remoteOverlay(itemID: a.id) != nil)
	}

	@Test func crossHostMoveCopiesThenDeletes() throws {
		let store = try makeRemoteStore()
		let other = RemoteEndpoint(serviceID: "svc_other", host: "other.test", port: 443, displayName: "other")
		try store.saveRemoteEndpoint(other)
		try store.bindRemoteFolder(
			RemoteFolderBinding(
				folderID: "music", accessMode: .remoteAPI, serviceID: "svc_other", remoteFolderID: "Music",
				permissions: RemotePermissions(read: true, write: true)),
			folder: FolderDescription(folderID: "music", label: "Music", folderType: "sendrecieve", isSelective: true),
			rootGatewayID: "itm_musicroot")
		try store.snapshot([remoteRoot, remoteFile("itm_a", "a.txt")])
		let musicRoot = try #require(try store.folder(id: "music")).rootItemID
		let a = try #require(try store.item(folderID: "docs", path: "a.txt"))
		let move = try store.recordRemoteCrossHostMove(
			itemID: a.id, toParentItemID: musicRoot, name: "a.txt", stagedFile: staged("a"))
		#expect(move.item?.folderID == "music" && move.mutation?.kind == .crossHostMove)

		// Step one: the copy on the destination
		var claim = try #require(try store.claimMutation(owner: "app", duration: 60))
		#expect(claim.record.kind == .create && claim.record.step == 1 && claim.record.serviceID == "svc_other")
		claim = try #require(
			try store.recordUploadCreated(
				claim,
				upload: UploadInfo(upload: "upl_m", token: "stu_m", offset: 6, length: 6, expires: .distantFuture),
				tokenReference: nil))
		claim = try #require(try store.recordUploadOffset(claim, offset: 6))
		guard case .ready(let copyClaim, let copyRequest, _) = try store.prepareSubmission(claim, authorityGeneration: 1)
		else {
			Issue.record("not submitted")
			return
		}
		#expect(copyRequest.folderID == "Music" && copyRequest.parentID == "itm_musicroot")
		var copy = remoteFile("itm_copy", "a.txt", parent: "itm_musicroot")
		copy.folderID = "Music"
		_ = try store.recordOperation(copyClaim, operation(copyClaim.record, state: .committed, item: copy))

		// Step two: the conditional deletion of the source, which fails: both copies remain
		let deleteClaim = try #require(try store.claimMutation(owner: "app", duration: 60))
		#expect(deleteClaim.record.kind == .delete && deleteClaim.record.gatewayID == "itm_a")
		guard
			case .ready(let submitted, let deleteRequest, _) = try store.prepareSubmission(
				deleteClaim, authorityGeneration: 3)
		else {
			Issue.record("not submitted")
			return
		}
		#expect(deleteRequest.baseRevision == revision("itm_aa.txt"))
		_ = try store.recordRejection(submitted, error: "revision_mismatch", currentItem: nil)
		#expect(try store.mutation(operationID: try #require(move.mutation?.operationID))?.state == .failed)
		#expect(try store.item(id: a.id)?.folderID == "music", "the moved item is the copy")
		#expect(try store.item(folderID: "docs", path: "a.txt") != nil, "the source is shown again")
	}

	@Test func rejectPolicyTravelsWithTheReplace() throws {
		let store = try makeRemoteStore()
		try store.snapshot([remoteRoot, remoteFile("itm_a", "a.txt"), remoteFile("itm_b", "b.txt")])
		let a = try #require(try store.item(folderID: "docs", path: "a.txt"))
		let b = try #require(try store.item(folderID: "docs", path: "b.txt"))
		let kept = try store.recordRemoteContentChange(itemID: b.id, modifiedNanoseconds: 1, stagedFile: staged("b1"))
		#expect(kept.mutation?.conflictPolicy == .keepBoth)

		// A newer unsent edit takes the place of the earlier one, with its own policy
		_ = try store.recordRemoteContentChange(itemID: a.id, modifiedNanoseconds: 1, stagedFile: staged("e1"))
		let edit = try store.recordRemoteContentChange(
			itemID: a.id, modifiedNanoseconds: 2, stagedFile: staged("e2"), onConflict: .reject)
		let operationID = try #require(edit.mutation?.operationID)
		#expect(try store.mutation(operationID: operationID)?.conflictPolicy == .reject)

		var claim = try #require(
			try store.claimMutation(owner: "extension", duration: 60, operationID: operationID))
		claim = try #require(
			try store.recordUploadCreated(
				claim,
				upload: UploadInfo(upload: "upl_r", token: "stu_r", offset: 6, length: 6, expires: .distantFuture),
				tokenReference: nil))
		claim = try #require(try store.recordUploadOffset(claim, offset: 6))
		guard case .ready(_, let request, let body) = try store.prepareSubmission(claim, authorityGeneration: 3) else {
			Issue.record("not submitted")
			return
		}
		#expect(request.kind == .replace && request.onConflict == .reject)
		#expect(String(decoding: body, as: UTF8.self).contains("\"onConflict\":\"reject\""))
	}

	@Test func expeditingLetsAWaitingOperationBeTriedNow() throws {
		let store = try makeRemoteStore()
		try store.snapshot([remoteRoot, remoteFile("itm_a", "a.txt"), remoteFile("itm_b", "b.txt")])
		let a = try #require(try store.item(folderID: "docs", path: "a.txt"))
		let b = try #require(try store.item(folderID: "docs", path: "b.txt"))
		let first = try #require(try store.recordRemoteDeletion(itemID: a.id).mutation)
		let second = try #require(try store.recordRemoteDeletion(itemID: b.id).mutation)

		// The first waits after a failed attempt; the second is claimed by another process
		let claim = try #require(try store.claimMutation(owner: "app", duration: 60, operationID: first.operationID))
		_ = try store.recordRetry(claim, error: "unreachable", notBefore: Date().addingTimeInterval(300))
		let held = try #require(try store.claimMutation(owner: "app", duration: 60, operationID: second.operationID))
		_ = try store.recordRetry(held, error: "unreachable", notBefore: Date().addingTimeInterval(300))
		_ = try #require(
			try store.claimMutation(
				owner: "app", duration: 600, operationID: second.operationID, now: Date().addingTimeInterval(301)))
		#expect(try store.claimMutation(owner: "extension", duration: 60, operationID: first.operationID) == nil)

		try store.expediteMutation(operationID: first.operationID)
		try store.expediteMutation(operationID: second.operationID)
		#expect(try store.mutation(operationID: first.operationID)?.notBefore == nil)
		#expect(try store.mutation(operationID: second.operationID)?.notBefore != nil, "left alone")
		#expect(try store.claimMutation(owner: "extension", duration: 60, operationID: first.operationID) != nil)
		#expect(
			try store.claimMutation(owner: "extension", duration: 60, operationID: second.operationID) == nil,
			"still claimed by the app")
	}

	/// Submits a claimed edit with all of its bytes uploaded
	func submit(_ store: IndexStore, operationID: String, upload: String, tokenReference: String? = nil) throws
		-> OutboxClaim
	{
		var claim = try #require(try store.claimMutation(owner: "app", duration: 60, operationID: operationID))
		claim = try #require(
			try store.recordUploadCreated(
				claim,
				upload: UploadInfo(upload: upload, token: "stu_\(upload)", offset: 6, length: 6, expires: .distantFuture),
				tokenReference: tokenReference))
		claim = try #require(try store.recordUploadOffset(claim, offset: 6))
		guard case .ready(let submitted, _, _) = try store.prepareSubmission(claim, authorityGeneration: 3) else {
			throw IndexError.notAllowed("not submitted")
		}
		return submitted
	}

	/// The row showed the edit's bytes since it was recorded: their commit is no change of its contents
	@Test func commitKeepsTheContentRevision() throws {
		let store = try makeRemoteStore()
		try store.snapshot([remoteRoot, remoteFile("itm_a", "a.txt")])
		let a = try #require(try store.item(folderID: "docs", path: "a.txt"))
		let edit = try store.recordRemoteContentChange(itemID: a.id, modifiedNanoseconds: 1, stagedFile: staged("e1"))
		let edited = try #require(edit.item)
		#expect(edited.contentRevision == a.contentRevision + 1)
		let submitted = try submit(store, operationID: try #require(edit.mutation?.operationID), upload: "upl_1")
		let committed = remoteFile("itm_a", "a.txt", rev: "after-e1", size: 6, content: "e1e1e1")
		_ = try store.recordOperation(
			submitted, operation(submitted.record, state: .committed, item: committed, content: committed.contentID))
		let shown = try #require(try store.item(id: a.id))
		#expect(shown.pending == .none && shown.blocksHash == committed.contentID?.rawValue)
		#expect(shown.contentRevision == edited.contentRevision, "not a second change of the contents")

		// The same for a new file
		let root = try #require(try store.folder(id: "docs")).rootItemID
		let created = try store.recordRemoteCreation(
			folderID: "docs", parentItemID: root, name: "n.txt", kind: .file, modifiedNanoseconds: 1,
			stagedFile: staged("n"))
		let row = try #require(created.item)
		let creation = try submit(store, operationID: try #require(created.mutation?.operationID), upload: "upl_n")
		let new = remoteFile("itm_n", "n.txt", size: 6, content: "nnnnnn")
		_ = try store.recordOperation(
			creation, operation(creation.record, state: .committed, item: new, content: new.contentID))
		#expect(try store.item(id: row.id)?.contentRevision == row.contentRevision)
		#expect(try store.item(id: row.id)?.pending == PendingChange.none)
	}

	/// An operation's result may predate changes the feed stored since: it does not overwrite them
	@Test func operationResultsDoNotOverwriteNewerChanges() throws {
		let store = try makeRemoteStore()
		try store.snapshot([remoteRoot, remoteFile("itm_a", "a.txt")])
		let a = try #require(try store.item(folderID: "docs", path: "a.txt"))
		let edit = try store.recordRemoteContentChange(itemID: a.id, modifiedNanoseconds: 1, stagedFile: staged("e1"))
		let submitted = try submit(store, operationID: try #require(edit.mutation?.operationID), upload: "upl_1")
		// The feed shows the committed edit, then a later change from another device, before the answer arrives
		let committed = remoteFile("itm_a", "a.txt", rev: "after-e1", size: 6, content: "e1e1e1")
		let later = remoteFile("itm_a", "a.txt", rev: "later", size: 5, content: "later")
		try store.changes([committed])
		try store.changes([later])
		_ = try store.recordOperation(
			submitted, operation(submitted.record, state: .committed, item: committed, content: committed.contentID))
		#expect(try store.remoteItem(itemID: a.id)?.revision == later.revision)
		let shown = try #require(try store.item(id: a.id))
		#expect(shown.size == 5 && shown.blocksHash == later.contentID?.rawValue && shown.pending == .none)
	}

	/// The gateway binds an upload to its folder: an unsent creation moved to another folder uploads again
	@Test func movingAnUnsentCreationToAnotherFolderRestartsItsUpload() throws {
		let store = try makeRemoteStore()
		try store.bindRemoteFolder(
			RemoteFolderBinding(
				folderID: "music", accessMode: .remoteAPI, serviceID: testServiceID, remoteFolderID: "Music",
				permissions: RemotePermissions(read: true, write: true)),
			folder: FolderDescription(folderID: "music", label: "Music", folderType: "sendrecieve", isSelective: true),
			rootGatewayID: "itm_musicroot")
		try store.snapshot([remoteRoot])
		let root = try #require(try store.folder(id: "docs")).rootItemID
		let musicRoot = try #require(try store.folder(id: "music")).rootItemID
		let created = try store.recordRemoteCreation(
			folderID: "docs", parentItemID: root, name: "n.txt", kind: .file, modifiedNanoseconds: 1,
			stagedFile: staged("n"))
		let operationID = try #require(created.mutation?.operationID)
		var claim = try #require(try store.claimMutation(owner: "app", duration: 60))
		claim = try #require(
			try store.recordUploadCreated(
				claim, upload: UploadInfo(upload: "upl_d", token: "stu_d", offset: 6, length: 6, expires: .distantFuture),
				tokenReference: "upload:upl_d"))
		claim = try #require(try store.recordUploadOffset(claim, offset: 6))
		#expect(claim.record.state == .uploaded)

		// Within the folder, the upload stays
		let item = try #require(created.item)
		_ = try store.recordRemoteMove(itemID: item.id, toParentItemID: root, name: "m.txt")
		#expect(try store.mutation(operationID: operationID)?.uploadID == "upl_d")
		// To the other folder, it goes
		_ = try store.recordRemoteMove(itemID: item.id, toParentItemID: musicRoot, name: "m.txt")
		let moved = try #require(try store.mutation(operationID: operationID))
		#expect(moved.folderID == "music" && moved.state == .staged)
		#expect(moved.uploadID == nil && moved.uploadOffset == 0 && moved.uploadTokenReference == nil)
		#expect(try store.releasedTokenReferences() == ["upload:upl_d"])
	}

	/// Upload tokens are removed from the Keychain once no operation refers to them
	@Test func uploadTokensAreReleased() throws {
		let store = try makeRemoteStore()
		let credentials = InMemoryCredentialStore()
		try store.snapshot([remoteRoot, remoteFile("itm_a", "a.txt"), remoteFile("itm_b", "b.txt")])
		let a = try #require(try store.item(folderID: "docs", path: "a.txt"))
		let b = try #require(try store.item(folderID: "docs", path: "b.txt"))

		// A newer unsent edit replaces the upload
		let first = try store.recordRemoteContentChange(itemID: a.id, modifiedNanoseconds: 1, stagedFile: staged("e1"))
		credentials.setToken("stu_1", reference: "upload:upl_1")
		var claim = try #require(try store.claimMutation(owner: "app", duration: 60))
		claim = try #require(
			try store.recordUploadCreated(
				claim, upload: UploadInfo(upload: "upl_1", token: "stu_1", offset: 0, length: 6, expires: .distantFuture),
				tokenReference: "upload:upl_1"))
		store.removeReleasedTokens(from: credentials)
		#expect(credentials.tokenReferences == ["upload:upl_1"], "still in use")
		_ = try store.recordRemoteContentChange(itemID: a.id, modifiedNanoseconds: 2, stagedFile: staged("e2"))
		store.removeReleasedTokens(from: credentials)
		#expect(credentials.tokenReferences.isEmpty)

		// A committed operation, and a canceled one
		credentials.setToken("stu_2", reference: "upload:upl_2")
		let submitted = try submit(
			store, operationID: try #require(first.mutation?.operationID), upload: "upl_2",
			tokenReference: "upload:upl_2")
		_ = try store.recordOperation(
			submitted,
			operation(submitted.record, state: .committed, item: remoteFile("itm_a", "a.txt", rev: "e2", size: 6)))
		let other = try store.recordRemoteContentChange(itemID: b.id, modifiedNanoseconds: 1, stagedFile: staged("b1"))
		credentials.setToken("stu_3", reference: "upload:upl_3")
		var claimB = try #require(
			try store.claimMutation(owner: "app", duration: 60, operationID: try #require(other.mutation?.operationID)))
		claimB = try #require(
			try store.recordUploadCreated(
				claimB, upload: UploadInfo(upload: "upl_3", token: "stu_3", offset: 0, length: 6, expires: .distantFuture),
				tokenReference: "upload:upl_3"))
		try store.releaseClaim(claimB)
		_ = try store.discardMutation(operationID: claimB.operationID)
		store.removeReleasedTokens(from: credentials)
		#expect(credentials.tokenReferences.isEmpty)
		#expect(try store.releasedTokenReferences().isEmpty)
		#expect(try store.mutations().allSatisfy { $0.uploadTokenReference == nil })
	}

	/// A compound operation is never sent itself: a new authority generation leaves it to its steps
	@Test func authorityGenerationChangeLeavesCrossHostMovesToTheirSteps() throws {
		let store = try makeRemoteStore()
		try store.saveRemoteEndpoint(RemoteEndpoint(serviceID: "svc_other", host: "other.test", port: 443, displayName: "o"))
		try store.bindRemoteFolder(
			RemoteFolderBinding(
				folderID: "music", accessMode: .remoteAPI, serviceID: "svc_other", remoteFolderID: "Music",
				permissions: RemotePermissions(read: true, write: true)),
			folder: FolderDescription(folderID: "music", label: "Music", folderType: "sendrecieve", isSelective: true),
			rootGatewayID: "itm_musicroot")
		try store.snapshot([remoteRoot, remoteFile("itm_a", "a.txt")])
		try store.recordAuthorityGeneration(3, serviceID: testServiceID)
		let a = try #require(try store.item(folderID: "docs", path: "a.txt"))
		let move = try store.recordRemoteCrossHostMove(
			itemID: a.id, toParentItemID: try #require(try store.folder(id: "music")).rootItemID, name: "a.txt",
			stagedFile: staged("a"))
		let compoundID = try #require(move.mutation?.operationID)
		let copyStep = try submit(store, operationID: try store.mutationSteps(of: compoundID)[0].operationID, upload: "upl_m")
		var copy = remoteFile("itm_copy", "a.txt", parent: "itm_musicroot")
		copy.folderID = "Music"
		_ = try store.recordOperation(copyStep, operation(copyStep.record, state: .committed, item: copy))
		#expect(try store.mutation(operationID: compoundID)?.state == .submitted)

		#expect(try store.recordAuthorityGeneration(4, serviceID: testServiceID))
		#expect(try store.mutation(operationID: compoundID)?.state == .submitted, "not of unknown outcome")
		let deleteStep = try #require(try store.claimMutation(owner: "app", duration: 60))
		#expect(deleteStep.record.kind == .delete && deleteStep.record.step == 2)
	}

	/// Operations that wait for others never hold back a ready one behind them
	@Test func blockedOperationsDoNotStarveReadyOnes() throws {
		let store = try makeRemoteStore()
		try store.snapshot([remoteRoot, remoteFile("itm_b", "b.txt")])
		let root = try #require(try store.folder(id: "docs")).rootItemID
		let directory = try #require(
			try store.recordRemoteCreation(
				folderID: "docs", parentItemID: root, name: "new", kind: .directory, modifiedNanoseconds: 1,
				stagedFile: nil
			).item)
		// The directory's creation is being sent by the other process; 60 files inside wait for it
		_ = try #require(try store.claimMutation(owner: "extension", duration: 600))
		for index in 0..<60 {
			_ = try store.recordRemoteCreation(
				folderID: "docs", parentItemID: directory.id, name: "f\(index).txt", kind: .file,
				modifiedNanoseconds: 1, stagedFile: staged("f\(index)"))
		}
		let b = try #require(try store.item(folderID: "docs", path: "b.txt"))
		let deletion = try #require(try store.recordRemoteDeletion(itemID: b.id).mutation)
		try store.withConnection { connection throws(IndexError) in
			try connection.run(
				"UPDATE remote_outbox SET created_at = created_at + 1000 WHERE operation_id = ?", [deletion.operationID])
		}
		#expect(try store.claimMutation(owner: "app", duration: 60)?.operationID == deletion.operationID)
	}

	@Test func stepsOfACrossHostMoveInOrder() throws {
		let store = try makeRemoteStore()
		try store.saveRemoteEndpoint(RemoteEndpoint(serviceID: "svc_other", host: "other.test", port: 443, displayName: "o"))
		try store.bindRemoteFolder(
			RemoteFolderBinding(
				folderID: "music", accessMode: .remoteAPI, serviceID: "svc_other", remoteFolderID: "Music",
				permissions: RemotePermissions(read: true, write: true)),
			folder: FolderDescription(folderID: "music", label: "Music", folderType: "sendrecieve", isSelective: true),
			rootGatewayID: "itm_musicroot")
		try store.snapshot([remoteRoot, remoteFile("itm_a", "a.txt")])
		let a = try #require(try store.item(folderID: "docs", path: "a.txt"))
		let move = try store.recordRemoteCrossHostMove(
			itemID: a.id, toParentItemID: try #require(try store.folder(id: "music")).rootItemID, name: "a.txt",
			stagedFile: staged("a"))
		let steps = try store.mutationSteps(of: try #require(move.mutation?.operationID))
		#expect(steps.map(\.step) == [1, 2] && steps.map(\.kind) == [.create, .delete])
		#expect(steps[1].dependencies == [steps[0].operationID])
		#expect(try store.mutationSteps(of: steps[0].operationID).isEmpty)
	}
}
