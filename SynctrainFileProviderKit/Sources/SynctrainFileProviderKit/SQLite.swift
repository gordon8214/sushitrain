// Copyright (C) 2026 Gordon
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this file,
// You can obtain one at https://mozilla.org/MPL/2.0/.
import Foundation
import SQLite3

public enum IndexError: Error, Sendable, Equatable {
	/// SQLite reported an error
	case sqlite(code: Int32, message: String)
	/// The index cannot be opened, e.g. because the device has not been unlocked since it booted
	case unavailable(String)
	/// The index was created by a newer version of the app
	case unsupportedSchema(Int)
	/// An item with this identifier does not exist
	case notFound
	/// A live item already exists with a name that cannot coexist with the requested one
	case nameCollision(existing: Int64)
	/// The requested operation is not allowed for the item
	case notAllowed(String)
	/// The sync anchor is from another generation of the index, or changes since it have been pruned
	case anchorExpired
}

enum SQLValue {
	case integer(Int64)
	case text(String)
	case real(Double)
	case null
}

protocol SQLBindable {
	var sqlValue: SQLValue { get }
}

extension Int64: SQLBindable {
	var sqlValue: SQLValue { .integer(self) }
}

extension Int: SQLBindable {
	var sqlValue: SQLValue { .integer(Int64(self)) }
}

extension Bool: SQLBindable {
	var sqlValue: SQLValue { .integer(self ? 1 : 0) }
}

extension String: SQLBindable {
	var sqlValue: SQLValue { .text(self) }
}

extension Double: SQLBindable {
	var sqlValue: SQLValue { .real(self) }
}

extension Optional: SQLBindable where Wrapped: SQLBindable {
	var sqlValue: SQLValue {
		switch self {
		case .none: return .null
		case .some(let value): return value.sqlValue
		}
	}
}

/// A single SQLite connection. Connections are short-lived and used from one thread at a time: the index is opened for
/// each operation and closed right after, so neither process holds a lock on a shared file while it may be suspended.
final class SQLiteConnection {
	private let handle: OpaquePointer
	private var statements: [String: SQLiteStatement] = [:]

	init(url: URL) throws(IndexError) {
		var db: OpaquePointer?
		let flags = SQLITE_OPEN_READWRITE | SQLITE_OPEN_CREATE | SQLITE_OPEN_NOMUTEX
		let result = sqlite3_open_v2(url.path(percentEncoded: false), &db, flags, nil)
		guard result == SQLITE_OK, let db else {
			let message = db.map { String(cString: sqlite3_errmsg($0)) } ?? "could not open database"
			sqlite3_close_v2(db)
			if result == SQLITE_CANTOPEN || result == SQLITE_PERM || result == SQLITE_AUTH {
				throw .unavailable(message)
			}
			throw .sqlite(code: result, message: message)
		}
		self.handle = db

		// Wait for the other process's locks before anything else: setting the journal mode needs a lock as well
		sqlite3_busy_timeout(db, 2500)
		// Rollback journal mode: unlike WAL, no locks are held while no transaction is active
		try execute("PRAGMA journal_mode=DELETE")
		// A small cache for reads. A write transaction whose dirty pages exceed the spill threshold writes them to the
		// database file early, which takes the exclusive lock until it commits and locks the other process out. The
		// app's batches stay well below the threshold. Beyond it (a huge removal in the extension) bounding memory
		// matters more: without a limit (cache_spill=OFF), or with SQLite's default on Apple platforms (20,000 pages),
		// such a transaction could grow far past the extension's memory limit.
		try execute("PRAGMA cache_size=-256")
		try execute("PRAGMA cache_spill=\(Self.spillThresholdPages)")
		try execute("PRAGMA mmap_size=0")
		try execute("PRAGMA temp_store=MEMORY")
	}

	/// The number of dirty pages (4 MiB) a write transaction may hold in memory before it spills them to the file
	static let spillThresholdPages = 1024

	deinit {
		statements.removeAll()
		sqlite3_close_v2(handle)
	}

	private func error(_ code: Int32) -> IndexError {
		.sqlite(code: code, message: String(cString: sqlite3_errmsg(handle)))
	}

	func execute(_ sql: String) throws(IndexError) {
		let result = sqlite3_exec(handle, sql, nil, nil, nil)
		if result != SQLITE_OK && result != SQLITE_ROW {
			throw error(result)
		}
	}

	/// Returns a cached prepared statement, reset and bound to the given values
	func statement(_ sql: String, _ values: [any SQLBindable] = []) throws(IndexError) -> SQLiteStatement {
		let stmt: SQLiteStatement
		if let cached = statements[sql] {
			stmt = cached
			stmt.reset()
		}
		else {
			var raw: OpaquePointer?
			let result = sqlite3_prepare_v2(handle, sql, -1, &raw, nil)
			guard result == SQLITE_OK, let raw else {
				throw error(result)
			}
			stmt = SQLiteStatement(handle: raw, connection: handle)
			statements[sql] = stmt
		}
		try stmt.bind(values)
		return stmt
	}

	func run(_ sql: String, _ values: [any SQLBindable] = []) throws(IndexError) {
		let stmt = try statement(sql, values)
		while try stmt.step() {}
	}

