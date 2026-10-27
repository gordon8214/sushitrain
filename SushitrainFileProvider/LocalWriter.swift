// Copyright (C) 2026 Gordon
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this file,
// You can obtain one at https://mozilla.org/MPL/2.0/.
import Darwin
@preconcurrency import FileProvider
import Foundation
import SynctrainFileProviderKit
import UniformTypeIdentifiers

/// Errors for changes that cannot be made. The system does not retry these (it retries any other error).
enum Rejection {
	static func cannotSynchronize(_ reason: String) -> Error {
		NSError(
			domain: NSFileProviderErrorDomain, code: NSFileProviderError.cannotSynchronize.rawValue,
			userInfo: [NSLocalizedDescriptionKey: reason])
	}

	static var readOnlyFolder: Error {
		cannotSynchronize(String(localized: "This folder only receives changes from other devices."))
	}

	static var notOnDevice: Error {
		cannotSynchronize(String(localized: "Keep this item on this device to change it."))
	}
}

/// Applies changes made in the Files app to the folders on disk and records them in the index. Syncthing picks them up
/// when the app has the changed paths scanned (and, in on-demand folders, selected), which it is asked to do through the
/// work queue. Writes go through a staging directory and land with an atomic rename, so Syncthing never sees a
/// partially written file.
struct LocalWriter: Sendable {
	let context: ExtensionContext

	struct Outcome {
		var item: IndexItem
		/// The version the system has is not current (after a conflict); it should fetch the contents again
		var shouldFetchContent = false
		/// Other items changed as well (a conflict copy was added)
		var addedItems = false
	}

	private var store: IndexStore { context.store }

	// MARK: - Creating

	func create(
		template: NSFileProviderItem, fields: NSFileProviderItemFields, contents: URL?,
		options: NSFileProviderCreateItemOptions
	) throws -> IndexItem {
		guard let parentID = template.parentItemIdentifier.itemID else {
			// When the system reimports, it asks about the folders themselves; nothing else can be added at the top
			let key = FileNames.collisionKey(template.filename)
			if options.contains(.mayAlreadyExist),
				let root = try store.children(of: nil, limit: ExtensionContext.maximumPageSize).first(where: {
					FileNames.collisionKey($0.name) == key
				})
			{
				return root
			}
			throw Rejection.cannotSynchronize(String(localized: "Items can only be added inside a folder."))
		}
		guard let parent = try store.item(id: parentID), parent.kind.isContainer else {
			throw NSFileProviderError(.noSuchItem)
		}
		let folder = try writableFolder(parent.folderID)
		let name = FileNames.normalized(template.filename)
		guard FileNames.isValid(name), !FileNames.isReserved(name) else {
			throw NSFileProviderError(.excludedFromSync)
		}
		let contentType = template.contentType ?? nil
		if contentType == .symbolicLink {
			throw NSFileProviderError(.excludedFromSync)
		}
		let kind: ItemKind = contentType?.conforms(to: .directory) == true ? .directory : .file

		if let existing = try store.child(of: parent.id, named: name) {
			if options.contains(.mayAlreadyExist), existing.kind.isContainer == (kind == .directory) {
				return existing
			}
			throw NSFileProviderError(.filenameCollision)
		}

		let path = FileNames.join(parent.path, name)
		let destination = url(folderID: folder.folderID, path: path)
		try materialize(parent)
		let modificationDate = fields.contains(.contentModificationDate) ? (template.contentModificationDate ?? nil) : nil

		let fingerprint: FileFingerprint
		if kind == .directory {
			if mkdir(destination.fileSystemPath, 0o777) != 0 {
				guard errno == EEXIST, FileFingerprint(path: destination.fileSystemPath)?.isRegularFile == false else {
					throw errno == EEXIST ? NSFileProviderError(.filenameCollision) : Self.posixError()
				}
			}
			if let modificationDate {
				try setModificationDate(destination, modificationDate)
			}
			fingerprint = try Self.fingerprint(destination)
		}
		else {
			let staged = try stage(contents, modificationDate: modificationDate)
			fingerprint = try Self.fingerprint(staged)
			try place(staged, at: destination)
		}

		let item = try store.recordLocalChanges { recorder throws(IndexError) in
			let item = try recorder.recordCreation(
				folderID: folder.folderID, parentID: parent.id, path: path, kind: kind, size: fingerprint.size,
				modifiedNanoseconds: fingerprint.modifiedNanoseconds, isSelected: true)
			if folder.isSelective {
				try recorder.enqueue(.select, folderID: folder.folderID, path: path)
			}
			try recorder.enqueue(.rescan, folderID: folder.folderID, path: path)
			return item
		}
		SharedNotification.workQueued.post()
		return item
	}

