// Copyright (C) 2026 Gordon
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this file,
// You can obtain one at https://mozilla.org/MPL/2.0/.
package sushitrain

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/syncthing/syncthing/lib/locations"
)

// filesWithConfiguration is a files directory holding a configuration and an identity, as a user may put in Documents
func filesWithConfiguration(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	for _, name := range []string{ConfigFileName, CertFileName, KeyFileName} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(name), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

// keepLocations restores the configuration and identity locations after a test
func keepLocations(t *testing.T) {
	t.Helper()
	keys := []locations.LocationEnum{locations.ConfigFile, locations.CertFile, locations.KeyFile}
	before := make(map[locations.LocationEnum]string, len(keys))
	for _, key := range keys {
		before[key] = locations.Get(key)
	}
	t.Cleanup(func() {
		for key, value := range before {
			locations.Set(key, value)
		}
	})
}

func TestFilesInTheHomeDirectoryAreNoConfiguration(t *testing.T) {
	keepLocations(t)
	before := locations.Get(locations.ConfigFile)
	if useCustomConfiguration(filesWithConfiguration(t), false) {
		t.Fatal("used files in the home directory as the configuration")
	}
	if locations.Get(locations.ConfigFile) != before {
		t.Fatalf("configuration moved to %s", locations.Get(locations.ConfigFile))
	}
}

func TestIOSUsesTheConfigurationInDocuments(t *testing.T) {
	keepLocations(t)
	documents := filesWithConfiguration(t)
	if !useCustomConfiguration(documents, true) {
		t.Fatal("did not use the configuration in Documents")
	}
	for key, name := range map[locations.LocationEnum]string{locations.ConfigFile: ConfigFileName,
		locations.CertFile: CertFileName, locations.KeyFile: KeyFileName} {
		if got := locations.Get(key); got != filepath.Join(documents, name) {
			t.Errorf("%s is %s", name, got)
		}
	}
}

func TestConfigurationIsExportedOnlyOnIOS(t *testing.T) {
	if err := (&Client{}).ExportConfigurationFile(); err == nil {
		t.Fatal("exported the configuration into the home directory")
	}
}
