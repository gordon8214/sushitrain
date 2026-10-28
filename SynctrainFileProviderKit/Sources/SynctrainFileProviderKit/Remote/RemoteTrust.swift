// Copyright (C) 2026 Gordon
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this file,
// You can obtain one at https://mozilla.org/MPL/2.0/.
import CryptoKit
import Foundation
import Security
import os

/// Server trust for gateways: a normally trusted certificate, or one whose public key matches a pin from the pairing
/// QR code. Arbitrary certificates are never accepted.
///
/// A pin is the base64url (unpadded) SHA-256 of the leaf certificate's DER-encoded SubjectPublicKeyInfo, as Go's
/// `sha256.Sum256(cert.RawSubjectPublicKeyInfo)` computes it. The SubjectPublicKeyInfo is taken from the certificate's
/// own bytes (the seventh field of `tbsCertificate`), not rebuilt from the key, so it is byte for byte what Go hashes,
/// for any key type.
public enum RemoteTrust {
	/// Whether the server trust is acceptable for the host: one of the pins matches the leaf certificate, or, without
	/// pins, the system trusts the certificate for the host
	public static func evaluate(_ trust: SecTrust, host: String, pins: [String]) -> Bool {
		if !pins.isEmpty {
			guard let chain = SecTrustCopyCertificateChain(trust) as? [SecCertificate], let leaf = chain.first,
				let hash = spkiHash(of: leaf)
			else {
				return false
			}
			return pins.contains(hash)
		}
		SecTrustSetPolicies(trust, SecPolicyCreateSSL(true, host as CFString))
		return SecTrustEvaluateWithError(trust, nil)
	}

	/// The pin of a certificate
	public static func spkiHash(of certificate: SecCertificate) -> String? {
		spkiHash(certificateDER: SecCertificateCopyData(certificate) as Data)
	}

	public static func spkiHash(certificateDER: Data) -> String? {
		guard let spki = subjectPublicKeyInfo(certificateDER: certificateDER) else {
			return nil
		}
		return Data(SHA256.hash(data: spki)).base64URLEncodedString()
	}

	/// The DER bytes (tag, length and contents) of the certificate's SubjectPublicKeyInfo
	static func subjectPublicKeyInfo(certificateDER der: Data) -> Data? {
		let bytes = [UInt8](der)
		// Certificate ::= SEQUENCE { tbsCertificate, signatureAlgorithm, signature }
		guard let certificate = element(in: bytes, at: 0), certificate.tag == 0x30,
			let tbs = element(in: bytes, at: certificate.contentStart), tbs.tag == 0x30
		else {
			return nil
		}
		// TBSCertificate ::= SEQUENCE { [0] version OPTIONAL, serialNumber, signature, issuer, validity, subject,
		// subjectPublicKeyInfo, ... }
		var position = tbs.contentStart
		var field = 0
		while position < tbs.end {
			guard let current = element(in: bytes, at: position), current.end <= tbs.end else {
				return nil
			}
			if field == 0 && current.tag == 0xA0 {
				position = current.end
				continue
			}
			if field == 5 {
				guard current.tag == 0x30 else {
					return nil
				}
				return Data(bytes[position..<current.end])
			}
			field += 1
			position = current.end
		}
		return nil
	}

	/// A DER element starting at the position: its tag, where its contents start, and where it ends
	private static func element(in bytes: [UInt8], at position: Int) -> (tag: UInt8, contentStart: Int, end: Int)? {
		guard position + 2 <= bytes.count else {
			return nil
		}
		let tag = bytes[position]
		var length = Int(bytes[position + 1])
		var contentStart = position + 2
		if length & 0x80 != 0 {
			let count = length & 0x7F
			guard count > 0, count <= 4, contentStart + count <= bytes.count else {
				return nil
			}
			length = 0
			for byte in bytes[contentStart..<contentStart + count] {
				length = (length << 8) | Int(byte)
			}
			contentStart += count
		}
		let end = contentStart + length
		guard end <= bytes.count else {
			return nil
		}
		return (tag, contentStart, end)
	}
}

/// Evaluates server trust and refuses redirects for one endpoint's requests (as the session's delegate, or a task's).
///
/// Only the task-level challenge method is implemented: when a delegate implements the session-level one, URLSession
/// sends server trust challenges there, to the session's delegate, and the task's delegate (whose `rejectedTrust`
/// tells a rejected certificate from a canceled request) never learns about them.
final class RemoteTrustDelegate: NSObject, URLSessionTaskDelegate, Sendable {
	let endpoint: RemoteEndpoint
	private let rejected = OSAllocatedFlag()

	init(endpoint: RemoteEndpoint) {
		self.endpoint = endpoint
	}

	/// Whether the delegate rejected the server's certificate
	var rejectedTrust: Bool { rejected.value }

	func urlSession(
		_ session: URLSession, task: URLSessionTask, didReceive challenge: URLAuthenticationChallenge,
		completionHandler: @escaping @Sendable (URLSession.AuthChallengeDisposition, URLCredential?) -> Void
	) {
		let space = challenge.protectionSpace
		let (disposition, credential) = answer(
			authenticationMethod: space.authenticationMethod, host: space.host, port: space.port,
			serverTrust: space.serverTrust)
		completionHandler(disposition, credential)
	}

	/// The answer to a challenge for the protection space
	func answer(authenticationMethod: String, host: String, port: Int, serverTrust: SecTrust?)
		-> (URLSession.AuthChallengeDisposition, URLCredential?)
	{
		guard authenticationMethod == NSURLAuthenticationMethodServerTrust else {
			// Credentials go in the Authorization header only, never in answer to a challenge
			return (.rejectProtectionSpace, nil)
		}
		guard let serverTrust, host.caseInsensitiveCompare(endpoint.host) == .orderedSame, port == endpoint.port,
			RemoteTrust.evaluate(serverTrust, host: endpoint.host, pins: endpoint.acceptedSPKIHashes)
		else {
			rejected.set()
			return (.cancelAuthenticationChallenge, nil)
		}
		return (.useCredential, URLCredential(trust: serverTrust))
	}

	func urlSession(
		_ session: URLSession, task: URLSessionTask, willPerformHTTPRedirection response: HTTPURLResponse,
		newRequest request: URLRequest, completionHandler: @escaping @Sendable (URLRequest?) -> Void
	) {
		// The gateway never redirects `/v1` paths: the redirect itself is the answer, a protocol violation
		completionHandler(nil)
	}
}

/// A flag that is set once, from any thread
final class OSAllocatedFlag: Sendable {
	private let storage = OSAllocatedUnfairLock(initialState: false)

	var value: Bool { storage.withLock { $0 } }

	func set() {
		storage.withLock { $0 = true }
	}
}
