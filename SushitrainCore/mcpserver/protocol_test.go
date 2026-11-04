// Copyright (C) 2026 Gordon
// SPDX-License-Identifier: MPL-2.0
package mcpserver

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"t-shaped.nl/sushitrain/v2/adminsock"
	"t-shaped.nl/sushitrain/v2/mcpapi"
	"t-shaped.nl/sushitrain/v2/mcpcontrol"
)

func TestMCPProcess(t *testing.T) {
	if os.Getenv("SYNCTRAIN_MCP_PROCESS_TEST") != "1" {
		return
	}
	endpoint := os.Getenv("SYNCTRAIN_MCP_TEST_SOCKET")
	if err := New("fixture", SocketBackend(func() (string, error) { return endpoint, nil })).Run(context.Background(), &mcp.StdioTransport{}); err != nil {
		os.Exit(1)
	}
	os.Exit(0)
}
func subprocessClient(t *testing.T, socket string) (context.Context, *mcp.ClientSession) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	t.Cleanup(cancel)
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	command := exec.CommandContext(ctx, executable, "-test.run=^TestMCPProcess$")
	command.Env = append(os.Environ(), "SYNCTRAIN_MCP_PROCESS_TEST=1", "SYNCTRAIN_MCP_TEST_SOCKET="+socket)
	command.Stderr = os.Stderr
	client := mcp.NewClient(&mcp.Implementation{Name: "Synctrain acceptance client", Version: "1"}, nil)
	session, err := client.Connect(ctx, &mcp.CommandTransport{Command: command, TerminateDuration: 5 * time.Second}, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := session.Close(); err != nil {
			t.Error(err)
		}
	})
	return ctx, session
}
func invokeTool(t *testing.T, ctx context.Context, session *mcp.ClientSession, name string, args map[string]any) mcpapi.Result {
	t.Helper()
	result, err := session.CallTool(ctx, &mcp.CallToolParams{Name: "synctrain_" + name, Arguments: args})
	if err != nil {
		t.Fatal(err)
	}
	b, err := json.Marshal(result.StructuredContent)
	if err != nil {
		t.Fatal(err)
	}
	var value mcpapi.Result
	if err = json.Unmarshal(b, &value); err != nil {
		t.Fatal(err)
	}
	if result.IsError != (value.Error != nil) {
		t.Fatalf("error annotations disagree: %s", b)
	}
	return value
}
func TestRealStdioClientDiscoveryAndAccess(t *testing.T) {
	dir, err := os.MkdirTemp("", "mcp-")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)
	a := adminsock.NewServer()
	a.Register("engine.status", func(context.Context, json.RawMessage) (any, error) {
		return map[string]any{"state": "running", "password": "must-not-leak", "nanoseconds": int64(1793811023123456789)}, nil
	})
	control, err := mcpcontrol.Register(a, filepath.Join(dir, "settings.json"), func() string { return "synchronize" })
	if err != nil {
		t.Fatal(err)
	}
	socket := testSocket(t, dir)
	if err = a.Listen(t.Context(), socket, false); err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	ctx, session := subprocessClient(t, socket)
	tools, err := session.ListTools(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(tools.Tools) != len(mcpapi.Catalog()) {
		t.Fatal("catalog missing tools", len(tools.Tools))
	}
	for _, tool := range tools.Tools {
		if tool.Annotations == nil || tool.InputSchema == nil || tool.OutputSchema == nil {
			t.Fatal("incomplete discovery", tool.Name)
		}
	}
	resources, err := session.ListResources(ctx, nil)
	if err != nil || len(resources.Resources) != 4 {
		t.Fatal(resources, err)
	}
	if r := invokeTool(t, ctx, session, "status", nil); r.Error == nil || r.Error.Code != "disabled" {
		t.Fatal(r)
	}
	if err = control.Set(mcpcontrol.Settings{Enabled: true}); err != nil {
		t.Fatal(err)
	}
	wire, err := session.CallTool(ctx, &mcp.CallToolParams{Name: "synctrain_status"})
	if err != nil {
		t.Fatal(err)
	}
	if len(wire.Content) == 0 || !strings.Contains(wire.Content[0].(*mcp.TextContent).Text, "1793811023123456789") {
		t.Fatal("IPC lost integer precision", wire)
	}
	r := invokeTool(t, ctx, session, "status", nil)
	if r.Error != nil {
		t.Fatal(r)
	}
	if r.Data.(map[string]any)["password"] != "[redacted]" {
		t.Fatal("secret leaked", r)
	}
	if r := invokeTool(t, ctx, session, "status", map[string]any{"arbitrary": true}); r.Error == nil || r.Error.Code != "invalid_arguments" {
		t.Fatal(r)
	}
	if r := invokeTool(t, ctx, session, "storage_get", nil); r.Error == nil || r.Error.Code != "unsupported" {
		t.Fatal(r)
	}
	if err = control.Set(mcpcontrol.Settings{}); err != nil {
		t.Fatal(err)
	}
	if r := invokeTool(t, ctx, session, "status", nil); r.Error == nil || r.Error.Code != "disabled" {
		t.Fatal(r)
	}
}
