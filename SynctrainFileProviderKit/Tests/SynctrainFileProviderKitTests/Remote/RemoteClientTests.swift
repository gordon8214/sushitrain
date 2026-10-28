// Copyright (C) 2026 Gordon
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this file,
// You can obtain one at https://mozilla.org/MPL/2.0/.
import Foundation
import Security
import Testing

@testable import SynctrainFileProviderKit

@Suite struct RemoteFixtureTests {
	@Test func decodesEveryFixture() throws {
		let names = try FileManager.default.contentsOfDirectory(atPath: fixturesURL.path(percentEncoded: false))
			.filter { $0.hasSuffix(".json") }.sorted()
		#expect(names.count == 20)
		for name in names {
			let data = try Data(contentsOf: fixturesURL.appending(path: name))
			let decoder = RemoteJSON.decoder()
			switch name {
			case "capabilities.json":
				let capabilities = try decoder.decode(Capabilities.self, from: data)
				#expect(capabilities.supports(.longPoll) && !capabilities.supports(.videoThumbnails))
				#expect(capabilities.limits.maxUploadSize == 1_099_511_627_776)
			case "changes-page.json":
				let page = try decoder.decode(ChangesPage.self, from: data)
				#expect(page.changes.count == 2 && page.changes[1].isDeleted && !page.more)
			case "error-cursor-expired.json", "error-revision-mismatch.json":
				let body = try decoder.decode(ErrorBody.self, from: data)
				#expect(body.error.correlationID != nil)
				if name == "error-revision-mismatch.json" {
					#expect(body.error.item?.kind == .file)
				}
			case "folders.json":
				let folders = try decoder.decode(RemoteFolderList.self, from: data).folders
				#expect(folders.map(\.permissions.write) == [true, false] && folders[0].type == .sendReceive)
			case "item-directory.json", "item-file.json":
				let item = try decoder.decode(RemoteItem.self, from: data)
				#expect(item.kind == (name == "item-file.json" ? .file : .directory))
				if let contentID = item.contentID {
					#expect(contentID.size == 266_240 && contentID.blocksHash?.count == 32)
				}
			case "manifest-page.json":
				let page = try decoder.decode(ManifestPage.self, from: data)
				#expect(page.blocks.count == 3 && page.next == nil)
				#expect(page.blocks.allSatisfy { Data(base64URLEncoded: $0.hash)?.count == 32 })
			case "operation-committed.json", "operation-conflict.json", "operation-pending.json":
				let operation = try decoder.decode(RemoteOperation.self, from: data)
				#expect(operation.authorityGeneration == 3)
				#expect(name != "operation-conflict.json" || operation.conflictItem != nil)
			case "operation-create.json", "operation-delete.json", "operation-move.json", "operation-replace.json":
				// Requests encode back to the same JSON
				let request = try decoder.decode(OperationRequest.self, from: data)
				let reencoded = try JSONSerialization.jsonObject(with: try request.canonicalJSON()) as? NSDictionary
				let original = try JSONSerialization.jsonObject(with: data) as? NSDictionary
				#expect(reencoded == original, "\(name)")
			case "pairing-approved.json":
				let result = try decoder.decode(PairingResult.self, from: data)
				#expect(result.state == .approved && result.credential?.hasPrefix("stc_") == true)
			case "pairing-request.json":
				let request = try decoder.decode(PairingRequest.self, from: data)
				let reencoded = try JSONSerialization.jsonObject(with: try RemoteJSON.encoder().encode(request))
				#expect(reencoded as? NSDictionary == (try JSONSerialization.jsonObject(with: data) as? NSDictionary))
			case "read-lease.json":
				let lease = try decoder.decode(ReadLease.self, from: data)
				#expect(lease.preparation.state == .preparing && lease.expires.timeIntervalSince1970 > 0)
				#expect(lease.contentID.size == lease.size)
			case "snapshot-page.json":
				let page = try decoder.decode(SnapshotPage.self, from: data)
				#expect(page.items.first?.isRoot == true && page.next != nil)
			case "upload-created.json":
				let upload = try decoder.decode(UploadInfo.self, from: data)
				#expect(upload.token.hasPrefix("stu_"))
			default:
				Issue.record("no test decodes \(name)")
			}
		}
	}

