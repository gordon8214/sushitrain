// Copyright (C) 2026 Gordon
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this file,
// You can obtain one at https://mozilla.org/MPL/2.0/.
import CryptoKit
import Foundation
import os

@testable import SynctrainFileProviderKit

/// A response of a mocked gateway
struct MockResponse: Sendable {
	var status: Int
	var headers: [String: String] = [:]
	var body = Data()
	/// The URL the response claims to come from (the request's by default)
	var url: URL?

	static func json<T: Encodable>(_ value: T, status: Int = 200) -> MockResponse {
		MockResponse(
			status: status, headers: ["Content-Type": "application/json"],
			body: (try? RemoteJSON.encoder().encode(value)) ?? Data())
	}

	static func error(_ status: Int, code: String, item: RemoteItem? = nil) -> MockResponse {
		.json(ErrorBody(error: ErrorDetail(code: code, message: code, item: item)), status: status)
	}
}

/// A request as the mocked gateway saw it
struct MockRequest: Sendable {
	var method: String
	var url: URL
	var headers: [String: String]
	var body: Data

	var path: String { url.path(percentEncoded: false) }

	func query(_ name: String) -> String? {
		URLComponents(url: url, resolvingAgainstBaseURL: false)?.queryItems?.first { $0.name == name }?.value
	}

	func header(_ name: String) -> String? {
		headers.first { $0.key.caseInsensitiveCompare(name) == .orderedSame }?.value
	}
}

/// Routes the requests of a session to a handler, by host (every test uses its own host)
final class MockURLProtocol: URLProtocol, @unchecked Sendable {
	typealias Handler = @Sendable (MockRequest) -> MockResponse

	private static let handlers = OSAllocatedUnfairLock<[String: Handler]>(initialState: [:])

	/// A configuration whose requests to the host go to the handler
	static func configuration(host: String, handler: @escaping Handler) -> URLSessionConfiguration {
		handlers.withLock { $0[host.lowercased()] = handler }
		let configuration = URLSessionConfiguration.ephemeral
		configuration.protocolClasses = [MockURLProtocol.self]
		return configuration
	}

	override class func canInit(with request: URLRequest) -> Bool {
		guard let host = request.url?.host()?.lowercased() else {
			return false
		}
		return handlers.withLock { $0[host] != nil }
	}

	override class func canonicalRequest(for request: URLRequest) -> URLRequest {
		request
	}

	override func startLoading() {
		guard let url = request.url, let host = url.host()?.lowercased(),
			let handler = Self.handlers.withLock({ $0[host] })
		else {
			client?.urlProtocol(self, didFailWithError: URLError(.cannotFindHost))
			return
		}
		var body = request.httpBody ?? Data()
		if body.isEmpty, let stream = request.httpBodyStream {
			stream.open()
			var buffer = [UInt8](repeating: 0, count: 64 << 10)
			while true {
				let count = stream.read(&buffer, maxLength: buffer.count)
				if count <= 0 {
					break
				}
				body.append(contentsOf: buffer[0..<count])
			}
			stream.close()
		}
		let mockRequest = MockRequest(
			method: request.httpMethod ?? "GET", url: url,
			headers: (request.allHTTPHeaderFields ?? [:]), body: body)
		let response = handler(mockRequest)
		guard
			let http = HTTPURLResponse(
				url: response.url ?? url, statusCode: response.status, httpVersion: "HTTP/1.1",
				headerFields: response.headers)
		else {
			client?.urlProtocol(self, didFailWithError: URLError(.badServerResponse))
			return
		}
		client?.urlProtocol(self, didReceive: http, cacheStoragePolicy: .notAllowed)
		if !response.body.isEmpty {
			client?.urlProtocol(self, didLoad: response.body)
		}
		client?.urlProtocolDidFinishLoading(self)
	}

	override func stopLoading() {}
}

/// A thread-safe box for test state shared with handlers
final class Locked<Value: Sendable>: Sendable {
	private let lock: OSAllocatedUnfairLock<Value>

	init(_ value: Value) {
		lock = OSAllocatedUnfairLock(initialState: value)
	}

	var value: Value {
		get { lock.withLock { $0 } }
		set { lock.withLock { $0 = newValue } }
	}

	@discardableResult
	func update<T: Sendable>(_ body: @Sendable (inout Value) -> T) -> T {
		lock.withLock { body(&$0) }
	}
}

func uniqueHost() -> String {
	"gw-\(UUID().uuidString.prefix(8).lowercased()).test"
}

/// A client of a mocked gateway at a host of its own
func mockClient(
	host: String = uniqueHost(), credential: String? = "stc_test", handler: @escaping MockURLProtocol.Handler
) -> RemoteFileClient {
	let endpoint = RemoteEndpoint(serviceID: testServiceID, host: host, port: 20920, displayName: "mock")
	let credentials = InMemoryCredentialStore(credentials: credential.map { [testServiceID: $0] } ?? [:])
	return RemoteFileClient(
		endpoint: endpoint, credentials: credentials,
		configuration: MockURLProtocol.configuration(host: host, handler: handler))
}

/// Content served by a mocked gateway, with knobs for the ways it can go wrong
final class ContentGateway: Sendable {
	let content: Data
	let blockSize: Int
	let leaseID = "lse_tUSnaG0RhmgKnY8k_1QrAA"
	let itemID = "itm_hF6RgxMZ6JxNZWvbgMJ4rA"
	let revision: RemoteRevision

