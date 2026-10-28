// Copyright (C) 2026 Gordon
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this file,
// You can obtain one at https://mozilla.org/MPL/2.0/.
import Foundation
import os

/// What went wrong talking to a gateway
public enum RemoteError: Error, Sendable, Equatable {
	/// Missing, expired or revoked credential (`401`)
	case unauthenticated
	/// Not permitted (`403`), with the gateway's code (`forbidden`, `folder_read_only`, `reserved_name`…)
	case forbidden(code: String)
	/// No such item, folder, lease, upload or operation (`404`)
	case notFound
	/// The name is taken (`409 name_conflict`), with the item that has it when the gateway says
	case nameConflict(current: RemoteItem?)
	/// The base revision or subtree generation is not current (`412`), with the current item
	case revisionMismatch(current: RemoteItem?)
	/// The operation ID was used with another payload (`422`)
	case operationMismatch
	/// The change cursor can no longer be served (`410 cursor_expired`): take a fresh snapshot
	case cursorExpired
	/// The snapshot session expired (`410 snapshot_expired`)
	case snapshotExpired
	/// The read lease expired (`410 lease_expired`): request a new one
	case leaseExpired
	/// The content is temporarily unavailable (`503`)
	case unavailable(retryAfter: TimeInterval?)
	/// The gateway sheds load (`429`)
	case overloaded(retryAfter: TimeInterval?)
	/// Storage or quota exhausted on the gateway (`507`)
	case insufficientStorage
	/// The gateway could not be reached, or its certificate was not trusted (`.serverCertificateUntrusted`)
	case unreachable(URLError.Code)
	/// The gateway answered in a way the contract does not allow (a redirect, another origin, an ignored range…)
	case protocolViolation(String)
	/// Another status, with the gateway's code
	case server(status: Int, code: String?)
	/// The local index failed
	case store(IndexError)
	/// A local file could not be read or written
	case fileSystem(String)
	/// The task was canceled
	case canceled

	/// Whether trying again later may succeed without any other change
	public var isTransient: Bool {
		switch self {
		case .unavailable, .overloaded, .unreachable, .insufficientStorage: return true
		case .server(let status, _): return status >= 500
		default: return false
		}
	}

	/// How long to wait before trying again, if the gateway said
	public var retryAfter: TimeInterval? {
		switch self {
		case .unavailable(let after), .overloaded(let after): return after
		default: return nil
		}
	}
}

/// A response with content (a range or a whole file), downloaded to a file
public struct ContentResponse: Sendable, Hashable {
	public var status: Int
	public var entityTag: String?
	/// The first byte of a `206` response
	public var rangeStart: Int64?
	/// The length of the whole content, from `Content-Range`
	public var completeLength: Int64?
	/// The number of bytes in the file
	public var receivedLength: Int64
}

/// An asynchronous client of version 1 of a gateway's remote file API. One client (and its session) per gateway, shared
/// by everything that talks to it, so that parallel requests share connections. Credentials are sent in the
/// `Authorization` header only; redirects to another origin are refused, and a response from another origin is a
/// protocol violation. Binary content is always downloaded to files and uploaded from files.
public final class RemoteFileClient: Sendable {
	public let endpoint: RemoteEndpoint
	let session: URLSession
	private let credentials: any RemoteCredentialStore
	private let sessionDelegate: RemoteTrustDelegate
	/// The largest JSON body accepted
	let maxControlBodyBytes: Int

	/// The correlation ID sent with the requests of the current task (`Synctrain-Correlation-ID`). Without one, every
	/// request gets its own.
	@TaskLocal public static var correlationID: String?

	public static let correlationHeader = "Synctrain-Correlation-ID"
	public static let tusVersion = "1.0.0"

