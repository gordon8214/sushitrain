// Copyright (C) 2026 Gordon
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this file,
// You can obtain one at https://mozilla.org/MPL/2.0/.
import Foundation
import Testing

@testable import SynctrainFileProviderKit

@Suite struct RemoteContentStreamTests {
	private func stream(_ gateway: ContentGateway) -> RemoteContentStream {
		RemoteContentStream(client: mockClient { gateway.handle($0) }, scheduler: nil, request: gateway.request)
	}

	/// A stream whose content responses the closure changes (the first one, when `once`)
	private func stream(
		_ gateway: ContentGateway, once: Bool = true, requestSize: Int64 = RemoteContentStream.maximumRequestSize,
		changing change: @escaping @Sendable (inout MockResponse, MockRequest) -> Void
	) -> RemoteContentStream {
		let changed = Locked(false)
		let client = mockClient { request in
			var response = gateway.handle(request)
			if request.method == "GET", request.path.hasPrefix("/v1/content/"), !request.path.hasSuffix("/manifest"),
				!(once && changed.value)
			{
				changed.update { $0 = true }
				change(&response, request)
			}
			return response
		}
		return RemoteContentStream(client: client, scheduler: nil, request: gateway.request, requestSize: requestSize)
	}

	@Test func readsRangesInAnyOrder() async throws {
		let gateway = ContentGateway(size: 300_000)
		let stream = stream(gateway)
		#expect(try await stream.open() == 300_000)

		// As when a player looks at the end of the file first, then seeks back
		#expect(try await stream.read(at: 299_000, length: 1000) == gateway.content[299_000..<300_000])
		#expect(try await stream.read(at: 10, length: 20) == gateway.content[10..<30])
		#expect(gateway.state.value.leaseRequests == 1, "one lease for the whole file")
		for request in gateway.state.value.requests {
			#expect(request.header("Authorization") == "Bearer stc_test", "the credential stays in the header")
			#expect(request.url.query?.contains("stc_") != true)
		}
	}

	@Test func readsAtMostAChunk() async throws {
		let size = Int(RemoteContentStream.maximumChunkSize) * 2
		let gateway = ContentGateway(size: size, blockSize: 1 << 20)
		let stream = stream(gateway)
		_ = try await stream.open()
		let data = try await stream.read(at: 0, length: Int64(size))
		#expect(data.count == Int(RemoteContentStream.maximumChunkSize))
		#expect(data == gateway.content[0..<data.count])
	}

	@Test func readingPastTheEndGivesNothing() async throws {
		let gateway = ContentGateway(size: 1000)
		let stream = stream(gateway)
		_ = try await stream.open()
		#expect(try await stream.read(at: 900, length: 500).count == 100, "cut off at the end of the file")
		#expect(try await stream.read(at: 1000, length: 10).isEmpty)
		#expect(try await stream.read(at: 5000, length: 10).isEmpty)
		#expect(gateway.state.value.rangeRequests == 1)
	}

	@Test func waitsWhileTheGatewayPreparesTheContent() async throws {
		let gateway = ContentGateway(size: 300_000)
		gateway.state.update { $0.preparation = [0, 100, 200] }
		let stream = stream(gateway)
		_ = try await stream.open()
		#expect(try await stream.read(at: 0, length: 150) == gateway.content[0..<150])
		#expect(gateway.state.value.leaseRequests >= 3, "asked until the bytes were ready")
	}

	/// The content of an expired lease continues with a new one
	@Test func renewsAnExpiredLease() async throws {
		let gateway = ContentGateway(size: 1000)
		let expired = Locked(true)
		let client = mockClient { request in
			if request.method == "GET", request.path.hasPrefix("/v1/content/"), expired.value {
				expired.update { $0 = false }
				return .error(410, code: "lease_expired")
			}
			return gateway.handle(request)
		}
		let stream = RemoteContentStream(client: client, scheduler: nil, request: gateway.request)
		_ = try await stream.open()
		#expect(try await stream.read(at: 0, length: 100) == gateway.content[0..<100])
		#expect(gateway.state.value.leaseRequests == 2)
	}

