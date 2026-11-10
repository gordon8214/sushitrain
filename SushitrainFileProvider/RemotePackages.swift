// Copyright (C) 2026 Gordon
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this file,
// You can obtain one at https://mozilla.org/MPL/2.0/.
@preconcurrency import FileProvider
import Foundation
import SynctrainFileProviderKit

extension RemoteProvider {
	/// Materializes the directory behind a package document. Each regular file uses the same verified, resumable
	/// download as a standalone file. Pages keep even large packages within the extension's memory budget.
	func fetchPackage(_ item: IndexItem, binding: RemoteFolderBinding, to destination: URL, progress: Progress)
		async throws -> NSFileProviderItem
	{
		let before = try context.store.directoryContentStamp(of: item)
		progress.totalUnitCount = max(try context.store.directoryContentSize(of: item), 1)
		try FileManager.default.createDirectory(at: destination, withIntermediateDirectories: false)
		try await fetchPackageChildren(of: item, binding: binding, to: destination, progress: progress)
		guard let current = try context.store.item(id: item.id), current == item,
			try context.store.directoryContentStamp(of: current) == before
		else {
			throw RemoteFailure.versionChanged
		}
		return context.makeItemForContents(current, remote: binding)
	}

	private func fetchPackageChildren(
		of directory: IndexItem, binding: RemoteFolderBinding, to destination: URL, progress: Progress
	) async throws {
		var after: Int64 = 0
		while true {
			try Task.checkCancellation()
			let children = try context.store.children(of: directory.id, after: after, limit: 100)
			for child in children {
				try Task.checkCancellation()
				let url = destination.appending(component: child.name)
				switch child.kind {
				case .directory:
					try FileManager.default.createDirectory(at: url, withIntermediateDirectories: false)
					try await fetchPackageChildren(of: child, binding: binding, to: url, progress: progress)
				case .file:
					let childProgress = Progress(totalUnitCount: max(child.size, 1))
					do {
						_ = try await fetch(child, binding: binding, to: url, progress: childProgress)
					}
					catch RemoteError.notFound {
						// A child disappeared, not the package itself. Never tell the system to delete the package.
						refreshSoon(folderIDs: [directory.folderID])
						throw RemoteFailure.versionChanged
					}
					progress.completedUnitCount += child.size
				case .symlink, .folderRoot:
					throw CocoaError(.fileReadUnsupportedScheme)
				}
			}
			guard children.count == 100, let last = children.last else { return }
			after = last.id
		}
	}
}
