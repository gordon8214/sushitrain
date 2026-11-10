// Copyright (C) 2026 Gordon
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this file,
// You can obtain one at https://mozilla.org/MPL/2.0/.
#if os(macOS)
	import Foundation
	@preconcurrency import SushitrainCore
	import SynctrainFileProviderKit

	/// Tells the remote file service which directories are packages, from the types this Mac declares: Apple's and
	/// those of every installed app. Phones show those as one document, though iOS knows few of them. The core keeps
	/// each answer for a minute, so a newly installed app's types count soon after.
	///
	/// The core asks on its own threads, sometimes while it holds a folder's lock: this stays stateless, never waits
	/// for the main thread and never calls back into the core.
	final class SystemPackageTypes: NSObject, SushitrainRemotePackageTypesProtocol, Sendable {
		func isPackageExtension(_ ext: String?) -> Bool {
			guard let ext else {
				return false
			}
			return PackageTypes.isDeclaredPackage(extension: ext)
		}
	}
#endif