	/// - Parameter configuration: the session's configuration (by default `makeConfiguration()`); tests pass one
	///   with their own protocol classes
	public init(
		endpoint: RemoteEndpoint, credentials: any RemoteCredentialStore,
		configuration: URLSessionConfiguration? = nil, maxControlBodyBytes: Int = 4 << 20
	) {
		self.endpoint = endpoint
		self.credentials = credentials
		self.maxControlBodyBytes = maxControlBodyBytes
		sessionDelegate = RemoteTrustDelegate(endpoint: endpoint)
		session = URLSession(
			configuration: configuration ?? Self.makeConfiguration(), delegate: sessionDelegate, delegateQueue: nil)
	}

	deinit {
		session.finishTasksAndInvalidate()
	}

	/// A foreground configuration: no cookies, no cache, connections shared by the transfers to one gateway
	public static func makeConfiguration() -> URLSessionConfiguration {
		let configuration = URLSessionConfiguration.ephemeral
		configuration.urlCache = nil
		configuration.requestCachePolicy = .reloadIgnoringLocalCacheData
		configuration.httpCookieStorage = nil
		configuration.httpShouldSetCookies = false
		configuration.httpMaximumConnectionsPerHost = 6
		configuration.timeoutIntervalForRequest = 60
		configuration.waitsForConnectivity = false
		return configuration
	}

	// MARK: - Requests

	enum Authorization: Sendable {
		/// The client credential of the gateway
		case credential
		/// A scoped token (of a lease, an upload or a pairing request)
		case token(String)
		case none
	}

	/// The URL of a path below the endpoint, every segment percent-encoded
	func url(_ segments: [String], query: [URLQueryItem] = []) -> URL {
		var allowed = CharacterSet.alphanumerics.intersection(
			CharacterSet(charactersIn: Unicode.Scalar(0)...Unicode.Scalar(127)))
		allowed.insert(charactersIn: "-._~")
		var components = URLComponents(url: endpoint.baseURL, resolvingAgainstBaseURL: false) ?? URLComponents()
		components.percentEncodedPath =
			"/" + segments.map { $0.addingPercentEncoding(withAllowedCharacters: allowed) ?? "" }.joined(separator: "/")
		if !query.isEmpty {
			components.percentEncodedQuery = query.compactMap { item in
				guard let value = item.value else {
					return nil
				}
				let name = item.name.addingPercentEncoding(withAllowedCharacters: allowed) ?? ""
				return name + "=" + (value.addingPercentEncoding(withAllowedCharacters: allowed) ?? "")
			}.joined(separator: "&")
		}
		return components.url ?? endpoint.baseURL
	}

	func request(
		_ method: String, _ url: URL, authorization: Authorization = .credential, timeout: TimeInterval? = nil
	) throws(RemoteError) -> URLRequest {
		var request = URLRequest(url: url)
		request.httpMethod = method
		request.cachePolicy = .reloadIgnoringLocalCacheData
		if let timeout {
			request.timeoutInterval = timeout
		}
		request.setValue(Self.correlationID ?? Self.newCorrelationID(), forHTTPHeaderField: Self.correlationHeader)
		switch authorization {
		case .credential:
			let credential: String?
			do {
				credential = try credentials.credential(serviceID: endpoint.serviceID)
			}
			catch {
				// The Keychain could not be read now (for instance before the first unlock): the credential is still
				// there, so this is worth trying again rather than pairing anew
				throw .unavailable(retryAfter: 10)
			}
			guard let credential else {
				throw .unauthenticated
			}
			request.setValue("Bearer \(credential)", forHTTPHeaderField: "Authorization")
		case .token(let token):
			request.setValue("Bearer \(token)", forHTTPHeaderField: "Authorization")
		case .none:
			break
		}
		return request
	}

	static func newCorrelationID() -> String {
		UUID().uuidString.replacingOccurrences(of: "-", with: "").lowercased().prefix(16).description
	}

