// Copyright (C) 2026 Gordon
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this file,
// You can obtain one at https://mozilla.org/MPL/2.0/.
#if os(macOS)
	import Foundation

	/// The remote file service's state, as `SushitrainRemoteFileService.statusJSON` returns it
	struct GatewayStatus: Decodable, Sendable {
		let serviceID: String
		let serviceName: String
		/// The address the service listens on
		let address: String
		/// Base64url SHA-256 of the certificate's public key
		let spki: String?
		let clients: Int
		let pendingOperations: Int
		let pendingPairings: Int
		let contentCacheBytes: Int64
		let contentCacheQuota: Int64
	}

	/// A folder made available to paired devices
	struct GatewayExport: Decodable, Sendable {
		let folderID: String
		let allowWrite: Bool
	}

	/// What a paired device may do in a folder
	enum GatewayAccess: String, CaseIterable, Sendable {
		case none = ""
		case read = "read"
		case write = "write"

		/// The access a folder's export allows at most
		init(export: GatewayExport?) {
			guard let export else {
				self = .none
				return
			}
			self = export.allowWrite ? .write : .read
		}

		var label: String {
			switch self {
			case .none: return String(localized: "No access")
			case .read: return String(localized: "Read only")
			case .write: return String(localized: "Read and write")
			}
		}
	}

	/// A paired device
	struct GatewayClient: Decodable, Sendable, Identifiable {
		let clientID: String
		let name: String
		let platform: String
		let created: Date
		let lastSeen: Date
		let revoked: Bool
		let permissions: [String: String]?

		var id: String { clientID }

		/// When the device last connected (nil if it never did)
		var lastSeenDate: Date? {
			lastSeen.timeIntervalSince1970 > 0 ? lastSeen : nil
		}

		var displayName: String {
			name.isEmpty ? String(localized: "Unnamed device") : name
		}

		func access(to folderID: String) -> GatewayAccess {
			GatewayAccess(rawValue: permissions?[folderID] ?? "") ?? .none
		}
	}

	/// A pairing code that is valid once, until it expires
	struct GatewayPairingSession: Decodable, Sendable {
		let sessionID: String
		let url: String
		let expires: Date
	}

	/// A device that scanned a pairing code and waits for approval
	struct GatewayPendingPairing: Decodable, Sendable, Identifiable, Equatable {
		let requestID: String
		let sessionID: String
		let clientID: String
		let clientName: String
		let platform: String
		let requested: Date

		var id: String { requestID }

		var displayName: String {
			clientName.isEmpty ? String(localized: "Unnamed device") : clientName
		}
	}

	/// A change from a paired device that is being saved
	struct GatewayPendingOperation: Decodable, Sendable, Identifiable {
		let operation: String
		let clientID: String
		let kind: String
		let folderID: String
		let created: Date

		var id: String { operation }

		var kindLabel: String {
			switch kind {
			case "create": return String(localized: "New item")
			case "replace": return String(localized: "Changed file")
			case "move": return String(localized: "Move or rename")
			case "delete": return String(localized: "Deletion")
			default: return kind
			}
		}
	}

	/// A folder on this Mac that can be exported
	struct GatewayFolder: Identifiable, Sendable {
		let id: String
		let name: String
		/// Why paired devices can't be allowed to change the folder (nil when they can)
		let writeRestriction: String?
	}

	extension JSONDecoder {
		/// Decodes JSON from the remote file service, whose dates are RFC 3339 with optional fractional seconds
		static func gateway() -> JSONDecoder {
			let decoder = JSONDecoder()
			decoder.dateDecodingStrategy = .custom { decoder in
				let container = try decoder.singleValueContainer()
				let text = try container.decode(String.self)
				guard let date = parseGatewayDate(text) else {
					throw DecodingError.dataCorruptedError(in: container, debugDescription: "Invalid date: \(text)")
				}
				return date
			}
			return decoder
		}

		/// Decodes an array that the service writes as `null` when it is empty
		func decodeList<T: Decodable>(_ type: T.Type, from data: Data) throws -> [T] {
			try decode([T]?.self, from: data) ?? []
		}
	}

	/// Parses an RFC 3339 date (Go's time.Time in JSON), with or without fractional seconds
	private func parseGatewayDate(_ text: String) -> Date? {
		var text = text
		var fraction = 0.0
		if let dot = text.firstIndex(of: ".") {
			let digitsStart = text.index(after: dot)
			let digitsEnd = text[digitsStart...].firstIndex(where: { !$0.isNumber }) ?? text.endIndex
			fraction = Double("0." + text[digitsStart..<digitsEnd]) ?? 0
			text.removeSubrange(dot..<digitsEnd)
		}
		let formatter = ISO8601DateFormatter()
		formatter.formatOptions = [.withInternetDateTime]
		return formatter.date(from: text)?.addingTimeInterval(fraction)
	}
#endif
