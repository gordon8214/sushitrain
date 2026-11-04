// Copyright (C) 2026 Gordon
// SPDX-License-Identifier: MPL-2.0
#if os(macOS)
	import Foundation
	import Darwin
	import Testing
	@testable import SynctrainHostKit

	@Test func historyBridgeCopyPreservesForkAndBinaryAttributes() throws {
		let directory = URL.temporaryDirectory.appending(path: "HistoryBridge-test-\(UUID())")
		try FileManager.default.createDirectory(at: directory, withIntermediateDirectories: false)
		defer { try? FileManager.default.removeItem(at: directory) }
		let source = directory.appending(path: "source")
		let target = directory.appending(path: "target")
		try Data("content".utf8).write(to: source)
		let fork = Data(repeating: 42, count: 2 << 20)
		let result = fork.withUnsafeBytes {
			setxattr(source.path, "com.apple.ResourceFork", $0.baseAddress, fork.count, 0, 0)
		}
		#expect(result == 0)
		let binary = Data([0, 255, 42])
		#expect(
			binary.withUnsafeBytes { setxattr(source.path, "com.synctrain.binary", $0.baseAddress, binary.count, 0, 0) } == 0)
		try HistoryBridgeFiles.copyVerified(from: source, to: target)
		#expect(try HistoryBridgeFiles.signature(source) == HistoryBridgeFiles.signature(target))
		#expect(throws: (any Error).self) { try HistoryBridgeFiles.copyVerified(from: source, to: target) }
	}

	@Test func historyBridgeRejectsAnUnsignedPeer() async throws {
		var descriptors = [Int32](repeating: 0, count: 2)
		#expect(socketpair(AF_UNIX, SOCK_STREAM, 0, &descriptors) == 0)
		let first = HistoryBridgeChannel(descriptors[0])
		let second = HistoryBridgeChannel(descriptors[1])
		#expect(throws: (any Error).self) { try first.verify(identifier: HistoryBridgeLocation.clientIdentifier) }
		try first.write(HistoryBridgeRequest(action: "list", filePath: "/tmp/Café"))
		let request = try second.read(HistoryBridgeRequest.self)
		#expect(request.filePath == "/tmp/Café")
	}
#endif

#if os(macOS)
	@Test @MainActor func historyBridgeQueuesLargeFramesWithoutBlockingTheMainActor() async throws {
		var descriptors = [Int32](repeating: 0, count: 2)
		#expect(socketpair(AF_UNIX, SOCK_STREAM, 0, &descriptors) == 0)
		let writer = HistoryBridgeChannel(descriptors[0])
		let reader = HistoryBridgeChannel(descriptors[1])
		let output = HistoryBridgeOutput(channel: writer)
		var response = HistoryBridgeResponse(event: "source")
		response.message = String(repeating: "x", count: 1 << 20)
		let started = ContinuousClock.now
		try output.send(response)
		try output.send(HistoryBridgeResponse(event: "finished"))
		#expect(started.duration(to: .now) < .seconds(1))
		let receive = Task.detached {
			let first = try reader.read(HistoryBridgeResponse.self)
			let last = try reader.read(HistoryBridgeResponse.self)
			return (first.message?.count, last.event)
		}
		try await output.flush()
		let result = try await receive.value
		#expect(result.0 == 1 << 20)
		#expect(result.1 == "finished")
	}
#endif