	// MARK: - Modifying

	func modify(
		template: NSFileProviderItem, baseVersion: NSFileProviderItemVersion, changedFields: NSFileProviderItemFields,
		contents: URL?, options: NSFileProviderModifyItemOptions
	) throws -> Outcome {
		guard let id = template.itemIdentifier.itemID, var item = try store.item(id: id) else {
			throw NSFileProviderError(.noSuchItem)
		}
		var outcome = Outcome(item: item)
		// A directory's modification time changes with its contents; Syncthing does not need to hear about it
		let writesSomething =
			changedFields.contains(.filename) || changedFields.contains(.parentItemIdentifier)
			|| (item.kind == .file && (changedFields.contains(.contents) || changedFields.contains(.contentModificationDate)))
		guard writesSomething else {
			return outcome
		}
		let folder = try writableFolder(item.folderID)

		if changedFields.contains(.filename) || changedFields.contains(.parentItemIdentifier) {
			item = try move(item, in: folder, to: template.parentItemIdentifier, name: template.filename)
			outcome.item = item
		}

		if changedFields.contains(.contents), let contents {
			let date = changedFields.contains(.contentModificationDate) ? (template.contentModificationDate ?? nil) : nil
			outcome = try write(
				contents, to: item, in: folder, modificationDate: date ?? Date(), baseVersion: baseVersion,
				options: options)
		}
		else if changedFields.contains(.contentModificationDate), let date = template.contentModificationDate ?? nil,
			item.kind == .file
		{
			outcome.item = try touch(item, in: folder, modificationDate: date)
		}
		return outcome
	}