	/// Sends a request whose answer is a bounded body, and checks its origin. Errors are thrown for statuses of `400` and
	/// above.
	func send(_ request: URLRequest, gone: RemoteError? = nil) async throws(RemoteError) -> (Data, HTTPURLResponse) {
		let delegate = BoundedBodyDelegate(trust: RemoteTrustDelegate(endpoint: endpoint), limit: maxControlBodyBytes)
		let body: Data
		let response: URLResponse
		do {
			(body, response) = try await delegate.load(request, in: session)
		}
		catch {
			throw Self.map(error, delegate: delegate.trust)
		}
		let http = try checked(response)
		if http.statusCode >= 400 {
			throw Self.error(status: http.statusCode, body: body, response: http, gone: gone)
		}
		return (body, http)
	}

	/// The response as HTTP, from this endpoint's origin
	private func checked(_ response: URLResponse) throws(RemoteError) -> HTTPURLResponse {
		guard let http = response as? HTTPURLResponse else {
			throw .protocolViolation("not an HTTP response")
		}
		guard endpoint.hasOrigin(of: http.url) else {
			throw .protocolViolation("a response came from another origin")
		}
		if (300..<400).contains(http.statusCode) {
			throw .protocolViolation("the gateway redirected a request")
		}
		return http
	}

	func decode<T: Decodable>(_ type: T.Type, from data: Data) throws(RemoteError) -> T {
		do {
			return try RemoteJSON.decoder().decode(T.self, from: data)
		}
		catch {
			throw .protocolViolation("invalid \(T.self): \(error)")
		}
	}

	func json<T: Decodable>(_ type: T.Type, _ request: URLRequest, gone: RemoteError? = nil) async throws(RemoteError)
		-> T
	{
		let (data, _) = try await send(request, gone: gone)
		return try decode(T.self, from: data)
	}

	static func map(_ error: any Error, delegate: RemoteTrustDelegate?) -> RemoteError {
		if let error = error as? RemoteError {
			return error
		}
		if delegate?.rejectedTrust == true {
			return .unreachable(.serverCertificateUntrusted)
		}
		if error is CancellationError {
			return .canceled
		}
		if let error = error as? URLError {
			return error.code == .cancelled ? .canceled : .unreachable(error.code)
		}
		return .fileSystem(error.localizedDescription)
	}

	/// The error for a status of `400` or above
	static func error(status: Int, body: Data?, response: HTTPURLResponse?, gone: RemoteError? = nil) -> RemoteError {
		let detail = body.flatMap { try? RemoteJSON.decoder().decode(ErrorBody.self, from: $0) }?.error
		let retryAfter =
			response?.value(forHTTPHeaderField: "Retry-After").flatMap(Self.parseRetryAfter)
			?? detail?.retryAfter.map(TimeInterval.init)
		let code = detail?.code
		switch status {
		case 401: return .unauthenticated
		case 403: return .forbidden(code: code ?? RemoteErrorCode.forbidden)
		case 404: return .notFound
		case 409 where code == RemoteErrorCode.nameConflict: return .nameConflict(current: detail?.item)
		case 410:
			switch code {
			case RemoteErrorCode.cursorExpired: return .cursorExpired
			case RemoteErrorCode.snapshotExpired: return .snapshotExpired
			case RemoteErrorCode.leaseExpired: return .leaseExpired
			case nil: return gone ?? .server(status: status, code: nil)
			default: return .server(status: status, code: code)
			}
		case 412: return .revisionMismatch(current: detail?.item)
		case 422: return .operationMismatch
		case 429: return .overloaded(retryAfter: retryAfter)
		case 503: return .unavailable(retryAfter: retryAfter)
		case 507: return .insufficientStorage
		default: return .server(status: status, code: code)
		}
	}

	static func parseRetryAfter(_ value: String) -> TimeInterval? {
		if let seconds = TimeInterval(value.trimmingCharacters(in: .whitespaces)) {
			return max(0, seconds)
		}
		let formatter = DateFormatter()
		formatter.locale = Locale(identifier: "en_US_POSIX")
		formatter.timeZone = TimeZone(identifier: "GMT")
		formatter.dateFormat = "EEE, dd MMM yyyy HH:mm:ss zzz"
		return formatter.date(from: value).map { max(0, $0.timeIntervalSinceNow) }
	}

	// MARK: - Service

