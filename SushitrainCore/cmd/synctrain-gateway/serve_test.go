// Copyright (C) 2026 Gordon
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this file,
// You can obtain one at https://mozilla.org/MPL/2.0/.

package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestPairingAddress(t *testing.T) {
	if host, port, err := pairingAddress("truenas.example.org", 0, ":20920"); err != nil || host != "truenas.example.org" || port != 20920 {
		t.Fatalf("%s:%d %v", host, port, err)
	}
	if _, port, _ := pairingAddress("nas", 443, ":20920"); port != 443 {
		t.Fatal("public port not used")
	}
	if host, _, err := pairingAddress("[fd00::1]", 0, ":20920"); err != nil || host != "fd00::1" {
		t.Fatalf("IPv6 host: %s %v", host, err)
	}
	if _, _, err := pairingAddress("nas.example.org:443", 0, ":20920"); err == nil {
		t.Fatal("a host with a port was accepted")
	}
}

func TestFolderGrants(t *testing.T) {
	grants := folderGrants{}
	for _, value := range []string{"Documents", "Photos:write", "a:b:read"} {
		if err := grants.Set(value); err != nil {
			t.Fatal(err)
		}
	}
	if grants["Documents"] != "read" || grants["Photos"] != "write" || grants["a:b"] != "read" {
		t.Fatalf("%v", grants)
	}
	for _, value := range []string{"a:b", ":read", "Photos:all"} {
		if err := grants.Set(value); err == nil {
			t.Fatalf("%q was accepted", value)
		}
	}
}

func TestIgnoresEverything(t *testing.T) {
	for _, pattern := range []string{"*", "**", "/*", "/**", "**/*", "(?d)*", "(?d)(?i)**"} {
		if !ignoresEverything(pattern) {
			t.Errorf("%q", pattern)
		}
	}
	for _, pattern := range []string{"!*", "(?d)!/**", "*.tmp", "/Photos", "**/.DS_Store"} {
		if ignoresEverything(pattern) {
			t.Errorf("%q", pattern)
		}
	}
}

func TestPrintable(t *testing.T) {
	got := printable("iPhone\x1b[2J\u202eevil\x9b")
	if strings.ContainsRune(got, 0x1b) || strings.ContainsRune(got, 0x202e) || strings.Contains(got, "\x9b") {
		t.Fatalf("%q", got)
	}
	if got = printable("Gordon’s iPad"); got != "Gordon’s iPad" {
		t.Fatalf("%q", got)
	}
}

func TestQRPNGIsPrivate(t *testing.T) {
	path := filepath.Join(t.TempDir(), "code.png")
	if err := os.WriteFile(path, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := writeQRPNG(path, "synctrain+pair://example.org:20920/?secret=x"); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 || info.Size() == 0 {
		t.Fatalf("%v, %d bytes", info.Mode(), info.Size())
	}
}

func TestLogLevelsAreLinked(t *testing.T) {
	t.Setenv("STLOGLEVEL", "warn")
	t.Setenv("STTRACE", "model:ERROR")
	if err := configureLogging(); err != nil {
		t.Fatal(err)
	}
	t.Setenv("STLOGLEVEL", "loud")
	if err := configureLogging(); err == nil {
		t.Fatal("an invalid level was accepted")
	}
	t.Setenv("STLOGLEVEL", "")
	t.Setenv("STTRACE", "")
	if err := configureLogging(); err != nil {
		t.Fatal(err)
	}
}
