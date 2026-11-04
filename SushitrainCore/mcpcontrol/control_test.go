// Copyright (C) 2026 Gordon
// SPDX-License-Identifier: MPL-2.0
package mcpcontrol

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"t-shaped.nl/sushitrain/v2/adminsock"
	"t-shaped.nl/sushitrain/v2/mcpapi"
	"testing"
	"time"
)

func fixture(t *testing.T) (*Control, *adminsock.Server, string) {
	t.Helper()
	a := adminsock.NewServer()
	a.Register("engine.folders", func(context.Context, json.RawMessage) (any, error) {
		list := []any{}
		for i := 0; i < 205; i++ {
			list = append(list, map[string]any{"id": i})
		}
		return list, nil
	})
	a.Register("engine.status", func(context.Context, json.RawMessage) (any, error) {
		return map[string]any{"apiKey": "secret", "name": "Fixture"}, nil
	})
	path := filepath.Join(t.TempDir(), "mcp.json")
	c, err := Register(a, path, func() string { return "synchronize" })
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(a.Close)
	return c, a, path
}
func call(c *Control, name string, args map[string]any) mcpapi.Result {
	return c.Call(context.Background(), mcpapi.Call{Name: "synctrain_" + name, Arguments: args})
}
func TestDisabledAndPersistedEnablement(t *testing.T) {
	c, _, path := fixture(t)
	if r := call(c, "status", nil); r.Error == nil || r.Error.Code != "disabled" {
		t.Fatal(r)
	}
	if r := call(c, "capabilities", nil); r.Error != nil {
		t.Fatal(r)
	}
	if err := c.Set(Settings{Enabled: true}); err != nil {
		t.Fatal(err)
	}
	r := call(c, "status", nil)
	b, _ := json.Marshal(r)
	if strings.Contains(string(b), "secret") {
		t.Fatal("credential leaked", string(b))
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS != "windows" && info.Mode().Perm()&0077 != 0 {
		t.Fatal("settings permissions", info.Mode())
	}
	other, err := New(context.Background(), path, func() string { return "files" }, nil, func() []string { return nil })
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close()
	if !other.Settings().Enabled {
		t.Fatal("enablement not persisted")
	}
}
func TestPagingAndCursorBinding(t *testing.T) {
	c, _, _ := fixture(t)
	if err := c.Set(Settings{Enabled: true}); err != nil {
		t.Fatal(err)
	}
	r := call(c, "folders_list", nil)
	if r.Error != nil || len(r.Data.([]any)) != 100 || r.NextCursor == "" {
		t.Fatal(r)
	}
	if bad := call(c, "status", map[string]any{"cursor": r.NextCursor}); bad.Error == nil || bad.Error.Code != "cursor_expired" {
		t.Fatal(bad)
	}
	r = call(c, "folders_list", map[string]any{"cursor": r.NextCursor})
	if r.Error != nil || len(r.Data.([]any)) != 100 {
		t.Fatal(r)
	}
	r = call(c, "folders_list", map[string]any{"cursor": r.NextCursor})
	if r.Error != nil || len(r.Data.([]any)) != 5 || r.NextCursor != "" {
		t.Fatal(r)
	}
}
func TestDisableCancelsInFlightAndLongOperations(t *testing.T) {
	c, a, _ := fixture(t)
	if err := c.Set(Settings{Enabled: true}); err != nil {
		t.Fatal(err)
	}
	started := make(chan struct{})
	a.Register("engine.logs", func(ctx context.Context, _ json.RawMessage) (any, error) {
		close(started)
		<-ctx.Done()
		return nil, ctx.Err()
	})
	result := make(chan mcpapi.Result, 1)
	go func() { result <- call(c, "logs", nil) }()
	<-started
	if err := c.Set(Settings{}); err != nil {
		t.Fatal(err)
	}
	select {
	case r := <-result:
		if r.Error == nil {
			t.Fatal("disabled operation returned data")
		}
	case <-time.After(time.Second):
		t.Fatal("disable did not cancel the read")
	}
	if err := c.Set(Settings{Enabled: true}); err != nil {
		t.Fatal(err)
	}
	a.Register("engine.action", func(ctx context.Context, _ json.RawMessage) (any, error) { <-ctx.Done(); return nil, ctx.Err() })
	r := call(c, "rescan_all", nil)
	if r.Error != nil {
		t.Fatal(r)
	}
	id := r.Data.(map[string]any)["operationId"].(string)
	if r := call(c, "operation_cancel", map[string]any{"id": id}); r.Error != nil {
		t.Fatal(r)
	}
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		r := call(c, "operation_get", map[string]any{"id": id})
		if r.Data.(Operation).State == "canceled" {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("job did not reach canceled")
}
func TestRejectTraversalBeforeNativeDispatch(t *testing.T) {
	c, a, _ := fixture(t)
	if err := c.Set(Settings{Enabled: true}); err != nil {
		t.Fatal(err)
	}
	a.Register("engine.inspect", func(context.Context, json.RawMessage) (any, error) {
		t.Error("unsafe path reached native operation")
		return nil, nil
	})
	for _, path := range []string{"../private", "/etc/passwd", "a/../../b", "C:\\private", "a\\b"} {
		r := call(c, "file_inspect", map[string]any{"folder": "fixture", "path": path})
		if r.Error == nil || r.Error.Code != "invalid_path" {
			t.Fatal(path, r)
		}
	}
}
func TestRedactionPreservesEditorTokens(t *testing.T) {
	value := map[string]any{"Token": "editor-token", "Fields": []any{map[string]any{"Path": "apiKey", "Value": "key-secret", "IsSecret": true}}, "url": "http://localhost/file?signature=signed-secret", "log": "Authorization: Bearer access-secret password=another-secret synctrain+pair://private-invitation"}
	b, _ := json.Marshal(Sanitize(value, nil))
	text := string(b)
	for _, secret := range []string{"key-secret", "signed-secret", "access-secret", "another-secret", "private-invitation"} {
		if strings.Contains(text, secret) {
			t.Fatal("credential leaked", secret, text)
		}
	}
	if !strings.Contains(text, "editor-token") {
		t.Fatal("editor identity was lost")
	}
}

func TestLogsBoundedAndPrecisionPreserved(t *testing.T) {
	c, a, _ := fixture(t)
	a.Register("engine.logs", func(_ context.Context, b json.RawMessage) (any, error) {
		var p struct{ Offset int64 }
		if err := json.Unmarshal(b, &p); err != nil {
			return nil, err
		}
		if p.Offset != 1793811023123456789 {
			t.Errorf("offset lost precision: %d", p.Offset)
		}
		list := []any{}
		for range 100 {
			list = append(list, map[string]any{"When": int64(1793811023123456790), "Message": strings.Repeat("x", 8000) + ` {"password":"credential-secret"}`})
		}
		return list, nil
	})
	if err := c.Set(Settings{Enabled: true}); err != nil {
		t.Fatal(err)
	}
	r := call(c, "logs", map[string]any{"after": "1793811023123456789"})
	b, _ := json.Marshal(r)
	if r.Error != nil || r.NextCursor == "" || len(b) > mcpapi.MaxContent || strings.Contains(string(b), "credential-secret") {
		t.Fatalf("invalid bounded logs: %s", b)
	}
	if !strings.Contains(string(b), `"When":"1793811023123456790"`) {
		t.Fatal("timestamp lost precision", string(b))
	}
}

func TestNetworkErrorsHavePlatformIndependentCode(t *testing.T) {
	err := &url.Error{Op: "Get", URL: "http://localhost/folders", Err: &net.OpError{Op: "dial", Net: "tcp", Err: errors.New("connectex: No connection could be made because the target machine actively refused it")}}
	r := failed(err)
	if r.Error == nil || r.Error.Code != "gateway_offline" || !r.Error.Retryable {
		t.Fatalf("wrong network failure: %+v", r.Error)
	}
}

func TestAuthorizationValuesAndFilePayload(t *testing.T) {
	structured, _ := json.Marshal(Sanitize(map[string]any{"Proxy-Authorization": "Basic c2VjcmV0"}, nil))
	if strings.Contains(string(structured), "c2VjcmV0") {
		t.Fatal("proxy credential leaked", string(structured))
	}
	for _, header := range []string{"Authorization: Basic dXNlcjpwYXNzd29yZA==", "Proxy-Authorization: Basic cHJveHk6c2VjcmV0", `{"Authorization":"Basic cXVvdGVkOnNlY3JldA=="}`} {
		result := scrub(header, nil)
		if strings.Contains(result, "Basic") || !strings.Contains(result, "[redacted]") {
			t.Fatal(result)
		}
		if strings.Contains(ScrubError(errors.New(header), nil).Error(), "Basic") {
			t.Fatal("error leaked authorization")
		}
	}
	c, a, _ := fixture(t)
	payload := "password=hello\nAuthorization: Basic dXNlcjpwYXNz\n日本語"
	a.Register("engine.read", func(context.Context, json.RawMessage) (any, error) {
		return map[string]any{"content": payload, "bytes": len(payload), "offset": 0}, nil
	})
	if err := c.Set(Settings{Enabled: true}); err != nil {
		t.Fatal(err)
	}
	r := call(c, "file_read", map[string]any{"folder": "fixture", "path": "file"})
	if r.Error != nil {
		t.Fatal(r.Error)
	}
	data := r.Data.(map[string]any)
	if data["content"] != payload || data["bytes"].(json.Number).String() != fmt.Sprint(len(payload)) {
		t.Fatal(data)
	}
}

func TestNestedCollectionsRetainMetadataAndPageAllVersions(t *testing.T) {
	c, _, _ := fixture(t)
	versions := []any{}
	for i := range 300 {
		versions = append(versions, map[string]any{"id": i, "detail": strings.Repeat("v", 5000)})
	}
	for _, fixture := range []struct {
		name  string
		data  any
		field string
	}{
		{"synctrain_versions_list", map[string]any{"itemID": "selected", "versions": versions}, "versions"},
		{"synctrain_folder_differences", map[string]any{"folder": "fixture", "Needed": versions}, "Needed"},
		{"synctrain_history_versions_list", []any{map[string]any{"source": "nas", "versions": versions}, map[string]any{"source": "offline", "error": "offline", "versions": []any{}}}, "versions"},
	} {
		t.Run(fixture.name, func(t *testing.T) {
			query := mcpapi.Call{Name: fixture.name, Arguments: map[string]any{"limit": 500.0}}
			r := c.paginate(query, fixture.data)
			count := 0
			offline := false
			pages := 0
			for {
				if r.Error != nil {
					t.Fatal(r.Error)
				}
				encoded, _ := json.Marshal(r)
				if len(encoded) > mcpapi.MaxResult {
					t.Fatal("oversize page", len(encoded))
				}
				pages++
				if fixture.name == "synctrain_history_versions_list" {
					for _, source := range r.Data.([]any) {
						object := source.(map[string]any)
						count += len(object[fixture.field].([]any))
						if object["source"] == "offline" {
							offline = true
							if object["error"] != "offline" {
								t.Fatal(object)
							}
						}
					}
				} else {
					object := r.Data.(map[string]any)
					count += len(object[fixture.field].([]any))
					if len(object) != 2 {
						t.Fatal("metadata lost", object)
					}
				}
				if r.NextCursor == "" {
					break
				}
				r = c.next(query, r.NextCursor)
			}
			if count != 300 || pages < 2 || fixture.name == "synctrain_history_versions_list" && !offline {
				t.Fatal(count, pages, offline)
			}
		})
	}
}

func TestCompletedOperationHistoryDoesNotBlockNewWork(t *testing.T) {
	c, a, _ := fixture(t)
	a.Register("engine.action", func(context.Context, json.RawMessage) (any, error) { return nil, nil })
	if err := c.Set(Settings{Enabled: true}); err != nil {
		t.Fatal(err)
	}
	// Fill the bounded history with completed jobs and one still-active job.
	now := time.Now().UTC()
	c.mu.Lock()
	for i := range 127 {
		c.jobs[fmt.Sprint(i)] = &Operation{Finished: &now}
	}
	c.jobs["active"] = &Operation{State: "running"}
	c.mu.Unlock()
	for range 130 {
		r := call(c, "rescan_all", nil)
		if r.Error != nil {
			t.Fatal(r.Error)
		}
		id := r.Data.(map[string]any)["operationId"].(string)
		deadline := time.Now().Add(time.Second)
		for {
			c.mu.Lock()
			finished := c.jobs[id].Finished != nil
			c.mu.Unlock()
			if finished {
				break
			}
			if time.Now().After(deadline) {
				t.Fatal("operation did not finish")
			}
			time.Sleep(time.Millisecond)
		}
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.jobs) != 128 || c.jobs["active"] == nil {
		t.Fatal("active work was evicted")
	}
	// Avoid a fake active record without a cancellation callback during cleanup.
	delete(c.jobs, "active")
}
