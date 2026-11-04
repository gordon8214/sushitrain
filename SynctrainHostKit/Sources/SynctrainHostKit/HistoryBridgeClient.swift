// Copyright (C) 2026 Gordon
// SPDX-License-Identifier: MPL-2.0
#if os(macOS)
	import Foundation
	import Darwin

	public struct HistoryBridgeClient: Sendable {
		public let directory: URL
		public init(directory: URL = HistoryBridgeLocation.directory) { self.directory = directory }
		public func request(_ request: HistoryBridgeRequest) -> AsyncThrowingStream<HistoryBridgeResponse, any Error> {
			AsyncThrowingStream { continuation in
				let descriptor = socket(AF_UNIX, SOCK_STREAM, 0)
				guard descriptor >= 0 else {
					continuation.finish(throwing: HistoryBridgeError.message("Cannot create a history connection."))
					return
				}
				let channel = HistoryBridgeChannel(descriptor)
				let task = Task.detached {
					do {
						var address = try historySocketAddress(directory.appending(path: "service.sock").path)
						let result = withUnsafePointer(to: &address) { pointer in
							pointer.withMemoryRebound(to: sockaddr.self, capacity: 1) {
								Darwin.connect(descriptor, $0, socklen_t(MemoryLayout<sockaddr_un>.size))
							}
						}
						guard result == 0 else {
							throw HistoryBridgeError.message(
								"SushiTrain's history service is unavailable. Update or launch Synctrain, then retry.")
						}
						try channel.verify(identifier: HistoryBridgeLocation.appIdentifier)
						try channel.write(request)
						while true {
							try Task.checkCancellation()
							let response = try channel.read(HistoryBridgeResponse.self)
							guard response.protocolVersion == 1 else {
								throw HistoryBridgeError.message("Update both apps to use this history service.")
							}
							if response.event == "error" {
								throw HistoryBridgeError.requestFailed(response.message ?? "The history request failed.")
							}
							if response.event == "finished" { break }
							continuation.yield(response)
						}
						continuation.finish()
					}
					catch { continuation.finish(throwing: error) }
				}
				continuation.onTermination = { _ in
					task.cancel()
					channel.cancel()
				}
			}
		}
	}
#endif
