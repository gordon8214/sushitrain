// Copyright (C) 2026 Gordon
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this file,
// You can obtain one at https://mozilla.org/MPL/2.0/.
#if os(macOS)
	import Foundation
	import SynctrainHostKit
	import SynctrainFileProviderKit

	enum HistoryAvailability {
		static var folderIDs: Set<String> {
			guard let store = RemoteEnvironment.shared?.store else { return [] }
			return (try? store.historyFolderIDs()) ?? []
		}
		static func applying(to status: FinderItemStatus, folderIDs: Set<String> = Self.folderIDs) -> FinderItemStatus {
			status.includingRemoteHistory(folderIDs: folderIDs)
		}
	}
#endif