	public func capabilities() async throws(RemoteError) -> Capabilities {
		try await json(Capabilities.self, try request("GET", url(["v1", "capabilities"])))
	}

	public func folders() async throws(RemoteError) -> [RemoteFolder] {
		try await json(RemoteFolderList.self, try request("GET", url(["v1", "folders"]))).folders
	}

	// MARK: - Metadata

	/// A page of a snapshot: the first page of a new session without `snapshot`, else the page of the session
	public func snapshotPage(folderID: String, snapshot: String?, page: String?, limit: Int? = nil)
		async throws(RemoteError) -> SnapshotPage
	{
		var query: [URLQueryItem] = []
		if let snapshot {
			query.append(URLQueryItem(name: "snapshot", value: snapshot))
		}
		if let page {
			query.append(URLQueryItem(name: "page", value: page))
		}
		if let limit {
			query.append(URLQueryItem(name: "limit", value: String(limit)))
		}
		return try await json(
			SnapshotPage.self, try request("GET", url(["v1", "folders", folderID, "snapshot"], query: query)))
	}

	/// Changes after the cursor; with `wait`, held up to that many seconds until a change arrives
	public func changes(folderID: String, cursor: String, limit: Int? = nil, wait: Int? = nil)
		async throws(RemoteError) -> ChangesPage
	{
		var query = [URLQueryItem(name: "cursor", value: cursor)]
		if let limit {
			query.append(URLQueryItem(name: "limit", value: String(limit)))
		}
		if let wait, wait > 0 {
			query.append(URLQueryItem(name: "wait", value: String(wait)))
		}
		let timeout = wait.map { TimeInterval($0) + 30 }
		return try await json(
			ChangesPage.self,
			try request("GET", url(["v1", "folders", folderID, "changes"], query: query), timeout: timeout))
	}

	public func item(id: String) async throws(RemoteError) -> RemoteItem {
		try await json(RemoteItem.self, try request("GET", url(["v1", "items", id])))
	}

	// MARK: - Content

	/// Creates (or, repeated, returns with its current preparation progress) a read lease for the item's revision
	public func createReadLease(itemID: String, revision: RemoteRevision) async throws(RemoteError) -> ReadLease {
		var request = try request("POST", url(["v1", "items", itemID, "read-leases"]))
		request.setValue("application/json", forHTTPHeaderField: "Content-Type")
		request.httpBody = Data("{\"revision\":\(Self.jsonString(revision.rawValue))}".utf8)
		return try await json(ReadLease.self, request, gone: .leaseExpired)
	}

	/// A page of the lease's block manifest (with the lease's read token when given, else the credential)
	public func manifestPage(leaseID: String, from: Int, limit: Int? = nil, token: String? = nil)
		async throws(RemoteError) -> ManifestPage
	{
		var query = [URLQueryItem(name: "from", value: String(from))]
		if let limit {
			query.append(URLQueryItem(name: "limit", value: String(limit)))
		}
		return try await json(
			ManifestPage.self,
			try request(
				"GET", url(["v1", "content", leaseID, "manifest"], query: query),
				authorization: token.map { .token($0) } ?? .credential),
			gone: .leaseExpired)
	}

	/// A request for the lease's content (for this client or a background session)
	public func contentRequest(
		leaseID: String, range: ClosedRange<Int64>?, openEnded start: Int64? = nil, ifRange: String?,
		token: String?
	) throws(RemoteError) -> URLRequest {
		var request = try request(
			"GET", url(["v1", "content", leaseID]), authorization: token.map { .token($0) } ?? .credential,
			timeout: 120)
		if let range {
			request.setValue("bytes=\(range.lowerBound)-\(range.upperBound)", forHTTPHeaderField: "Range")
		}
		else if let start {
			request.setValue("bytes=\(start)-", forHTTPHeaderField: "Range")
		}
		if let ifRange {
			request.setValue(ifRange, forHTTPHeaderField: "If-Range")
		}
		return request
	}