	@Test func unknownFieldsAndValuesAreTolerated() throws {
		let json = """
			{"id": "itm_x", "folderID": "f", "parentID": "itm_r", "name": "s", "path": "s", "kind": "socket",
			 "modifiedNs": 1, "revision": "r1.x", "availability": "archived", "color": "blue",
			 "contentID": "not a content id"}
			"""
		let item = try RemoteJSON.decoder().decode(RemoteItem.self, from: Data(json.utf8))
		#expect(item.kind == .unknown("socket") && item.availability == .unknown("archived"))
		#expect(item.contentID?.isValid == false && item.contentID?.size == nil)
		let operation = try RemoteJSON.decoder().decode(
			RemoteOperation.self,
			from: Data(
				#"{"id":"x","operation":"op_1","kind":"copy","state":"queued","authorityGeneration":1,"new":true}"#.utf8))
		#expect(operation.kind == .unknown("copy") && operation.state == .unknown("queued"))
		#expect(
			try RemoteJSON.decoder().decode(
				ReadLease.Preparation.self, from: Data(#"{"state":"ready","readyBytes":1}"#.utf8)
			).state == .ready)
		let fractional = try RemoteJSON.decoder().decode(
			UploadInfo.self,
			from: Data(#"{"upload":"u","token":"t","offset":0,"length":1,"expires":"2026-11-03T19:00:00.123456Z"}"#.utf8))
		#expect(fractional.expires.timeIntervalSince1970 > 1_793_000_000)
	}

	@Test func contentIdentityFormat() throws {
		let id = ContentIdentity("c1.0PQuLA1Ythwbj9UtAT7Ozmyq4hWb4yL8x0Mr5i8QwRI.266240")
		#expect(id.isValid && id.size == 266_240 && id.entityTag == "\"\(id.rawValue)\"")
		#expect(ContentIdentity(blocksHash: try #require(id.blocksHash), size: 266_240) == id)
		#expect(!ContentIdentity("c2.abc.1").isValid && !ContentIdentity("c1.abc.-1").isValid)
		#expect(!ContentIdentity("../x").isSafeFileName && id.isSafeFileName)
	}

	@Test func pairingInvitation() throws {
		let url = try #require(
			URL(string: "synctrain+pair://truenas.local:20920/?service=svc_abc&secret=K7gNU3sdo-OL0wNhqoVWhg&spki=xyz"))
		let invitation = try #require(PairingInvitation(url: url))
		#expect(invitation.host == "truenas.local" && invitation.port == 20920 && invitation.pinnedSPKIHash == "xyz")
		#expect(invitation.endpoint.baseURL.absoluteString == "https://truenas.local:20920")
		#expect(PairingInvitation(url: try #require(URL(string: "https://x:1/?service=a&secret=b"))) == nil)
		let ipv6URL = try #require(URL(string: "synctrain+pair://[fd00::1]:443/?service=a&secret=b"))
		let ipv6 = try #require(PairingInvitation(url: ipv6URL))
		#expect(ipv6.host == "fd00::1" && ipv6.endpoint.baseURL.absoluteString == "https://[fd00::1]:443")
	}
}

@Suite struct RemoteClientTests {
	@Test func mapsErrors() async throws {
		let current = remoteFile("itm_a", "a.txt")
		let responses: [(MockResponse, RemoteError)] = [
			(.error(401, code: "unauthenticated"), .unauthenticated),
			(.error(403, code: "folder_read_only"), .forbidden(code: "folder_read_only")),
			(.error(404, code: "not_found"), .notFound),
			(.error(409, code: "name_conflict", item: current), .nameConflict(current: current)),
			(.error(409, code: "not_empty"), .server(status: 409, code: "not_empty")),
			(.error(410, code: "cursor_expired"), .cursorExpired),
			(.error(410, code: "snapshot_expired"), .snapshotExpired),
			(.error(412, code: "revision_mismatch", item: current), .revisionMismatch(current: current)),
			(.error(422, code: "operation_mismatch"), .operationMismatch),
			(MockResponse(status: 429, headers: ["Retry-After": "7"]), .overloaded(retryAfter: 7)),
			(MockResponse(status: 503, headers: ["Retry-After": "2"]), .unavailable(retryAfter: 2)),
			(.error(507, code: "insufficient_storage"), .insufficientStorage),
			(.error(500, code: "internal"), .server(status: 500, code: "internal")),
		]
		for (response, expected) in responses {
			let client = mockClient { _ in response }
			await #expect(throws: expected) { try await client.item(id: "itm_a") }
		}
	}

	/// A Keychain that cannot be read now (before the first unlock) is no reason to pair again
	@Test func unreadableKeychainIsTransient() async throws {
		struct LockedKeychain: RemoteCredentialStore {
			func credential(serviceID: String) throws(RemoteCredentialError) -> String? {
				throw .keychain(errSecInteractionNotAllowed)
			}
			func setCredential(_ credential: String, serviceID: String) {}
			func removeCredential(serviceID: String) {}
			func token(reference: String) -> String? { nil }
			func setToken(_ token: String, reference: String) {}
			func removeToken(reference: String) {}
		}
		let host = uniqueHost()
		let client = RemoteFileClient(
			endpoint: RemoteEndpoint(serviceID: testServiceID, host: host, port: 20920, displayName: "mock"),
			credentials: LockedKeychain(),
			configuration: MockURLProtocol.configuration(host: host) { _ in .error(500, code: "unexpected") })
		await #expect(throws: RemoteError.unavailable(retryAfter: 10)) { try await client.capabilities() }
		#expect(RemoteError.unavailable(retryAfter: 10).isTransient)
	}

	@Test func refusesOtherOriginsAndRedirects() async throws {
		let otherOrigin = mockClient { request in
			var response = MockResponse.json(remoteFile("itm_a", "a.txt"))
			response.url = URL(string: "https://elsewhere.test/v1/items/itm_a")
			return response
		}
		await #expect(throws: RemoteError.protocolViolation("a response came from another origin")) {
			try await otherOrigin.item(id: "itm_a")
		}
		let redirect = mockClient { _ in MockResponse(status: 302, headers: ["Location": "https://elsewhere.test/"]) }
		await #expect(throws: RemoteError.protocolViolation("the gateway redirected a request")) {
			try await redirect.item(id: "itm_a")
		}
	}

	@Test func headersAndPaths() async throws {
		let seen = Locked<[MockRequest]>([])
		let client = mockClient { request in
			seen.update { $0.append(request) }
			return .json(ChangesPage(changes: [], cursor: "g1.2", more: false))
		}
		try await RemoteFileClient.$correlationID.withValue("open-1234") {
			_ = try await client.changes(folderID: "My Folder/1", cursor: "g1.1", wait: 5)
		}
		let request = try #require(seen.value.first)
		#expect(request.url.path(percentEncoded: true) == "/v1/folders/My%20Folder%2F1/changes")
		#expect(request.query("cursor") == "g1.1" && request.query("wait") == "5")
		#expect(request.header(RemoteFileClient.correlationHeader) == "open-1234")
		#expect(request.header("Authorization") == "Bearer stc_test")

		let unpaired = mockClient(credential: nil) { _ in .json(ChangesPage(changes: [], cursor: "g", more: false)) }
		await #expect(throws: RemoteError.unauthenticated) {
			try await unpaired.changes(folderID: "f", cursor: "g")
		}
	}

