// Copyright (C) 2026 Gordon. SPDX-License-Identifier: MPL-2.0
package hostbridge

import (
	"context"
	"encoding/json"
	"t-shaped.nl/sushitrain/v2/adminsock"
	"testing"
)

func TestNativeCallDoesNotEnableOrExposeAssistantControl(t *testing.T) {
	api := adminsock.NewServer()
	defer api.Close()
	api.Register("engine.inspect", func(context.Context, json.RawMessage) (any, error) {
		return map[string]string{"path": "metadata-only"}, nil
	})
	server := &Server{admin: api}
	data, err := server.NativeCall("engine.inspect", []byte(`{}`))
	if err != nil || string(data) != `{"path":"metadata-only"}` {
		t.Fatalf("native result %s: %v", data, err)
	}
	for _, method := range []string{"mcp.settings", "mcp.call", "history.sources"} {
		if _, err = server.NativeCall(method, []byte(`{}`)); err == nil {
			t.Fatalf("unexpected method allowed: %s", method)
		}
	}
}