	func query<T>(_ sql: String, _ values: [any SQLBindable] = [], _ row: (SQLiteStatement) -> T) throws(IndexError)
		-> [T]
	{
		let stmt = try statement(sql, values)
		var results: [T] = []
		while try stmt.step() {
			results.append(row(stmt))
		}
		return results
	}

	func queryOne<T>(_ sql: String, _ values: [any SQLBindable] = [], _ row: (SQLiteStatement) -> T) throws(IndexError)
		-> T?
	{
		let stmt = try statement(sql, values)
		guard try stmt.step() else {
			return nil
		}
		let result = row(stmt)
		stmt.reset()
		return result
	}

	var changes: Int {
		Int(sqlite3_changes(handle))
	}

	var lastInsertRowID: Int64 {
		sqlite3_last_insert_rowid(handle)
	}

	var userVersion: Int {
		get throws(IndexError) {
			try queryOne("PRAGMA user_version") { Int($0.int64(0)) } ?? 0
		}
	}

	func setUserVersion(_ version: Int) throws(IndexError) {
		try execute("PRAGMA user_version=\(version)")
	}

	/// Runs the block in a write transaction. BEGIN IMMEDIATE takes the write lock up front, so two processes never
	/// deadlock trying to upgrade read locks.
	func transaction<T>(_ block: () throws(IndexError) -> T) throws(IndexError) -> T {
		try transaction(begin: "BEGIN IMMEDIATE", block)
	}

	/// Runs the block in a read transaction, so that all of its queries see the same state of the database
	func readTransaction<T>(_ block: () throws(IndexError) -> T) throws(IndexError) -> T {
		try transaction(begin: "BEGIN DEFERRED", block)
	}

	private func transaction<T>(begin: String, _ block: () throws(IndexError) -> T) throws(IndexError) -> T {
		try execute(begin)
		do {
			let result = try block()
			try execute("COMMIT")
			return result
		}
		catch {
			try? execute("ROLLBACK")
			throw error
		}
	}
}

final class SQLiteStatement {
	private let handle: OpaquePointer
	private let connection: OpaquePointer

	init(handle: OpaquePointer, connection: OpaquePointer) {
		self.handle = handle
		self.connection = connection
	}

	deinit {
		sqlite3_finalize(handle)
	}

	func reset() {
		sqlite3_reset(handle)
		sqlite3_clear_bindings(handle)
	}

	func bind(_ values: [any SQLBindable]) throws(IndexError) {
		// SQLITE_TRANSIENT makes SQLite copy the string, which must not outlive this call
		let transient = unsafeBitCast(-1, to: sqlite3_destructor_type.self)
		for (index, value) in values.enumerated() {
			let position = Int32(index + 1)
			let result: Int32
			switch value.sqlValue {
			case .integer(let int): result = sqlite3_bind_int64(handle, position, int)
			case .text(let text): result = Self.bind(text, to: handle, at: position, destructor: transient)
			case .real(let real): result = sqlite3_bind_double(handle, position, real)
			case .null: result = sqlite3_bind_null(handle, position)
			}
			if result != SQLITE_OK {
				throw .sqlite(code: result, message: String(cString: sqlite3_errmsg(connection)))
			}
		}
	}

	/// Binds the text with its length in bytes, so that names containing NUL are not cut short
	private static func bind(
		_ text: String, to handle: OpaquePointer, at position: Int32, destructor: sqlite3_destructor_type
	) -> Int32 {
		var text = text
		return text.withUTF8 { bytes in
			guard let base = bytes.baseAddress, !bytes.isEmpty else {
				return sqlite3_bind_text(handle, position, "", 0, destructor)
			}
			return base.withMemoryRebound(to: CChar.self, capacity: bytes.count) { chars in
				sqlite3_bind_text(handle, position, chars, Int32(bytes.count), destructor)
			}
		}
	}

	/// Returns true when a row is available
	func step() throws(IndexError) -> Bool {
		let result = sqlite3_step(handle)
		switch result {
		case SQLITE_ROW: return true
		case SQLITE_DONE: return false
		default: throw .sqlite(code: result, message: String(cString: sqlite3_errmsg(connection)))
		}
	}

	func int64(_ column: Int32) -> Int64 {
		sqlite3_column_int64(handle, column)
	}

	func bool(_ column: Int32) -> Bool {
		sqlite3_column_int64(handle, column) != 0
	}

	func double(_ column: Int32) -> Double {
		sqlite3_column_double(handle, column)
	}

	func isNull(_ column: Int32) -> Bool {
		sqlite3_column_type(handle, column) == SQLITE_NULL
	}

	func text(_ column: Int32) -> String {
		guard let raw = sqlite3_column_text(handle, column) else {
			return ""
		}
		// The length in bytes (asked for after the text, as SQLite recommends), so that NUL does not end the text
		let count = Int(sqlite3_column_bytes(handle, column))
		return String(decoding: UnsafeBufferPointer(start: raw, count: count), as: UTF8.self)
	}

	func optionalText(_ column: Int32) -> String? {
		isNull(column) ? nil : text(column)
	}

	func optionalInt64(_ column: Int32) -> Int64? {
		isNull(column) ? nil : int64(column)
	}
}
