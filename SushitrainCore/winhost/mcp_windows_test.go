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
	"t-shaped.nl/sushitrain/v2/mcpapi"
	"testing"
)

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
