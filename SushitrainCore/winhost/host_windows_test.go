// Copyright (C) 2026 Gordon
// SPDX-License-Identifier: MPL-2.0
package winhost

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/google/uuid"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"t-shaped.nl/sushitrain/v2/adminsock"
	"testing"
	"time"
)

func TestModeFailureRestoresFilesIdentity(t *testing.T) {
	base := t.TempDir()
	h, err := Open(filepath.Join(base, "state"), filepath.Join(base, "cloud"), base)
	if err != nil {
		t.Fatal(err)
	}
	defer h.Close()
	var provider string
	h.OnModeChange = func(mode, id, root string) error { provider = mode; return nil }
	if err := h.switchMode(context.Background(), "files"); err != nil {
		t.Fatal(err)
	}
	before, err := h.files.ClientIdentity()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(h.State, "Engine"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(h.State, "Engine", "config.xml"), []byte("<malformed"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := h.switchMode(context.Background(), "synchronize"); err == nil {
		t.Fatal("invalid engine configuration was accepted")
	}
	after, err := h.files.ClientIdentity()
	if err != nil {
		t.Fatal(err)
	}
	if after != before || h.settings.Mode != "files" || provider != "files" {
		t.Fatalf("rollback changed identity/mode/provider: %s %s %s %s", before, after, h.settings.Mode, provider)
	}
	if _, err := os.Stat(filepath.Join(h.State, "mode-transition.json")); !os.IsNotExist(err) {
		t.Fatalf("successful rollback retained transition: %v", err)
	}
}
func TestModeChangeCancelsReadersBeforeClosingService(t *testing.T) {
	base := t.TempDir()
	h, err := Open(filepath.Join(base, "state"), filepath.Join(base, "cloud"), base)
	if err != nil {
		t.Fatal(err)
	}
	defer h.Close()
	operation, finish, err := h.begin(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- h.switchMode(context.Background(), "files") }()
	select {
	case <-operation.Done():
	case <-time.After(time.Second):
		t.Fatal("mode change did not cancel active request")
	}
	select {
	case <-done:
		t.Fatal("mode changed before the request released its lease")
	default:
	}
	finish()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("mode change deadlocked behind canceled request")
	}
}
func TestProviderStartFailureRestoresOnboarding(t *testing.T) {
	base := t.TempDir()
	h, err := Open(filepath.Join(base, "state"), filepath.Join(base, "cloud"), base)
	if err != nil {
		t.Fatal(err)
	}
	defer h.Close()
	h.OnModeChange = func(mode, id, root string) error {
		if mode == "files" {
			return errors.New("injected provider startup failure")
		}
		return nil
	}
	if err := h.switchMode(context.Background(), "files"); err == nil {
		t.Fatal("provider startup failure ignored")
	}
	if h.settings.Mode != "" || h.files != nil {
		t.Fatal("failed startup left Files mode selected")
	}
}

func TestRollbackRecoveryKeepsItsBackupUntilCommitted(t *testing.T) {
	base := t.TempDir()
	state := filepath.Join(base, "state")
	h, err := Open(state, filepath.Join(base, "cloud"), base)
	if err != nil {
		t.Fatal(err)
	}
	if err := h.switchMode(context.Background(), "files"); err != nil {
		t.Fatal(err)
	}
	identity, err := h.files.ClientIdentity()
	if err != nil {
		t.Fatal(err)
	}
	h.Close()
	backup, err := os.MkdirTemp(state, ".files-before-mode-")
	if err != nil {
		t.Fatal(err)
	}
	if err := copyState(filepath.Join(state, "Files"), backup); err != nil {
		t.Fatal(err)
	}
	// Simulate a crash after restoring Files but before saving the old mode.
	h.settings.Mode = "synchronize"
	if err := h.save(); err != nil {
		t.Fatal(err)
	}
	journal, _ := json.Marshal(modeJournal{From: "files", To: "synchronize", Backup: backup, Phase: "rollback"})
	if err := h.writeAtomic("mode-transition.json", journal); err != nil {
		t.Fatal(err)
	}
	if err := h.restoreModeBackup(backup); err != nil {
		t.Fatal(err)
	}
	if err := h.restoreModeBackup(backup); err != nil {
		t.Fatal("repeated restore", err)
	}
	if _, err := os.Stat(backup); err != nil {
		t.Fatal("restoration consumed its only backup", err)
	}
	restored, err := Open(state, filepath.Join(base, "cloud"), base)
	if err != nil {
		t.Fatal(err)
	}
	defer restored.Close()
	actual, err := restored.files.ClientIdentity()
	if err != nil || actual != identity || restored.settings.Mode != "files" {
		t.Fatalf("restored mode/identity: %q %v", actual, err)
	}
	if _, err := os.Stat(backup); !os.IsNotExist(err) {
		t.Fatalf("committed recovery retained backup: %v", err)
	}
}

func TestModePreparationFailureResumesTheProvider(t *testing.T) {
	base := t.TempDir()
	h, err := Open(filepath.Join(base, "state"), filepath.Join(base, "cloud"), base)
	if err != nil {
		t.Fatal(err)
	}
	defer h.Close()
	expected := errors.New("final native refresh failed")
	prepared, resumed := 0, 0
	h.BeforeModeChange = func(context.Context) error {
		prepared++
		if prepared == 2 {
			return expected
		}
		return nil
	}
	h.AfterModeChange = func() { resumed++ }
	if err := h.switchMode(context.Background(), "files"); !errors.Is(err, expected) {
		t.Fatal(err)
	}
	if prepared != 2 || resumed != 1 || h.transitioning || h.settings.Mode != "" {
		t.Fatalf("preparation left provider paused or changed mode: %d %d %v %s", prepared, resumed, h.transitioning, h.settings.Mode)
	}
}

func TestProviderInitializationExcludesModeChanges(t *testing.T) {
	base := t.TempDir()
	h, err := Open(filepath.Join(base, "state"), filepath.Join(base, "cloud"), base)
	if err != nil {
		t.Fatal(err)
	}
	defer h.Close()
	entered, release := make(chan struct{}), make(chan struct{})
	defer func() {
		select {
		case <-release:
		default:
			close(release)
		}
	}()
	first := true
	h.OnModeChange = func(mode, id, root string) error {
		if first {
			first = false
			close(entered)
			<-release
		}
		return nil
	}
	initialized := make(chan error, 1)
	go func() { initialized <- h.InitializeProvider(func() error { return nil }) }()
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("initialization did not begin")
	}
	changed := make(chan error, 1)
	go func() { changed <- h.switchMode(context.Background(), "files") }()
	select {
	case <-changed:
		t.Fatal("mode changed before native initialization completed")
	case <-time.After(20 * time.Millisecond):
	}
	close(release)
	for _, done := range []chan error{initialized, changed} {
		select {
		case err := <-done:
			if err != nil {
				t.Fatal(err)
			}
		case <-time.After(time.Second):
			t.Fatal("startup transition did not complete")
		}
	}
	if h.settings.Mode != "files" {
		t.Fatal("initialization overwrote the chosen mode")
	}
}

