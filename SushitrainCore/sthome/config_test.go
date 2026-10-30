// Copyright (C) 2026 Gordon
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this file,
// You can obtain one at https://mozilla.org/MPL/2.0/.

package sthome

import (
	"bytes"
	"crypto/tls"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/syncthing/syncthing/lib/config"
	"github.com/syncthing/syncthing/lib/events"
	"github.com/syncthing/syncthing/lib/protocol"
)

func testCertificate(t *testing.T) tls.Certificate {
	t.Helper()
	dir := t.TempDir()
	cert, _, err := LoadIdentity(filepath.Join(dir, "cert.pem"), filepath.Join(dir, "key.pem"))
	must(t, err)
	return cert
}

// writeConfig writes a configuration with one folder, at the given version
func writeConfig(t *testing.T, path string, cert tls.Certificate, version int) {
	t.Helper()
	cfg := config.New(protocol.NewDeviceID(cert.Certificate[0]))
	cfg.Version = version
	cfg.Folders = append(cfg.Folders, config.FolderConfiguration{ID: "photos", Path: "/tmp/photos"})
	var buf bytes.Buffer
	must(t, cfg.WriteXML(&buf))
	must(t, os.WriteFile(path, buf.Bytes(), 0o600))
}

func TestLoadConfigCreatesAMissingConfigurationOnce(t *testing.T) {
	cert := testCertificate(t)
	dir := t.TempDir()
	path := filepath.Join(dir, "config.xml")
	guiOff := func(cfg *config.Configuration) { cfg.GUI.Enabled = false }
	cfg, created, err := LoadConfig(path, cert, events.NoopLogger, false, guiOff)
	if err != nil || !created {
		t.Fatalf("created %v, %v", created, err)
	}
	if names := dirNames(t, dir); names != "config.xml " {
		t.Fatalf("files after creating: %s", names)
	}
	if cfg.GUI().Address() != "127.0.0.1:8384" || cfg.GUI().Enabled {
		t.Fatalf("GUI %v at %s", cfg.GUI().Enabled, cfg.GUI().Address())
	}
	saved, err := os.ReadFile(path)
	must(t, err)
	if !strings.Contains(string(saved), `<gui enabled="false"`) {
		t.Fatal("the new configuration was first saved without the adjustment")
	}
}

func TestLoadConfigLeavesAnUnusableConfigurationAlone(t *testing.T) {
	cert := testCertificate(t)
	cases := []struct {
		name  string
		want  error
		write func(t *testing.T, path string)
	}{
		{"corrupt", ErrConfiguration, func(t *testing.T, path string) {
			must(t, os.WriteFile(path, []byte("<configuration version="), 0o600))
		}},
		{"empty", ErrConfiguration, func(t *testing.T, path string) { must(t, os.WriteFile(path, nil, 0o600)) }},
		{"newer", ErrNewerConfiguration, func(t *testing.T, path string) {
			writeConfig(t, path, cert, config.CurrentVersion+1)
		}},
		{"unreadable", ErrUnreadableConfiguration, func(t *testing.T, path string) {
			if os.Geteuid() == 0 {
				t.Skip("permissions don't stop root")
			}
			writeConfig(t, path, cert, config.CurrentVersion)
			must(t, os.Chmod(path, 0))
		}},
		{"broken link", ErrUnreadableConfiguration, func(t *testing.T, path string) {
			must(t, os.Symlink(filepath.Join(filepath.Dir(path), "gone.xml"), path))
		}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "config.xml")
			c.write(t, path)
			before := snapshot(t, dir)

			_, created, err := LoadConfig(path, cert, events.NoopLogger, false, nil)
			if created || !errors.Is(err, c.want) {
				t.Fatalf("created %v, %v", created, err)
			}
			if after := snapshot(t, dir); after != before {
				t.Fatalf("the directory changed:\n%s\n%s", before, after)
			}
		})
	}
}

func TestLoadConfigArchivesAnOlderConfiguration(t *testing.T) {
	cert := testCertificate(t)
	dir := t.TempDir()
	path := filepath.Join(dir, "config.xml")
	writeConfig(t, path, cert, config.CurrentVersion-1)
	original, err := os.ReadFile(path)
	must(t, err)

	cfg, created, err := LoadConfig(path, cert, events.NoopLogger, false, nil)
	if err != nil || created {
		t.Fatalf("created %v, %v", created, err)
	}
	archived, err := os.ReadFile(path + ".v" + strconv.Itoa(config.CurrentVersion-1))
	if err != nil || !bytes.Equal(archived, original) {
		t.Fatalf("the original was not archived: %v", err)
	}
	if _, ok := cfg.Folder("photos"); !ok {
		t.Fatal("the folder was lost")
	}
	saved, err := os.ReadFile(path)
	must(t, err)
	if !strings.Contains(string(saved), `version="`+strconv.Itoa(config.CurrentVersion)+`"`) {
		t.Fatal("the migrated configuration was not saved")
	}
}
