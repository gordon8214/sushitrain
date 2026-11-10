// Copyright (C) 2026 Gordon
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this file,
// You can obtain one at https://mozilla.org/MPL/2.0/.

package remoteclient

import "testing"

// The same cases as RemoteAddressTests in SynctrainFileProviderKit, so that every client takes the same addresses
func TestParseAddress(t *testing.T) {
	for text, want := range map[string]Address{
		"studio.example.org":                {Host: "studio.example.org"},
		" studio.example.org:20920\n":       {Host: "studio.example.org", Port: 20920},
		"Gordons-Mac.local:1":               {Host: "Gordons-Mac.local", Port: 1},
		"192.168.14.133:65535":              {Host: "192.168.14.133", Port: 65535},
		"nas_1":                             {Host: "nas_1"},
		"[fd00::1]:20920":                   {Host: "fd00::1", Port: 20920},
		"[fd00::1]":                         {Host: "fd00::1"},
		"fd00::1":                           {Host: "fd00::1"},
		"::ffff:192.168.1.2":                {Host: "::ffff:192.168.1.2"},
		"studio.example.org.":               {Host: "studio.example.org."},
		"xn--bcher-kva.example":             {Host: "xn--bcher-kva.example"},
		"https://studio.example.org:20920/": {Host: "studio.example.org", Port: 20920},
		"https://[fd00::1]:8443":            {Host: "fd00::1", Port: 8443},
		// A link without a port is at HTTPS's
		"HTTPS://gw.example.org/": {Host: "gw.example.org", Port: 443},
	} {
		got, err := ParseAddress(text)
		if err != nil || got != want {
			t.Errorf("ParseAddress(%q) = %+v, %v; want %+v", text, got, err, want)
		}
	}
}

func TestParseAddressRefusesWhatIsNotAnAddress(t *testing.T) {
	for _, text := range []string{
		"", "  ", ":20920", "studio.example.org:", "studio.example.org:0", "studio.example.org:65536",
		"studio.example.org:20a", "studio.example.org:+80", "studio example.org", "studio.example.org/path",
		"user@studio.example.org", "[fd00::1", "[fd00::1]20920", "[studio.example.org]:20920", "fd00::zz",
		"fe80::1%en0", "studio.example.org:20920:1", "studio.example.org:٢٠",
		// A pairing link carries a secret: it is not an address, and neither is a link of another scheme
		"synctrain+pair://nas.local:20920/?service=s&secret=x", "synctrain+pair://nas.local:20920",
		"http://studio.example.org:20920",
		// Names that aren't ASCII would be converted when connecting
		"bücher.example", "ｓｔｕｄｉｏ.example", "٢٠٢.١٦٨.١.٢", "studio²", "cafe\u0301.example",
		// Empty labels, and labels that start or end with a hyphen
		".", "...", "-", "a..b", ".studio.example.org", "-studio.example.org", "studio-.example.org",
	} {
		if got, err := ParseAddress(text); err == nil {
			t.Errorf("ParseAddress(%q) = %+v, want an error", text, got)
		}
	}
}

func TestFormatAddress(t *testing.T) {
	if got := FormatAddress("studio.example.org", 20920); got != "studio.example.org:20920" {
		t.Errorf("got %q", got)
	}
	if got := FormatAddress("fd00::1", 20920); got != "[fd00::1]:20920" {
		t.Errorf("got %q", got)
	}
	for _, text := range []string{"studio.example.org:20920", "[fd00::1]:20920"} {
		if a, err := ParseAddress(text); err != nil || FormatAddress(a.Host, a.Port) != text {
			t.Errorf("%q doesn't come back as it was: %+v, %v", text, a, err)
		}
	}
}
