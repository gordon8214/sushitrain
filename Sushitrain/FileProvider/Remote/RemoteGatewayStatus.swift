// Copyright (C) 2026 Gordon
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this file,
// You can obtain one at https://mozilla.org/MPL/2.0/.
#if os(iOS)
	import Foundation
	import SynctrainFileProviderKit

	/// What the app knows about a paired gateway: whether it can be reached and accepts this device, and what it serves
	struct RemoteGatewayStatus: Identifiable, Hashable, Sendable {
		var endpoint: RemoteEndpoint
		var connection: Connection = .unknown
		var capabilities: Capabilities?
		/// The folders this device may access, as of the last check
		var folders: [RemoteFolder]?
		/// Problems with single folders accessed through the gateway, by local folder ID
		var folderProblems: [String: String] = [:]
		var lastChecked: Date?
		/// When the gateway was seen to have started a new authority generation (it may have lost state); operations
		/// it could not confirm are then listed for review
		var authorityChangedAt: Date?

		var id: String { endpoint.serviceID }

		enum Connection: Hashable, Sendable {
			case unknown
			case reachable
			case unreachable(String)
			/// The certificate is not the pinned one, or not trusted normally
			case untrusted
			/// The gateway does not accept this device's credential (revoked, or never stored)
			case unauthorized
			/// The gateway speaks another version of the API
			case incompatible(apiVersion: Int)
			case failed(String)
		}

		/// The API version this app speaks
		static let apiVersion = 1

		var isReachable: Bool? {
			switch connection {
			case .unknown: return nil
			case .unreachable, .untrusted: return false
			default: return true
			}
		}

		var isAuthorized: Bool? {
			switch connection {
			case .reachable, .incompatible: return true
			case .unauthorized: return false
			default: return nil
			}
		}

		var isCompatible: Bool? {
			if case .incompatible = connection {
				return false
			}
			return capabilities.map { $0.apiVersion == Self.apiVersion }
		}

		/// Whether requests can be made now (reachable, authorized and compatible)
		var isUsable: Bool {
			connection == .reachable && isCompatible == true
		}

		var displayName: String { endpoint.displayName }

		var address: String {
			endpoint.host.contains(":") ? "[\(endpoint.host)]:\(endpoint.port)" : "\(endpoint.host):\(endpoint.port)"
		}

		/// A short description of the connection, for lists
		var summary: String {
			switch connection {
			case .unknown: return String(localized: "Not checked yet")
			case .reachable:
				if isCompatible == false {
					return String(localized: "Needs a newer version of Synctrain")
				}
				return String(localized: "Connected")
			case .unreachable: return String(localized: "Not reachable")
			case .untrusted: return String(localized: "Certificate not trusted")
			case .unauthorized: return String(localized: "This device is not authorized")
			case .incompatible: return String(localized: "Incompatible version")
			case .failed: return String(localized: "Error")
			}
		}

		/// A full description of a problem with the connection, if there is one
		var problem: String? {
			switch connection {
			case .unknown, .reachable:
				return nil
			case .unreachable(let reason):
				return String(
					localized:
						"\(displayName) cannot be reached (\(reason)). Make sure it is running and that this device is on the same network or VPN. Edits wait on this device until it is back."
				)
			case .untrusted:
				return String(
					localized:
						"The certificate of \(displayName) is not the one it was paired with. If its certificate was replaced, pair with it again."
				)
			case .unauthorized:
				return String(
					localized:
						"\(displayName) no longer accepts this device. Pair with it again; edits that were not sent stay on this device."
				)
			case .incompatible(let version):
				return String(
					localized:
						"\(displayName) uses version \(version) of the remote file API, which this version of Synctrain does not support."
				)
			case .failed(let message):
				return message
			}
		}

		static func connection(for error: RemoteError) -> Connection {
			switch error {
			case .unreachable(.serverCertificateUntrusted): return .untrusted
			case .unreachable(let code): return .unreachable(URLError(code).localizedDescription)
			case .unauthenticated: return .unauthorized
			default: return .failed(error.userDescription)
			}
		}
	}

	extension RemoteError {
		/// A description for the user
		var userDescription: String {
			switch self {
			case .unauthenticated:
				return String(localized: "The gateway does not accept this device. Pair with it again.")
			case .forbidden(let code):
				return String(localized: "The gateway does not allow this (\(code)).")
			case .notFound:
				return String(localized: "The gateway does not know this item.")
			case .nameConflict:
				return String(localized: "An item with this name exists already.")
			case .revisionMismatch:
				return String(localized: "The item was changed on another device.")
			case .operationMismatch:
				return String(localized: "The gateway received a different change with the same identifier.")
			case .cursorExpired, .snapshotExpired, .leaseExpired:
				return String(localized: "The gateway's listing expired. Try again.")
			case .unavailable:
				return String(localized: "The content is temporarily unavailable on the gateway.")
			case .overloaded:
				return String(localized: "The gateway is busy. Try again later.")
			case .insufficientStorage:
				return String(localized: "The gateway is out of storage.")
			case .unreachable(.serverCertificateUntrusted):
				return String(localized: "The gateway's certificate is not trusted.")
			case .unreachable(let code):
				return URLError(code).localizedDescription
			case .protocolViolation(let message):
				return String(localized: "The gateway answered unexpectedly: \(message)")
			case .server(let status, let code):
				return String(localized: "The gateway reported an error (\(status) \(code ?? "")).")
			case .store(let error):
				return error.userDescription
			case .fileSystem(let message):
				return message
			case .canceled:
				return String(localized: "Canceled.")
			}
		}
	}

	extension IndexError {
		/// A description for the user
		var userDescription: String {
			switch self {
			case .notAllowed(let reason):
				return reason.prefix(1).uppercased() + reason.dropFirst() + "."
			case .unavailable:
				return String(localized: "The Files index is not available until this device has been unlocked.")
			case .unsupportedSchema:
				return String(localized: "The Files index was created by a newer version of Synctrain.")
			case .notFound:
				return String(localized: "The item does not exist anymore.")
			case .nameCollision:
				return String(localized: "An item with this name exists already.")
			case .anchorExpired, .sqlite:
				return String(localized: "The Files index could not be read or written.")
			}
		}
	}
#endif