	/// Downloads content (a byte range, or all of it) into the destination file, which is replaced. A request for a range
	/// answered with the whole content (`200`) is a protocol violation: its bytes are discarded, never appended.
	public func downloadContent(
		leaseID: String, range: ClosedRange<Int64>?, ifRange: String?, token: String? = nil, to destination: URL
	) async throws(RemoteError) -> ContentResponse {
		let request = try contentRequest(leaseID: leaseID, range: range, ifRange: ifRange, token: token)
		let response = try await download(request, to: destination, gone: .leaseExpired)
		if range != nil, response.status != 206 {
			try? FileManager.default.removeItem(at: destination)
			throw .protocolViolation("the gateway answered a range request with status \(response.status)")
		}
		if let range, response.rangeStart != range.lowerBound {
			try? FileManager.default.removeItem(at: destination)
			throw .protocolViolation("the gateway answered with another range")
		}
		return response
	}

	/// The thumbnail of an exact content revision, as a JPEG file
	public func thumbnail(itemID: String, revision: RemoteRevision, size: Int = 256, to destination: URL)
		async throws(RemoteError)
	{
		let query = [
			URLQueryItem(name: "revision", value: revision.rawValue), URLQueryItem(name: "size", value: String(size)),
		]
		_ = try await download(
			try request("GET", url(["v1", "items", itemID, "thumbnail"], query: query)), to: destination)
	}

	/// Runs a download task into the destination. Error bodies (bounded) are read from the file and mapped.
	func download(_ request: URLRequest, to destination: URL, gone: RemoteError? = nil) async throws(RemoteError)
		-> ContentResponse
	{
		let delegate = RemoteTrustDelegate(endpoint: endpoint)
		let location: URL
		let response: URLResponse
		do {
			(location, response) = try await session.download(for: request, delegate: delegate)
		}
		catch {
			throw Self.map(error, delegate: delegate)
		}
		defer { try? FileManager.default.removeItem(at: location) }
		let http: HTTPURLResponse
		do {
			http = try checked(response)
		}
		catch {
			throw error
		}
		if http.statusCode >= 400 {
			let body = Self.boundedContents(of: location, limit: maxControlBodyBytes)
			throw Self.error(status: http.statusCode, body: body, response: http, gone: gone)
		}
		do {
			try? FileManager.default.removeItem(at: destination)
			try FileManager.default.moveItem(at: location, to: destination)
		}
		catch {
			throw .fileSystem("could not keep a download: \(error.localizedDescription)")
		}
		let received =
			((try? FileManager.default.attributesOfItem(atPath: destination.path(percentEncoded: false)))?[.size]
			as? NSNumber)?.int64Value ?? 0
		let contentRange = http.value(forHTTPHeaderField: "Content-Range").flatMap(Self.parseContentRange)
		return ContentResponse(
			status: http.statusCode, entityTag: http.value(forHTTPHeaderField: "ETag"), rangeStart: contentRange?.start,
			completeLength: contentRange?.complete, receivedLength: received)
	}

	static func boundedContents(of url: URL, limit: Int) -> Data? {
		guard let handle = try? FileHandle(forReadingFrom: url) else {
			return nil
		}
		defer { try? handle.close() }
		return try? handle.read(upToCount: limit)
	}

	/// `bytes <start>-<end>/<complete or *>`
	static func parseContentRange(_ value: String) -> (start: Int64, end: Int64, complete: Int64?)? {
		let trimmed = value.trimmingCharacters(in: .whitespaces)
		guard trimmed.hasPrefix("bytes ") else {
			return nil
		}
		let parts = trimmed.dropFirst(6).split(separator: "/", maxSplits: 1)
		let bounds = parts.first?.split(separator: "-", maxSplits: 1) ?? []
		guard bounds.count == 2, let start = Int64(bounds[0]), let end = Int64(bounds[1]), start <= end else {
			return nil
		}
		let complete = parts.count == 2 ? Int64(parts[1]) : nil
		return (start, end, complete)
	}

