// Copyright (C) 2026 Gordon
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this file,
// You can obtain one at https://mozilla.org/MPL/2.0/.
@preconcurrency import FileProvider
import Foundation
import SynctrainFileProviderKit

/// A change that is recorded in the outbox (its contents staged) but that the gateway has not committed yet. The system
/// is told so and tries again later; that attempt continues the same operation.
struct RemoteChangeWaiting: Error {
	let record: MutationRecord
	/// What stopped the last attempt (nil while the gateway is still applying the change)
	let reason: RemoteError?
	/// The gateway's version of the remote file API, when it is one this version of Synctrain does not speak (nothing
	/// is sent to it then)
	var incompatibleVersion: Int? = nil
	/// Whether a background transfer sends the change's contents meanwhile
	var uploadingInBackground = false
}

/// The errors of folders accessed through a gateway, as the File Provider framework takes them (it only accepts Cocoa
/// and File Provider errors), with explanations for the user
enum RemoteFailure {
	/// The name to show for a gateway
	static func gatewayName(_ endpoint: RemoteEndpoint?) -> String {
		guard let endpoint else {
			return String(localized: "the computer that shares this folder")
		}
		return endpoint.displayName.isEmpty ? endpoint.host : endpoint.displayName
	}

	private static func error(
		_ domain: String, _ code: Int, _ description: String, underlying: (any Error)? = nil
	) -> NSError {
		var userInfo: [String: Any] = [NSLocalizedDescriptionKey: description]
		if let underlying {
			userInfo[NSUnderlyingErrorKey] = underlying as NSError
		}
		return NSError(domain: domain, code: code, userInfo: userInfo)
	}

	/// An error the system tries again after on its own (it treats every error it does not know as transient)
	static func transient(_ description: String, underlying: (any Error)? = nil) -> Error {
		error(NSCocoaErrorDomain, NSXPCConnectionReplyInvalid, description, underlying: underlying)
	}

	static func notAuthenticated(gateway: String) -> Error {
		error(
			NSFileProviderErrorDomain, NSFileProviderError.notAuthenticated.rawValue,
			String(localized: "\(gateway) no longer accepts this device. Pair with it again in Synctrain."))
	}

	// MARK: - Changes

	/// A change that cannot be made. The system does not retry these (it retries any other error).
	static func cannotSynchronize(_ reason: String) -> Error {
		NSError(
			domain: NSFileProviderErrorDomain, code: NSFileProviderError.cannotSynchronize.rawValue,
			userInfo: [NSLocalizedDescriptionKey: reason])
	}

	static func readOnly(gateway: String) -> Error {
		cannotSynchronize(String(localized: "\(gateway) only allows reading this folder."))
	}

	static func needsReview(gateway: String) -> Error {
		cannotSynchronize(
			String(localized: "An earlier change of this item was not accepted by \(gateway). Open Synctrain to review it."))
	}

	static func notAccepted(gateway: String) -> Error {
		cannotSynchronize(String(localized: "\(gateway) did not accept this change."))
	}

	static var crossHostDirectory: Error {
		cannotSynchronize(
			String(
				localized:
					"Folders cannot be moved to a folder that another computer shares. Copy the folder there, then delete it here."
			))
	}

	/// For a change that is recorded but not committed. `cannotSynchronize` keeps browsing available and makes the
	/// system hold the change and back off until the error is signaled resolved, which the extension does once it
	/// reaches the gateway again (`RemoteProvider.reached(serviceID:)`); `serverUnreachable` would pause the whole
	/// location, including the folders this device synchronizes itself.
	static func waiting(_ waiting: RemoteChangeWaiting, gateway: String) -> Error {
		if let version = waiting.incompatibleVersion {
			return error(
				NSFileProviderErrorDomain, NSFileProviderError.cannotSynchronize.rawValue,
				String(
					localized:
						"\(gateway) uses version \(version) of the remote file API, which this version of Synctrain does not support."
				))
		}
		if waiting.reason == .unauthenticated {
			return notAuthenticated(gateway: gateway)
		}
		if case .unreachable(.serverCertificateUntrusted) = waiting.reason {
			return error(
				NSFileProviderErrorDomain, NSFileProviderError.cannotSynchronize.rawValue,
				String(localized: "\(gateway) presented a certificate Synctrain does not trust. Pair with it again."))
		}
		let description =
			if waiting.record.state == .pending {
				String(localized: "\(gateway) is still saving this change. Synctrain checks again later.")
			}
			else if waiting.uploadingInBackground {
				String(localized: "Synctrain is sending this change to \(gateway) in the background.")
			}
			else {
				String(localized: "Synctrain sends this change to \(gateway) once it can be reached.")
			}
		return error(NSFileProviderErrorDomain, NSFileProviderError.cannotSynchronize.rawValue, description)
	}

