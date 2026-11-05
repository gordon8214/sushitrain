// Copyright (C) 2026 Gordon. SPDX-License-Identifier: MPL-2.0
package linuxdesktop

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"t-shaped.nl/sushitrain/v2/adminsock"
	"t-shaped.nl/sushitrain/v2/gatewayadmin"
)

func TestExternalRemoteCopyKeepsItsFilenameAndLease(t *testing.T) {
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Write([]byte("remote document"))
	}))
	defer server.Close()
	params, _ := json.Marshal(map[string]any{
		"Location": server.URL, "Name": "document.docx", "Limit": 1024, "External": true,
	})
	result, err := Dispatch(t.Context(), "desktop.preview", params)
	if err != nil {
		t.Fatal(err)
	}
	local := result.(string)
	if filepath.Base(local) != "document.docx" {
		t.Fatal("external application lost the file extension", local)
	}
	if data, err := os.ReadFile(local); err != nil || string(data) != "remote document" {
		t.Fatal("remote content was not materialized", string(data), err)
	}
	if _, err := os.Stat(filepath.Join(filepath.Dir(local), ".external")); err != nil {
		t.Fatal("external application copy is not retained", err)
	}
}

func fakeCommands(t *testing.T, failure string) string {
	t.Helper()
	dir := t.TempDir()
	log := filepath.Join(dir, "calls")
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(dir, "config"))
	t.Setenv("SYNCTRAIN_TEST_LOG", log)
	t.Setenv("SYNCTRAIN_TEST_FAIL", failure)
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	script := `#!/bin/sh
echo "$0 $*" >> "$SYNCTRAIN_TEST_LOG"
case "$*" in *"$SYNCTRAIN_TEST_FAIL"*) exit 1;; esac
exit 0
`
	for _, name := range []string{"systemctl", "synctrain-gateway"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(script), 0700); err != nil {
			t.Fatal(err)
		}
	}
	return log
}
func TestFilesFailureRestoresSynchronize(t *testing.T) {
	log := fakeCommands(t, "enable --now synctrain-files.service")
	if err := WriteMode(modeSynchronize); err != nil {
		t.Fatal(err)
	}
	if err := Files(true); err == nil {
		t.Fatal("startup failure was hidden")
	}
	if ReadMode() != modeSynchronize {
		t.Fatal("mode changed before successful startup")
	}
	data, _ := os.ReadFile(log)
	if !strings.Contains(string(data), "enable --now synctrain-gateway.service") {
		t.Fatal(string(data))
	}
}
func TestTakeoverFailureBeforeEngineRestoresOldService(t *testing.T) {
	log := fakeCommands(t, "home --stop")
	_, err := Synchronize(gatewayadmin.Home{Dir: t.TempDir(), Exists: true}, false)
	if err == nil {
		t.Fatal("takeover failure was hidden")
	}
	data, _ := os.ReadFile(log)
	if !strings.Contains(string(data), "unmask syncthing.service") || !strings.Contains(string(data), "start syncthing.service") {
		t.Fatal(string(data))
	}
}
func TestFailureAfterEngineStartLeavesOldServiceMasked(t *testing.T) {
	log := fakeCommands(t, "enable --now synctrain-gateway.service")
	result, err := Synchronize(gatewayadmin.Home{Dir: t.TempDir(), Exists: true}, false)
	if err == nil || !result.GatewayStarted || result.Backup == "" {
		t.Fatalf("%+v: %v", result, err)
	}
	data, _ := os.ReadFile(log)
	if strings.Contains(string(data), "unmask syncthing.service") {
		t.Fatal("restarted an old engine after its database may have migrated")
	}
	if ReadMode() != "" {
		t.Fatal("failed transition persisted its mode")
	}
}
func TestRejectUnknownOperations(t *testing.T) {
	if _, err := Dispatch(context.Background(), "desktop.switch", []byte(`{"Mode":"invalid"}`)); err == nil {
		t.Fatal("invalid mode accepted")
	}
	if err := Service("disable"); err == nil {
		t.Fatal("arbitrary service command accepted")
	}
}

func TestPendingEditsPreventLeavingFiles(t *testing.T) {
	log := fakeCommands(t, "__no_failure__")
	if err := WriteMode(modeFiles); err != nil {
		t.Fatal(err)
	}
	socket := filepath.Join(t.TempDir(), "files.sock")
	t.Setenv("SYNCTRAIN_FILES_SOCKET", socket)
	server := adminsock.NewServer()
	server.Register("leave", func(context.Context, json.RawMessage) (any, error) {
		return nil, errors.New("pending edits have not finished sending")
	})
	if err := server.Listen(t.Context(), socket, false); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { server.Close() })
	if _, err := Synchronize(gatewayadmin.Home{Taken: true}, true); err == nil || !strings.Contains(err.Error(), "pending edits") {
		t.Fatalf("pending edits did not prevent the transition: %v", err)
	}
	if ReadMode() != modeFiles {
		t.Fatal("failed transition changed the mode")
	}
	data, _ := os.ReadFile(log)
	if strings.Contains(string(data), "disable --now synctrain-files.service") || strings.Contains(string(data), "enable --now synctrain-gateway.service") {
		t.Fatalf("changed services while edits were pending: %s", data)
	}
}