	@Test func bodiesAreBounded() async throws {
		let host = uniqueHost()
		let endpoint = RemoteEndpoint(serviceID: testServiceID, host: host, port: 20920, displayName: "mock")
		let client = RemoteFileClient(
			endpoint: endpoint, credentials: InMemoryCredentialStore(credentials: [testServiceID: "stc"]),
			configuration: MockURLProtocol.configuration(host: host) { _ in
				MockResponse(status: 200, body: Data(repeating: 0x20, count: 10_000))
			}, maxControlBodyBytes: 1000)
		await #expect(throws: RemoteError.self) { try await client.capabilities() }
	}
}

@Suite struct RemoteTrustTests {
	/// Self-signed certificates for gateway.test, and their pins (from openssl: the SHA-256 of the DER public key)
	static let ecCertificate = """
		MIIBfTCCASOgAwIBAgIUbgvdePJGBupHUvZw/6TBhTqHL3UwCgYIKoZIzj0EAwIwFzEVMBMGA1UEAwwMZ2F0ZXdheS50ZXN0MCAXDTI2MTAy\
		ODAxMzY1NloYDzIxMjYxMDA0MDEzNjU2WjAXMRUwEwYDVQQDDAxnYXRld2F5LnRlc3QwWTATBgcqhkjOPQIBBggqhkjOPQMBBwNCAASZQ3k+\
		vsWwa2qGl2mzINnHNcwNy1Ax5rL3hb9KFpYTZqYpwMDLCwQe/nNy91x5k33hCPmjgljN0ItZ+uiTkeYHo0swSTAdBgNVHQ4EFgQU4IyEDq2Q\
		I9SLVVBC/SxMcXRh3pwwDwYDVR0TAQH/BAUwAwEB/zAXBgNVHREEEDAOggxnYXRld2F5LnRlc3QwCgYIKoZIzj0EAwIDSAAwRQIhAOINxSok\
		H9/qzrcez2xUEtbBFsJteq8to1JkehmZJIyyAiB2Pgb0OFmaVhc112yaS65GAcXNCgiYMUW+eDJ53U9lWw==
		"""
	static let ecPin = "wMIrEhcMnRd3xHGPZZK45kvoYPXsPRrU0bmPwmIudcs"
	static let rsaCertificate = """
		MIIDCTCCAfGgAwIBAgIUalCiEYvpflKMMbzby3Ca4l27BDUwDQYJKoZIhvcNAQELBQAwFzEVMBMGA1UEAwwMZ2F0ZXdheS50ZXN0MCAXDTI2\
		MTAyODAxMzY1NloYDzIxMjYxMDA0MDEzNjU2WjAXMRUwEwYDVQQDDAxnYXRld2F5LnRlc3QwggEiMA0GCSqGSIb3DQEBAQUAA4IBDwAwggEK\
		AoIBAQDUtaqY+iQUQIUJs711BynSnk8xyNLpjh4fy/2ARne+i+FC8s2h9e8P2l24NnaJvKcU/qrkCODVQFSQKcknD/AZsGRpc+q5TVdrBkni\
		C2Q3xeI/2/8ODik7wUNLR/pJEthkR70xBjP1vZbxEaSKg43foqnQC/hUHjJSZV9dg10RLJKiOtKVSRT//mGOkFBjyHTJImMgjFX39WRwcOKn\
		bAtF+g5SFXZ4LT4QjoePzCbMFqMDBJQE/pHCS4Awj2RypynlHw8wxikk6/sNNPb/S3/Ru4ypl9R8gAhazjLDf8D8LXDzsKuT7M/nlAFeCqF1\
		bgVY2HWr3l/LUFPVxqv1TzIFAgMBAAGjSzBJMB0GA1UdDgQWBBSI8j01XLxvqbHPgmXZpNlvwOk1pjAPBgNVHRMBAf8EBTADAQH/MBcGA1Ud\
		EQQQMA6CDGdhdGV3YXkudGVzdDANBgkqhkiG9w0BAQsFAAOCAQEAK61k8moxXHYtY+w29kF5Pl6z1QFDjWfEAMx+pO5Gsjy1ul4/m9acyd0w\
		Uj42u3QBZCZUN+F+oMvyNJ7+wBEqO7XLFMWIZk9bF0UCaKOSpawXTt+D1HJj1q4bJzCSgcXrgdMJK5yh/o07oXVDPeX4t3NW8RVBUSZHkGhV\
		I+HwAjt9zdvmtEr2Ir3q7X7/2EirpMtRTUGmaSq/m84nbptHZLVJciNY5cjN1eXH0tCsBmgQdq7pgbe2e7RDQ7bGV9Wb1uxdDDFsThifWP4J\
		pTPSRTsk/s0g04OilcWekKWLkB92319intHTkMLmu6BR0pZnb814RpnLgQJwMeawNE//sg==
		"""
	static let rsaPin = "q5ip9g0e6ehdwrGoB9cAT2UFQ6tOFihSWK2Hrz5XYQs"

