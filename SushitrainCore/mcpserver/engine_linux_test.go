// Copyright (C) 2026 Gordon
// SPDX-License-Identifier: MPL-2.0
package mcpserver

import (
	"crypto/tls"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/syncthing/syncthing/lib/config"
	"github.com/syncthing/syncthing/lib/locations"
	"github.com/syncthing/syncthing/lib/protocol"
	"t-shaped.nl/sushitrain/v2/adminsock"
	"t-shaped.nl/sushitrain/v2/enginecontrol"
	"t-shaped.nl/sushitrain/v2/gateway/gatewaytest"
	"t-shaped.nl/sushitrain/v2/mcpcontrol"
	core "t-shaped.nl/sushitrain/v2/src"
)

func TestRealClientEngineManagementAndSettings(t *testing.T) {
	engine := gatewaytest.NewEngine(t, "native", nil)
	oldConfig, oldData := locations.GetBaseDir(locations.ConfigBaseDir), locations.GetBaseDir(locations.DataBaseDir)
	locations.SetBaseDir(locations.ConfigBaseDir, engine.Dir)
	locations.SetBaseDir(locations.DataBaseDir, engine.Dir)
	defer locations.SetBaseDir(locations.ConfigBaseDir, oldConfig)
	defer locations.SetBaseDir(locations.DataBaseDir, oldData)
	cert, err := tls.LoadX509KeyPair(filepath.Join(engine.Dir, "cert.pem"), filepath.Join(engine.Dir, "key.pem"))
	if err != nil {
		t.Fatal(err)
	}
	client, detach, err := core.AttachDesktopEngine(t.Context(), engine.App, engine.Config, engine.Events, cert)
	if err != nil {
		t.Fatal(err)
	}
	defer detach()
	a := adminsock.NewServer()
	control := enginecontrol.New(client, func() error { return nil })
	control.Register(a)
	defer control.Close()
	dir, err := os.MkdirTemp("", "mcp-engine-")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)
	access, err := mcpcontrol.Register(a, filepath.Join(dir, "mcp.json"), func() string { return "synchronize" })
	if err != nil {
		t.Fatal(err)
	}
	if err = access.Set(mcpcontrol.Settings{Enabled: true}); err != nil {
		t.Fatal(err)
	}
	socket := testSocket(t, dir)
	if err = a.Listen(t.Context(), socket, false); err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	ctx, session := subprocessClient(t, socket)
	for _, name := range []string{"status", "diagnostics", "statistics", "folders_list", "devices_list"} {
		if r := invokeTool(t, ctx, session, name, nil); r.Error != nil {
			t.Fatal(name, r.Error)
		}
	}
	editor := invokeTool(t, ctx, session, "settings_get", map[string]any{"section": "basic/folder/native"})
	if editor.Error != nil {
		t.Fatal(editor.Error)
	}
	waiter, err := engine.Config.Modify(func(c *config.Configuration) { f, _, _ := c.Folder("native"); f.MaxConflicts = 17; c.SetFolder(f) })
	if err != nil {
		t.Fatal(err)
	}
	waiter.Wait()
	if r := invokeTool(t, ctx, session, "settings_save", map[string]any{"token": editor.Data.(map[string]any)["Token"], "values": map[string]string{"label": "MCP fixture"}}); r.Error != nil {
		t.Fatal(r.Error)
	}
	f, _ := engine.Config.Folder("native")
	if f.Label != "MCP fixture" || f.MaxConflicts != 17 {
		t.Fatal("MCP overwrote a concurrent UI setting", f)
	}
	peer := protocol.NewDeviceID([]byte("isolated MCP peer")).String()
	if r := invokeTool(t, ctx, session, "device_add", map[string]any{"id": peer}); r.Error != nil {
		t.Fatal(r.Error)
	}
	for _, name := range []string{"device_pause", "device_resume", "device_remove"} {
		if r := invokeTool(t, ctx, session, name, map[string]any{"device": peer}); r.Error != nil {
			t.Fatal(r.Error)
		}
	}
	path := filepath.Join(dir, "new-folder")
	if err = os.Mkdir(path, 0700); err != nil {
		t.Fatal(err)
	}
	if r := invokeTool(t, ctx, session, "folder_add", map[string]any{"id": "added", "path": path, "name": "Added over MCP"}); r.Error != nil {
		t.Fatal(r.Error)
	}
	if _, ok := engine.Config.Folder("added"); !ok {
		t.Fatal("folder not persisted")
	}
	finishOperation(t, ctx, session, invokeTool(t, ctx, session, "rescan", map[string]any{"folder": "added"}))
	finishOperation(t, ctx, session, invokeTool(t, ctx, session, "folder_remove", map[string]any{"folder": "added"}))
	if _, ok := engine.Config.Folder("added"); ok {
		t.Fatal("removed folder persisted")
	}
	if _, err = os.Stat(path); err != nil {
		t.Fatal("configuration removal deleted files", err)
	}
	if r := invokeTool(t, ctx, session, "ignores_set", map[string]any{"folder": "native", "patterns": "ignored.tmp"}); r.Error != nil {
		t.Fatal(r.Error)
	}
	ignores, err := os.ReadFile(filepath.Join(engine.FolderPath, ".stignore"))
	if err != nil || !strings.Contains(string(ignores), "ignored.tmp") {
		t.Fatal("ignore patterns not persisted", string(ignores), err)
	}
	if r := invokeTool(t, ctx, session, "folder_differences", map[string]any{"folder": "native"}); r.Error != nil {
		t.Fatal(r.Error)
	}
	configData, err := os.ReadFile(filepath.Join(engine.Dir, "config.xml"))
	if err != nil {
		t.Fatal(err)
	}
	if len(configData) == 0 {
		t.Fatal("settings not saved to the owning engine configuration")
	}
}
