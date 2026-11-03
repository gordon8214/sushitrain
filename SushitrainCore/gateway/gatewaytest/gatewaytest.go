// Copyright (C) 2026 Gordon
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this file,
// You can obtain one at https://mozilla.org/MPL/2.0/.

// Package gatewaytest runs a real Syncthing engine and remote file service in-process, for tests of clients of the
// service in other packages. It follows the gateway package's own test harness, which can't be imported.
package gatewaytest

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/syncthing/syncthing/lib/config"
	"github.com/syncthing/syncthing/lib/events"
	"github.com/syncthing/syncthing/lib/protocol"
	"github.com/syncthing/syncthing/lib/svcutil"
	"github.com/syncthing/syncthing/lib/syncthing"
	"t-shaped.nl/sushitrain/v2/gateway"
)

// Engine is a running Syncthing instance with one folder of its own, shared with no other device
type Engine struct {
	t          testing.TB
	App        *syncthing.App
	Config     config.Wrapper
	Events     events.Logger
	ID         protocol.DeviceID
	Dir        string
	FolderID   string
	FolderPath string
	Backend    *gateway.EngineBackend
}

// NewEngine starts an engine with one folder; modify, if not nil, changes the folder's configuration first
func NewEngine(t testing.TB, folderID string, modify func(*config.FolderConfiguration)) *Engine {
	t.Helper()
	dir := t.TempDir()
	cert, err := syncthing.GenerateCertificate(filepath.Join(dir, "cert.pem"), filepath.Join(dir, "key.pem"))
	if err != nil {
		t.Fatal(err)
	}
	id := protocol.NewDeviceID(cert.Certificate[0])
	folderPath := filepath.Join(dir, "folder")
	if err := os.MkdirAll(filepath.Join(folderPath, ".stfolder"), 0o700); err != nil {
		t.Fatal(err)
	}

	cfg := config.New(id)
	cfg.GUI.Enabled = false
	cfg.Options.GlobalAnnEnabled = false
	cfg.Options.LocalAnnEnabled = false
	cfg.Options.RelaysEnabled = false
	cfg.Options.NATEnabled = false
	cfg.Options.CREnabled = false
	cfg.Options.URAccepted = -1
	cfg.Options.RawListenAddresses = []string{fmt.Sprintf("tcp://127.0.0.1:%d", freePort(t))}
	folder := cfg.Defaults.Folder.Copy()
	folder.ID = folderID
	folder.Label = folderID
	folder.Path = folderPath
	folder.FilesystemType = config.FilesystemTypeBasic
	folder.RescanIntervalS = 0
	folder.FSWatcherEnabled = false
	folder.Devices = []config.FolderDeviceConfiguration{{DeviceID: id}}
	if modify != nil {
		modify(&folder)
	}
	cfg.Folders = []config.FolderConfiguration{folder}

	ctx, cancel := context.WithCancel(context.Background())
	evLogger := events.NewLogger()
	go evLogger.Serve(ctx)
	wrapper := config.Wrap(filepath.Join(dir, "config.xml"), cfg, id, evLogger)
	served := make(chan struct{})
	go func() {
		defer close(served)
		_ = wrapper.Serve(ctx)
	}()
	sdb, err := syncthing.OpenDatabase(filepath.Join(dir, "index"), time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	app, err := syncthing.New(wrapper, sdb, evLogger, cert, syncthing.Options{})
	if err != nil {
		t.Fatal(err)
	}
	if err := app.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		app.Stop(svcutil.ExitSuccess)
		app.Wait()
		cancel()
		<-served
	})
	// Start doesn't wait for the model to start its folders, and until then a scan fails with "folder is not running"
	WaitFor(t, 30*time.Second, "the folder to start", func() bool {
		state, _, _ := app.Internals.FolderState(folderID)
		return state != ""
	})
	return &Engine{t: t, App: app, Config: wrapper, Events: evLogger, ID: id, Dir: dir, FolderID: folderID,
		FolderPath: folderPath, Backend: &gateway.EngineBackend{App: app, Config: wrapper, Events: evLogger, ID: id}}
}

// Path is where a slash-separated name in the folder is on disk
func (e *Engine) Path(name string) string {
	return filepath.Join(e.FolderPath, filepath.FromSlash(name))
}

