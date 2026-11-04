// Copyright (C) 2026 Gordon
// SPDX-License-Identifier: MPL-2.0
// Package hostbridge exposes a gomobile-compatible macOS host adapter.
package hostbridge

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"

	"t-shaped.nl/sushitrain/v2/adminsock"
	"t-shaped.nl/sushitrain/v2/enginecontrol"
	"t-shaped.nl/sushitrain/v2/mcpapi"
	"t-shaped.nl/sushitrain/v2/mcpcontrol"
	core "t-shaped.nl/sushitrain/v2/src"
)

// Delegate runs app-owned Swift operations; it is called off the main thread.
// Implementations must observe cancellation and never echo credential inputs.
type Delegate interface {
	Call(method string, params []byte, cancellation *Cancellation) ([]byte, error)
}
type Cancellation struct{ ctx context.Context }

func (c *Cancellation) IsCanceled() bool { return c.ctx.Err() != nil }
func (c *Cancellation) ReportProgress(completed, total int64) {
	mcpcontrol.ReportProgress(c.ctx, completed, total, "transferring")
}

type Server struct {
	admin   *adminsock.Server
	control *mcpcontrol.Control
	engine  *enginecontrol.Control
	socket  string
}

func NewServer(client *core.Client, delegate Delegate, directory string) (*Server, error) {
	if delegate == nil {
		return nil, errors.New("MCP requires an app delegate")
	}
	if err := os.MkdirAll(directory, 0700); err != nil {
		return nil, err
	}
	a := adminsock.NewServer()
	s := &Server{admin: a, socket: filepath.Join(directory, "control.sock")}
	if client != nil {
		s.engine = enginecontrol.New(client, client.RestartEngine)
		s.engine.Pause = func(ctx context.Context, ids []string, paused bool) error {
			b, _ := json.Marshal(map[string]any{"ids": ids, "paused": paused})
			_, err := delegate.Call("native.pause", b, &Cancellation{ctx: ctx})
			return err
		}
		s.engine.Register(a)
		for _, method := range []string{"settings.get", "settings.save", "settings.close"} {
			a.Register(method, func(ctx context.Context, b json.RawMessage) (any, error) {
				var p enginecontrol.Request
				if err := json.Unmarshal(b, &p); err != nil {
					return nil, err
				}
				if p.Section == "app" || p.Section == "files" || strings.HasPrefix(p.Token, "app:") {
					data, err := delegate.Call(strings.Replace(method, "settings.", "mcp.preferences.", 1), b, &Cancellation{ctx: ctx})
					return json.RawMessage(data), err
				}
				return s.engine.Call(ctx, method, p)
			})
		}
		a.OnClose(s.engine.Close)
	}
	// Only explicitly implemented native methods are advertised. The Swift
	// delegate returns this list without exposing credentials or app data.
	b, err := delegate.Call("native.methods", []byte("{}"), &Cancellation{ctx: a.Context()})
	if err != nil {
		a.Close()
		return nil, err
	}
	var methods []string
	if err = json.Unmarshal(b, &methods); err != nil {
		a.Close()
		return nil, err
	}
	for _, method := range methods {
		if method == "mcp.call" || method == "mcp.settings" || strings.HasPrefix(method, "engine.") {
			continue
		}
		a.Register(method, func(ctx context.Context, b json.RawMessage) (any, error) {
			data, err := delegate.Call(method, b, &Cancellation{ctx: ctx})
			if err != nil {
				return nil, err
			}
			if len(data) == 0 {
				return nil, nil
			}
			return json.RawMessage(data), nil
		})
	}
	mode := "files"
	if client != nil {
		mode = "synchronize"
	}
	s.control, err = mcpcontrol.Register(a, filepath.Join(directory, "settings.json"), func() string { return mode })
	if err != nil {
		a.Close()
		return nil, err
	}
	a.Authorize = authorizePeer
	if err = a.Listen(context.Background(), s.socket, false); err != nil {
		a.Close()
		return nil, err
	}
	return s, nil
}
func (s *Server) SetEnabled(enabled bool) error {
	return s.control.Set(mcpcontrol.Settings{Enabled: enabled})
}
func (s *Server) IsEnabled() bool { return s.control.Settings().Enabled }
func (s *Server) Socket() string  { return s.socket }
func (s *Server) Close()          { s.admin.Close() }

// CatalogJSON lets the native UI and documentation inspect the same definitions.
func CatalogJSON() ([]byte, error) { return json.Marshal(mcpapi.Catalog()) }