	/// For errors recording a change in the outbox. The reasons are those of `RemoteMutationRecorder`.
	static func recording(_ failure: IndexError, gateway: String) -> Error {
		switch failure {
		case .notAllowed(let reason):
			if reason.contains("still being sent") {
				// Earlier changes have to be confirmed first; the system tries again
				return transient(String(localized: "Synctrain is still sending earlier changes. It tries again shortly."))
			}
			if reason.contains("read-only") {
				return readOnly(gateway: gateway)
			}
			if reason.contains("review") {
				return needsReview(gateway: gateway)
			}
			return error(
				NSFileProviderErrorDomain, NSFileProviderError.cannotSynchronize.rawValue,
				String(localized: "This change cannot be made in this folder."), underlying: failure)
		default:
			return failure.fileProviderError
		}
	}

	// MARK: - Contents

	static var versionChanged: Error {
		error(
			NSCocoaErrorDomain, NSFileReadUnknownError,
			String(localized: "This file just changed on another device. Try again in a moment."))
	}

	/// For a download (`fetchContents`). The gateway being unreachable is reported as `serverUnreachable` only when the
	/// user asked for the file: the system then pauses the whole location until that is signaled resolved. Requests the
	/// system makes on its own (such as for the working set) get a read error instead, which it simply tries again
	/// later; `cannotSynchronize` is not among the errors a download may report.
	static func download(_ failure: RemoteError, gateway: String, isSystemRequest: Bool) -> Error {
		switch failure {
		case .canceled:
			return CocoaError(.userCancelled)
		case .unauthenticated:
			return notAuthenticated(gateway: gateway)
		case .forbidden:
			return error(
				NSCocoaErrorDomain, NSFileReadNoPermissionError,
				String(localized: "\(gateway) does not allow reading this file."))
		case .unreachable(let code):
			let description =
				code == .serverCertificateUntrusted
				? String(localized: "\(gateway) presented a certificate Synctrain does not trust. Pair with it again.")
				: String(localized: "Synctrain cannot reach \(gateway) to download this file.")
			if isSystemRequest {
				return error(NSCocoaErrorDomain, NSFileReadUnknownError, description)
			}
			return error(NSFileProviderErrorDomain, NSFileProviderError.serverUnreachable.rawValue, description)
		case .unavailable, .overloaded:
			return error(
				NSCocoaErrorDomain, NSFileReadUnknownError,
				String(localized: "\(gateway) cannot provide this file right now. Synctrain tries again later."))
		default:
			return error(
				NSCocoaErrorDomain, NSFileReadUnknownError,
				String(localized: "Synctrain could not download this file from \(gateway)."), underlying: failure)
		}
	}
}

extension RemoteError {
	/// For logs: without the items some errors carry (their names are private)
	var summary: String {
		switch self {
		case .nameConflict: return "nameConflict"
		case .revisionMismatch: return "revisionMismatch"
		default: return "\(self)"
		}
	}
}

extension MutationRecord {
	/// The gateway's error code when the operation failed (`name_conflict`, `revision_mismatch`, `forbidden`…), or the
	/// recorded reason
	var failureCode: String? {
		if let code = result?.error?.code {
			return code
		}
		return lastError.map { String($0.prefix { $0 != ":" }) }
	}
}