// Write writes a file in the folder on disk, creating its directories; Gateway.Sync makes the service see it
func (e *Engine) Write(name, contents string) {
	e.t.Helper()
	if err := os.MkdirAll(filepath.Dir(e.Path(name)), 0o755); err != nil {
		e.t.Fatal(err)
	}
	if err := os.WriteFile(e.Path(name), []byte(contents), 0o644); err != nil {
		e.t.Fatal(err)
	}
}

// Read returns a file's contents on disk, and whether it exists
func (e *Engine) Read(name string) (string, bool) {
	data, err := os.ReadFile(e.Path(name))
	return string(data), err == nil
}

// Remove removes a file or directory in the folder on disk
func (e *Engine) Remove(name string) {
	e.t.Helper()
	if err := os.RemoveAll(e.Path(name)); err != nil {
		e.t.Fatal(err)
	}
}

// Scan has Syncthing scan the folder
func (e *Engine) Scan() {
	e.t.Helper()
	if err := e.App.Internals.ScanFolderSubdirs(e.FolderID, nil); err != nil {
		e.t.Fatal(err)
	}
}

// Gateway is the remote file service on a test engine, listening on loopback with a self-signed certificate
type Gateway struct {
	*gateway.Service
	t      testing.TB
	Engine *Engine
	Home   string
	// Where clients reach the service
	Host string
	Port int
}

// NewGateway starts the service on e, keeping its state in home (a new temporary directory when empty)
func NewGateway(t testing.TB, e *Engine, home string) *Gateway {
	t.Helper()
	if home == "" {
		home = t.TempDir()
	}
	g := &Gateway{t: t, Engine: e, Home: home, Host: "127.0.0.1"}
	g.Start()
	t.Cleanup(func() { g.Stop() })
	return g
}

// Start starts the service, on the port it had before if it was stopped
func (g *Gateway) Start() {
	g.t.Helper()
	if g.Service != nil {
		g.t.Fatal("the service is running")
	}
	s, err := gateway.New(gateway.Config{
		Home:          g.Home,
		ListenAddress: net.JoinHostPort(g.Host, strconv.Itoa(g.Port)),
		ServiceName:   "gateway.test",
		Logger:        slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn})),
		SpoolQuota:    64 << 20,
		Thumbnails:    gateway.NewCommandRenderer(),
	}, g.Engine.Backend)
	if err != nil {
		g.t.Fatal(err)
	}
	if err := s.Start(); err != nil {
		g.t.Fatal(err)
	}
	g.Service = s
	g.Port = s.Addr().(*net.TCPAddr).Port
}

// Stop stops the service, as an outage would; Start brings it back with its state
func (g *Gateway) Stop() {
	if g.Service != nil {
		_ = g.Service.Close()
		g.Service = nil
	}
}

// Export exports the engine's folder, for reading and writing or only reading, and brings its change log up to date
func (g *Gateway) Export(write bool) {
	g.t.Helper()
	if err := g.SetExport(g.Engine.FolderID, write); err != nil {
		g.t.Fatal(err)
	}
	g.Sync()
}

// Sync scans the engine's folder and brings the service's change log up to date with it
func (g *Gateway) Sync() {
	g.t.Helper()
	g.Engine.Scan()
	if err := g.ReconcileNow(context.Background(), g.Engine.FolderID); err != nil {
		g.t.Fatal(err)
	}
}

// PairingLink starts a pairing session and returns its synctrain+pair link
func (g *Gateway) PairingLink() string {
	g.t.Helper()
	session, err := g.StartPairing(g.Host, g.Port, g.SPKI())
	if err != nil {
		g.t.Fatal(err)
	}
	return session.URL
}

// ApproveNext waits for a client to ask to pair and approves it with the given grants ("read" or "write" per folder
// ID), returning the request
func (g *Gateway) ApproveNext(grants map[string]string) gateway.PendingPairing {
	g.t.Helper()
	var request gateway.PendingPairing
	WaitFor(g.t, 30*time.Second, "a pairing request", func() bool {
		pending, err := g.PendingPairings()
		if err != nil {
			g.t.Fatal(err)
		}
		if len(pending) == 0 {
			return false
		}
		request = pending[0]
		return true
	})
	if err := g.ApprovePairing(request.RequestID, grants); err != nil {
		g.t.Fatal(err)
	}
	return request
}

// WaitFor polls cond until it holds, failing the test after timeout
func WaitFor(t testing.TB, timeout time.Duration, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func freePort(t testing.TB) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port
}
