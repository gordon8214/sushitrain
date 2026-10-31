// Copyright (C) 2026 Gordon
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this file,
// You can obtain one at https://mozilla.org/MPL/2.0/.
import Foundation
import SynctrainFileProviderKit

/// Why pairing did not work
enum RemotePairingError: LocalizedError, Sendable {
	case invalidLink
	case denied
	case expired
	case unavailable
	case remote(RemoteError)

	var errorDescription: String? {
		switch self {
		case .invalidLink:
			return String(
				localized: "This is not a pairing code of a remote file service. It starts with synctrain+pair://.")
		case .denied:
			return String(localized: "The gateway denied the request.")
		case .expired:
			return String(localized: "The pairing code expired. Start pairing again on the gateway for a new code.")
		case .unavailable:
			return String(localized: "Gateways are not available in this build of Synctrain.")
		case .remote(let error):
			return error.userDescription
		}
	}
}

/// Pairs this device with a gateway from the code it shows: asks for access with the code's single-use secret, waits
/// until the request is approved on the gateway, then keeps the credential in the Keychain and the gateway (with
/// its certificate pin) in the index
@Observable @MainActor final class RemotePairing {
	enum Phase: Equatable {
		case entering
		case requesting
		/// Waiting for the user to approve the request on the gateway
		case waiting(gateway: String, expires: Date)
		case paired(serviceID: String, name: String)
		case failed(String)
	}

	private(set) var phase: Phase = .entering
	@ObservationIgnored private var task: Task<Void, Never>? = nil

	/// Whether the text is a pairing link
	static func isPairingLink(_ text: String) -> Bool {
		invitation(from: text) != nil
	}

	private static func invitation(from text: String) -> PairingInvitation? {
		URL(string: text.trimmingCharacters(in: .whitespacesAndNewlines)).flatMap(PairingInvitation.init(url:))
	}

	func start(link: String) {
		guard let invitation = Self.invitation(from: link) else {
			phase = .failed(RemotePairingError.invalidLink.localizedDescription)
			return
		}
		guard let environment = RemoteEnvironment.shared else {
			phase = .failed(RemotePairingError.unavailable.localizedDescription)
			return
		}
		task?.cancel()
		phase = .requesting
		let clientName = DeviceName.current
		task = Task {
			do throws(RemotePairingError) {
				let (client, pending) = try await Self.request(
					invitation, clientName: clientName, environment: environment)
				phase = .waiting(gateway: invitation.host, expires: pending.expires)
				let endpoint = try await Self.awaitApproval(
					invitation, client: client, pending: pending, environment: environment)
				await RemoteCoordinator.shared.didPair(serviceID: endpoint.serviceID)
				let name = RemoteCoordinator.shared.status(serviceID: endpoint.serviceID)?.displayName
				phase = .paired(serviceID: endpoint.serviceID, name: name ?? endpoint.displayName)
			}
			catch .remote(.canceled) {
				return
			}
			catch {
				phase = .failed(error.localizedDescription)
			}
		}
	}

	func cancel() {
		task?.cancel()
		task = nil
	}

	func reset() {
		cancel()
		phase = .entering
	}

	#if os(macOS)
		private nonisolated static let platform = "macOS"

		/// Pairs this Mac's File Provider with this Mac's own remote file service. Nobody has to approve that on a
		/// gateway: `approve` is given the request to approve.
		static func pairThisMac(
			link: String, approve: @escaping @MainActor (String) async throws -> Void
		) async throws -> RemoteEndpoint {
			guard let invitation = invitation(from: link) else {
				throw RemotePairingError.invalidLink
			}
			guard let environment = RemoteEnvironment.shared else {
				throw RemotePairingError.unavailable
			}
			let clientName = DeviceName.current
			let (client, pending) = try await request(invitation, clientName: clientName, environment: environment)
			try await approve(pending.request)
			return try await awaitApproval(invitation, client: client, pending: pending, environment: environment)
		}
	#else
		private nonisolated static let platform = "iOS"
	#endif