	func certificate(_ base64: String) throws -> SecCertificate {
		let der = try #require(Data(base64Encoded: base64))
		return try #require(SecCertificateCreateWithData(nil, der as CFData))
	}

	func trust(_ certificate: SecCertificate) throws -> SecTrust {
		var trust: SecTrust?
		let status = SecTrustCreateWithCertificates(
			certificate, SecPolicyCreateSSL(true, "gateway.test" as CFString), &trust)
		#expect(status == errSecSuccess)
		return try #require(trust)
	}

	@Test func pinsAreTheHashOfTheSubjectPublicKeyInfo() throws {
		#expect(RemoteTrust.spkiHash(of: try certificate(Self.ecCertificate)) == Self.ecPin)
		#expect(RemoteTrust.spkiHash(of: try certificate(Self.rsaCertificate)) == Self.rsaPin)
		#expect(RemoteTrust.spkiHash(certificateDER: Data([0x30, 0x03, 0x02, 0x01, 0x00])) == nil)
	}

	@Test func evaluatesPinsAndSystemTrust() throws {
		let ec = try certificate(Self.ecCertificate)
		#expect(RemoteTrust.evaluate(try trust(ec), host: "gateway.test", pins: [Self.ecPin]))
		#expect(RemoteTrust.evaluate(try trust(ec), host: "gateway.test", pins: [Self.rsaPin, Self.ecPin]))
		#expect(!RemoteTrust.evaluate(try trust(ec), host: "gateway.test", pins: [Self.rsaPin]))
		#expect(!RemoteTrust.evaluate(try trust(ec), host: "gateway.test", pins: []), "self-signed: not trusted")
		let rotated = RemoteEndpoint(
			serviceID: "s", host: "gateway.test", port: 1, pinnedSPKIHash: Self.rsaPin, nextPinnedSPKIHash: Self.ecPin,
			displayName: "g")
		#expect(RemoteTrust.evaluate(try trust(ec), host: "gateway.test", pins: rotated.acceptedSPKIHashes))
	}