	struct State: Sendable {
		var requests: [MockRequest] = []
		/// Blocks whose bytes are damaged in every response
		var tamperedBlocks: Set<Int> = []
		/// Responses to range requests cut short (after this many bytes), once each
		var truncateNext: [Int] = []
		/// Ranges answered with the whole content (`200`)
		var ignoreRanges = false
		/// Range requests after this many get another validator
		var changeValidatorAfter: Int?
		/// Hashes the manifest reports instead of the real ones
		var forgedManifest = false
		/// Bytes reported ready by successive lease requests (then ready)
		var preparation: [Int64] = []
		var rangeRequests = 0
		var leaseRequests = 0
	}

	let state = Locked(State())

	init(size: Int, blockSize: Int = 128 << 10) {
		var bytes = Data(count: size)
		for index in 0..<size {
			bytes[index] = UInt8(truncatingIfNeeded: index &* 31 &+ index >> 8)
		}
		content = bytes
		self.blockSize = blockSize
		revision = SynctrainFileProviderKitTests.revision("content")
	}

	var blocks: [(offset: Int, size: Int, hash: Data)] {
		if content.isEmpty {
			return [(0, 0, Data(SHA256.hash(data: Data())))]
		}
		return stride(from: 0, to: content.count, by: blockSize).map { offset in
			let size = min(blockSize, content.count - offset)
			return (offset, size, Data(SHA256.hash(data: content[offset..<offset + size])))
		}
	}

	var contentID: ContentIdentity {
		var hasher = SHA256()
		for block in blocks {
			hasher.update(data: block.hash)
		}
		return ContentIdentity(blocksHash: Data(hasher.finalize()), size: Int64(content.count))
	}

	var request: RemoteDownloader.Request {
		RemoteDownloader.Request(itemID: 1, gatewayID: itemID, revision: revision, contentID: contentID)
	}

	func handle(_ request: MockRequest) -> MockResponse {
		state.update { $0.requests.append(request) }
		let path = request.path
		if request.method == "POST", path == "/v1/items/\(itemID)/read-leases" {
			let ready = state.update { state -> Int64? in
				state.leaseRequests += 1
				return state.preparation.isEmpty ? nil : state.preparation.removeFirst()
			}
			let lease = ReadLease(
				lease: leaseID, token: "stl_token", itemID: itemID, revision: revision, contentID: contentID,
				size: Int64(content.count), blockSize: blockSize, blockCount: blocks.count,
				expires: Date().addingTimeInterval(86400),
				preparation: ReadLease.Preparation(
					state: ready == nil ? .ready : .preparing, readyBytes: ready ?? Int64(content.count),
					retryAfter: ready == nil ? nil : 0))
			return .json(lease, status: 201)
		}
		if request.method == "GET", path == "/v1/content/\(leaseID)/manifest" {
			let from = Int(request.query("from") ?? "0") ?? 0
			let limit = Int(request.query("limit") ?? "1024") ?? 1024
			let all = blocks
			let forged = state.value.forgedManifest
			let page = all[min(from, all.count)..<min(from + limit, all.count)].map { block in
				ManifestPage.Block(
					offset: Int64(block.offset), size: block.size,
					hash: (forged ? Data(SHA256.hash(data: block.hash)) : block.hash).base64URLEncodedString())
			}
			let next = from + page.count < all.count ? from + page.count : nil
			return .json(ManifestPage(contentID: contentID, blockSize: blockSize, blocks: page, next: next))
		}
		if request.method == "GET", path == "/v1/content/\(leaseID)" {
			return content(request)
		}
		return .error(404, code: "not_found")
	}

	private func content(_ request: MockRequest) -> MockResponse {
		let (count, settings) = state.update { state -> (Int, State) in
			state.rangeRequests += 1
			return (state.rangeRequests, state)
		}
		var bytes = content
		for block in settings.tamperedBlocks where block < blocks.count {
			bytes[blocks[block].offset] ^= 0xFF
		}
		var entityTag = contentID.entityTag
		if let after = settings.changeValidatorAfter, count > after {
			entityTag = "\"c1.other.\(content.count)\""
		}
		guard let range = request.header("Range"), !settings.ignoreRanges else {
			return MockResponse(status: 200, headers: ["ETag": entityTag], body: bytes)
		}
		let bounds = range.dropFirst("bytes=".count).split(separator: "-", omittingEmptySubsequences: false)
		let start = Int(bounds[0]) ?? 0
		let end = bounds.count > 1 && !bounds[1].isEmpty ? Int(bounds[1]) ?? content.count - 1 : content.count - 1
		var body = bytes[start...min(end, content.count - 1)]
		let truncate = state.update { state -> Int? in
			state.truncateNext.isEmpty ? nil : state.truncateNext.removeFirst()
		}
		if let truncate {
			body = body.prefix(truncate)
		}
		return MockResponse(
			status: 206,
			headers: ["ETag": entityTag, "Content-Range": "bytes \(start)-\(end)/\(content.count)"],
			body: Data(body))
	}

	var rangeHeaders: [String] {
		state.value.requests.filter { $0.path == "/v1/content/\(leaseID)" }.compactMap { $0.header("Range") }
	}
}