	// MARK: - Requests

	/// Submits the code's secret with the stable client identity and the device's name
	nonisolated private static func request(
		_ invitation: PairingInvitation, clientName: String, environment: RemoteEnvironment
	) async throws(RemotePairingError) -> (RemoteFileClient, PairingPending) {
		let clientID: String
		do {
			clientID = try environment.store.remoteClientID()
		}
		catch {
			throw .remote(.store(error))
		}
		// Pairing requests carry no credential; this client is not shared with anything else
		let client = RemoteFileClient(endpoint: invitation.endpoint, credentials: environment.credentials)
		let pending: PairingPending
		do throws(RemoteError) {
			pending = try await client.requestPairing(
				PairingRequest(
					secret: invitation.secret, serviceID: invitation.serviceID, clientID: clientID,
					clientName: clientName, platform: Self.platform))
		}
		catch .forbidden(code: RemoteErrorCode.pairingDenied) {
			throw .denied
		}
		catch .server(status: _, code: .some(RemoteErrorCode.pairingExpired)) {
			throw .expired
		}
		catch {
			throw .remote(error)
		}
		guard pending.serviceID == invitation.serviceID else {
			throw .remote(.protocolViolation("the gateway is another service (\(pending.serviceID))"))
		}
		return (client, pending)
	}

	/// Polls the request until it is approved, denied or expired. Once approved, the credential is stored and the
	/// gateway saved (named after itself, once it tells its name).
	nonisolated private static func awaitApproval(
		_ invitation: PairingInvitation, client: RemoteFileClient, pending: PairingPending,
		environment: RemoteEnvironment
	) async throws(RemotePairingError) -> RemoteEndpoint {
		var credential: String?
		while credential == nil {
			if Task.isCancelled {
				throw .remote(.canceled)
			}
			if Date() > pending.expires.addingTimeInterval(30) {
				throw .expired
			}
			do throws(RemoteError) {
				let result = try await client.pollPairing(
					requestID: pending.request, pollToken: pending.pollToken, wait: 25)
				if result.state == .approved {
					guard let approved = result.credential, !approved.isEmpty else {
						throw .protocolViolation("the request was approved without a credential")
					}
					credential = approved
				}
			}
			catch .forbidden(code: RemoteErrorCode.pairingDenied) {
				throw .denied
			}
			catch .server(status: _, code: .some(RemoteErrorCode.pairingExpired)), .notFound {
				throw .expired
			}
			catch .canceled {
				throw .remote(.canceled)
			}
			catch {
				guard error.isTransient else {
					throw .remote(error)
				}
				try? await Task.sleep(for: .seconds(3))
			}
		}
		guard let credential else {
			throw .expired
		}

		var endpoint = invitation.endpoint
		endpoint.serviceID = pending.serviceID
		do {
			try environment.credentials.setCredential(credential, serviceID: endpoint.serviceID)
			if let existing = try environment.store.remoteEndpoint(serviceID: endpoint.serviceID) {
				// Paired again: keep the name it had
				endpoint.displayName = existing.displayName
			}
			try environment.store.saveRemoteEndpoint(endpoint)
		}
		catch let error as IndexError {
			throw .remote(.store(error))
		}
		catch {
			throw .remote(.fileSystem(error.localizedDescription))
		}
		environment.forgetClient(serviceID: endpoint.serviceID)
		// Learn the gateway's name (and its authority generation) right away
		let sync = environment.sync(for: endpoint)
		if let capabilities = try? await sync.refreshCapabilities(), !capabilities.serviceName.isEmpty,
			var stored = try? environment.store.remoteEndpoint(serviceID: endpoint.serviceID)
		{
			stored.displayName = capabilities.serviceName
			try? environment.store.saveRemoteEndpoint(stored)
			endpoint = stored
		}
		return endpoint
	}
}
