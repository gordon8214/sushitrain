// Copyright (C) 2026 Gordon
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this file,
// You can obtain one at https://mozilla.org/MPL/2.0/.

package files

import (
	"bufio"
	"context"
	"errors"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/godbus/dbus/v5"
)

type unresponsiveKeyring struct {
	entered chan struct{}
	release chan struct{}
}

func TestSecretServiceAuthenticationCancellation(t *testing.T) {
	listener, err := net.Listen("unix", filepath.Join(t.TempDir(), "bus"))
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	t.Setenv("DBUS_SESSION_BUS_ADDRESS", "unix:path="+listener.Addr().String())
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	result := make(chan error, 1)
	go func() {
		s := &SecretService{Application: "synctrain-test"}
		result <- s.connectContext(ctx)
	}()
	// Accept the connection but never answer its authentication exchange.
	if err := listener.(*net.UnixListener).SetDeadline(time.Now().Add(3 * time.Second)); err != nil {
		t.Fatal(err)
	}
	peer, err := listener.Accept()
	if err != nil {
		t.Fatal(err)
	}
	defer peer.Close()
	cancel()
	select {
	case err := <-result:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("expected canceled authentication, got %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("an unresponsive bus held the authentication caller")
	}
}

func (k *unresponsiveKeyring) OpenSession(_ string, _ dbus.Variant) (dbus.Variant, dbus.ObjectPath, *dbus.Error) {
	close(k.entered)
	<-k.release
	return dbus.MakeVariant(""), "/session", nil
}

func TestSecretServiceConnectCancellation(t *testing.T) {
	executable, err := exec.LookPath("dbus-daemon")
	if err != nil {
		t.Fatal("install dbus-daemon for the Linux credential integration test")
	}
	budget, stop := context.WithTimeout(t.Context(), 5*time.Second)
	defer stop()
	daemon := exec.CommandContext(budget, executable, "--session", "--nofork", "--print-address=1")
	out, err := daemon.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err = daemon.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { daemon.Process.Kill(); daemon.Wait() }()
	address, err := bufio.NewReader(out).ReadString('\n')
	if err != nil {
		t.Fatal(err)
	}
	address = strings.TrimSpace(address)
	t.Setenv("DBUS_SESSION_BUS_ADDRESS", address)
	server, err := dbus.Connect(address)
	if err != nil {
		t.Fatal(err)
	}
	defer server.Close()
	keyring := &unresponsiveKeyring{entered: make(chan struct{}), release: make(chan struct{})}
	defer close(keyring.release)
	if err = server.Export(keyring, secretsPath, secretsService); err != nil {
		t.Fatal(err)
	}
	if _, err = server.RequestName(secretsName, dbus.NameFlagDoNotQueue); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(budget)
	defer cancel()
	result := make(chan error, 1)
	go func() {
		s := &SecretService{Application: "synctrain-test"}
		result <- s.connectContext(ctx)
	}()
	select {
	case <-keyring.entered:
	case <-budget.Done():
		t.Fatal("the credential request did not reach the test keyring")
	}
	cancel()
	select {
	case err := <-result:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("expected canceled connection, got %v", err)
		}
	case <-budget.Done():
		t.Fatal("an unresponsive keyring held the caller after cancellation")
	}
}

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
