// Copyright (C) 2026 Gordon
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this file,
// You can obtain one at https://mozilla.org/MPL/2.0/.
@preconcurrency import FileProvider
import Foundation
import SynctrainFileProviderKit
import UniformTypeIdentifiers

/// What a change needs of the system's template (which is not Sendable, and stays with the call)
struct ItemTemplate: Sendable {
	/// For a creation, the identifier the system gave its pending item (not one of ours): the same for the item when the
	/// system creates it again
	var identifier: String
	var parentIdentifier: NSFileProviderItemIdentifier
	var filename: String
	var contentType: UTType?
	var contentModificationDate: Date?
	var tagData: Data?
	var favoriteRank: Int64?
	var lastUsedDate: Date?

	init(_ item: NSFileProviderItem) {
		identifier = item.itemIdentifier.rawValue
		parentIdentifier = item.parentItemIdentifier
		filename = item.filename
		contentType = item.contentType ?? nil
		contentModificationDate = item.contentModificationDate ?? nil
		tagData = item.tagData ?? nil
		favoriteRank = (item.favoriteRank ?? nil).map { Int64(bitPattern: $0.uint64Value) }
		lastUsedDate = item.lastUsedDate ?? nil
	}
}

/// What a change of an item in a remote folder came to
struct RemoteChangeResult {
	/// The item as it is now (nil when the system's item is refused)
	var item: NSFileProviderItem?
	/// The system's contents are not the current version (after a conflict): it should fetch them again
	var shouldFetchContent = false
	/// Other items changed as well (a conflict copy was added, items inside a directory went with it)
	var changedOthers = false
}

extension RemoteProvider {
	/// How long a call waits for the gateway to commit a change it accepted (or for another process working on it)
	/// before the change is reported as not synchronized yet. Uploads take the time they need.
	static let commitWait: TimeInterval = 20

	// MARK: - Calls

	func createItem(
		basedOn itemTemplate: NSFileProviderItem, fields: NSFileProviderItemFields, contents url: URL?,
		options: NSFileProviderCreateItemOptions, parent: IndexItem, binding: RemoteFolderBinding,
		completionHandler: @escaping (NSFileProviderItem?, NSFileProviderItemFields, Bool, (any Error)?) -> Void
	) -> Progress {
		let template = ItemTemplate(itemTemplate)
		let progress = Progress(totalUnitCount: 1)
		let completion = Completion(call: completionHandler)
		let task = call { [self] in
			do {
				try Task.checkCancellation()
				let result = try await create(
					template, fields: fields, contents: url, options: options, parent: parent, binding: binding,
					progress: progress)
				progress.completedUnitCount = progress.totalUnitCount
				completion.call(result.item, [], result.shouldFetchContent, nil)
				if result.changedOthers {
					signalWorkingSet()
				}
			}
			catch {
				trace("remote: create \(template.filename) in \(parent.id) failed: \(error)")
				log.info("Could not create \(template.filename, privacy: .private): \(error.localizedDescription)")
				completion.call(nil, [], false, changeError(error, binding: binding, progress: progress))
			}
		}
		progress.cancellationHandler = { task.cancel() }
		return progress
	}

