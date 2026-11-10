// Copyright (C) 2026 Gordon
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this file,
// You can obtain one at https://mozilla.org/MPL/2.0/.
import Testing

@testable import SynctrainFileProviderKit

@Suite struct RemoteAddressTests {
	@Test func readsHostsWithAndWithoutPorts() {
		#expect(RemoteAddress(parsing: "studio.example.org") == RemoteAddress(host: "studio.example.org"))
		#expect(
			RemoteAddress(parsing: " studio.example.org:20920\n") == RemoteAddress(host: "studio.example.org", port: 20920))
		#expect(RemoteAddress(parsing: "Gordons-Mac.local:1") == RemoteAddress(host: "Gordons-Mac.local", port: 1))
		#expect(RemoteAddress(parsing: "192.168.14.133:65535") == RemoteAddress(host: "192.168.14.133", port: 65535))
		#expect(RemoteAddress(parsing: "nas_1") == RemoteAddress(host: "nas_1"))
		#expect(RemoteAddress(parsing: "studio.example.org.") == RemoteAddress(host: "studio.example.org."))
		#expect(RemoteAddress(parsing: "xn--bcher-kva.example") == RemoteAddress(host: "xn--bcher-kva.example"))
	}

	@Test func readsIPv6Addresses() {
		#expect(RemoteAddress(parsing: "[fd00::1]:20920") == RemoteAddress(host: "fd00::1", port: 20920))
		#expect(RemoteAddress(parsing: "[fd00::1]") == RemoteAddress(host: "fd00::1"))
		#expect(RemoteAddress(parsing: "fd00::1") == RemoteAddress(host: "fd00::1"))
		#expect(RemoteAddress(parsing: "::ffff:192.168.1.2") == RemoteAddress(host: "::ffff:192.168.1.2"))
	}

	@Test func readsPastedLinks() {
		#expect(
			RemoteAddress(parsing: "https://studio.example.org:20920/")
				== RemoteAddress(host: "studio.example.org", port: 20920))
		#expect(RemoteAddress(parsing: "https://[fd00::1]:8443") == RemoteAddress(host: "fd00::1", port: 8443))
		// A link without a port is at HTTPS's
		#expect(RemoteAddress(parsing: "HTTPS://gw.example.org/") == RemoteAddress(host: "gw.example.org", port: 443))
		// A pairing link carries a secret: it is not an address, and neither is a link of another scheme
		#expect(RemoteAddress(parsing: "synctrain+pair://nas.local:20920/?service=s&secret=x") == nil)
		#expect(RemoteAddress(parsing: "synctrain+pair://nas.local:20920") == nil)
		#expect(RemoteAddress(parsing: "http://studio.example.org:20920") == nil)
	}

	@Test func refusesWhatIsNotAnAddress() {
		for text in [
			"", "  ", ":20920", "studio.example.org:", "studio.example.org:0", "studio.example.org:65536",
			"studio.example.org:20a", "studio.example.org:+80", "studio example.org", "studio.example.org/path",
			"user@studio.example.org", "[fd00::1", "[fd00::1]20920", "[studio.example.org]:20920", "fd00::zz",
			"fe80::1%en0", "studio.example.org:20920:1", "studio.example.org:٢٠",
			// Names that aren't ASCII would be converted when connecting
			"bücher.example", "ｓｔｕｄｉｏ.example", "٢٠٢.١٦٨.١.٢", "studio²", "cafe\u{301}.example",
			// Empty labels, and labels that start or end with a hyphen
			".", "...", "-", "a..b", ".studio.example.org", "-studio.example.org", "studio-.example.org",
		] {
			#expect(RemoteAddress(parsing: text) == nil, "\(text)")
		}
	}

	@Test func onlyPinnedEndpointsMoveToAnotherHost() {
		let pinned = RemoteEndpoint(serviceID: "s", host: "nas.local", port: 20920, pinnedSPKIHash: "pin", displayName: "NAS")
		#expect(pinned.allowsHost("nas.example.org"))
		// Trusted by name: another host would get the credential if it has a valid certificate for its own name
		let unpinned = RemoteEndpoint(serviceID: "s", host: "nas.example.org", port: 443, displayName: "NAS")
		#expect(!unpinned.allowsHost("nas2.example.org"))
		#expect(unpinned.allowsHost("NAS.example.org"))
	}

	@Test func formatsWithBracketsForIPv6() {
		#expect(RemoteAddress.format(host: "studio.example.org", port: 20920) == "studio.example.org:20920")
		#expect(RemoteAddress.format(host: "fd00::1", port: 20920) == "[fd00::1]:20920")
		for text in ["studio.example.org:20920", "[fd00::1]:20920"] {
			let address = RemoteAddress(parsing: text)
			#expect(address.map { RemoteAddress.format(host: $0.host, port: $0.port ?? 0) } == text)
		}
	}
}