func TestProjectionRegistrationAvailableDuringTransition(t *testing.T) {
	base := t.TempDir()
	h, err := Open(filepath.Join(base, "state"), filepath.Join(base, "cloud"), base)
	if err != nil {
		t.Fatal(err)
	}
	defer h.Close()
	server := adminsock.NewServer()
	h.Register(server)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	pipe := `\\.\pipe\Synctrain-Test-` + uuid.NewString()
	if err := server.Listen(ctx, pipe, false); err != nil {
		t.Fatal(err)
	}
	defer server.Close()
	h.mu.Lock()
	defer h.mu.Unlock()
	h.transitioning = true
	var result json.RawMessage
	if err := adminsock.CallContext(ctx, pipe, "projection.root.save", map[string]any{"Snapshot": map[string]any{"ClientID": "test", "RootKey": "fixture"}}, nil); err != nil {
		t.Fatal(err)
	}
	if err := adminsock.CallContext(ctx, pipe, "projection.root.get", nil, &result); err != nil {
		t.Fatal(err)
	}
	if !json.Valid(result) {
		t.Fatalf("invalid ownership response: %s", result)
	}
	if err := adminsock.CallContext(ctx, pipe, "projection.load", nil, &result); err != nil {
		t.Fatal(err)
	}
}

func TestDetachedCheckpointFailureRequestsRecovery(t *testing.T) {
	base := t.TempDir()
	h, err := Open(filepath.Join(base, "state"), filepath.Join(base, "cloud"), base)
	if err != nil {
		t.Fatal(err)
	}
	defer h.Close()
	if err := h.switchMode(context.Background(), "files"); err != nil {
		t.Fatal(err)
	}
	fatal := false
	h.OnFatalError = func(error) { fatal = true }
	h.OnModeChange = func(mode, id, root string) error {
		if mode == "" {
			path := filepath.Join(h.State, "projection.json")
			if err := os.Remove(path); err != nil {
				return err
			}
			return os.Mkdir(path, 0700)
		}
		return nil
	}
	if err := h.notifyMode(""); err == nil {
		t.Fatal("checkpoint write failure was ignored")
	}
	if !fatal {
		t.Fatal("lost native ownership did not request supervisor recovery")
	}
}

