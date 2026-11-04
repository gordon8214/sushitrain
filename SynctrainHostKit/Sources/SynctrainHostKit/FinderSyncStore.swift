// Copyright (C) 2026 Gordon
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this file,
// You can obtain one at https://mozilla.org/MPL/2.0/.
import Foundation
import Dispatch
#if os(macOS)
	import Darwin
#endif

/// Only metadata is exchanged here. Neither process shares an engine database or credentials.
public struct FinderSyncStore: Sendable {
	public let directory: URL
	public init(directory: URL) { self.directory = directory }
	public static func sharedDirectory(bundle: Bundle = .main) -> URL? {
		guard let group = bundle.object(forInfoDictionaryKey: "SynctrainAppGroup") as? String else { return nil }
		return FileManager.default.containerURL(forSecurityApplicationGroupIdentifier: group)?
			.appending(path: "Library/Application Support/FinderSync", directoryHint: .isDirectory)
	}
	public func prepare() throws {
		try FileManager.default.createDirectory(at: directory, withIntermediateDirectories: true)
	}
	public func read<Value: Decodable>(_ type: Value.Type, name: String) -> Value? {
		let url = directory.appendingPathComponent(name)
		guard let size = try? url.resourceValues(forKeys: [.fileSizeKey]).fileSize, size <= 4 * 1024 * 1024,
			let data = try? Data(contentsOf: url)
		else { return nil }
		return try? JSONDecoder().decode(type, from: data)
	}
	public func write<Value: Encodable>(_ value: Value, name: String) throws {
		try JSONEncoder().encode(value).write(to: directory.appendingPathComponent(name), options: .atomic)
	}
	public func remove(name: String) { try? FileManager.default.removeItem(at: directory.appendingPathComponent(name)) }
	public static func requestName(_ instance: UUID) -> String { "request-\(instance.uuidString).json" }
	public static func responseName(_ instance: UUID) -> String { "response-\(instance.uuidString).json" }
	public func demands(now: Date = Date()) -> [FinderDemand] {
		let files = (try? FileManager.default.contentsOfDirectory(at: directory, includingPropertiesForKeys: nil)) ?? []
		var demands: [FinderDemand] = []
		for file in files where file.lastPathComponent.hasPrefix("request-") {
			guard let demand = read(FinderDemand.self, name: file.lastPathComponent),
				Self.requestName(demand.instance) == file.lastPathComponent
			else { continue }
			if demand.isFresh(at: now) {
				demands.append(demand)
			}
			else {
				remove(name: file.lastPathComponent)
				remove(name: Self.responseName(demand.instance))
			}
		}
		return demands
	}
}

/// Request removal must follow all older writes, including writes still waiting for disk I/O.
public final class FinderDemandWriter: Sendable {
	private let queue = DispatchQueue(label: "Synctrain.FinderDemandWriter", qos: .utility)
	private let operation: @Sendable (FinderDemand) -> Void
	public convenience init(store: FinderSyncStore) {
		self.init { demand in
			if demand.items.isEmpty {
				store.remove(name: FinderSyncStore.requestName(demand.instance))
				store.remove(name: FinderSyncStore.responseName(demand.instance))
			}
			else {
				try? store.write(demand, name: FinderSyncStore.requestName(demand.instance))
			}
		}
	}
	init(operation: @escaping @Sendable (FinderDemand) -> Void) { self.operation = operation }
	public func publish(_ demand: FinderDemand) {
		queue.async { self.operation(demand) }
	}
	public func waitUntilIdle() async {
		await withCheckedContinuation { continuation in queue.async { continuation.resume() } }
	}
}

/// Coalesce notifications without dropping a change that arrives during an asynchronous read.
@MainActor public final class FinderReloadCoordinator {
	private var task: Task<Void, Never>?
	private var pending = false
	public init() {}
	public func request(_ operation: @escaping @MainActor @Sendable () async -> Void) {
		pending = true
		guard task == nil else { return }
		task = Task {
			repeat {
				pending = false
				await operation()
			} while pending
			task = nil
		}
	}
}

#if os(macOS)
	/// Watch the directory because atomic writes replace the file's inode.
	public final class FinderDirectoryWatcher: @unchecked Sendable {
		private let source: DispatchSourceFileSystemObject
		public init(directory: URL, changed: @escaping @Sendable () -> Void) throws {
			let descriptor = open(directory.path, O_EVTONLY)
			guard descriptor >= 0 else { throw CocoaError(.fileReadUnknown) }
			source = DispatchSource.makeFileSystemObjectSource(
				fileDescriptor: descriptor, eventMask: [.write, .rename, .delete], queue: .global(qos: .utility))
			source.setEventHandler(handler: changed)
			source.setCancelHandler { close(descriptor) }
			source.resume()
		}
		deinit { source.cancel() }
	}
#endif
