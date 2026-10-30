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
}