func TestRecoveredCaptureExportRejectsCloudJunction(t *testing.T) {
	base := t.TempDir()
	h := &Host{State: filepath.Join(base, "state"), Root: filepath.Join(base, "cloud")}
	captured := filepath.Join(h.State, "Captured")
	for _, path := range []string{captured, h.Root} {
		if err := os.MkdirAll(path, 0700); err != nil {
			t.Fatal(err)
		}
	}
	const name, body = "retained.snapshot", "the recovered edit stays private"
	if err := os.WriteFile(filepath.Join(captured, name), []byte(body), 0600); err != nil {
		t.Fatal(err)
	}
	alias := filepath.Join(base, "outside-junction")
	if output, err := exec.Command("cmd.exe", "/c", "mklink", "/J", alias, h.Root).CombinedOutput(); err != nil {
		t.Fatalf("create the NTFS junction: %s %v", output, err)
	}
	if err := h.exportCapture(name, filepath.Join(alias, "recovered.txt")); err == nil || !strings.Contains(err.Error(), "outside") {
		parent, parentErr := filepath.EvalSymlinks(alias)
		root, rootErr := filepath.EvalSymlinks(h.Root)
		t.Logf("resolved parent=%q (%v), root=%q (%v)", parent, parentErr, root, rootErr)
		t.Fatalf("export through the cloud junction was not rejected: %v", err)
	}
	if _, err := os.Stat(filepath.Join(h.Root, "recovered.txt")); !os.IsNotExist(err) {
		t.Fatalf("rejected export wrote into the cloud root: %v", err)
	}
	destination := filepath.Join(base, "safe-copy.txt")
	if err := h.exportCapture(name, destination); err != nil {
		t.Fatal("export outside the cloud root:", err)
	}
	for _, path := range []string{destination, filepath.Join(captured, name)} {
		if data, err := os.ReadFile(path); err != nil || string(data) != body {
			t.Fatalf("capture/export content %q: %q %v", path, data, err)
		}
	}
}

func TestGatewayAddressSetting(t *testing.T) {
	name, _ := os.Hostname()
	for text, want := range map[string]struct {
		host string
		port int
	}{
		"":                               {"", 0},
		"studio.example.org":             {"studio.example.org", 0},
		"studio.example.org:20920":       {"studio.example.org", 0},
		"studio.example.org:443":         {"studio.example.org", 443},
		"[fd00::1]:8443":                 {"fd00::1", 8443},
		strings.ToLower(name):            {"", 0},
		strings.ToUpper(name) + ":20921": {"", 20921},
	} {
		host, port, err := gatewayAddressSetting(text)
		if err != nil || host != want.host || port != want.port {
			t.Errorf("%q: %q, %d, %v; want %q, %d", text, host, port, err, want.host, want.port)
		}
	}
	if _, _, err := gatewayAddressSetting("studio example.org"); err == nil {
		t.Error("an address with a space was taken")
	}
	// MCP gives the host and the port apart, each kept when missing
	str := func(s string) *string { return &s }
	num := func(n int) *int { return &n }
	for _, c := range []struct {
		host        *string
		port        *int
		wantHost    string
		wantPort    int
		wantRefused bool
	}{
		{host: str("nas.example.org"), wantHost: "nas.example.org", wantPort: 443},
		{port: num(8443), wantHost: "studio.example.org", wantPort: 8443},
		{port: num(20920), wantHost: "studio.example.org"},
		{host: str(""), port: num(0)},
		{host: str(strings.ToUpper(name)), wantPort: 443},
		{host: str("[fd00::1]"), wantHost: "fd00::1", wantPort: 443},
		{host: str("nas.example.org:8443"), wantRefused: true},
		{host: str("https://nas.example.org"), wantRefused: true},
		{host: str("nas example.org"), wantRefused: true},
		{port: num(70000), wantRefused: true},
	} {
		host, port, err := mergeGatewayAddress("studio.example.org", 443, c.host, c.port)
		if c.wantRefused != (err != nil) || !c.wantRefused && (host != c.wantHost || port != c.wantPort) {
			t.Errorf("%+v: %q, %d, %v", c, host, port, err)
		}
	}
	// Pairing codes always carry a host: this PC's name when none is set
	h := &Host{}
	if host, port := h.gatewayAddress(); host == "" || port != gatewayListenPort {
		t.Errorf("default address %q, %d", host, port)
	}
}