	/// Server trust reaches the task's delegate, whose rejection the client reports as an untrusted certificate (not
	/// as a canceled request); other challenges are refused
	@Test func trustDelegateAnswersChallenges() throws {
		let endpoint = RemoteEndpoint(
			serviceID: "s", host: "gateway.test", port: 20920, pinnedSPKIHash: Self.ecPin, displayName: "g")
		let sessionLevel = NSSelectorFromString("URLSession:didReceiveChallenge:completionHandler:")
		#expect(!RemoteTrustDelegate(endpoint: endpoint).responds(to: sessionLevel))
		#expect(!BoundedBodyDelegate(trust: RemoteTrustDelegate(endpoint: endpoint), limit: 1).responds(to: sessionLevel))
		let serverTrust = NSURLAuthenticationMethodServerTrust

		let accepted = RemoteTrustDelegate(endpoint: endpoint)
		let ec = try trust(try certificate(Self.ecCertificate))
		#expect(
			accepted.answer(authenticationMethod: serverTrust, host: "gateway.test", port: 20920, serverTrust: ec).0
				== .useCredential)
		#expect(!accepted.rejectedTrust)
		#expect(RemoteFileClient.map(URLError(.cancelled), delegate: accepted) == .canceled)

		let mismatch = RemoteTrustDelegate(endpoint: endpoint)
		let rsa = try trust(try certificate(Self.rsaCertificate))
		#expect(
			mismatch.answer(authenticationMethod: serverTrust, host: "gateway.test", port: 20920, serverTrust: rsa).0
				== .cancelAuthenticationChallenge)
		#expect(mismatch.rejectedTrust)
		#expect(RemoteFileClient.map(URLError(.cancelled), delegate: mismatch) == .unreachable(.serverCertificateUntrusted))

		let elsewhere = RemoteTrustDelegate(endpoint: endpoint)
		#expect(
			elsewhere.answer(authenticationMethod: serverTrust, host: "elsewhere.test", port: 20920, serverTrust: ec).0
				== .cancelAuthenticationChallenge)

		let basic = RemoteTrustDelegate(endpoint: endpoint)
		#expect(
			basic.answer(
				authenticationMethod: NSURLAuthenticationMethodHTTPBasic, host: "gateway.test", port: 20920,
				serverTrust: nil
			).0 == .rejectProtectionSpace)
		#expect(!basic.rejectedTrust)
	}

	@Test func redirectsAreNeverFollowed() throws {
		let endpoint = RemoteEndpoint(serviceID: "s", host: "gateway.test", port: 20920, displayName: "g")
		let url = try #require(URL(string: "https://gateway.test:20920/v1/items/itm_a"))
		let session = URLSession(configuration: .ephemeral)
		let task = session.dataTask(with: url)
		let response = try #require(HTTPURLResponse(url: url, statusCode: 307, httpVersion: nil, headerFields: nil))
		let followed = Locked<Bool?>(nil)
		RemoteTrustDelegate(endpoint: endpoint).urlSession(
			session, task: task, willPerformHTTPRedirection: response, newRequest: URLRequest(url: url)
		) { request in
			followed.value = request != nil
		}
		#expect(followed.value == false, "not even to the same origin")
	}
}

