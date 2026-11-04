// Copyright (C) 2026 Gordon
// SPDX-License-Identifier: MPL-2.0
#if os(macOS)
	import Foundation
	import Darwin
	import os

	public final class HistoryBridgeServer: @unchecked Sendable {
		public typealias Handler =
			@Sendable (HistoryBridgeRequest, @escaping @Sendable (HistoryBridgeResponse) throws -> Void) async throws -> Void
		private let listener: Int32
		private let socketPath: String
		private let state = OSAllocatedUnfairLock(initialState: false)
		public init(directory: URL = HistoryBridgeLocation.directory, handler: @escaping Handler) throws {
			try FileManager.default.createDirectory(
				at: directory, withIntermediateDirectories: true, attributes: [.posixPermissions: 0o700])
			let attributes = try FileManager.default.attributesOfItem(atPath: directory.path)
			guard (attributes[.ownerAccountID] as? NSNumber)?.uint32Value == getuid(),
				(attributes[.type] as? FileAttributeType) == .typeDirectory
			else { throw HistoryBridgeError.message("The history service directory is unsafe.") }
			try FileManager.default.setAttributes([.posixPermissions: 0o700], ofItemAtPath: directory.path)
			socketPath = directory.appending(path: "service.sock").path
			listener = socket(AF_UNIX, SOCK_STREAM, 0)
			guard listener >= 0 else { throw HistoryBridgeError.message("Cannot create the history listener.") }
			// The host has one instance; remove only its own stale socket, never another file.
			var status = stat()
			if lstat(socketPath, &status) == 0 {
				guard status.st_mode & S_IFMT == S_IFSOCK, status.st_uid == getuid() else {
					Darwin.close(listener)
					throw HistoryBridgeError.message("The history socket path is occupied.")
				}
				unlink(socketPath)
			}
			var address = try historySocketAddress(socketPath)
			let result = withUnsafePointer(to: &address) { pointer in
				pointer.withMemoryRebound(to: sockaddr.self, capacity: 1) {
					Darwin.bind(listener, $0, socklen_t(MemoryLayout<sockaddr_un>.size))
				}
			}
			guard result == 0, chmod(socketPath, 0o600) == 0, listen(listener, 8) == 0 else {
				Darwin.close(listener)
				throw HistoryBridgeError.message("Cannot start the history listener.")
			}
			DispatchQueue(label: "Synctrain.HistoryBridge.accept").async { [self] in
				while !state.withLock({ $0 }) {
					let descriptor = accept(listener, nil, nil)
					if descriptor < 0 {
						if errno == EINTR { continue }
						break
					}
					let channel = HistoryBridgeChannel(descriptor)
					let output = HistoryBridgeOutput(channel: channel)
					Task.detached {
						do {
							try channel.verify(identifier: HistoryBridgeLocation.clientIdentifier)
							let request = try channel.read(HistoryBridgeRequest.self)
							guard request.protocolVersion == 1 else {
								throw HistoryBridgeError.message("Update both apps to use this history service.")
							}
							let task = Task { try await handler(request) { try output.send($0) } }
							let monitor = DispatchSource.makeReadSource(fileDescriptor: descriptor, queue: .global(qos: .utility))
							monitor.setEventHandler {
								var byte: UInt8 = 0
								if recv(descriptor, &byte, 1, MSG_PEEK | MSG_DONTWAIT) == 0 { task.cancel() }
							}
							monitor.resume()
							defer { monitor.cancel() }
							try await task.value
							try output.send(HistoryBridgeResponse(event: "finished"))
						}
						catch {
							var response = HistoryBridgeResponse(event: "error")
							response.message = error.localizedDescription
							try? output.send(response)
						}
						try? await output.flush()
					}
				}
			}
		}
		public func stop() {
			if !state.withLock({ value in
				let old = value
				value = true
				return old
			}) {
				Darwin.shutdown(listener, SHUT_RDWR)
				Darwin.close(listener)
				unlink(socketPath)
			}
		}
	}

	final class HistoryBridgeOutput: @unchecked Sendable {
		private let channel: HistoryBridgeChannel
		private let queue = DispatchQueue(label: "Synctrain.HistoryBridge.output", qos: .utility)
		private let failure = OSAllocatedUnfairLock<(any Error)?>(initialState: nil)
		init(channel: HistoryBridgeChannel) { self.channel = channel }
		func send(_ response: HistoryBridgeResponse) throws {
			if let error = failure.withLock({ $0 }) { throw error }
			queue.async { [self] in
				do { try channel.write(response) }
				catch {
					failure.withLock { $0 = error }
					channel.cancel()
				}
			}
		}
		func flush() async throws {
			await withCheckedContinuation { continuation in queue.async { continuation.resume() } }
			if let error = failure.withLock({ $0 }) { throw error }
		}
	}
#endif