	private func move(
		_ item: IndexItem, in folder: IndexFolder, to parentIdentifier: NSFileProviderItemIdentifier, name: String
	) throws -> IndexItem {
		guard item.kind != .folderRoot else {
			throw Rejection.cannotSynchronize(String(localized: "Folders shared with Syncthing cannot be renamed here."))
		}
		guard let parentID = parentIdentifier.itemID, let parent = try store.item(id: parentID),
			parent.kind.isContainer
		else {
			throw Rejection.cannotSynchronize(String(localized: "Items can only be moved within their folder."))
		}
		guard parent.folderID == item.folderID else {
			throw Rejection.cannotSynchronize(String(localized: "Items can only be moved within their folder."))
		}
		let name = FileNames.normalized(name)
		guard FileNames.isValid(name), !FileNames.isReserved(name) else {
			throw Rejection.cannotSynchronize(String(localized: "This name is reserved for Syncthing."))
		}
		let path = FileNames.join(parent.path, name)
		if path == item.path {
			return item
		}
		if item.kind.isContainer, FileNames.isSelfOrDescendant(parent.path, of: item.path) {
			throw Rejection.cannotSynchronize(String(localized: "A folder cannot be moved into itself."))
		}
		if let existing = try store.child(of: parent.id, named: name), existing.id != item.id {
			throw NSFileProviderError(.filenameCollision)
		}
		guard isOnDevice(item) else {
			throw Rejection.notOnDevice
		}
		if folder.isSelective && item.kind.isContainer && item.pending != .create {
			throw Rejection.cannotSynchronize(
				String(localized: "Folders in on-demand folders cannot be renamed or moved yet."))
		}

		let source = url(folderID: item.folderID, path: item.path)
		let destination = url(folderID: item.folderID, path: path)
		try materialize(parent)

		// In an on-demand folder, Syncthing ignores paths until they are selected. Renaming a file it knows would make
		// it look deleted, so the new name is linked and the app finishes the rename after selecting it.
		let isKnownToSyncthing = item.pending != .create && !(folder.isSelective && item.pending == .rename)
		let linksNewName = folder.isSelective && isKnownToSyncthing
		let unfinishedRename =
			folder.isSelective && item.pending == .rename
			? try store.queuedWork(.finishRename, folderID: item.folderID, extra: item.path) : nil
		if let unfinishedRename, unfinishedRename.path == path {
			// Renamed back before the app finished the rename: the original name is still there
			guard unlink(source.fileSystemPath) == 0 || errno == ENOENT else {
				throw Self.posixError()
			}
			let moved = try store.recordLocalChanges { recorder throws(IndexError) in
				try recorder.dequeue(.finishRename, folderID: item.folderID, path: path)
				try recorder.enqueue(.rescan, folderID: item.folderID, path: path)
				return try recorder.recordMove(item, to: parent.id, path: path)
			}
			SharedNotification.workQueued.post()
			return moved
		}
		if linksNewName {
			guard link(source.fileSystemPath, destination.fileSystemPath) == 0 else {
				throw errno == EEXIST ? NSFileProviderError(.filenameCollision) : Self.posixError()
			}
		}
		else {
			try Self.rename(source, destination, flags: RENAME_EXCL)
		}

		let moved = try store.recordLocalChanges { recorder throws(IndexError) in
			let moved = try recorder.recordMove(item, to: parent.id, path: path)
			if linksNewName {
				try recorder.enqueue(.finishRename, folderID: item.folderID, path: item.path, extra: path)
			}
			else if folder.isSelective && item.pending == .rename {
				try recorder.retargetQueuedWork(.finishRename, folderID: item.folderID, from: item.path, to: path)
			}
			else if folder.isSelective {
				// Selecting a new directory selects everything in it
				try recorder.dequeue(.select, folderID: item.folderID, path: item.path, includingDescendants: true)
				try recorder.enqueue(.select, folderID: item.folderID, path: path)
			}
			if !linksNewName {
				try recorder.enqueue(.rescan, folderID: item.folderID, path: item.path)
				try recorder.enqueue(.rescan, folderID: item.folderID, path: path)
			}
			return moved
		}
		SharedNotification.workQueued.post()
		return moved
	}

