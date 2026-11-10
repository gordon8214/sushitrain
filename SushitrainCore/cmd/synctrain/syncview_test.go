// Copyright (C) 2026 Gordon
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this file,
// You can obtain one at https://mozilla.org/MPL/2.0/.

//go:build linux && cgo

package main

import (
	"os"
	"strings"
	"testing"
)

// The address for clients reads back as it shows, and the default host follows synctrain-gateway's
func TestPublicAddress(t *testing.T) {
	name, _ := os.Hostname()
	type parsed struct {
		host string
		port int
	}
	for _, c := range []struct {
		text, listen string
		want         parsed
		shown        string
	}{
		{"", ":20920", parsed{}, ""},
		{"studio.example.org", ":20920", parsed{"studio.example.org", 0}, "studio.example.org"},
		{"studio.example.org:443", ":20920", parsed{"studio.example.org", 443}, "studio.example.org:443"},
		{"https://studio.example.org", ":20920", parsed{"studio.example.org", 443}, "studio.example.org:443"},
		{"[fd00::1]:443", ":20920", parsed{"fd00::1", 443}, "[fd00::1]:443"},
		{"fd00::1", ":20920", parsed{"fd00::1", 0}, "fd00::1"},
		// The default host, in any case, follows the default
		{strings.ToUpper(name), ":20920", parsed{}, ""},
		{strings.ToUpper(name) + ":443", "0.0.0.0:20920", parsed{"", 443}, name + ":443"},
		// A specific listening host is the default, and the machine's name isn't then
		{"100.64.1.2:443", "100.64.1.2:20920", parsed{"", 443}, "100.64.1.2:443"},
		{name, "100.64.1.2:20920", parsed{name, 0}, name},
	} {
		host, port, err := publicAddress(c.text, c.listen)
		if err != nil || (parsed{host, port}) != c.want {
			t.Errorf("publicAddress(%q, %q) = %q, %d, %v; want %+v", c.text, c.listen, host, port, err, c.want)
			continue
		}
		if shown := displayedAddress(host, port, c.listen); shown != c.shown {
			t.Errorf("%q shows as %q, want %q", c.text, shown, c.shown)
		}
	}
	if _, _, err := publicAddress("studio example.org", ":20920"); err == nil {
		t.Error("an address with a space was taken")
	}
}
