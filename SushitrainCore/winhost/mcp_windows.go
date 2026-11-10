// Copyright (C) 2026 Gordon
// SPDX-License-Identifier: MPL-2.0
package winhost

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"strconv"
	"strings"

	"t-shaped.nl/sushitrain/v2/adminsock"
	"t-shaped.nl/sushitrain/v2/enginecontrol"
	"t-shaped.nl/sushitrain/v2/files"
	"t-shaped.nl/sushitrain/v2/history"
	"t-shaped.nl/sushitrain/v2/mcpapi"
	"t-shaped.nl/sushitrain/v2/mcpcontrol"
	"t-shaped.nl/sushitrain/v2/mcpfiles"
	"t-shaped.nl/sushitrain/v2/remoteclient"
)

func (h *Host) registerMCP(server *adminsock.Server) error {
	// A private, unlistened facade normalizes MCP independently of Explorer's
	// stable provider protocol. Every operation takes the same host lease.
	api := adminsock.NewServer()
	if h.State != "" {
		sources, err := history.Open(filepath.Join(h.State, "History"), filepath.Join(h.State, "HistoryCache"))
		if err != nil {
			return err
		}
		historyAPI := adminsock.NewServer()
		sources.Register(historyAPI)
		historyAPI.OnClose(sources.Close)
		api.OnClose(historyAPI.Close)
		for _, method := range historyAPI.Methods() {
			handler := func(ctx context.Context, b json.RawMessage) (any, error) {
				ctx, finish, err := h.begin(ctx)
				if err != nil {
					return nil, err
				}
				defer finish()
				if method == "history.export" {
					var p history.Request
					if err := json.Unmarshal(b, &p); err != nil {
						return nil, err
					}
					return nil, sources.ExportWithForbidden(ctx, p, h.Root)
				}
				return historyAPI.Invoke(ctx, method, b)
			}
			api.Register(method, handler)
			server.Register(method, handler)
		}
	}
	probe := enginecontrol.New(nil, nil)
	probe.Register(api)
	names := []string{}
	for _, name := range api.Methods() {
		if strings.HasPrefix(name, "engine.") || strings.HasPrefix(name, "settings.") || name == "configuration.export" {
			names = append(names, name)
		}
	}
	probe.Close()
	for _, name := range names {
		handler := func(ctx context.Context, b json.RawMessage) (any, error) {
			ctx, finish, err := h.begin(ctx)
			if err != nil {
				return nil, err
			}
			defer finish()
			if h.client == nil {
				// Mode changes already dispose the previous engine's editors.
				if name == "settings.close" {
					return nil, nil
				}
				return nil, errors.New("the engine is not running in Files mode")
			}
			h.mcpMu.Lock()
			if h.mcpEngine == nil {
				h.mcpEngine = enginecontrol.New(h.client, h.client.RestartEngine)
			}
			control := h.mcpEngine
			h.mcpMu.Unlock()
			var p enginecontrol.Request
			if err := json.Unmarshal(b, &p); err != nil {
				return nil, err
			}
			return control.Call(ctx, name, p)
		}
		api.Register(name, handler)
		// Native UI access uses the host lease and existing engine validation, independently of assistant enablement.
		server.Register("desktop."+name, handler)
	}
	mcpfiles.Register(api, func(ctx context.Context, call func(context.Context, *files.Service, string) (any, error)) (any, error) {
		ctx, finish, err := h.begin(ctx)
		if err != nil {
			return nil, err
		}
		defer finish()
		if h.files == nil {
			return nil, errors.New("this machine is not in Files mode")
		}
		result, err := call(ctx, h.files, h.Root)
		if err == nil {
			h.revision.Add(1)
		}
		return result, err
	})
	// The native Files UI uses the same contained operations and optimistic identities as assistants.
	for _, method := range api.Methods() {
		if strings.HasPrefix(method, "mcp.files.") {
			server.Register("desktop."+strings.TrimPrefix(method, "mcp."), func(ctx context.Context, b json.RawMessage) (any, error) {
				return api.Invoke(ctx, method, b)
			})
		}
	}

	for method, native := range map[string]string{"exports.list": "gateway.exports", "exports.set": "gateway.export", "exports.remove": "gateway.unexport", "clients.list": "gateway.clients", "clients.revoke": "gateway.revoke", "clients.permission": "gateway.permission", "pairing.pending": "gateway.pending", "pairing.approve": "gateway.approve", "pairing.deny": "gateway.deny"} {
		api.Register(method, func(ctx context.Context, b json.RawMessage) (any, error) {
			var p struct {
				ClientID, FolderID, RequestID, Access string
				AllowWrite                            bool
				Grants                                json.RawMessage
			}
			if err := json.Unmarshal(b, &p); err != nil {
				return nil, err
			}
			id := p.ClientID
			if p.RequestID != "" {
				id = p.RequestID
			}
			params, _ := json.Marshal(request{ID: id, Folder: p.FolderID, Action: p.Access, Enabled: p.AllowWrite, Grants: p.Grants})
			return server.Invoke(ctx, native, params)
		})
	}
	api.Register("mcp.files.storage.budget", func(ctx context.Context, b json.RawMessage) (any, error) {
		var p struct{ Bytes int64 }
		if err := json.Unmarshal(b, &p); err != nil {
			return nil, err
		}
		params, _ := json.Marshal(request{Budget: p.Bytes})
		return server.Invoke(ctx, "files.budget", params)
	})
	api.Register("status", func(ctx context.Context, b json.RawMessage) (any, error) { return server.Invoke(ctx, "status", b) })
	api.Register("service.status", func(ctx context.Context, b json.RawMessage) (any, error) {
		ctx, finish, err := h.begin(ctx)
		if err != nil {
			return nil, err
		}
		defer finish()
		h.gatewayMu.Lock()
		defer h.gatewayMu.Unlock()
		h.optionsMu.Lock()
		defer h.optionsMu.Unlock()
		return map[string]any{"enabled": h.settings.Gateway, "running": h.gateway != nil, "listen": ":" + strconv.Itoa(gatewayListenPort), "publicHost": h.settings.GatewayHost, "publicPort": h.settings.GatewayPort, "address": remoteclient.FormatAddress(h.gatewayAddress()), "error": h.gatewayError}, nil
	})
	api.Register("service.set", func(ctx context.Context, b json.RawMessage) (any, error) {
		var p struct {
			Enabled    bool
			Listen     string
			PublicHost *string
			PublicPort *int
		}
		if err := json.Unmarshal(b, &p); err != nil {
			return nil, err
		}
		if p.Listen != "" && p.Listen != ":"+strconv.Itoa(gatewayListenPort) {
			return nil, fmt.Errorf("unsupported: Windows listens on :%d; set publicPort for another port a router forwards", gatewayListenPort)
		}
		if p.PublicHost != nil || p.PublicPort != nil {
			// Merged in the handler, under its lock, with what it has then
			params, _ := json.Marshal(request{PublicHost: p.PublicHost, PublicPort: p.PublicPort})
			if _, err := server.Invoke(ctx, "gateway.address", params); err != nil {
				return nil, err
			}
		}
		params, _ := json.Marshal(request{Enabled: p.Enabled})
		return server.Invoke(ctx, "gateway.enable", params)
	})
	control, err := mcpcontrol.New(server.Context(), filepath.Join(h.State, "mcp.json"), func() string { h.gate.Lock(); defer h.gate.Unlock(); return h.modeSnapshot }, api.Invoke, api.Methods)
	if err != nil {
		return err
	}
	server.Register("mcp.call", func(ctx context.Context, b json.RawMessage) (any, error) {
		if len(b) > mcpapi.MaxResult {
			return nil, errors.New("MCP request exceeds 1 MiB")
		}
		var call mcpapi.Call
		if err := json.Unmarshal(b, &call); err != nil {
			return nil, err
		}
		return control.Call(ctx, call), nil
	})
	server.Register("mcp.settings", func(ctx context.Context, b json.RawMessage) (any, error) {
		if len(b) > 0 && string(b) != "null" && string(b) != "{}" {
			var settings mcpcontrol.Settings
			if err := json.Unmarshal(b, &settings); err != nil {
				return nil, err
			}
			if err := control.Set(settings); err != nil {
				return nil, err
			}
		}
		return control.Settings(), nil
	})
	server.OnClose(func() { control.Close(); api.Close() })
	return nil
}