	@Test func aFileThatChangesEndsTheStream() async throws {
		let gateway = ContentGateway(size: 1000)
		gateway.state.update { $0.changeValidatorAfter = 1 }
		let stream = stream(gateway)
		_ = try await stream.open()
		_ = try await stream.read(at: 0, length: 100)
		await #expect(throws: RemoteError.revisionMismatch(current: nil)) {
			_ = try await stream.read(at: 100, length: 100)
		}
	}

	@Test func aGatewayThatIgnoresTheRangeIsNotBelieved() async throws {
		let gateway = ContentGateway(size: 1000)
		gateway.state.update { $0.ignoreRanges = true }
		let stream = stream(gateway)
		_ = try await stream.open()
		await #expect(throws: RemoteError.self) {
			_ = try await stream.read(at: 100, length: 100)
		}
	}

	/// Everything `stream` handed over, in order
	private func streamed(_ stream: RemoteContentStream, from start: Int64, to end: Int64) async throws -> Data {
		let received = Locked(Data())
		try await stream.stream(from: start, to: end) { chunk in
			received.update { $0.append(chunk) }
			return true
		}
		return received.value
	}

	@Test func streamsAPreparedFileInOneRequest() async throws {
		let size = Int(RemoteContentStream.maximumChunkSize) * 3 + 500
		let gateway = ContentGateway(size: size)
		let stream = stream(gateway)
		_ = try await stream.open()
		#expect(try await streamed(stream, from: 0, to: Int64(size)) == gateway.content)
		#expect(gateway.rangeHeaders == ["bytes=0-\(size - 1)"])
		#expect(try await streamed(stream, from: 1000, to: 3000) == gateway.content[1000..<3000])
		#expect(try await streamed(stream, from: Int64(size) - 10, to: Int64(size) + 100).count == 10)
		#expect(try await streamed(stream, from: Int64(size), to: Int64(size) + 100).isEmpty)
	}

	/// While the gateway prepares the content, what it has prepared is asked for, then the rest once it is prepared
	@Test func streamsWhatIsPreparedThenTheRest() async throws {
		let chunk = Int(RemoteContentStream.maximumChunkSize)
		let gateway = ContentGateway(size: chunk * 3)
		gateway.state.update {
			$0.preparation = [0, Int64(chunk + chunk / 2)]
			$0.preparationAdvice = 30
		}
		let stream = stream(gateway)
		_ = try await stream.open()
		let clock = ContinuousClock()
		let started = clock.now
		#expect(try await streamed(stream, from: 0, to: Int64(chunk * 3)) == gateway.content)
		#expect(clock.now - started < .seconds(10), "not the 30 seconds the gateway advised")
		#expect(
			gateway.rangeHeaders == [
				"bytes=0-\(chunk + chunk / 2 - 1)", "bytes=\(chunk + chunk / 2)-\(chunk * 3 - 1)",
			])
	}

	@Test func aStreamThatStopsEarlyContinuesWhereItStopped() async throws {
		let gateway = ContentGateway(size: 300_000)
		gateway.state.update { $0.truncateNext = [1000] }
		let stream = stream(gateway)
		_ = try await stream.open()
		#expect(try await streamed(stream, from: 0, to: 300_000) == gateway.content)
		#expect(gateway.rangeHeaders == ["bytes=0-299999", "bytes=1000-299999"])
	}

	@Test func aStreamOfAChangedFileHandsNothingOver() async throws {
		let gateway = ContentGateway(size: 1000)
		gateway.state.update { $0.changeValidatorAfter = 0 }
		let stream = stream(gateway)
		_ = try await stream.open()
		let received = Locked(0)
		await #expect(throws: RemoteError.revisionMismatch(current: nil)) {
			try await stream.stream(from: 0, to: 1000) { chunk in
				received.update { $0 += chunk.count }
				return true
			}
		}
		#expect(received.value == 0)
		#expect(gateway.rangeHeaders.count == 1, "the request carried the content's validator")
		#expect(gateway.state.value.requests.last?.header("If-Range") == gateway.contentID.entityTag)
	}

	@Test func aStreamOfTheWholeContentForARangeHandsNothingOver() async throws {
		let gateway = ContentGateway(size: 1000)
		gateway.state.update { $0.ignoreRanges = true }
		let stream = stream(gateway)
		_ = try await stream.open()
		let received = Locked(0)
		await #expect(throws: RemoteError.self) {
			try await stream.stream(from: 100, to: 200) { chunk in
				received.update { $0 += chunk.count }
				return true
			}
		}
		#expect(received.value == 0)
	}

	@Test func aStreamRenewsAnExpiredLease() async throws {
		let gateway = ContentGateway(size: 1000)
		let expired = Locked(true)
		let client = mockClient { request in
			if request.method == "GET", request.path.hasPrefix("/v1/content/"), expired.value {
				expired.update { $0 = false }
				return .error(410, code: "lease_expired")
			}
			return gateway.handle(request)
		}
		let stream = RemoteContentStream(client: client, scheduler: nil, request: gateway.request)
		_ = try await stream.open()
		#expect(try await streamed(stream, from: 0, to: 1000) == gateway.content)
		#expect(gateway.state.value.leaseRequests == 2)
	}

	/// Bytes handed over, and the error the stream ended with
	private func outcome(_ stream: RemoteContentStream, from start: Int64, to end: Int64) async -> (Data, RemoteError?) {
		let received = Locked(Data())
		do throws(RemoteError) {
			try await stream.stream(from: start, to: end) { chunk in
				received.update { $0.append(chunk) }
				return true
			}
			return (received.value, nil)
		}
		catch {
			return (received.value, error)
		}
	}

	@Test func aLongSpanTakesSeveralRequests() async throws {
		let gateway = ContentGateway(size: 300_000)
		let stream = stream(gateway, once: false, requestSize: 100_000) { _, _ in }
		_ = try await stream.open()
		#expect(try await streamed(stream, from: 0, to: 300_000) == gateway.content)
		#expect(gateway.rangeHeaders == ["bytes=0-99999", "bytes=100000-199999", "bytes=200000-299999"])
	}

	@Test func aDroppedConnectionContinuesWhereItStopped() async throws {
		let gateway = ContentGateway(size: 300_000)
		let stream = stream(gateway) { response, _ in
			response.chunkSize = 400
			response.failAfter = 1000
		}
		_ = try await stream.open()
		#expect(try await streamed(stream, from: 0, to: 300_000) == gateway.content)
		#expect(gateway.rangeHeaders == ["bytes=0-299999", "bytes=1000-299999"])
	}

	@Test func aConnectionThatDropsBeforeAnyByteEndsTheStream() async throws {
		let gateway = ContentGateway(size: 1000)
		let stream = stream(gateway) { response, _ in response.failAfter = 0 }
		_ = try await stream.open()
		let (data, error) = await outcome(stream, from: 0, to: 1000)
		#expect(data.isEmpty)
		#expect(error == .unreachable(.networkConnectionLost))
	}

	@Test func theReceiverCanStopTheStream() async throws {
		let gateway = ContentGateway(size: 300_000)
		let stream = stream(gateway) { response, _ in response.chunkSize = 1000 }
		_ = try await stream.open()
		let calls = Locked(0)
		await #expect(throws: RemoteError.canceled) {
			try await stream.stream(from: 0, to: 300_000) { _ in
				calls.update { $0 += 1 }
				return false
			}
		}
		#expect(calls.value == 1, "nothing after the chunk it refused")
		#expect(gateway.rangeHeaders.count == 1)
	}

	@Test func aCanceledTaskAsksForNothing() async throws {
		let gateway = ContentGateway(size: 1000)
		let stream = stream(gateway)
		_ = try await stream.open()
		let task = Task { () async throws in
			withUnsafeCurrentTask { $0?.cancel() }
			try await stream.stream(from: 0, to: 1000) { _ in true }
		}
		await #expect(throws: RemoteError.canceled) { try await task.value }
		#expect(gateway.rangeHeaders.isEmpty)
	}

	@Test func aResponseLongerThanTheRangeIsCutOff() async throws {
		let gateway = ContentGateway(size: 1000)
		let stream = stream(gateway) { response, _ in
			response.body = gateway.content[0..<200]
			response.chunkSize = 64
		}
		_ = try await stream.open()
		let (data, error) = await outcome(stream, from: 0, to: 100)
		#expect(data == gateway.content[0..<100])
		#expect(error == .protocolViolation("the response is longer than the range"))
	}

	/// Responses that hand nothing over, and the error each ends the stream with
	@Test(arguments: [
		("another range", RemoteError.protocolViolation("the gateway answered with another range")),
		("another origin", .protocolViolation("a response came from another origin")),
		("all of other content", .revisionMismatch(current: nil)),
		("a weak validator", .protocolViolation("the content has no strong validator")),
		("nothing", .protocolViolation("the gateway sent none of the range")),
		("unavailable", .unavailable(retryAfter: 2)),
	])
	func responsesThatHandNothingOver(_ kind: String, _ expected: RemoteError) async throws {
		let gateway = ContentGateway(size: 1000)
		let stream = stream(gateway) { response, _ in
			switch kind {
			case "another range":
				response.headers["Content-Range"] = "bytes 0-99/1000"
			case "another origin":
				response.url = URL(string: "https://elsewhere.test/v1/content/x")
			case "all of other content":
				response = MockResponse(status: 200, headers: ["ETag": "\"c1.other.1000\""], body: gateway.content)
			case "a weak validator":
				response.headers["ETag"] = "W/" + gateway.contentID.entityTag
			case "nothing":
				response.body = Data()
			default:
				response = .error(503, code: "unavailable")
				response.headers["Retry-After"] = "2"
			}
		}
		_ = try await stream.open()
		let (data, error) = await outcome(stream, from: 100, to: 200)
		#expect(data.isEmpty)
		#expect(error == expected)
	}
}
