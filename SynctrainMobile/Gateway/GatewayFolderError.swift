// Copyright (C) 2026 Gordon
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this file,
// You can obtain one at https://mozilla.org/MPL/2.0/.
import Foundation
import SynctrainFileProviderKit

/// Why a folder could not be added or removed, or a gateway forgotten
enum GatewayFolderError: LocalizedError, Sendable {
	case notEligible(String)
	case unresolvedOperations
	case store(IndexError)
	case remote(RemoteError)

	var errorDescription: String? {
		switch self {
		case .notEligible(let reason):
			return reason
		case .unresolvedOperations:
			return String(
				localized:
					"Edits made in this folder have not been confirmed by the gateway. Wait until they are sent, or export or discard them, then try again."
			)
		case .store(let error):
			return error.userDescription
		case .remote(let error):
			return error.userDescription
		}
	}
}