@Suite struct BackgroundTransferTests {
	@Test func taskDescriptionsNameTheirTransfers() {
		let description = BackgroundTransfers.taskDescription(kind: .download, transferID: "abc")
		#expect(BackgroundTransfers.transfer(fromTaskDescription: description)?.transferID == "abc")
		#expect(BackgroundTransfers.transfer(fromTaskDescription: description)?.kind == .download)
		#expect(BackgroundTransfers.transfer(fromTaskDescription: "other/download/abc") == nil)
	}

	@Test func backgroundResponsesAreChecked() throws {
		let store = try makeStore()
		let (created, _) = try store.downloadTransfer(itemID: 1, contentID: ContentIdentity("c1.x.10"), totalSize: 10)
		try store.assignTask(
			transferID: created.transferID, sessionIdentifier: "s", taskIdentifier: 1, taskDescription: "d", offset: 4)
		let transfer = try #require(try store.transfer(id: created.transferID))
		let url = try #require(URL(string: "https://gateway.test:20920/v1/content/lse_1"))
		let task = URLSession(configuration: .ephemeral).downloadTask(with: URLRequest(url: url))
		func response(_ status: Int, url: URL = url, etag: String = "\"c1.x.10\"", range: String = "bytes 4-9/10")
			-> HTTPURLResponse?
		{
			HTTPURLResponse(
				url: url, statusCode: status, httpVersion: nil, headerFields: ["ETag": etag, "Content-Range": range])
		}
		#expect(BackgroundTransfers.problem(with: response(206), of: task, transfer: transfer) == nil)
		#expect(BackgroundTransfers.problem(with: response(200), of: task, transfer: transfer) != nil)
		#expect(BackgroundTransfers.problem(with: response(206, etag: "\"other\""), of: task, transfer: transfer) != nil)
		#expect(BackgroundTransfers.problem(with: response(206, range: "bytes 0-9/10"), of: task, transfer: transfer) != nil)
		let elsewhere = try #require(URL(string: "https://elsewhere.test/v1/content/lse_1"))
		#expect(
			BackgroundTransfers.problem(with: response(206, url: elsewhere), of: task, transfer: transfer)?
				.hasPrefix("protocol violation") == true)
	}
}
