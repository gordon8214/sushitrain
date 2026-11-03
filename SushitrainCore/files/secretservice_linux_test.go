// Copyright (C) 2026 Gordon
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this file,
// You can obtain one at https://mozilla.org/MPL/2.0/.

package files

import (
	"os"
	"path/filepath"
	"testing"
)

// The Secret Service store against the session's real keyring, which this test writes an item to and removes it
// from again: run only when SYNCTRAIN_TEST_KEYRING is set
func TestSecretService(t *testing.T) {
	if os.Getenv("SYNCTRAIN_TEST_KEYRING") == "" {
		t.Skip("set SYNCTRAIN_TEST_KEYRING to use the session's keyring")
	}
	s, err := NewSecretService("synctrain-test")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	const service = "svc_synctraintest"
	defer s.DeleteCredential(service)
	if c, err := s.Credential(service); err != nil || c != "" {
		t.Fatalf("before: %q, %v", c, err)
	}
	if err := s.SetCredential(service, "stc_one"); err != nil {
		t.Fatal(err)
	}
	if err := s.SetCredential(service, "stc_two"); err != nil {
		t.Fatal(err)
	}
	if c, err := s.Credential(service); err != nil || c != "stc_two" {
		t.Fatalf("after setting: %q, %v", c, err)
	}
	// Moving from the file
	file := &FileCredentials{Path: filepath.Join(t.TempDir(), "credentials.json")}
	file.SetCredential(service, "stc_three")
	if moved, err := MoveCredentials(file, s); err != nil || moved != 1 {
		t.Fatalf("moved %d, %v", moved, err)
	}
	if c, _ := file.Credential(service); c != "" {
		t.Fatal("the moved credential is still in the file")
	}
	if c, _ := s.Credential(service); c != "stc_three" {
		t.Fatalf("the keyring has %q", c)
	}
	if err := s.DeleteCredential(service); err != nil {
		t.Fatal(err)
	}
	if c, err := s.Credential(service); err != nil || c != "" {
		t.Fatalf("after deleting: %q, %v", c, err)
	}
}
