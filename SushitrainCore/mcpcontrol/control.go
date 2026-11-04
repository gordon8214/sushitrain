// Copyright (C) 2026 Gordon
// SPDX-License-Identifier: MPL-2.0

// Package mcpcontrol enforces MCP access inside the process that owns the data.
package mcpcontrol

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"t-shaped.nl/sushitrain/v2/adminsock"
	"t-shaped.nl/sushitrain/v2/mcpapi"
)

type Settings struct {
	Enabled bool `json:"enabled"`
}

// DefaultSettingsPath is shared by the two Linux modes. Fixture processes can
// explicitly redirect it without reading or altering production preferences.
func DefaultSettingsPath() string {
	if p := os.Getenv("SYNCTRAIN_MCP_SETTINGS"); filepath.IsAbs(p) {
		return p
	}
	dir, _ := os.UserConfigDir()
	return filepath.Join(dir, "synctrain", "mcp.json")
}

type Operation struct {
	ID       string         `json:"id"`
	Tool     string         `json:"tool"`
	State    string         `json:"state"`
	Started  time.Time      `json:"started"`
	Finished *time.Time     `json:"finished,omitempty"`
	Result   *mcpapi.Result `json:"result,omitempty"`
	Progress Progress       `json:"progress"`
	cancel   context.CancelFunc
}
type page struct {
	Size       int
	Data       []any
	Wrap       func([]any) any
	HeaderSize int
	Key        string
	Expires    time.Time
}
type Control struct {
	mu       sync.Mutex
	settings Settings
	path     string
	mode     func() string
	invoke   func(context.Context, string, json.RawMessage) (any, error)
	methods  func() []string
	ctx      context.Context
	cancel   context.CancelFunc
	jobs     map[string]*Operation
	pages    map[string]page
	active   map[string]context.CancelFunc
	wg       sync.WaitGroup
}

// Register attaches an allowlisted MCP endpoint to an existing native API.
// The settings method is for native UI/CLI controls, never an MCP tool.
func Register(a *adminsock.Server, settingsPath string, mode func() string) (*Control, error) {
	c, err := New(a.Context(), settingsPath, mode, a.Invoke, a.Methods)
	if err != nil {
		return nil, err
	}
	a.Register("mcp.call", func(ctx context.Context, b json.RawMessage) (any, error) {
		var call mcpapi.Call
		if len(b) > mcpapi.MaxResult || json.Unmarshal(b, &call) != nil {
			return mcpapi.Result{Error: mcpapi.Failure("invalid_arguments", "Invalid MCP request.")}, nil
		}
		return c.Call(ctx, call), nil
	})
	a.Register("mcp.settings", func(ctx context.Context, b json.RawMessage) (any, error) {
		if len(b) > 0 && string(b) != "null" && string(b) != "{}" {
			var s Settings
			if err := json.Unmarshal(b, &s); err != nil {
				return nil, err
			}
			if err := c.Set(s); err != nil {
				return nil, err
			}
		}
		c.mu.Lock()
		defer c.mu.Unlock()
		return c.settings, nil
	})
	a.OnClose(c.Close)
	return c, nil
}
func New(ctx context.Context, path string, mode func() string, invoke func(context.Context, string, json.RawMessage) (any, error), methods func() []string) (*Control, error) {
	ctx, cancel := context.WithCancel(ctx)
	c := &Control{path: path, mode: mode, invoke: invoke, methods: methods, ctx: ctx, cancel: cancel, jobs: map[string]*Operation{}, pages: map[string]page{}, active: map[string]context.CancelFunc{}}
	b, err := os.ReadFile(path)
	if err == nil {
		err = json.Unmarshal(b, &c.settings)
	}
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		cancel()
		return nil, fmt.Errorf("MCP settings: %w", err)
	}
	return c, nil
}
func (c *Control) Settings() Settings { c.mu.Lock(); defer c.mu.Unlock(); return c.settings }