	func modifyItem(
		_ itemTemplate: NSFileProviderItem, baseVersion version: NSFileProviderItemVersion,
		changedFields: NSFileProviderItemFields, contents newContents: URL?, options: NSFileProviderModifyItemOptions,
		item: IndexItem, binding: RemoteFolderBinding,
		completionHandler: @escaping (NSFileProviderItem?, NSFileProviderItemFields, Bool, (any Error)?) -> Void
	) -> Progress {
		let template = ItemTemplate(itemTemplate)
		let baseContentVersion = version.contentVersion
		var failOnConflict = false
		if #available(iOS 26.0, macOS 26.0, *) {
			failOnConflict = options.contains(.failOnConflict)
		}
		let rejectsConflicts = failOnConflict
		let progress = Progress(totalUnitCount: 1)
		let completion = Completion(call: completionHandler)
		let task = call { [self] in
			do {
				try Task.checkCancellation()
				let result = try await modify(
					template, baseContentVersion: baseContentVersion, changedFields: changedFields,
					contents: newContents, failOnConflict: rejectsConflicts, item: item, binding: binding,
					progress: progress)
				progress.completedUnitCount = progress.totalUnitCount
				trace(
					"remote: modify \(item.id) fields \(changedFields.rawValue) fetch \(result.shouldFetchContent) \(memoryFootprint())"
				)
				completion.call(result.item, [], result.shouldFetchContent, nil)
				if result.changedOthers {
					signalWorkingSet()
				}
			}
			catch {
				trace("remote: modify \(item.id) fields \(changedFields.rawValue) failed: \(error)")
				log.info("Could not modify \(item.id): \(error.localizedDescription)")
				completion.call(nil, [], false, changeError(error, binding: binding, progress: progress))
			}
		}
		progress.cancellationHandler = { task.cancel() }
		return progress
	}

	func deleteItem(
		identifier: NSFileProviderItemIdentifier, baseVersion version: NSFileProviderItemVersion,
		options: NSFileProviderDeleteItemOptions, binding: RemoteFolderBinding,
		completionHandler: @escaping ((any Error)?) -> Void
	) -> Progress {
		let baseContentVersion = version.contentVersion
		let progress = Progress(totalUnitCount: 1)
		let completion = Completion(call: completionHandler)
		let task = call { [self] in
			do {
				try Task.checkCancellation()
				let removedDescendants = try await delete(
					identifier, baseContentVersion: baseContentVersion, options: options, binding: binding,
					progress: progress)
				progress.completedUnitCount = progress.totalUnitCount
				trace("remote: delete \(identifier.rawValue)")
				completion.call(nil)
				if removedDescendants {
					// The system learns about the items inside from the working set
					signalWorkingSet()
				}
			}
			catch {
				trace("remote: delete \(identifier.rawValue) failed: \(error)")
				log.info("Could not delete \(identifier.rawValue): \(error.localizedDescription)")
				completion.call(changeError(error, binding: binding, progress: progress))
			}
		}
		progress.cancellationHandler = { task.cancel() }
		return progress
	}

	// MARK: - Creating

	private func create(
		_ template: ItemTemplate, fields: NSFileProviderItemFields, contents: URL?,
		options: NSFileProviderCreateItemOptions, parent: IndexItem, binding: RemoteFolderBinding, progress: Progress
	) async throws -> RemoteChangeResult {
		let store = context.store
		guard parent.kind.isContainer else {
			throw NSFileProviderError(.noSuchItem)
		}
		let name = FileNames.normalized(template.filename)
		guard FileNames.isValid(name), !FileNames.isReserved(name), template.contentType != .symbolicLink else {
			throw NSFileProviderError(.excludedFromSync)
		}
		let kind: ItemKind = template.contentType?.conforms(to: .directory) == true ? .directory : .file
		let date = fields.contains(.contentModificationDate) ? template.contentModificationDate : nil
		let modifiedNs = Self.nanoseconds(date ?? Date())
		let existing = try store.child(of: parent.id, named: name)
		if kind == .file, contents == nil, options.contains(.mayAlreadyExist), existing == nil {
			// Reimported without contents, and not known here: the system removes its item
			return RemoteChangeResult(item: nil)
		}

		// Staged before anything is recorded, so that the outbox never refers to bytes that are not there
		var staged: StagedFile?
		if kind == .file, contents != nil || !options.contains(.mayAlreadyExist) {
			let file = try stage(contents, modifiedNs: modifiedNs)
			progress.totalUnitCount = max(file.size, 1)
			staged = file
		}
		if kind == .directory || staged != nil {
			let discard: RemoteDiscard?
			do {
				discard = try store.consumeDiscardedCreation(
					templateIdentifier: template.identifier, parentItemID: parent.id, name: name, kind: kind,
					digest: staged?.digest)
			}
			catch {
				discardStaged(staged)
				throw error
			}
			if let discard {
				// The user discarded this creation in Synctrain, and the system tries it again (also where it may no
				// longer create anything)
				discardStaged(staged)
				trace("remote: create \(name) in \(parent.id) was discarded (operation \(discard.operationID))")
				guard discard.templateIdentifier != nil else {
					// Recognized by its place and contents only (recorded before the system's identifier was kept), which
					// another item could share: the system keeps it on this device, without synchronizing it
					throw NSFileProviderError(.excludedFromSync)
				}
				// The system removes its item (and what is inside it)
				return RemoteChangeResult(item: nil)
			}
		}
		let earlier: (creation: MutationRecord, row: IndexItem)?
		var contents = staged
		var discardedContents = false
		do {
			guard binding.permissions.write else {
				throw RemoteFailure.readOnly(gateway: gatewayName(binding))
			}
			try checkNotSwitching(parent.folderID)
			earlier = try earlierCreation(for: template, kind: kind, binding: binding)
			if let earlier, let file = staged,
				try store.consumeDiscardedEdit(itemID: earlier.row.id, digest: file.digest) != nil
			{
				// The user discarded these contents in Synctrain: the system gets the item's current ones
				discardStaged(file)
				contents = nil
				discardedContents = true
			}
		}
		catch {
			discardStaged(staged)
			throw error
		}
		if let (creation, row) = earlier {
			// Created before for the same pending item of the system, which asks again: its creation continues
			var result: RemoteChangeResult
			if row.id == existing?.id, row.name == name {
				result = try await repeatCreation(
					of: row, kind: kind, name: name, staged: contents, template: template, fields: fields,
					binding: binding, progress: progress)
			}
			else {
				// Elsewhere, or under another name (that only the item itself stands in the way of)
				result = try await continueCreation(
					creation, of: row, existing: existing?.id == row.id ? nil : existing, staged: contents,
					template: template, fields: fields, progress: progress)
			}
			result.shouldFetchContent = result.shouldFetchContent || discardedContents
			return result
		}
		if let existing {
			return try await repeatCreation(
				of: existing, kind: kind, name: name, staged: staged, template: template, fields: fields,
				binding: binding, progress: progress)
		}

		let result: RemoteMutationResult
		do {
			result = try store.recordRemoteCreation(
				folderID: parent.folderID, parentItemID: parent.id, name: name, kind: kind,
				modifiedNanoseconds: modifiedNs, stagedFile: staged, origin: .system,
				templateIdentifier: template.identifier)
		}
		catch {
			discardStaged(staged)
			throw error
		}
		removeStagedFiles(result.releasedStagedFiles)
		guard let created = result.item, let mutation = result.mutation else {
			throw NSFileProviderError(.noSuchItem)
		}
		try context.saveAttributes(template, fields: fields, itemID: created.id)
		trace("remote: create \(name) in \(parent.id) -> \(created.id), operation \(mutation.operationID)")
		let record = try await settle(mutation.operationID, progress: progress)
		return try creationResult(record, itemID: created.id, parentID: parent.id, name: name, binding: binding)
	}

	/// The creation recorded for the system's pending item, if it continues now that the system creates the item again.
	/// It does not when its row is no longer in a folder accessed through a gateway, or when the item is now created in
	/// a folder on another gateway while nothing was sent yet: the creation is canceled then, and the item created there.
	private func earlierCreation(for template: ItemTemplate, kind: ItemKind, binding: RemoteFolderBinding) throws
		-> (creation: MutationRecord, row: IndexItem)?
	{
		let store = context.store
		guard let creation = try store.remoteCreation(templateIdentifier: template.identifier),
			let itemID = creation.itemID, let row = try store.item(id: itemID), row.kind == kind,
			let source = try context.remoteBinding(folderID: row.folderID)
		else {
			return nil
		}
		guard source.serviceID != binding.serviceID, row.pending == .create, creation.state.isUnsent else {
			return (creation, row)
		}
		let result = try store.recordRemoteDeletion(itemID: row.id)
		removeStagedFiles(result.releasedStagedFiles)
		trace("remote: creation \(creation.operationID) of \(row.id) canceled: created on another gateway instead")
		return nil
	}

	/// The system creates its pending item again somewhere else: it was renamed or moved while its creation was not
	/// synchronized (or before the system learned that it was). The creation goes to the new place instead when it was
	/// not sent yet, or the item moves there after it, and new contents follow as an edit (of the version the creation
	/// committed, if the row still shows it).
	private func continueCreation(
		_ creation: MutationRecord, of row: IndexItem, existing: IndexItem?, staged: StagedFile?,
		template: ItemTemplate, fields: NSFileProviderItemFields, progress: Progress
	) async throws -> RemoteChangeResult {
		let source: RemoteFolderBinding
		var base = row.contentRevision
		do {
			guard let binding = try context.remoteBinding(folderID: row.folderID) else {
				throw NSFileProviderError(.noSuchItem)
			}
			if let existing {
				throw collision(with: existing, binding: binding)
			}
			if row.pending != .create, try !showsCommittedContents(row, of: creation) {
				// Changed on the gateway since: other contents of the system's are kept as a conflict copy
				base = -1
			}
			source = binding
		}
		catch {
			discardStaged(staged)
			throw error
		}
		var changedFields = fields.union([.filename, .parentItemIdentifier])
		if staged == nil {
			changedFields.remove(.contents)
		}
		else {
			changedFields.insert(.contents)
		}
		trace("remote: create \(template.filename) again elsewhere -> \(row.id), operation \(creation.operationID)")
		return try await applyChanges(
			template, baseContentVersion: FileProviderItem.versionData(base), changedFields: changedFields,
			staged: staged, failOnConflict: false, item: row, binding: source, progress: progress)
	}

	/// The system creates an item that is there already: the repetition of a creation reported as not synchronized
	/// (which continues the operation recorded then), or an item that is the same (a directory, or a file with the same
	/// bytes, such as after the earlier attempt was committed meanwhile). Anything else is a collision. Takes care of
	/// the staged file.
	private func repeatCreation(
		of existing: IndexItem, kind: ItemKind, name: String, staged: StagedFile?, template: ItemTemplate,
		fields: NSFileProviderItemFields, binding: RemoteFolderBinding, progress: Progress
	) async throws -> RemoteChangeResult {
		var unrecorded = staged
		defer { discardStaged(unrecorded) }
		let store = context.store
		guard existing.kind == kind, existing.name == name else {
			throw collision(with: existing, binding: binding)
		}

		if existing.pending == .create, let overlay = try store.remoteOverlay(itemID: existing.id),
			let earlier = try store.mutation(operationID: overlay.operationID)
		{
			guard !earlier.state.isTerminal else {
				throw RemoteFailure.needsReview(gateway: gatewayName(binding))
			}
			var operationID = earlier.operationID
			if let file = staged, !hasSameContents(file, as: earlier, overlay: overlay) {
				// Changed since the first attempt: the newer contents take the place of the earlier ones (or follow them)
				let result = try store.recordRemoteContentChange(
					itemID: existing.id, modifiedNanoseconds: file.modifiedNs ?? existing.modifiedNanoseconds,
					stagedFile: file)
				unrecorded = nil
				removeStagedFiles(result.releasedStagedFiles)
				operationID = result.mutation?.operationID ?? operationID
			}
			try context.saveAttributes(template, fields: fields, itemID: existing.id)
			trace("remote: create \(name) again -> \(existing.id), operation \(operationID)")
			let record = try await settle(operationID, progress: progress)
			return try creationResult(
				record, itemID: existing.id, parentID: existing.parentID, name: name, binding: binding)
		}

		let isSame: Bool
		if kind == .directory {
			isSame = true
		}
		else if let file = staged {
			isSame = try localCopy(of: existing).map { Self.hasContents($0, of: file) } ?? false
		}
		else {
			// Reimported without contents: the system's item is this one
			isSame = true
		}
		guard isSame else {
			throw collision(with: existing, binding: binding)
		}
		try context.saveAttributes(template, fields: fields, itemID: existing.id)
		return RemoteChangeResult(item: try context.makeItem(existing))
	}

	/// Whether the item's row shows the contents a committed operation carried: the bytes the system gave for it. A
	/// refresh may have shown a newer version of the gateway since (or a later local change), which the system does not
	/// have; it then has to fetch the contents rather than take its bytes for that version.
	private func showsCommittedContents(_ row: IndexItem, of operation: MutationRecord) throws -> Bool {
		guard row.kind == .file else {
			return true
		}
		let store = context.store
		// As stored: the record a call returns may predate the result
		let record = try store.mutation(operationID: operation.operationID) ?? operation
		if let overlay = try store.remoteOverlay(itemID: row.id), overlay.operationID != record.operationID {
			return false
		}
		if let committed = record.committedContentID ?? record.result?.contentID ?? record.result?.item?.contentID {
			return row.blocksHash == committed.rawValue
		}
		// Without the content identity, the gateway's revision tells
		guard let revision = record.result?.item?.revision, let state = try store.remoteItem(itemID: row.id) else {
			return false
		}
		return state.revision == revision
	}

	/// Whether an unsent or unconfirmed change already carries the bytes of the staged file
	private func hasSameContents(_ staged: StagedFile, as record: MutationRecord, overlay: RemoteOverlay) -> Bool {
		if let digest = record.uploadDigest {
			return digest == staged.digest
		}
		return overlay.stagedPath.map { Self.hasContents(storage.url(relativePath: $0), of: staged) } ?? false
	}

	/// The result of a creation call, as `createdResult`. When the user discarded the change in Synctrain while the call
	/// waited for it, the system's change ends with the call: a discarded creation removes the system's item, and after
	/// a discarded edit the system gets the item's current contents.
	private func creationResult(
		_ record: MutationRecord, itemID: Int64, parentID: Int64?, name: String, binding: RemoteFolderBinding
	) throws -> RemoteChangeResult {
		if record.state == .canceled, let discard = try context.store.consumeDiscard(operationID: record.operationID) {
			if discard.kind == .create {
				return RemoteChangeResult(item: nil)
			}
			if let item = try context.store.item(id: itemID) {
				return RemoteChangeResult(item: context.makeItemForContents(item, remote: binding), shouldFetchContent: true)
			}
		}
		return try createdResult(record, itemID: itemID, parentID: parentID, name: name, binding: binding)
	}

	private func createdResult(
		_ record: MutationRecord, itemID: Int64, parentID: Int64?, name: String, binding: RemoteFolderBinding
	) throws -> RemoteChangeResult {
		let gateway = gatewayName(binding)
		switch record.state {
		case .committed:
			guard let item = try context.store.item(id: itemID) else {
				throw NSFileProviderError(.noSuchItem)
			}
			return RemoteChangeResult(
				item: context.makeItemForContents(item, remote: binding),
				shouldFetchContent: try !showsCommittedContents(item, of: record))
		case .failed:
			switch record.failureCode {
			case RemoteErrorCode.nameConflict:
				// The gateway has an item of that name that the index does not show yet. The creation is given up; the
				// system renames one of the two and creates its item again.
				try discard(record)
				refreshSoon(folderIDs: [record.folderID])
				if let parentID, let other = try context.store.child(of: parentID, named: name) {
					throw collision(with: other, binding: binding)
				}
				throw NSFileProviderError(.filenameCollision)
			case RemoteErrorCode.reservedName, RemoteErrorCode.ignoredPath:
				// The gateway would not show it: the system keeps the item without synchronizing it
				try discard(record)
				throw NSFileProviderError(.excludedFromSync)
			case RemoteErrorCode.forbidden, RemoteErrorCode.folderReadOnly:
				// Kept (with its staged contents) for review in the app
				throw RemoteFailure.readOnly(gateway: gateway)
			default:
				throw RemoteFailure.notAccepted(gateway: gateway)
			}
		case .outcomeUnknown:
			throw RemoteFailure.needsReview(gateway: gateway)
		case .canceled:
			// Given up in the app
			throw RemoteFailure.notAccepted(gateway: gateway)
		default:
			guard let item = try context.store.item(id: itemID) else {
				throw NSFileProviderError(.noSuchItem)
			}
			return RemoteChangeResult(item: try context.makeItem(item))
		}
	}

	// MARK: - Modifying

	private func modify(
		_ template: ItemTemplate, baseContentVersion: Data, changedFields: NSFileProviderItemFields, contents: URL?,
		failOnConflict: Bool, item: IndexItem, binding: RemoteFolderBinding, progress: Progress
	) async throws -> RemoteChangeResult {
		let moves = changedFields.contains(.filename) || changedFields.contains(.parentItemIdentifier)
		let writes = item.kind == .file && changedFields.contains(.contents) && contents != nil
		guard moves || writes else {
			// Fields only the system keeps (opening a file updates its last used date, for instance), or modification
			// dates, which the gateway changes only with the contents: the system takes the item as it is
			try context.saveAttributes(template, fields: changedFields, itemID: item.id)
			return RemoteChangeResult(item: try context.makeItem(item))
		}

		// Staged before anything is recorded
		var staged: StagedFile?
		if writes, let contents {
			let date = changedFields.contains(.contentModificationDate) ? template.contentModificationDate : nil
			staged = try stage(contents, modifiedNs: Self.nanoseconds(date ?? Date()))
		}
		return try await applyChanges(
			template, baseContentVersion: baseContentVersion, changedFields: changedFields, staged: staged,
			failOnConflict: failOnConflict, item: item, binding: binding, progress: progress)
	}

	/// Records and drives a move (from the template's place) and new contents (the staged file, which this takes care
	/// of), in order
	private func applyChanges(
		_ template: ItemTemplate, baseContentVersion: Data, changedFields: NSFileProviderItemFields, staged: StagedFile?,
		failOnConflict: Bool, item: IndexItem, binding: RemoteFolderBinding, progress: Progress
	) async throws -> RemoteChangeResult {
		let store = context.store
		let moves = changedFields.contains(.filename) || changedFields.contains(.parentItemIdentifier)
		var unrecorded = staged
		defer { discardStaged(unrecorded) }
		if let staged {
			progress.totalUnitCount = max(staged.size, 1)
		}
		var discardedContents = false
		if let file = staged, try store.consumeDiscardedEdit(itemID: item.id, digest: file.digest) != nil {
			// The user discarded these contents in Synctrain, and the system tries the edit again: it gets the item's
			// current contents instead (also where it may no longer change anything)
			trace("remote: edit of \(item.id) was discarded")
			discardedContents = true
			if !moves {
				try context.saveAttributes(template, fields: changedFields, itemID: item.id)
				guard let current = try store.item(id: item.id) else {
					throw NSFileProviderError(.noSuchItem)
				}
				return RemoteChangeResult(item: context.makeItemForContents(current, remote: binding), shouldFetchContent: true)
			}
		}
		guard binding.permissions.write else {
			throw RemoteFailure.readOnly(gateway: gatewayName(binding))
		}
		try checkNotSwitching(item.folderID)

		var current = item
		var move: String?
		if moves {
			let recorded = try await recordMove(
				current, to: template.parentIdentifier, name: template.filename, binding: binding, progress: progress)
			current = recorded.item
			move = recorded.operationID
		}
		var write: ContentRecording?
		if let file = unrecorded, !discardedContents {
			let recorded = try recordContents(
				file, of: current, baseContentVersion: baseContentVersion, failOnConflict: failOnConflict,
				binding: binding)
			if recorded.consumedStaged {
				unrecorded = nil
			}
			write = recorded
		}
		try context.saveAttributes(template, fields: changedFields, itemID: current.id)

		// In order: a change of the contents after a move waits for the move
		var result = RemoteChangeResult(shouldFetchContent: discardedContents)
		/// The committed operation that carried the system's contents, if one did
		var committedContents: MutationRecord?
		if let move {
			let record = try await settle(move, progress: write == nil ? progress : nil)
			if record.kind == .create {
				// The item's creation was not sent yet, and goes to the new place instead (with the system's contents)
				_ = try createdResult(
					record, itemID: current.id, parentID: current.parentID, name: current.name, binding: binding)
				committedContents = record
			}
			else {
				result.changedOthers = try movedResult(record, binding: binding)
			}
		}
		if let write {
			if write.isConflictCopy {
				// The item stays the gateway's version; the edit is its own item, which the outbox sends if it cannot be
				// sent now
				result.shouldFetchContent = true
				result.changedOthers = true
				if let operationID = write.operationID {
					_ = try? await settle(operationID, progress: progress)
				}
			}
			else if let operationID = write.operationID {
				let record = try await settle(operationID, progress: progress)
				if record.kind == .create {
					// The item's creation was not sent yet, and carries the new contents instead
					_ = try createdResult(
						record, itemID: current.id, parentID: current.parentID, name: current.name, binding: binding)
				}
				let written = try await writtenResult(record, binding: binding)
				result.shouldFetchContent = result.shouldFetchContent || written.shouldFetchContent
				result.changedOthers = result.changedOthers || written.changedOthers
				if record.state == .committed {
					committedContents = record
				}
			}
		}
		guard let updated = try store.item(id: item.id) else {
			throw NSFileProviderError(.noSuchItem)
		}
		if !result.shouldFetchContent, updated.kind == .file {
			if let committedContents {
				// The returned item describes the system's bytes only if the row still shows what was committed
				result.shouldFetchContent = try !showsCommittedContents(updated, of: committedContents)
			}
			else if updated.contentRevision != current.contentRevision {
				// Only moved (or the same bytes again): the contents changed on the gateway meanwhile
				result.shouldFetchContent = true
			}
		}
		result.item = context.makeItemForContents(updated, remote: binding)
		return result
	}

	/// How the system's new contents were recorded
	private struct ContentRecording {
		/// The operation that carries them (nil when the item has these bytes already)
		var operationID: String?
		/// Whether the outbox holds the staged file now
		var consumedStaged: Bool
		/// They were kept as a conflict copy instead (the system's version was not the current one)
		var isConflictCopy = false
	}

	/// Records new contents of a file. Bytes the item has already are not sent. An edit of a version older than the index
	/// shows is kept as a conflict copy, or refused when the system asks for that.
	private func recordContents(
		_ staged: StagedFile, of item: IndexItem, baseContentVersion: Data, failOnConflict: Bool,
		binding: RemoteFolderBinding
	) throws -> ContentRecording {
		let store = context.store
		let overlay = try store.remoteOverlay(itemID: item.id)
		let earlier = try overlay.flatMap { try store.mutation(operationID: $0.operationID) }
		if let earlier, earlier.state == .failed || earlier.state == .outcomeUnknown {
			throw RemoteFailure.needsReview(gateway: gatewayName(binding))
		}
		let modifiedNs = staged.modifiedNs ?? item.modifiedNanoseconds
		if let overlay, let earlier, !earlier.state.isTerminal {
			// The row shows a local change the gateway has not confirmed, so the system cannot have seen another version:
			// these contents follow that change. The same bytes again are the system repeating an attempt.
			if hasSameContents(staged, as: earlier, overlay: overlay) {
				return ContentRecording(operationID: earlier.operationID, consumedStaged: false)
			}
			if overlay.stagedPath == nil, let copy = try localCopy(of: item), Self.hasContents(copy, of: staged) {
				// A pending move or rename keeps the gateway's contents, and these bytes are them: nothing to send
				return ContentRecording(operationID: nil, consumedStaged: false)
			}
			let result = try store.recordRemoteContentChange(
				itemID: item.id, modifiedNanoseconds: modifiedNs, stagedFile: staged,
				onConflict: failOnConflict ? .reject : .keepBoth)
			removeStagedFiles(result.releasedStagedFiles)
			return ContentRecording(operationID: result.mutation?.operationID, consumedStaged: true)
		}
		if let copy = try localCopy(of: item), Self.hasContents(copy, of: staged) {
			// The item has these bytes already: an app saved a file it only opened, or an earlier attempt was committed
			// meanwhile. Sending them would only change the file's modification time on the gateway.
			return ContentRecording(operationID: nil, consumedStaged: false)
		}
		if LocalWriter.revision(baseContentVersion) != item.contentRevision {
			// The file changed on the gateway since the system's version
			if failOnConflict, #available(iOS 26.0, macOS 26.0, *) {
				throw NSFileProviderError(.localVersionConflictingWithServer)
			}
			let copy = try recordConflictCopy(staged, of: item)
			return ContentRecording(operationID: copy, consumedStaged: true, isConflictCopy: true)
		}
		let result = try store.recordRemoteContentChange(
			itemID: item.id, modifiedNanoseconds: modifiedNs, stagedFile: staged,
			onConflict: failOnConflict ? .reject : .keepBoth)
		removeStagedFiles(result.releasedStagedFiles)
		return ContentRecording(operationID: result.mutation?.operationID, consumedStaged: true)
	}

	/// Keeps an edit as a new file next to the item, named the way Syncthing names conflict copies. Returns its
	/// creation.
	private func recordConflictCopy(_ staged: StagedFile, of item: IndexItem) throws -> String? {
		guard let parentID = item.parentID else {
			throw NSFileProviderError(.noSuchItem)
		}
		let shortID = ((try? context.store.localShortDeviceID()) ?? nil) ?? "LOCAL"
		var date = Date()
		for attempt in 0...5 {
			let name = FileNames.conflictCopyName(for: item.name, date: date, shortDeviceID: shortID)
			do throws(IndexError) {
				// Made here, so the system learns about it from enumerations (it is listed while it is not committed)
				let result = try context.store.recordRemoteCreation(
					folderID: item.folderID, parentItemID: parentID, name: name, kind: .file,
					modifiedNanoseconds: staged.modifiedNs ?? item.modifiedNanoseconds, stagedFile: staged,
					origin: .provider)
				log.info("Kept an edit of \(item.id) as conflict copy \(result.item?.id ?? 0)")
				return result.mutation?.operationID
			}
			catch .nameCollision where attempt < 5 {
				// Another conflict copy from the same second
				date.addTimeInterval(1)
			}
		}
		throw NSFileProviderError(.filenameCollision)
	}

	private func writtenResult(_ record: MutationRecord, binding: RemoteFolderBinding) async throws
		-> RemoteChangeResult
	{
		let gateway = gatewayName(binding)
		switch record.state {
		case .committed:
			return RemoteChangeResult()
		case .conflict:
			// The file changed on the gateway: it kept the edit as a conflict copy, which the system gets through the
			// working set, and the system gets the current version
			return RemoteChangeResult(shouldFetchContent: true, changedOthers: true)
		case .failed:
			switch record.failureCode {
			case RemoteErrorCode.revisionMismatch where record.conflictPolicy == .reject:
				// The system merges the versions and tries again with the current one
				try discard(record)
				refreshSoon(folderIDs: [record.folderID])
				if #available(iOS 26.0, macOS 26.0, *) {
					throw NSFileProviderError(.localVersionConflictingWithServer)
				}
				throw RemoteFailure.notAccepted(gateway: gateway)
			case RemoteErrorCode.revisionMismatch:
				// Refused although it could have kept both: the edit is kept as a conflict copy here
				guard let path = record.stagedPath, let itemID = record.itemID else {
					throw RemoteFailure.notAccepted(gateway: gateway)
				}
				let copy = try RemoteUploader.stage(
					storage.url(relativePath: path), in: storage, modifiedNs: record.modifiedNs)
				do {
					try discard(record)
					guard let item = try context.store.item(id: itemID) else {
						throw NSFileProviderError(.noSuchItem)
					}
					if let creation = try recordConflictCopy(copy, of: item) {
						_ = try? await settle(creation, progress: nil)
					}
				}
				catch {
					discardStaged(copy)
					throw error
				}
				return RemoteChangeResult(shouldFetchContent: true, changedOthers: true)
			case RemoteErrorCode.forbidden, RemoteErrorCode.folderReadOnly:
				throw RemoteFailure.readOnly(gateway: gateway)
			default:
				throw RemoteFailure.notAccepted(gateway: gateway)
			}
		case .outcomeUnknown:
			throw RemoteFailure.needsReview(gateway: gateway)
		case .canceled:
			// Given up in the app: the gateway's version stands. The system's edit ends with this call, so it will not
			// make it again.
			_ = try context.store.consumeDiscard(operationID: record.operationID)
			return RemoteChangeResult(shouldFetchContent: true)
		default:
			return RemoteChangeResult()
		}
	}

	/// What a move recorded
	private struct MoveRecording {
		var item: IndexItem
		/// The operation to drive (nil when the item is where the system put it)
		var operationID: String?
	}

	/// Records a rename or a move: within the gateway (to another of its folders as well), or of a file to a folder on
	/// another gateway (a copy there, then the deletion here)
	private func recordMove(
		_ item: IndexItem, to parentIdentifier: NSFileProviderItemIdentifier, name rawName: String,
		binding: RemoteFolderBinding, progress: Progress
	) async throws -> MoveRecording {
		let store = context.store
		guard item.kind != .folderRoot else {
			throw Rejection.cannotSynchronize(String(localized: "Folders shared with Syncthing cannot be renamed here."))
		}
		guard let parentID = parentIdentifier.itemID, let parent = try store.item(id: parentID) else {
			// The system creates the parent first
			throw NSError.fileProviderErrorForNonExistentItem(withIdentifier: parentIdentifier)
		}
		guard parent.kind.isContainer else {
			throw Rejection.cannotSynchronize(String(localized: "Items can only be moved into a folder."))
		}
		let name = FileNames.normalized(rawName)
		guard FileNames.isValid(name), !FileNames.isReserved(name) else {
			// Refused by keeping the item as it is: the item returned carries its current name and place, which the
			// system takes back. An error would leave the system retrying a change that can never be sent.
			log.info("Not moving \(item.id) to a name reserved for Syncthing")
			return MoveRecording(item: item, operationID: nil)
		}
		if parent.id == item.parentID && name == item.name {
			// Where the system put it: the repetition of a move reported as not synchronized continues
			return MoveRecording(item: item, operationID: try unresolvedMove(of: item))
		}
		if item.kind.isContainer, parent.folderID == item.folderID,
			FileNames.isSelfOrDescendant(parent.path, of: item.path)
		{
			throw Rejection.cannotSynchronize(String(localized: "A folder cannot be moved into itself."))
		}
		guard let target = try context.remoteBinding(folderID: parent.folderID) else {
			// A folder that Syncthing on this device synchronizes
			throw RemoteFailure.acrossEngines
		}
		guard target.permissions.write else {
			throw RemoteFailure.readOnly(gateway: gatewayName(target))
		}

		let result: RemoteMutationResult
		if target.serviceID == binding.serviceID {
			result = try store.recordRemoteMove(itemID: item.id, toParentItemID: parent.id, name: name)
		}
		else {
			guard item.kind == .file else {
				throw RemoteFailure.crossHostDirectory
			}
			// The copy on the other gateway is made from a staged copy of the contents, downloaded first if needed
			let source = try await contentsForCopy(of: item, binding: binding, progress: progress)
			let staged = try RemoteUploader.stage(source, in: storage, modifiedNs: item.modifiedNanoseconds)
			do {
				result = try store.recordRemoteCrossHostMove(
					itemID: item.id, toParentItemID: parent.id, name: name, stagedFile: staged)
			}
			catch {
				discardStaged(staged)
				throw error
			}
		}
		removeStagedFiles(result.releasedStagedFiles)
		trace("remote: move \(item.id) to \(parent.id), operation \(result.mutation?.operationID ?? "none")")
		return MoveRecording(item: result.item ?? item, operationID: result.mutation?.operationID)
	}

	/// The latest move of the item that the gateway has not resolved: the overlay's operation, or an earlier one that a
	/// later change of the item (such as an edit after the move) follows
	private func unresolvedMove(of item: IndexItem) throws -> String? {
		let store = context.store
		guard let overlay = try store.remoteOverlay(itemID: item.id) else {
			return nil
		}
		if let latest = try store.mutation(operationID: overlay.operationID), !latest.state.isTerminal,
			latest.kind == .move || latest.kind == .crossHostMove
		{
			return latest.operationID
		}
		let unresolved = try store.mutations(
			folderID: item.folderID, states: [.staged, .uploading, .uploaded, .submitted, .pending], limit: 1000)
		return unresolved.last {
			$0.itemID == item.id && $0.parentOperationID == nil && ($0.kind == .move || $0.kind == .crossHostMove)
		}?.operationID
	}

	/// The contents of a file this device holds, downloaded when it does not
	private func contentsForCopy(of item: IndexItem, binding: RemoteFolderBinding, progress: Progress) async throws
		-> URL
	{
		if let local = try localCopy(of: item) {
			return local
		}
		guard let state = try context.store.remoteItem(itemID: item.id), state.contentID?.rawValue == item.blocksHash
		else {
			throw RemoteFailure.versionChanged
		}
		return try await download(state, binding: binding, progress: progress)
	}

	/// Checks the outcome of a move; returns whether other items changed (the original of a move to another gateway
	/// that could not be deleted is shown again)
	private func movedResult(_ record: MutationRecord, binding: RemoteFolderBinding) throws -> Bool {
		let gateway = gatewayName(binding)
		switch record.state {
		case .committed:
			return false
		case .failed:
			if record.kind == .crossHostMove,
				try context.store.mutationSteps(of: record.operationID).first?.state == .committed
			{
				// The copy is where the system put the item; the original remains as well
				return true
			}
			switch record.failureCode {
			case RemoteErrorCode.nameConflict:
				refreshSoon(folderIDs: [record.folderID])
				throw NSFileProviderError(.filenameCollision)
			case RemoteErrorCode.revisionMismatch:
				// The item changed on the gateway; the index has its current revision now, which the system's next
				// attempt moves
				refreshSoon(folderIDs: [record.folderID])
				throw RemoteFailure.transient(
					String(localized: "This item just changed on another device. Synctrain tries again shortly."))
			case RemoteErrorCode.forbidden, RemoteErrorCode.folderReadOnly:
				throw RemoteFailure.readOnly(gateway: gateway)
			default:
				throw RemoteFailure.notAccepted(gateway: gateway)
			}
		case .outcomeUnknown:
			throw RemoteFailure.needsReview(gateway: gateway)
		default:
			// Given up in the app: the item is back where it was, and the system moves it there
			return false
		}
	}

	// MARK: - Deleting

	/// Returns whether items inside the deleted one were deleted as well
	private func delete(
		_ identifier: NSFileProviderItemIdentifier, baseContentVersion: Data, options: NSFileProviderDeleteItemOptions,
		binding: RemoteFolderBinding, progress: Progress
	) async throws -> Bool {
		guard let id = identifier.itemID else {
			return false
		}
		let store = context.store
		guard let item = try store.item(id: id) else {
			// Gone already. The repetition of a deletion reported as not synchronized continues.
			if let overlay = try store.remoteOverlay(itemID: id), overlay.pending == .delete,
				let earlier = try store.mutation(operationID: overlay.operationID), !earlier.state.isTerminal
			{
				return try deletedResult(try await settle(earlier.operationID, progress: progress), binding: binding)
			}
			return false
		}
		guard item.kind != .folderRoot, binding.permissions.write else {
			throw NSFileProviderError(.deletionRejected)
		}
		try checkNotSwitching(item.folderID)
		// Deleting removes the file on all devices, so only the version the user saw may be deleted. While the row shows a
		// local change, the system cannot have seen another one.
		if item.kind == .file, item.pending == .none, LocalWriter.revision(baseContentVersion) != item.contentRevision {
			throw NSFileProviderError(.deletionRejected)
		}
		let hasDescendants = item.kind.isContainer ? try store.hasDescendants(of: item) : false
		if hasDescendants && !options.contains(.recursive) {
			throw NSFileProviderError(.directoryNotEmpty)
		}
		if let overlay = try store.remoteOverlay(itemID: id),
			let earlier = try store.mutation(operationID: overlay.operationID),
			earlier.state == .failed || earlier.state == .outcomeUnknown
		{
			guard item.pending == .create else {
				throw NSFileProviderError(.deletionRejected)
			}
			// A creation the gateway did not accept: deleting the item gives it up
			try discard(earlier)
			return hasDescendants
		}

		let result = try store.recordRemoteDeletion(itemID: id)
		removeStagedFiles(result.releasedStagedFiles)
		guard let mutation = result.mutation else {
			// The creation of the item was not sent yet, and is canceled: nothing to send
			return hasDescendants
		}
		let removedOthers = try deletedResult(try await settle(mutation.operationID, progress: progress), binding: binding)
		return removedOthers || hasDescendants
	}

	private func deletedResult(_ record: MutationRecord, binding: RemoteFolderBinding) throws -> Bool {
		switch record.state {
		case .committed:
			return record.itemKind?.isContainer == true
		case .outcomeUnknown:
			throw RemoteFailure.needsReview(gateway: gatewayName(binding))
		case .failed, .canceled:
			// Shown again, with what is inside it; the system re-creates it
			signalWorkingSet()
			if record.failureCode == RemoteErrorCode.notEmpty {
				throw NSFileProviderError(.directoryNotEmpty)
			}
			if let itemID = record.itemID, let item = try context.store.item(id: itemID) {
				throw NSError.fileProviderErrorForRejectedDeletion(of: try context.makeItem(item))
			}
			throw NSFileProviderError(.deletionRejected)
		default:
			return false
		}
	}

	// MARK: - Operations

	/// Drives the operation, after the ones it depends on (and the steps of a compound one), until the gateway has
	/// resolved it. Throws `RemoteChangeWaiting` when it is not resolved yet: an attempt failed (such as when the gateway
	/// cannot be reached), another process holds it, the gateway still applies it after `commitWait`, or the gateway
	/// speaks another version of the API.
	func settle(_ operationID: String, progress: Progress?) async throws -> MutationRecord {
		let store = context.store
		/// Not resolved here: the app sends what the extension cannot finish (claims keep the two from sending it
		/// twice). Only then is it told, so that it does not take over an operation the extension is working on.
		func handedOver(_ record: MutationRecord, reason: RemoteError? = nil) -> RemoteChangeWaiting {
			SharedNotification.workQueued.post()
			let uploading = (try? store.runningBackgroundUpload(of: record)) != nil
			return RemoteChangeWaiting(record: record, reason: reason, uploadingInBackground: uploading)
		}
		// Called because the system makes (or repeats) the change now: no need to wait for a delay an earlier attempt set
		try store.expediteMutation(operationID: operationID)
		/// Since when the gateway has been applying it, or another process has held it
		var waitingSince: Date?
		while true {
			try Task.checkCancellation()
			guard let record = try store.mutation(operationID: operationID) else {
				throw NSFileProviderError(.noSuchItem)
			}
			if record.state.isTerminal {
				return record
			}
			if let version = incompatibleVersion(serviceID: record.serviceID) {
				// Kept until one of the two is updated (the app does not send it either)
				throw RemoteChangeWaiting(record: record, reason: nil, incompatibleVersion: version)
			}
			var waitedForOthers = false
			for dependency in record.dependencies {
				if let earlier = try store.mutation(operationID: dependency), !earlier.state.isTerminal {
					_ = try await settle(dependency, progress: nil)
					waitedForOthers = true
				}
			}
			if record.kind == .crossHostMove {
				for step in try store.mutationSteps(of: operationID) where !step.state.isTerminal {
					_ = try await settle(step.operationID, progress: step.step == 1 ? progress : nil)
					waitedForOthers = true
				}
				guard waitedForOthers else {
					// Its steps are resolved, and it follows them
					throw handedOver(record)
				}
			}
			if waitedForOthers {
				continue
			}

			// Upload progress, from the offsets the gateway confirmed
			let monitor = progress.map { progress in
				Task { [store] in
					while !Task.isCancelled {
						try? await Task.sleep(for: .milliseconds(500))
						if let current = try? store.mutation(operationID: operationID), (current.uploadLength ?? 0) > 0 {
							progress.completedUnitCount = min(current.uploadOffset, max(progress.totalUnitCount - 1, 0))
						}
					}
				}
			}
			defer { monitor?.cancel() }
			let outcome: Outbox.Outcome
			do throws(RemoteError) {
				outcome = try await outbox.process(operationID: operationID)
			}
			catch {
				// Errors that concern the whole gateway (no credential, an untrusted certificate) or a canceled call
				throw handedOver((try? store.mutation(operationID: operationID)) ?? record, reason: error)
			}
			switch outcome {
			case .resolved(let resolved):
				return resolved
			case .waiting(let waiting, let notBefore):
				// Accepted and still being applied: ask again shortly. Any other attempt failed, and is tried later.
				let since = waitingSince ?? Date()
				waitingSince = since
				let next = notBefore ?? Date()
				guard waiting.state == .pending, next.timeIntervalSince(since) < Self.commitWait else {
					throw handedOver(waiting)
				}
				try await Task.sleep(for: .seconds(max(0.05, next.timeIntervalSinceNow)))
			case .lost:
				// Another process works on it (or it was resolved meanwhile)
				let since = waitingSince ?? Date()
				waitingSince = since
				guard Date().timeIntervalSince(since) < Self.commitWait else {
					throw handedOver(record)
				}
				try await Task.sleep(for: .milliseconds(500))
				try store.expediteMutation(operationID: operationID)
			}
		}
	}

	/// Gives up an operation (and shows the gateway's state of its item again). Unlike a discard in the app, this is not
	/// remembered: the system makes the change differently (renamed after a collision, merged after a conflict), or not
	/// at all.
	private func discard(_ record: MutationRecord) throws {
		let result = try context.store.discardMutation(operationID: record.operationID, remembering: false)
		removeStagedFiles(result.releasedStagedFiles)
	}

	// MARK: - Helpers

	/// Stages the system's contents (an empty file without them) with the modification time
	private func stage(_ contents: URL?, modifiedNs: Int64) throws -> StagedFile {
		if let contents {
			return try RemoteUploader.stage(contents, in: storage, modifiedNs: modifiedNs)
		}
		try FileManager.default.createDirectory(at: storage.transfersURL, withIntermediateDirectories: true)
		let empty = storage.transfersURL.appending(path: "empty-\(UUID().uuidString)")
		guard FileManager.default.createFile(atPath: empty.path(percentEncoded: false), contents: nil) else {
			throw CocoaError(.fileWriteUnknown)
		}
		defer { try? FileManager.default.removeItem(at: empty) }
		return try RemoteUploader.stage(empty, in: storage, modifiedNs: modifiedNs)
	}

	/// New changes wait while the folder is switched between Syncthing on this device and its gateway (the system tries
	/// them again on its own)
	private func checkNotSwitching(_ folderID: String) throws {
		if try context.store.isRemoteMigrationFenced(folderID: folderID) {
			throw RemoteFailure.transient(
				String(localized: "Synctrain is switching how this folder is synchronized. It tries again shortly."))
		}
	}

	static func nanoseconds(_ date: Date) -> Int64 {
		Int64((date.timeIntervalSince1970 * 1_000_000_000).rounded())
	}

	private func collision(with existing: IndexItem, binding: RemoteFolderBinding) -> Error {
		NSError.fileProviderErrorForCollision(with: context.makeItemForContents(existing, remote: binding))
	}

	/// The error to report for a change that did not complete
	private func changeError(_ error: any Error, binding: RemoteFolderBinding, progress: Progress) -> Error {
		if progress.isCancelled || error is CancellationError {
			return CocoaError(.userCancelled)
		}
		let gateway = gatewayName(binding)
		switch error {
		case let waiting as RemoteChangeWaiting:
			if waiting.reason == .canceled {
				return CocoaError(.userCancelled)
			}
			let serviceID = waiting.record.serviceID ?? binding.serviceID
			reported(waiting.reason == .unauthenticated ? .notAuthenticated : .cannotSynchronize, serviceID: serviceID)
			return RemoteFailure.waiting(waiting, gateway: gateway)
		case let failure as RemoteError:
			// Before anything was recorded (such as the download for a move to another gateway)
			switch failure {
			case .canceled:
				return CocoaError(.userCancelled)
			case .unauthenticated:
				reported(.notAuthenticated, serviceID: binding.serviceID)
				return RemoteFailure.notAuthenticated(gateway: gateway)
			case .unreachable, .unavailable, .overloaded:
				reported(.cannotSynchronize, serviceID: binding.serviceID)
				return Rejection.cannotSynchronize(
					String(localized: "Synctrain makes this change once \(gateway) can be reached."))
			default:
				return RemoteFailure.transient(
					String(localized: "Synctrain could not prepare this change. It tries again later."),
					underlying: failure)
			}
		case let failure as IndexError:
			if case .nameCollision(let existingID) = failure, let existing = try? context.store.item(id: existingID) {
				return collision(with: existing, binding: binding)
			}
			return RemoteFailure.recording(failure, gateway: gateway)
		default:
			return FileProviderExtension.reportable(error)
		}
	}
}

extension ExtensionContext {
	/// Stores the attributes only the system keeps (tags, favorite rank, last used date) that are among the fields
	func saveAttributes(_ template: ItemTemplate, fields: NSFileProviderItemFields, itemID: Int64) throws(IndexError) {
		guard !fields.isDisjoint(with: [.tagData, .favoriteRank, .lastUsedDate]) else {
			return
		}
		var attributes = try store.attributes(of: [itemID])[itemID] ?? ItemAttributes()
		if fields.contains(.tagData) {
			attributes.tagData = template.tagData
		}
		if fields.contains(.favoriteRank) {
			attributes.favoriteRank = template.favoriteRank
		}
		if fields.contains(.lastUsedDate) {
			attributes.lastUsedDate = template.lastUsedDate
		}
		try store.setAttributes(attributes, of: itemID)
	}
}