	private static func jsonString(_ value: String) -> String {
		(try? JSONEncoder().encode(value)).map { String(decoding: $0, as: UTF8.self) } ?? "\"\""
	}

	// MARK: - Uploads (tus 1.0)

	public func uploadOptions() async throws(RemoteError) -> TusOptions {
		let (_, response) = try await send(try request("OPTIONS", url(["v1", "uploads"]), authorization: .none))
		func list(_ header: String) -> [String] {
			(response.value(forHTTPHeaderField: header) ?? "").split(separator: ",").map {
				$0.trimmingCharacters(in: .whitespaces)
			}.filter { !$0.isEmpty }
		}
		return TusOptions(
			versions: list("Tus-Version"), extensions: list("Tus-Extension"),
			maxSize: response.value(forHTTPHeaderField: "Tus-Max-Size").flatMap { Int64($0) },
			checksumAlgorithms: list("Tus-Checksum-Algorithm"))
	}

	/// Creates a staged upload for a file of the length whose whole-file SHA-256 (base64url) is the digest
	public func createUpload(folderID: String, length: Int64, digest: String) async throws(RemoteError) -> UploadInfo {
		var request = try request("POST", url(["v1", "uploads"]))
		request.setValue(Self.tusVersion, forHTTPHeaderField: "Tus-Resumable")
		request.setValue(String(length), forHTTPHeaderField: "Upload-Length")
		let metadata = [("folder", folderID), ("digest", digest)].map {
			"\($0.0) \(Data($0.1.utf8).base64EncodedString())"
		}.joined(separator: ",")
		request.setValue(metadata, forHTTPHeaderField: "Upload-Metadata")
		request.httpBody = Data()
		let (data, response) = try await send(request)
		if let info = try? RemoteJSON.decoder().decode(UploadInfo.self, from: data) {
			return info
		}
		// The same in headers, as tus describes it
		guard let location = response.value(forHTTPHeaderField: "Location"),
			let upload = location.split(separator: "/").last.map(String.init),
			let token = response.value(forHTTPHeaderField: "Synctrain-Upload-Token")
		else {
			throw .protocolViolation("the upload was created without its ID and token")
		}
		let expires =
			response.value(forHTTPHeaderField: "Upload-Expires").flatMap(Self.parseHTTPDate)
			?? Date().addingTimeInterval(
				7 * 86400)
		return UploadInfo(upload: upload, token: token, offset: 0, length: length, expires: expires)
	}

	/// The offset the gateway confirmed (`404`: unknown, terminated or expired)
	public func uploadStatus(uploadID: String, token: String? = nil) async throws(RemoteError) -> UploadStatus {
		var request = try request(
			"HEAD", url(["v1", "uploads", uploadID]), authorization: token.map { .token($0) } ?? .credential)
		request.setValue(Self.tusVersion, forHTTPHeaderField: "Tus-Resumable")
		let (_, response) = try await send(request)
		guard let offset = response.value(forHTTPHeaderField: "Upload-Offset").flatMap({ Int64($0) }) else {
			throw .protocolViolation("the upload's offset is missing")
		}
		return UploadStatus(
			offset: offset, length: response.value(forHTTPHeaderField: "Upload-Length").flatMap { Int64($0) },
			expires: response.value(forHTTPHeaderField: "Upload-Expires").flatMap(Self.parseHTTPDate))
	}

	/// A request appending bytes at the offset (for this client or a background session)
	public func appendRequest(uploadID: String, offset: Int64, checksum: String?, token: String?)
		throws(RemoteError) -> URLRequest
	{
		var request = try request(
			"PATCH", url(["v1", "uploads", uploadID]), authorization: token.map { .token($0) } ?? .credential,
			timeout: 300)
		request.setValue(Self.tusVersion, forHTTPHeaderField: "Tus-Resumable")
		request.setValue("application/offset+octet-stream", forHTTPHeaderField: "Content-Type")
		request.setValue(String(offset), forHTTPHeaderField: "Upload-Offset")
		if let checksum {
			request.setValue("sha256 \(checksum)", forHTTPHeaderField: "Upload-Checksum")
		}
		return request
	}

