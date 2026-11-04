// Copyright (C) 2026 Gordon
// SPDX-License-Identifier: MPL-2.0
package history

import (
	"os"
	"path/filepath"
	"t-shaped.nl/sushitrain/v2/files"
	"t-shaped.nl/sushitrain/v2/remoteclient"
	"testing"
)

func TestSourcesSurviveReopen(t *testing.T) {
	dir := t.TempDir()
	store := &Store{path: filepath.Join(dir, "sources.json"), credentials: &files.FileCredentials{Path: filepath.Join(dir, "credentials.json")}}
	list := []Source{{Name: "NAS", Endpoint: remoteclient.Endpoint{ServiceID: "gateway", Host: "nas.local", Port: 20920}, Enabled: true, ClientID: "history-client"}}
	if err := store.save(list); err != nil {
		t.Fatal(err)
	}
	read, err := store.Sources()
	if err != nil || len(read) != 1 || read[0].ClientID != "history-client" {
		t.Fatalf("sources lost: %#v %v", read, err)
	}
	st, _ := os.Stat(store.path)
	if st.Mode().Perm() != 0600 {
		t.Fatalf("sources permissions: %v", st.Mode())
	}
	list[0].Enabled = false
	store.save(list)
	if _, err := store.source("gateway"); err == nil {
		t.Fatal("disabled source was usable")
	}
}
