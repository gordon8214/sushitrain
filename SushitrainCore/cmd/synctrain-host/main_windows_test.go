// Copyright (C) 2026 Gordon
// SPDX-License-Identifier: MPL-2.0
package main

import (
	"context"
	"errors"
	"github.com/google/uuid"
	"os"
	"os/exec"
	"path/filepath"
	"t-shaped.nl/sushitrain/v2/adminsock"
	"testing"
	"time"
)

// Expected duration is below three seconds. A broken worker must never make
// the signed-in supervisor impossible to quit during its restart backoff.
func TestSupervisorCanQuitDuringStartupFailure(t *testing.T) {
	provider := os.Getenv("SYNCTRAIN_TEST_PROVIDER")
	if provider == "" {
		t.Fatal("SYNCTRAIN_TEST_PROVIDER is required")
	}
	host := filepath.Join(filepath.Dir(provider), "synctrain-host.exe")
	state := t.TempDir()
	if err := os.WriteFile(filepath.Join(state, "settings.json"), []byte(`{"Mode":"invalid-for-test"}`), 0600); err != nil {
		t.Fatal(err)
	}
	pipe := `\\.\pipe\Synctrain-Test-` + uuid.NewString()
	cmd := exec.Command(host, "--state", state, "--root", filepath.Join(state, "cloud"), "--pipe", pipe)
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	defer func() {
		if cmd.ProcessState == nil {
			_ = cmd.Process.Kill()
			<-done
		}
	}()
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
	defer cancel()
	for {
		if err := adminsock.CallContext(ctx, pipe+"-Supervisor", "quit", nil, nil); err == nil {
			break
		}
		select {
		case <-ctx.Done():
			t.Fatal("supervisor control was unavailable")
		case <-time.After(25 * time.Millisecond):
		}
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(4 * time.Second):
		t.Fatal("supervisor did not stop after Quit")
	}
}

func TestFailedDetachRequestsSupervisorRecovery(t *testing.T) {
	run := &providerRun{done: make(chan struct{}), err: errors.New("injected provider failure")}
	run.intentional.Store(true)
	close(run.done)
	p := &providerProcess{state: t.TempDir(), run: run, failed: make(chan error, 1)}
	if err := p.change("", "", ""); err == nil {
		t.Fatal("failed native detach was accepted")
	}
	select {
	case <-p.failed:
	default:
		t.Fatal("lost native owner did not request recovery")
	}
}