	/// Appends the file's bytes at the offset, streamed from the file; returns the new offset. `checksum` is the
	/// standard base64 SHA-256 of the file (tus checksum extension): with it, a request that is cut off appends nothing.
	/// A `409` (another offset) or `460` (checksum mismatch) is thrown as `.server`.
	public func appendUpload(uploadID: String, offset: Int64, file: URL, checksum: String?, token: String? = nil)
		async throws(RemoteError) -> Int64
	{
		let request = try appendRequest(uploadID: uploadID, offset: offset, checksum: checksum, token: token)
		let delegate = RemoteTrustDelegate(endpoint: endpoint)
		let data: Data
		let response: URLResponse
		do {
			(data, response) = try await session.upload(for: request, fromFile: file, delegate: delegate)
		}
		catch {
			throw Self.map(error, delegate: delegate)
		}
		let http = try checked(response)
		if http.statusCode >= 400 {
			throw Self.error(status: http.statusCode, body: data.prefix(maxControlBodyBytes), response: http)
		}
		guard let newOffset = http.value(forHTTPHeaderField: "Upload-Offset").flatMap({ Int64($0) }) else {
			throw .protocolViolation("the upload's new offset is missing")
		}
		return newOffset
	}

	/// Terminates an upload (tus termination)
	public func terminateUpload(uploadID: String, token: String? = nil) async throws(RemoteError) {
		var request = try request(
			"DELETE", url(["v1", "uploads", uploadID]), authorization: token.map { .token($0) } ?? .credential)
		request.setValue(Self.tusVersion, forHTTPHeaderField: "Tus-Resumable")
		_ = try await send(request)
	}

	static func parseHTTPDate(_ value: String) -> Date? {
		if let date = RemoteJSON.parseDate(value) {
			return date
		}
		let formatter = DateFormatter()
		formatter.locale = Locale(identifier: "en_US_POSIX")
		formatter.timeZone = TimeZone(identifier: "GMT")
		formatter.dateFormat = "EEE, dd MMM yyyy HH:mm:ss zzz"
		return formatter.date(from: value)
	}

	// MARK: - Operations

	/// Submits an operation. `body` is its canonical encoding, sent unchanged on every replay.
	public func submitOperation(_ body: Data) async throws(RemoteError) -> RemoteOperation {
		var request = try request("POST", url(["v1", "operations"]))
		request.setValue("application/json", forHTTPHeaderField: "Content-Type")
		request.httpBody = body
		return try await json(RemoteOperation.self, request)
	}

	public func submitOperation(_ operation: OperationRequest) async throws(RemoteError) -> RemoteOperation {
		let body: Data
		do {
			body = try operation.canonicalJSON()
		}
		catch {
			throw .protocolViolation("the operation cannot be encoded")
		}
		return try await submitOperation(body)
	}

	/// The operation's state, by its server ID
	public func operation(id: String) async throws(RemoteError) -> RemoteOperation {
		try await json(RemoteOperation.self, try request("GET", url(["v1", "operations", id])))
	}

	// MARK: - Pairing

	/// Submits the pairing secret from the QR code (without any credential)
	public func requestPairing(_ pairing: PairingRequest) async throws(RemoteError) -> PairingPending {
		var request = try request("POST", url(["v1", "pairing"]), authorization: .none)
		request.setValue("application/json", forHTTPHeaderField: "Content-Type")
		do {
			request.httpBody = try RemoteJSON.encoder().encode(pairing)
		}
		catch {
			throw .protocolViolation("the pairing request cannot be encoded")
		}
		return try await json(PairingPending.self, request)
	}