func (c *Control) Close() { c.mu.Lock(); c.cancel(); c.mu.Unlock(); c.wg.Wait() }
func (c *Control) Set(s Settings) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if err := os.MkdirAll(filepath.Dir(c.path), 0700); err != nil {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(c.path), ".mcp-settings-")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if err = f.Chmod(0600); err == nil {
		err = json.NewEncoder(f).Encode(s)
	}
	if err == nil {
		err = f.Sync()
	}
	closeErr := f.Close()
	if err == nil {
		err = closeErr
	}
	if err == nil {
		err = os.Rename(f.Name(), c.path)
	}
	if err != nil {
		return err
	}
	c.settings = s
	if !s.Enabled {
		for _, cancel := range c.active {
			cancel()
		}
		for _, op := range c.jobs {
			if op.State == "running" {
				op.cancel()
			}
		}
		clear(c.pages)
	}
	return nil
}
func (c *Control) Call(ctx context.Context, call mcpapi.Call) mcpapi.Result {
	t, ok := mcpapi.Find(call.Name)
	if !ok {
		return failed(mcpapi.Failure("unsupported", "Unknown Synctrain tool."))
	}
	if err := t.Validate(call.Arguments); err != nil {
		return failed(err)
	}
	c.mu.Lock()
	enabled := c.settings.Enabled
	c.prune()
	c.mu.Unlock()
	if call.Name == "synctrain_capabilities" {
		return mcpapi.Result{Data: c.capabilities(enabled)}
	}
	if !enabled {
		return failed(mcpapi.Failure("disabled", "Enable MCP in Synctrain Settings before accessing app data."))
	}
	if strings.HasPrefix(call.Name, "synctrain_operation_") {
		return c.operation(call)
	}
	if cursor, _ := call.Arguments["cursor"].(string); cursor != "" {
		return c.next(call, cursor)
	}
	r := t.Synchronize
	if c.mode() == "files" {
		r = t.Files
	}
	if call.Name == "synctrain_settings_get" && (call.Arguments["section"] == "app" || call.Arguments["section"] == "files") {
		r.Method = "mcp.preferences.get"
	}
	if token, ok := call.Arguments["token"].(string); ok && strings.HasPrefix(token, "app:") {
		if call.Name == "synctrain_settings_save" {
			r.Method = "mcp.preferences.save"
		}
		if call.Name == "synctrain_settings_close" {
			r.Method = "mcp.preferences.close"
		}
	}
	if r.Method == "" {
		return failed(mcpapi.Failure("unsupported", "This capability is not available in the current mode."))
	}
	available := false
	for _, method := range c.methods() {
		if method == r.Method {
			available = true
			break
		}
	}
	if !available {
		return failed(mcpapi.Failure("unsupported", "This host does not provide "+t.Name+". See synctrain://capabilities for platform coverage."))
	}
	if err := validatePaths(call); err != nil {
		return failed(err)
	}
	if t.Long {
		c.mu.Lock()
		if !c.settings.Enabled || c.ctx.Err() != nil {
			c.mu.Unlock()
			return failed(mcpapi.Failure("unavailable", "The host stopped or MCP was disabled."))
		}
		if len(c.jobs) >= 128 {
			oldest := ""
			for id, job := range c.jobs {
				if job.Finished != nil && (oldest == "" || job.Finished.Before(*c.jobs[oldest].Finished)) {
					oldest = id
				}
			}
			if oldest == "" {
				c.mu.Unlock()
				return failed(mcpapi.Failure("busy", "Too many active operations; wait for work to finish."))
			}
			delete(c.jobs, oldest)
		}
		jobCtx, cancel := context.WithTimeout(c.ctx, 11*time.Minute)
		op := &Operation{ID: uuid.NewString(), Tool: t.Name, State: "running", Started: time.Now().UTC(), cancel: cancel}
		op.Progress = Progress{Phase: "running"}
		jobCtx = context.WithValue(jobCtx, progressKey{}, func(progress Progress) {
			c.mu.Lock()
			defer c.mu.Unlock()
			if op.State == "running" {
				op.Progress = progress
			}
		})
		c.jobs[op.ID] = op
		c.wg.Add(1)
		id := op.ID
		c.mu.Unlock()
		go func() {
			defer c.wg.Done()
			defer cancel()
			result := c.execute(jobCtx, t, r, call)
			c.mu.Lock()
			defer c.mu.Unlock()
			now := time.Now().UTC()
			op.Finished = &now
			op.Result = &result
			op.State = "completed"
			op.Progress.Phase = "completed"
			if result.Error != nil {
				op.State = "failed"
				op.Progress.Phase = "failed"
				if result.Error.Code == "canceled" {
					op.State = "canceled"
					op.Progress.Phase = "canceled"
				}
			}
		}()
		return mcpapi.Result{Data: map[string]any{"operationId": id, "state": "running", "pollTool": "synctrain_operation_get"}}
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	return c.execute(ctx, t, r, call)
}
func (c *Control) execute(ctx context.Context, t mcpapi.Tool, r mcpapi.Route, call mcpapi.Call) mcpapi.Result {
	c.mu.Lock()
	if !c.settings.Enabled || c.ctx.Err() != nil {
		c.mu.Unlock()
		return failed(mcpapi.Failure("disabled", "MCP was disabled or the host stopped."))
	}
	ctx, cancel := context.WithCancel(ctx)
	id := uuid.NewString()
	c.active[id] = cancel
	c.mu.Unlock()
	stop := context.AfterFunc(c.ctx, cancel)
	defer func() { stop(); cancel(); c.mu.Lock(); delete(c.active, id); c.mu.Unlock() }()

	data, err := c.invoke(ctx, r.Method, r.Params(call.Arguments))
	if err != nil {
		return failed(ScrubError(err, call.Arguments))
	}
	if err := ctx.Err(); err != nil {
		return failed(err)
	}
	c.mu.Lock()
	enabled := c.settings.Enabled
	c.mu.Unlock()
	if !enabled {
		return failed(mcpapi.Failure("disabled", "MCP was disabled while the operation was running."))
	}
	data = sanitize(data, call.Arguments, call.Name == "synctrain_file_read")
	if t.Name == "synctrain_logs" {
		data = boundedLogs(data)
	}
	if t.ReadOnly {
		return c.paginate(call, data)
	}
	b, err := json.Marshal(data)
	if err != nil {
		return failed(err)
	}
	if len(b) > mcpapi.MaxResult {
		return failed(mcpapi.Failure("response_too_large", "Operation completed, but its result exceeds the MCP response limit. Inspect its state with a paginated read tool."))
	}
	return mcpapi.Result{Data: data}
}
func failed(err error) mcpapi.Result {
	var e *mcpapi.Error
	if errors.As(err, &e) {
		return mcpapi.Result{Error: e}
	}
	var network net.Error
	code := "operation_failed"
	message := err.Error()
	lower := strings.ToLower(message)
	switch {
	case errors.Is(err, context.Canceled):
		code = "canceled"
	case errors.Is(err, context.DeadlineExceeded):
		code = "timeout"
	case strings.Contains(lower, "unsupported"):
		code = "unsupported"
	case strings.Contains(lower, "not running"), strings.Contains(lower, "still loading"), strings.Contains(lower, "not in "):
		code = "unavailable"
	case strings.Contains(lower, "permission"), strings.Contains(lower, "read only"), strings.Contains(lower, "read-only"), strings.Contains(lower, "read_only"), strings.Contains(lower, "forbidden"):
		code = "permission_denied"
	case errors.As(err, &network):
		code = "gateway_offline"
	case strings.Contains(lower, "actively refused"), strings.Contains(lower, "offline"), strings.Contains(lower, "connection refused"), strings.Contains(lower, "no such host"):
		code = "gateway_offline"
	case strings.Contains(lower, "changed"), strings.Contains(lower, "revision"), strings.Contains(lower, "expired"):
		code = "stale_revision"
	}
	return mcpapi.Result{Error: &mcpapi.Error{Code: code, Message: message, Retryable: code == "gateway_offline" || code == "busy"}}
}
func (c *Control) operation(call mcpapi.Call) mcpapi.Result {
	c.mu.Lock()
	defer c.mu.Unlock()
	if call.Name == "synctrain_operation_list" {
		list := []any{}
		for _, v := range c.jobs {
			copy := *v
			copy.Result = nil
			list = append(list, copy)
		}
		return mcpapi.Result{Data: list}
	}
	id, _ := call.Arguments["id"].(string)
	op := c.jobs[id]
	if op == nil {
		return failed(mcpapi.Failure("not_found", "Operation not found; completed operations expire after one hour, when history fills, or after a host restart."))
	}
	if call.Name == "synctrain_operation_cancel" && op.State == "running" {
		op.cancel()
	}
	return mcpapi.Result{Data: *op}
}
func (c *Control) prune() {
	for id, op := range c.jobs {
		if op.Finished != nil && time.Since(*op.Finished) > time.Hour {
			delete(c.jobs, id)
		}
	}
	for id, p := range c.pages {
		if time.Now().After(p.Expires) {
			delete(c.pages, id)
		}
	}
}
func (c *Control) capabilities(enabled bool) any {
	tools := []any{}
	methods := map[string]bool{}
	for _, m := range c.methods() {
		methods[m] = true
	}
	for _, t := range mcpapi.Catalog() {
		r := t.Synchronize
		if c.mode() == "files" {
			r = t.Files
		}
		supported := methods[r.Method] || t.Name == "synctrain_capabilities" || strings.HasPrefix(t.Name, "synctrain_operation_")
		reason := ""
		if !supported {
			reason = "No supported native operation in this platform/mode."
		}
		tools = append(tools, map[string]any{"name": t.Name, "supported": supported, "reason": reason, "readOnly": t.ReadOnly, "destructive": t.Destructive})
	}
	return map[string]any{"platform": runtime.GOOS, "mode": c.mode(), "enabled": enabled, "transport": "stdio", "tools": tools, "limits": map[string]any{"resultBytes": mcpapi.MaxResult, "inlineBytes": mcpapi.MaxContent, "defaultPageSize": 100, "maximumPageSize": 500}, "lifecycle": "Start Synctrain yourself. Enabling MCP grants full management access. Disabling it cancels interruptible work. Committed changes remain committed."}
}
