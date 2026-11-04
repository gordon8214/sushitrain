// Copyright (C) 2026 Gordon
// SPDX-License-Identifier: MPL-2.0
package mcpserver

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/syncthing/syncthing/lib/config"
	"t-shaped.nl/sushitrain/v2/adminsock"
	"t-shaped.nl/sushitrain/v2/files"
	"t-shaped.nl/sushitrain/v2/gateway/gatewaytest"
	"t-shaped.nl/sushitrain/v2/mcpapi"
	"t-shaped.nl/sushitrain/v2/mcpcontrol"
	"t-shaped.nl/sushitrain/v2/mcpfiles"
)

func finishOperation(t *testing.T, ctx context.Context, session *mcp.ClientSession, result mcpapi.Result) mcpapi.Result {
	t.Helper()
	if result.Error != nil {
		t.Fatal(result.Error)
	}
	data, ok := result.Data.(map[string]any)
	if !ok {
		return result
	}
	id, ok := data["operationId"].(string)
	if !ok {
		return result
	}
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		r := invokeTool(t, ctx, session, "operation_get", map[string]any{"id": id})
		if r.Error != nil {
			t.Fatal(r)
		}
		job := r.Data.(map[string]any)
		if job["state"] != "running" {
			b, _ := json.Marshal(job["result"])
			var completed mcpapi.Result
			if err := json.Unmarshal(b, &completed); err != nil {
				t.Fatal(err)
			}
			if completed.Error != nil {
				t.Fatal(completed.Error)
			}
			return completed
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("MCP operation did not finish within its expected 20 seconds")
	return mcpapi.Result{}
}
func TestRealClientFilesGatewayAndVersionRestore(t *testing.T) {
	// Normally 3–5 seconds. The suite budget is 90 seconds, including pairing
	// and real-engine cleanup; no production identities, roots, or ports are used.
	engine := gatewaytest.NewEngine(t, "docs", func(f *config.FolderConfiguration) {
		f.Versioning = config.VersioningConfiguration{Type: "trashcan", Params: map[string]string{"cleanoutDays": "0"}}
	})
	engine.Write("doc.txt", "first")
	gateway := gatewaytest.NewGateway(t, engine, "")
	gateway.Export(true)
	dir, err := os.MkdirTemp("", "mcp-files-")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)
	service, err := files.Open(files.Config{StateDir: filepath.Join(dir, "state"), CacheDir: filepath.Join(dir, "cache"), Credentials: &files.FileCredentials{Path: filepath.Join(dir, "credentials.json")}, AllowLocalGateway: true, SettleDelay: 10 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	defer service.Close()
	if err = service.Start(); err != nil {
		t.Fatal(err)
	}
	a := adminsock.NewServer()
	mcpfiles.Register(a, func(ctx context.Context, call func(context.Context, *files.Service, string) (any, error)) (any, error) {
		return call(ctx, service, filepath.Join(dir, "mount"))
	})
	control, err := mcpcontrol.Register(a, filepath.Join(dir, "mcp.json"), func() string { return "files" })
	if err != nil {
		t.Fatal(err)
	}
	if err = control.Set(mcpcontrol.Settings{Enabled: true}); err != nil {
		t.Fatal(err)
	}
	socket := testSocket(t, dir)
	if err = a.Listen(t.Context(), socket, false); err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	ctx, session := subprocessClient(t, socket)
	pairing := invokeTool(t, ctx, session, "gateway_pair", map[string]any{"link": gateway.PairingLink()})
	gateway.ApproveNext(map[string]string{"docs": "write"})
	paired := finishOperation(t, ctx, session, pairing)
	serviceID := paired.Data.(map[string]any)["serviceID"].(string)
	folder := invokeTool(t, ctx, session, "folder_add", map[string]any{"id": "docs", "serviceId": serviceID})
	if folder.Error != nil {
		t.Fatal(folder.Error)
	}
	folders, err := service.Folders()
	if err != nil || len(folders) != 1 {
		t.Fatal(folders, err)
	}
	if err = service.Refresh(ctx, folders[0].ID); err != nil {
		t.Fatal(err)
	}
	entry, err := service.Resolve(folders[0].DirName + "/doc.txt")
	if err != nil {
		t.Fatal(err)
	}
	if r := invokeTool(t, ctx, session, "file_pin", map[string]any{"item": entry.Item, "keep": false}); r.Error != nil {
		t.Fatal(r.Error)
	}
	if r := invokeTool(t, ctx, session, "transfers_list", nil); r.Error != nil {
		t.Fatal(r.Error)
	}
	read := invokeTool(t, ctx, session, "file_read", map[string]any{"item": entry.Item, "bytes": 5})
	if read.Error != nil || read.Data.(map[string]any)["content"] != "first" {
		t.Fatal(read)
	}
	source := filepath.Join(dir, "replacement.txt")
	if err = os.WriteFile(source, []byte("second"), 0600); err != nil {
		t.Fatal(err)
	}
	finishOperation(t, ctx, session, invokeTool(t, ctx, session, "file_replace", map[string]any{"item": entry.Item, "node": entry.Node, "source": source}))
	if err = service.Wait(ctx, entry.Item); err != nil {
		t.Fatal(err)
	}
	if content, ok := engine.Read("doc.txt"); !ok || content != "second" {
		t.Fatal("replacement did not reach engine", content)
	}
	versions := invokeTool(t, ctx, session, "versions_list", map[string]any{"item": entry.Item})
	if versions.Error != nil {
		t.Fatal(versions.Error)
	}
	data, _ := json.Marshal(versions.Data)
	var listing files.Versions
	if err = json.Unmarshal(data, &listing); err != nil || len(listing.Versions) != 1 {
		t.Fatal(string(data), err)
	}
	selected := listing.Versions[0]
	finishOperation(t, ctx, session, invokeTool(t, ctx, session, "version_restore", map[string]any{"item": entry.Item, "version": selected.ID, "token": selected.EntityTag}))
	if content, ok := engine.Read("doc.txt"); !ok || content != "first" {
		t.Fatal("version restore did not reach engine", content)
	}
	// Administer the running gateway through its own MCP endpoint, separate
	// from the Files client's narrower capabilities.
	admin := adminsock.NewServer()
	gateway.RegisterAdmin(admin, gateway.Host, gateway.Port)
	adminControl, err := mcpcontrol.Register(admin, filepath.Join(dir, "gateway-mcp.json"), func() string { return "synchronize" })
	if err != nil {
		t.Fatal(err)
	}
	if err = adminControl.Set(mcpcontrol.Settings{Enabled: true}); err != nil {
		t.Fatal(err)
	}
	adminDir := filepath.Join(dir, "admin")
	if err = os.Mkdir(adminDir, 0700); err != nil {
		t.Fatal(err)
	}
	adminSocket := testSocket(t, adminDir)
	if err = admin.Listen(t.Context(), adminSocket, false); err != nil {
		t.Fatal(err)
	}
	defer admin.Close()
	adminCtx, adminSession := subprocessClient(t, adminSocket)
	for _, name := range []string{"gateway_exports_list", "gateway_clients_list", "gateway_pairing_requests"} {
		if r := invokeTool(t, adminCtx, adminSession, name, nil); r.Error != nil {
			t.Fatal(r.Error)
		}
	}
	// A copied read result never grants write access after revocation.
	if r := invokeTool(t, adminCtx, adminSession, "gateway_permission_set", map[string]any{"id": serviceIDFromGateway(t, gateway), "folder": "docs", "access": "read"}); r.Error != nil {
		t.Fatal(r.Error)
	}
	if err = service.Refresh(ctx, folders[0].ID); err != nil {
		t.Fatal(err)
	}
	entry, err = service.Resolve(folders[0].DirName + "/doc.txt")
	if err != nil {
		t.Fatal(err)
	}
	denied := invokeTool(t, ctx, session, "file_replace", map[string]any{"item": entry.Item, "node": entry.Node, "source": source})
	if denied.Error == nil {
		data := denied.Data.(map[string]any)
		id := data["operationId"].(string)
		gatewaytest.WaitFor(t, time.Second, "permission denial", func() bool {
			job := invokeTool(t, ctx, session, "operation_get", map[string]any{"id": id})
			return job.Data.(map[string]any)["state"] == "failed"
		})
	}
	gateway.Stop()
	offline := invokeTool(t, ctx, session, "gateway_folders_list", map[string]any{"serviceId": serviceID})
	if offline.Error == nil || offline.Error.Code != "gateway_offline" {
		t.Fatalf("offline gateway not reported: %+v", offline.Error)
	}
}
func serviceIDFromGateway(t *testing.T, g *gatewaytest.Gateway) string {
	t.Helper()
	clients, err := g.Clients()
	if err != nil || len(clients) != 1 {
		t.Fatal(clients, err)
	}
	return clients[0].ClientID
}
