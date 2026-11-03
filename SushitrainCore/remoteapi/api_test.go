// Copyright (C) 2026 Gordon
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this file,
// You can obtain one at https://mozilla.org/MPL/2.0/.

package remoteapi

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

// The fixtures are the contract the gateway and both clients are tested against: every field must round-trip
func TestFixtures(t *testing.T) {
	types := map[string]any{
		"capabilities.json":            &Capabilities{},
		"folders.json":                 &FoldersResponse{},
		"item-file.json":               &Item{},
		"item-directory.json":          &Item{},
		"snapshot-page.json":           &SnapshotPage{},
		"changes-page.json":            &ChangesPage{},
		"read-lease.json":              &ReadLease{},
		"manifest-page.json":           &ManifestPage{},
		"upload-created.json":          &Upload{},
		"operation-create.json":        &OperationRequest{},
		"operation-replace.json":       &OperationRequest{},
		"operation-move.json":          &OperationRequest{},
		"operation-delete.json":        &OperationRequest{},
		"operation-pending.json":       &Operation{},
		"operation-committed.json":     &Operation{},
		"operation-conflict.json":      &Operation{},
		"error-cursor-expired.json":    &ErrorResponse{},
		"error-revision-mismatch.json": &ErrorResponse{},
		"pairing-request.json":         &PairingRequest{},
		"pairing-approved.json":        &PairingResult{},
		"version-list.json":            &ItemVersionList{},
		"operation-restore.json":       &OperationRequest{},
		"operation-copy-version.json":  &OperationRequest{},
	}
	dir := filepath.Join("..", "..", "Docs", "remote-api", "fixtures")
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		target, ok := types[entry.Name()]
		if !ok {
			t.Errorf("fixture %s has no Go type", entry.Name())
			continue
		}
		data, err := os.ReadFile(filepath.Join(dir, entry.Name()))
		if err != nil {
			t.Fatal(err)
		}
		if err := json.Unmarshal(data, target); err != nil {
			t.Errorf("%s: %v", entry.Name(), err)
			continue
		}
		encoded, err := json.Marshal(target)
		if err != nil {
			t.Fatal(err)
		}
		var want, got any
		_ = json.Unmarshal(data, &want)
		_ = json.Unmarshal(encoded, &got)
		if !reflect.DeepEqual(want, got) {
			t.Errorf("%s does not round-trip:\nfixture %s\nencoded %s", entry.Name(), data, encoded)
		}
	}
	item := types["item-file.json"].(*Item)
	if _, size, ok := ParseContentID(item.ContentID); !ok || size != item.Size {
		t.Errorf("fixture content ID %q does not parse to the item's size", item.ContentID)
	}
	if !HasIDFormat(item.ID, PrefixItem) {
		t.Error("fixture item ID has the wrong format")
	}
}

func TestContentIDs(t *testing.T) {
	hash := bytes.Repeat([]byte{0xab}, 32)
	id := FormatContentID(hash, 1234)
	got, size, ok := ParseContentID(id)
	if !ok || !bytes.Equal(got, hash) || size != 1234 {
		t.Fatalf("%s parsed to %x, %d, %v", id, got, size, ok)
	}
	for _, bad := range []string{"", "c1.", "c1.abc", "c2." + B64.EncodeToString(hash) + ".1", "c1.!!.1", "c1.AAAA.-1", "c1.AAAA.x"} {
		if _, _, ok := ParseContentID(bad); ok {
			t.Errorf("%q parsed", bad)
		}
	}
}

func TestBlocksHash(t *testing.T) {
	a, b := sha256.Sum256([]byte("a")), sha256.Sum256([]byte("b"))
	want := sha256.Sum256(append(a[:], b[:]...))
	if got := BlocksHash([][]byte{a[:], b[:]}); !bytes.Equal(got, want[:]) {
		t.Errorf("got %x, want %x", got, want)
	}
}