	/// Writes new contents for a file. When the file changed on another device since the version the edit is based on,
	/// the edit is kept as a conflict copy next to it, the way Syncthing does it.
	private func write(
		_ contents: URL, to item: IndexItem, in folder: IndexFolder, modificationDate: Date,
		baseVersion: NSFileProviderItemVersion, options: NSFileProviderModifyItemOptions
	) throws -> Outcome {
		guard item.kind == .file else {
			throw Rejection.cannotSynchronize(String(localized: "Only files can be written."))
		}
		let destination = url(folderID: item.folderID, path: item.path)
		let staged = try stage(contents, modificationDate: modificationDate)
		let written = try Self.fingerprint(staged)

		let isBasedOnCurrentVersion = Self.revision(baseVersion.contentVersion) == item.contentRevision
		let onDisk = FileFingerprint(path: destination.fileSystemPath)
		let diskHasIndexedVersion =
			onDisk?.matches(size: item.size, modifiedNanoseconds: item.modifiedNanoseconds) == true

		if isBasedOnCurrentVersion && (onDisk == nil || diskHasIndexedVersion) {
			do {
				if onDisk == nil {
					// A file that was only on other devices: it is kept on this device from now on
					try materialize(parentOf: item)
					try place(staged, at: destination)
				}
				else {
					try replace(destination, with: staged, expecting: item)
				}
				let updated = try store.recordLocalChanges { recorder throws(IndexError) in
					let updated = try recorder.recordContentChange(
						item, size: written.size, modifiedNanoseconds: written.modifiedNanoseconds)
					if folder.isSelective && onDisk == nil {
						try recorder.enqueue(.select, folderID: item.folderID, path: item.path)
					}
					try recorder.enqueue(.rescan, folderID: item.folderID, path: item.path)
					return updated
				}
				SharedNotification.workQueued.post()
				return Outcome(item: updated)
			}
			catch ReplaceError.changedMeanwhile {
				// Fall through to the conflict below
			}
		}

		if #available(iOS 26.0, macOS 26.0, *), options.contains(.failOnConflict) {
			unlink(staged.fileSystemPath)
			throw NSFileProviderError(.localVersionConflictingWithServer)
		}
		let copy = try addConflictCopy(staged, of: item, in: folder)
		log.info("Kept an edit of \(item.id) as conflict copy \(copy.id)")
		return Outcome(item: item, shouldFetchContent: true, addedItems: true)
	}

	private func addConflictCopy(_ staged: URL, of item: IndexItem, in folder: IndexFolder) throws -> IndexItem {
		guard let parentID = item.parentID else {
			throw NSFileProviderError(.noSuchItem)
		}
		let shortID = ((try? store.localShortDeviceID()) ?? nil) ?? "LOCAL"
		let parentPath = FileNames.parentPath(item.path)
		var date = Date()
		var path = ""
		for attempt in 0... {
			path = FileNames.join(
				parentPath, FileNames.conflictCopyName(for: item.name, date: date, shortDeviceID: shortID))
			do {
				try Self.rename(staged, url(folderID: item.folderID, path: path), flags: RENAME_EXCL)
				break
			}
			catch let error as NSFileProviderError where error.code == .filenameCollision && attempt < 5 {
				// Another conflict copy from the same second
				date.addTimeInterval(1)
			}
			catch {
				unlink(staged.fileSystemPath)
				throw error
			}
		}

		let fingerprint = try Self.fingerprint(url(folderID: item.folderID, path: path))
		let copy = try store.recordLocalChanges { [path] recorder throws(IndexError) in
			let copy = try recorder.recordCreation(
				folderID: item.folderID, parentID: parentID, path: path, kind: .file, size: fingerprint.size,
				modifiedNanoseconds: fingerprint.modifiedNanoseconds, isSelected: true)
			if folder.isSelective {
				try recorder.enqueue(.select, folderID: item.folderID, path: path)
			}
			try recorder.enqueue(.rescan, folderID: item.folderID, path: path)
			return copy
		}
		SharedNotification.workQueued.post()
		return copy
	}

	private func touch(_ item: IndexItem, in folder: IndexFolder, modificationDate: Date) throws -> IndexItem {
		guard isOnDevice(item) else {
			return item
		}
		let target = url(folderID: item.folderID, path: item.path)
		try setModificationDate(target, modificationDate)
		let fingerprint = try Self.fingerprint(target)
		let updated = try store.recordLocalChanges { recorder throws(IndexError) in
			let updated = try recorder.recordModificationDate(
				item, modifiedNanoseconds: fingerprint.modifiedNanoseconds)
			try recorder.enqueue(.rescan, folderID: item.folderID, path: item.path)
			return updated
		}
		SharedNotification.workQueued.post()
		return updated
	}

	// MARK: - Deleting

	func delete(
		identifier: NSFileProviderItemIdentifier, baseVersion: NSFileProviderItemVersion,
		options: NSFileProviderDeleteItemOptions
	) throws {
		guard let id = identifier.itemID, let item = try store.item(id: id) else {
			// Already gone
			return
		}
		guard item.kind != .folderRoot, let folder = try store.folder(id: item.folderID), folder.acceptsLocalChanges
		else {
			throw NSFileProviderError(.deletionRejected)
		}

		let target = url(folderID: item.folderID, path: item.path)
		if item.kind == .file {
			// Deleting removes the file on all devices, so only the version the user saw may be deleted
			guard Self.revision(baseVersion.contentVersion) == item.contentRevision, isOnDevice(item) else {
				throw NSFileProviderError(.deletionRejected)
			}
			guard unlink(target.fileSystemPath) == 0 || errno == ENOENT else {
				throw Self.posixError()
			}
		}
		else {
			let descendants = try store.descendants(of: item)
			if !descendants.isEmpty && !options.contains(.recursive) {
				throw NSFileProviderError(.directoryNotEmpty)
			}
			// Files that are only on other devices cannot be deleted from here
			if descendants.contains(where: { !$0.isSelected }) {
				throw NSFileProviderError(.directoryNotEmpty)
			}
			do {
				try FileManager.default.removeItem(at: target)
			}
			catch CocoaError.fileNoSuchFile {
				// Already gone
			}
		}

		try store.recordLocalChanges { recorder throws(IndexError) in
			try recorder.recordDeletion(item)
			if folder.isSelective {
				try recorder.dequeue(.select, folderID: item.folderID, path: item.path, includingDescendants: true)
			}
			try recorder.enqueue(.rescan, folderID: item.folderID, path: item.path)
		}
		SharedNotification.workQueued.post()
	}

	// MARK: - Disk

	private func writableFolder(_ folderID: String) throws -> IndexFolder {
		guard let folder = try store.folder(id: folderID) else {
			throw NSFileProviderError(.noSuchItem)
		}
		guard folder.acceptsLocalChanges else {
			throw Rejection.readOnlyFolder
		}
		return folder
	}

	private func url(folderID: String, path: String) -> URL {
		context.container.fileURL(folderID: folderID, path: path)
	}

	private func isOnDevice(_ item: IndexItem) -> Bool {
		context.isOnDevice(item)
	}

	/// Creates the directory on disk (and the ones above it) if it is not there, as Syncthing would when pulling it
	private func materialize(_ directory: IndexItem) throws {
		let target = url(folderID: directory.folderID, path: directory.path)
		if FileFingerprint(path: target.fileSystemPath) != nil {
			return
		}
		if directory.kind != .folderRoot, let parentID = directory.parentID, let parent = try store.item(id: parentID) {
			try materialize(parent)
		}
		guard mkdir(target.fileSystemPath, 0o777) == 0 || errno == EEXIST else {
			throw Self.posixError()
		}
		if let date = directory.modificationDate {
			try? setModificationDate(target, date)
		}
	}

	private func materialize(parentOf item: IndexItem) throws {
		if let parentID = item.parentID, let parent = try store.item(id: parentID) {
			try materialize(parent)
		}
	}

	/// Copies the contents into the staging directory (a clone where possible) with the given modification time
	private func stage(_ contents: URL?, modificationDate: Date?) throws -> URL {
		let staged = context.container.stagingURL.appending(path: UUID().uuidString, directoryHint: .notDirectory)
		if let contents {
			if clonefile(contents.fileSystemPath, staged.fileSystemPath, UInt32(CLONE_NOFOLLOW)) != 0 {
				guard copyfile(contents.fileSystemPath, staged.fileSystemPath, nil, copyfile_flags_t(COPYFILE_DATA)) == 0
				else {
					throw Self.posixError()
				}
			}
			Self.removeFileProviderAttributes(staged)
		}
		else if !FileManager.default.createFile(atPath: staged.fileSystemPath, contents: nil) {
			throw CocoaError(.fileWriteUnknown)
		}
		try? FileManager.default.setAttributes(
			[.protectionKey: FileProtectionType.completeUntilFirstUserAuthentication],
			ofItemAtPath: staged.fileSystemPath)
		if let modificationDate {
			try setModificationDate(staged, modificationDate)
		}
		return staged
	}

	/// Moves a staged file to a path where nothing exists yet
	private func place(_ staged: URL, at destination: URL) throws {
		do {
			try Self.rename(staged, destination, flags: RENAME_EXCL)
		}
		catch {
			unlink(staged.fileSystemPath)
			throw error
		}
	}

	private enum ReplaceError: Error {
		/// The file on disk is not the version that was checked (Syncthing replaced it in the meantime)
		case changedMeanwhile
	}

	/// Atomically puts the staged file in place of the existing file, if that is still the indexed version
	private func replace(_ destination: URL, with staged: URL, expecting item: IndexItem) throws {
		try Self.rename(staged, destination, flags: RENAME_SWAP)
		// The staged path now holds what was in place; make sure it was the version the edit was based on
		let replaced = FileFingerprint(path: staged.fileSystemPath)
		if replaced?.matches(size: item.size, modifiedNanoseconds: item.modifiedNanoseconds) != true {
			try Self.rename(staged, destination, flags: RENAME_SWAP)
			throw ReplaceError.changedMeanwhile
		}
		unlink(staged.fileSystemPath)
	}

	private func setModificationDate(_ url: URL, _ date: Date) throws {
		let nanoseconds = Int64((date.timeIntervalSince1970 * 1_000_000_000).rounded())
		var times = [
			timespec(tv_sec: 0, tv_nsec: Int(UTIME_OMIT)),
			timespec(tv_sec: Int(nanoseconds / 1_000_000_000), tv_nsec: Int(nanoseconds % 1_000_000_000)),
		]
		guard utimensat(AT_FDCWD, url.fileSystemPath, &times, AT_SYMLINK_NOFOLLOW) == 0 else {
			throw Self.posixError()
		}
	}

	/// Extended attributes the system uses to track files in its replica must not travel into the folder
	private static func removeFileProviderAttributes(_ url: URL) {
		let path = url.fileSystemPath
		let size = listxattr(path, nil, 0, XATTR_NOFOLLOW)
		guard size > 0 else {
			return
		}
		var buffer = [CChar](repeating: 0, count: size)
		guard listxattr(path, &buffer, size, XATTR_NOFOLLOW) == size else {
			return
		}
		for name in buffer.split(separator: 0).map({ String(decoding: $0.map { UInt8(bitPattern: $0) }, as: UTF8.self) })
		where name.hasPrefix("com.apple.fileprovider") {
			removexattr(path, name, XATTR_NOFOLLOW)
		}
	}

	private static func rename(_ source: URL, _ destination: URL, flags: Int32) throws {
		guard
			renamex_np(source.fileSystemPath, destination.fileSystemPath, UInt32(flags)) == 0
		else {
			throw errno == EEXIST ? NSFileProviderError(.filenameCollision) : posixError()
		}
	}

	private static func fingerprint(_ url: URL) throws -> FileFingerprint {
		guard let fingerprint = FileFingerprint(path: url.fileSystemPath) else {
			throw posixError()
		}
		return fingerprint
	}

	/// The revision encoded in a version from `FileProviderItem.itemVersion`
	static func revision(_ data: Data) -> Int64? {
		guard data.count == 8 else {
			return nil
		}
		return data.reduce(Int64(0)) { ($0 << 8) | Int64($1) }
	}

	/// The current POSIX error as an error the framework accepts (it only takes Cocoa and File Provider errors)
	static func posixError(_ code: Int32 = errno) -> Error {
		switch code {
		case ENOSPC, EDQUOT:
			return CocoaError(.fileWriteOutOfSpace)
		case EACCES, EPERM:
			return CocoaError(.fileWriteNoPermission)
		case ENOENT:
			return CocoaError(.fileNoSuchFile)
		default:
			return NSError(
				domain: NSCocoaErrorDomain, code: NSXPCConnectionReplyInvalid,
				userInfo: [NSUnderlyingErrorKey: POSIXError(POSIXErrorCode(rawValue: code) ?? .EIO)])
		}
	}
}

extension URL {
	/// For passing to POSIX functions
	fileprivate var fileSystemPath: String { path(percentEncoded: false) }
}
