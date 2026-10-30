// Copyright (C) 2026 Gordon
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this file,
// You can obtain one at https://mozilla.org/MPL/2.0/.

package sthome

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestLoadIdentityCreatesOnlyWhenBothFilesAreMissing(t *testing.T) {
	dir := t.TempDir()
	certFile, keyFile := filepath.Join(dir, "cert.pem"), filepath.Join(dir, "key.pem")

	first, created, err := LoadIdentity(certFile, keyFile)
	if err != nil || !created {
		t.Fatalf("created %v, %v", created, err)
	}
	if names := dirNames(t, dir); names != "cert.pem key.pem " {
		t.Fatalf("files after creating: %s", names)
	}
	second, created, err := LoadIdentity(certFile, keyFile)
	if err != nil || created {
		t.Fatalf("created %v, %v", created, err)
	}
	if !bytes.Equal(first.Certificate[0], second.Certificate[0]) {
		t.Fatal("the identity changed on loading")
	}
}

func TestLoadIdentityLeavesABrokenIdentityAlone(t *testing.T) {
	cases := map[string]func(t *testing.T, dir, certFile, keyFile string){
		"key missing":  func(t *testing.T, _, _, keyFile string) { must(t, os.Remove(keyFile)) },
		"cert missing": func(t *testing.T, _, certFile, _ string) { must(t, os.Remove(certFile)) },
		"corrupt cert": func(t *testing.T, _, certFile, _ string) {
			must(t, os.WriteFile(certFile, []byte("not a certificate"), 0o600))
		},
		"broken links": func(t *testing.T, dir, certFile, keyFile string) {
			for _, file := range []string{certFile, keyFile} {
				must(t, os.Remove(file))
				must(t, os.Symlink(filepath.Join(dir, "gone", filepath.Base(file)), file))
			}
		},
	}
	for name, damage := range cases {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			certFile, keyFile := filepath.Join(dir, "cert.pem"), filepath.Join(dir, "key.pem")
			if _, _, err := LoadIdentity(certFile, keyFile); err != nil {
				t.Fatal(err)
			}
			damage(t, dir, certFile, keyFile)
			before := snapshot(t, dir)

			_, created, err := LoadIdentity(certFile, keyFile)
			if err == nil || created {
				t.Fatalf("created %v, %v", created, err)
			}
			if name != "broken links" && !errors.Is(err, ErrIdentity) {
				t.Fatalf("not reported as unusable: %v", err)
			}
			if after := snapshot(t, dir); after != before {
				t.Fatalf("the files changed:\n%s\n%s", before, after)
			}
		})
	}
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

func dirNames(t *testing.T, dir string) string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	must(t, err)
	names := ""
	for _, entry := range entries {
		names += entry.Name() + " "
	}
	return names
}

// snapshot describes the names, sizes, contents and modification times of the files in dir, without following links
func snapshot(t *testing.T, dir string) string {
	t.Helper()
	var out bytes.Buffer
	entries, err := os.ReadDir(dir)
	must(t, err)
	for _, entry := range entries {
		info, err := os.Lstat(filepath.Join(dir, entry.Name()))
		must(t, err)
		data, _ := os.ReadFile(filepath.Join(dir, entry.Name()))
		out.WriteString(entry.Name() + " " + info.Mode().String() + " " + info.ModTime().String() + " " + string(data) + "\n")
	}
	return out.String()
}
