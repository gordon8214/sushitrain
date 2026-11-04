// Copyright (C) 2026 Gordon
// SPDX-License-Identifier: MPL-2.0
#if os(macOS)
	import Foundation
	import FileProvider
	import SynctrainFileProviderKit
	import SynctrainHostKit
	@preconcurrency import SushitrainCore
	extension MCPIntegration {
		nonisolated static func search(store: IndexStore, parentID: Int64, query: String) async throws -> Data {
			let search = Task.detached { () throws -> Data in
				let parent = parentID == 0 ? nil : try store.item(id: parentID)
				if parentID != 0 && (parent == nil || parent?.isDeleted == true || parent?.kind.isContainer != true) {
					throw MCPAppError("The search parent is unavailable; choose an existing folder.")
				}
				var found: [[String: Any]] = []
				var after: Int64 = 0
				var visited = 0
				while true {
					try Task.checkCancellation()
					let items = try store.liveItems(after: after, limit: 500)
					if items.isEmpty { break }
					visited += items.count
					guard visited <= 100_000 else { throw MCPAppError("Search exceeds 100000 indexed items; narrow the search.") }
					for item in items {
						if let parent,
							item.folderID != parent.folderID || !(item.path.hasPrefix(parent.path + "/") || parent.path.isEmpty) {
							continue
						}
						if item.name.localizedCaseInsensitiveContains(query) { found.append(Self.item(item)) }
					}
					after = items.last!.id
				}
				return try Self.json(found)
			}
			return try await withTaskCancellationHandler {
				try await search.value
			} onCancel: {
				search.cancel()
			}

		}
	}
#endif
