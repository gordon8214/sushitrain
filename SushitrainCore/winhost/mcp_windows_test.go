// Copyright (C) 2026 Gordon
// SPDX-License-Identifier: MPL-2.0
package winhost

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"t-shaped.nl/sushitrain/v2/adminsock"
	"t-shaped.nl/sushitrain/v2/desktopapi"
	"t-shaped.nl/sushitrain/v2/mcpapi"
	"testing"
)

func TestNativeDesktopSettingsUseSharedEditorsWithoutMCP(t *testing.T) {
	base := t.TempDir()
	h, err := Open(filepath.Join(base, "state"), filepath.Join(base, "cloud"), base)
	if err != nil {
		t.Fatal(err)
	}
	defer h.Close()
	if err = h.switchMode(t.Context(), "synchronize"); err != nil {
		t.Fatal(err)
	}
	server := adminsock.NewServer()
	h.Register(server)
	defer server.Close()
	data, err := server.Invoke(t.Context(), "desktop.settings.get", json.RawMessage(`{"Section":"connections"}`))
	if err != nil {
		t.Fatal(err)
	}
	editor := data.(desktopapi.Editor)
	if editor.Token == "" || len(editor.Fields) == 0 {
		t.Fatalf("missing shared editor: %+v", editor)
	}
	values := map[string]string{}
	for _, field := range editor.Fields {
		values[field.Path] = field.Value
	}
	original := values["ConnectionLimitMax"]
	values["ConnectionLimitMax"] = "invalid-number"
	p, _ := json.Marshal(desktopapi.Request{Token: editor.Token, Values: values})
	if _, err = server.Invoke(t.Context(), "desktop.settings.save", p); err == nil {
		t.Fatal("invalid input accepted")
	}
	values["ConnectionLimitMax"] = original
	p, _ = json.Marshal(desktopapi.Request{Token: editor.Token, Values: values})
	if _, err = server.Invoke(t.Context(), "desktop.settings.save", p); err != nil {
		t.Fatalf("failed save lost its editor: %v", err)
	}
	if _, err = server.Invoke(t.Context(), "desktop.settings.save", p); err == nil {
		t.Fatal("successful save left its token active")
	}
	if err = h.switchMode(t.Context(), "files"); err != nil {
		t.Fatal(err)
	}
	if _, err = server.Invoke(t.Context(), "desktop.settings.close", p); err != nil {
		t.Fatalf("closing an expired editor after changing modes: %v", err)
	}
	if _, err = server.Invoke(t.Context(), "desktop.files.children", json.RawMessage(`{}`)); err != nil {
		t.Fatalf("native Files metadata without MCP: %v", err)
	}
}

func TestMCPHistoryRetainsNativeHandlersAndCurrentRoot(t *testing.T) {
	base := t.TempDir()
	h, err := Open(filepath.Join(base, "state"), filepath.Join(base, "cloud"), base)
	if err != nil {
		t.Fatal(err)
	}
	defer h.Close()
	if err = h.switchMode(t.Context(), "files"); err != nil {
		t.Fatal(err)
	}
	server := adminsock.NewServer()
	h.Register(server)
	defer server.Close()
	if _, err = server.Invoke(t.Context(), "mcp.settings", json.RawMessage(`{"enabled":true}`)); err != nil {
		t.Fatal(err)
	}
	data, err := server.Invoke(t.Context(), "mcp.call", json.RawMessage(`{"name":"synctrain_history_sources_list","arguments":{}}`))
	if err != nil {
		t.Fatal(err)
	}
	if result := data.(mcpapi.Result); result.Error != nil {
		t.Fatal(result.Error)
	}
	root := filepath.Join(base, "new-cloud")
	if err = os.MkdirAll(root, 0700); err != nil {
		t.Fatal(err)
	}
	if err = h.switchMode(t.Context(), "synchronize"); err != nil {
		t.Fatal(err)
	}
	if err = h.configureRoot(root); err != nil {
		t.Fatal(err)
	}
	p, _ := json.Marshal(map[string]any{"Destination": filepath.Join(root, "forbidden-export"), "ID": "unknown"})
	_, err = server.Invoke(context.Background(), "history.export", p)
	if err == nil || !strings.Contains(err.Error(), "outside") {
		t.Fatalf("current Explorer root was not protected: %v", err)
	}
}