	/// Polls a pairing request with its poll token: the credential is returned once, when approved. A denial is
	/// `.forbidden("pairing_denied")`, an expired request `.server(410, "pairing_expired")`.
	public func pollPairing(requestID: String, pollToken: String, wait: Int? = nil) async throws(RemoteError)
		-> PairingResult
	{
		var query: [URLQueryItem] = []
		if let wait, wait > 0 {
			query.append(URLQueryItem(name: "wait", value: String(wait)))
		}
		return try await json(
			PairingResult.self,
			try request(
				"GET", url(["v1", "pairing", requestID], query: query), authorization: .token(pollToken),
				timeout: wait.map { TimeInterval($0) + 30 }))
	}
}

/// Loads a response body of at most `limit` bytes as a data task's own delegate: chunk by chunk as it arrives, and
/// never more than the limit (the task is canceled once a body would exceed it), with the endpoint's trust and
/// redirect rules
final class BoundedBodyDelegate: NSObject, URLSessionDataDelegate, Sendable {
	let trust: RemoteTrustDelegate
	let limit: Int

	private struct State: Sendable {
		var body = Data()
		var response: URLResponse?
		var tooLarge = false
		var continuation: CheckedContinuation<(Data, URLResponse), any Error>?
	}

	private let state = OSAllocatedUnfairLock(initialState: State())

	init(trust: RemoteTrustDelegate, limit: Int) {
		self.trust = trust
		self.limit = limit
	}

	/// Runs a data task for the request (canceled with the current task) and returns its body and response
	func load(_ request: URLRequest, in session: URLSession) async throws -> (Data, URLResponse) {
		let task = session.dataTask(with: request)
		task.delegate = self
		return try await withTaskCancellationHandler {
			try await withCheckedThrowingContinuation { continuation in
				state.withLock { $0.continuation = continuation }
				task.resume()
			}
		} onCancel: {
			task.cancel()
		}
	}

	func urlSession(
		_ session: URLSession, dataTask: URLSessionDataTask, didReceive response: URLResponse,
		completionHandler: @escaping @Sendable (URLSession.ResponseDisposition) -> Void
	) {
		let allowed = response.expectedContentLength <= Int64(limit)
		state.withLock { state in
			state.response = response
			state.tooLarge = !allowed
		}
		completionHandler(allowed ? .allow : .cancel)
	}

	func urlSession(_ session: URLSession, dataTask: URLSessionDataTask, didReceive data: Data) {
		let exceeded = state.withLock { state -> Bool in
			guard !state.tooLarge else {
				return false
			}
			guard state.body.count + data.count <= limit else {
				state.tooLarge = true
				state.body = Data()
				return true
			}
			state.body.append(data)
			return false
		}
		if exceeded {
			dataTask.cancel()
		}
	}

	func urlSession(_ session: URLSession, task: URLSessionTask, didCompleteWithError error: (any Error)?) {
		let (continuation, result) = state.withLock {
			state -> (CheckedContinuation<(Data, URLResponse), any Error>?, Result<(Data, URLResponse), any Error>) in
			defer {
				state.continuation = nil
				state.body = Data()
			}
			if state.tooLarge {
				return (state.continuation, .failure(RemoteError.protocolViolation("the response is larger than allowed")))
			}
			if let error {
				return (state.continuation, .failure(error))
			}
			guard let response = state.response else {
				return (state.continuation, .failure(URLError(.badServerResponse)))
			}
			return (state.continuation, .success((state.body, response)))
		}
		continuation?.resume(with: result)
	}

	func urlSession(
		_ session: URLSession, task: URLSessionTask, didReceive challenge: URLAuthenticationChallenge,
		completionHandler: @escaping @Sendable (URLSession.AuthChallengeDisposition, URLCredential?) -> Void
	) {
		trust.urlSession(session, task: task, didReceive: challenge, completionHandler: completionHandler)
	}

	func urlSession(
		_ session: URLSession, task: URLSessionTask, willPerformHTTPRedirection response: HTTPURLResponse,
		newRequest request: URLRequest, completionHandler: @escaping @Sendable (URLRequest?) -> Void
	) {
		trust.urlSession(
			session, task: task, willPerformHTTPRedirection: response, newRequest: request,
			completionHandler: completionHandler)
	}
}
