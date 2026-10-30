// Copyright (C) 2026 Gordon
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this file,
// You can obtain one at https://mozilla.org/MPL/2.0/.

// These tests start the engine. Run them with:
//
//	go test -tags noassets,engineintegration -race -timeout 600s -run Engine ./src

//go:build engineintegration

package sushitrain

import (
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gofrs/flock"
	"github.com/syncthing/syncthing/lib/config"
	"github.com/syncthing/syncthing/lib/svcutil"

	"t-shaped.nl/sushitrain/v2/gateway"
	"t-shaped.nl/sushitrain/v2/sthome"
)

// stateRecorder is a client delegate that records the engine states it is told about
type stateRecorder struct {
	mutex  sync.Mutex
	states []string
	seen   int // states that waitFor has looked at
	added  chan struct{}
}

func newStateRecorder() *stateRecorder {
	return &stateRecorder{added: make(chan struct{}, 1)}
}

func (s *stateRecorder) OnEvent(string)                            {}
func (s *stateRecorder) OnFolderScanFinished(string)               {}
func (s *stateRecorder) OnDeviceDiscovered(string, *ListOfStrings) {}
func (s *stateRecorder) OnListenAddressesChanged(*ListOfStrings)   {}
func (s *stateRecorder) OnChange(*Change)                          {}
func (s *stateRecorder) OnMeasurementsUpdated()                    {}
func (s *stateRecorder) OnEngineStateChanged(state string, _ string) {
	s.mutex.Lock()
	s.states = append(s.states, state)
	s.mutex.Unlock()
	select {
	case s.added <- struct{}{}:
	default:
	}
}

// waitFor waits until the delegate was told about state, after the states an earlier waitFor found
func (s *stateRecorder) waitFor(t *testing.T, state string) {
	t.Helper()
	deadline := time.After(30 * time.Second)
	for {
		s.mutex.Lock()
		for i := s.seen; i < len(s.states); i++ {
			if s.states[i] == state {
				s.seen = i + 1
				s.mutex.Unlock()
				return
			}
		}
		s.mutex.Unlock()
		select {
		case <-s.added:
		case <-deadline:
			t.Fatalf("the engine did not become %s; states: %v", state, s.history())
		}
	}
}

func (s *stateRecorder) history() []string {
	s.mutex.Lock()
	defer s.mutex.Unlock()
	return slices.Clone(s.states)
}

// startTestEngine loads and starts an engine in a new directory, with no networking beyond a local listener and one
// folder, and stops it when the test ends
func startTestEngine(t *testing.T) (*Client, *stateRecorder, string) {
	t.Helper()
	return startTestEngineWith(t, func(*config.Configuration) {})
}

// startTestEngineWith starts an engine whose configuration was adjusted; its GUI stays off unless adjust turns it on
func startTestEngineWith(t *testing.T, adjust func(*config.Configuration)) (*Client, *stateRecorder, string) {
	t.Helper()
	clt, dir := newLoadTestClient(t)
	cert := testIdentity(t, dir)
	cfg := config.New(protocolID(cert))
	cfg.Options.GlobalAnnEnabled = false
	cfg.Options.LocalAnnEnabled = false
	cfg.Options.RelaysEnabled = false
	cfg.Options.NATEnabled = false
	cfg.Options.URAccepted = -1
	cfg.Options.RawListenAddresses = []string{"tcp://127.0.0.1:0"}
	cfg.GUI.Enabled = false
	folder := cfg.Defaults.Folder.Copy()
	folder.ID = "test"
	folder.Path = filepath.Join(dir, "folder")
	folder.FilesystemType = config.FilesystemTypeBasic
	folder.FSWatcherEnabled = false
	folder.RescanIntervalS = 0
	cfg.Folders = []config.FolderConfiguration{folder}
	if err := os.MkdirAll(folder.Path, 0o700); err != nil {
		t.Fatal(err)
	}
	adjust(&cfg)
	writeConfiguration(t, filepath.Join(dir, "config.xml"), cfg)
	// The marker keeps the GUI as adjust sets it
	writeGUIMarker(dir, guiMarker{})

	recorder := newStateRecorder()
	clt.Delegate = recorder
	if err := clt.Load(false); err != nil {
		t.Fatal(err)
	}
	if err := clt.Start(); err != nil {
		t.Fatal(err)
	}
	if clt.EngineState() != EngineStateRunning || clt.internals() == nil {
		t.Fatalf("not running: %s", clt.EngineState())
	}
	return clt, recorder, dir
}

func TestEngineRestartsInPlace(t *testing.T) {
	clt, recorder, dir := startTestEngine(t)
	server, measurements := clt.Server, clt.Measurements
	var goroutines int

	for i := range 6 {
		before := clt.currentRun()
		if err := clt.RestartEngine(); err != nil {
			t.Fatal(err)
		}
		after := clt.currentRun()
		if after == before || after.internals.Load() == nil || !before.isClosed() || clt.EngineState() != EngineStateRunning {
			t.Fatal("the engine was not replaced by a running one")
		}
		if i == 0 {
			time.Sleep(time.Second)
			goroutines = runtime.NumGoroutine()
		}
	}
	// Each restart is told in order
	recorder.waitFor(t, EngineStateRunning)
	for i, state := range recorder.history() {
		if want := []string{EngineStateRestarting, EngineStateRunning}[i%2]; state != want {
			t.Fatalf("state %d is %s, want %s: %v", i, state, want, recorder.history())
		}
	}
	if clt.Server != server || clt.Measurements != measurements {
		t.Fatal("the streaming server or the measurements were replaced")
	}

	// A change after the restart is applied and saved
	if err := clt.changeConfiguration(func(cfg *config.Configuration) { cfg.Options.MaxSendKbps = 7 }); err != nil {
		t.Fatal(err)
	}
	if saved := savedConfig(t, filepath.Join(dir, "config.xml"), *clt.cert); saved.Options.MaxSendKbps != 7 {
		t.Fatal("the change was not saved")
	}

	// The lock stays with the client
	if locked, err := flock.New(filepath.Join(dir, "syncthing.lock")).TryLock(); err != nil || locked {
		t.Fatalf("the lock was released: %v %v", locked, err)
	}

	// Each run's goroutines end with it
	time.Sleep(time.Second)
	if now := runtime.NumGoroutine(); now > goroutines+5 {
		t.Fatalf("%d goroutines after five more restarts, %d before", now, goroutines)
	}
}

func TestEngineShutdownRequestIsReported(t *testing.T) {
	clt, recorder, _ := startTestEngine(t)
	// As the web GUI's or the REST API's shutdown does
	clt.currentRun().app.Stop(svcutil.ExitSuccess)
	recorder.waitFor(t, EngineStateShutdownRequested)

	if err := clt.changeConfiguration(func(*config.Configuration) {}); !errors.Is(err, ErrEngineStopped) {
		t.Fatalf("a change of a stopped engine's configuration: %v", err)
	}
	clt.Stop()
	if clt.EngineState() != EngineStateStopped {
		t.Fatalf("state %s", clt.EngineState())
	}
}

func TestEngineRestartsAfterAShutdownRequest(t *testing.T) {
	clt, recorder, _ := startTestEngine(t)
	clt.currentRun().app.Stop(svcutil.ExitSuccess)
	recorder.waitFor(t, EngineStateShutdownRequested)
	if !clt.EngineCanRestart() {
		t.Fatal("cannot restart after a shutdown request")
	}
	if err := clt.RestartEngine(); err != nil {
		t.Fatal(err)
	}
	if clt.EngineState() != EngineStateRunning || clt.currentRun().isClosed() {
		t.Fatalf("state %s", clt.EngineState())
	}
}

func TestEngineFailedRestartCanBeRetried(t *testing.T) {
	clt, _, dir := startTestEngine(t)
	// Mark the database as written by a newer Syncthing, which the next run refuses
	db, err := sql.Open(gateway.SQLiteDriver, "file:"+filepath.Join(dir, "index-v2", "main.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	newer := sthome.SupportedSchemaVersion + 1
	if _, err := db.Exec(`INSERT INTO schemamigrations (schema_version, applied_at, syncthing_version) VALUES (?, 0, 'v9')`,
		newer); err != nil {
		t.Fatal(err)
	}

	if err := clt.RestartEngine(); err == nil {
		t.Fatal("restarted on a newer database")
	}
	if clt.EngineState() != EngineStateFailed || !clt.EngineCanRestart() {
		t.Fatalf("state %s", clt.EngineState())
	}
	if err := clt.changeConfiguration(func(*config.Configuration) {}); !errors.Is(err, ErrEngineStopped) {
		t.Fatalf("a change while failed: %v", err)
	}

	if _, err := db.Exec(`DELETE FROM schemamigrations WHERE schema_version = ?`, newer); err != nil {
		t.Fatal(err)
	}
	if err := clt.RestartEngine(); err != nil {
		t.Fatal(err)
	}
	if clt.EngineState() != EngineStateRunning || clt.StartupProblem() != "" {
		t.Fatalf("state %s, problem %q", clt.EngineState(), clt.StartupProblem())
	}
}

func TestEngineStopDuringARestart(t *testing.T) {
	clt, recorder, _ := startTestEngine(t)
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = clt.RestartEngine()
	}()
	clt.Stop()
	<-done
	recorder.waitFor(t, EngineStateStopped)
	time.Sleep(200 * time.Millisecond)
	history := recorder.history()
	if history[len(history)-1] != EngineStateStopped || clt.EngineState() != EngineStateStopped {
		t.Fatalf("not stopped last: %v", history)
	}
}

func TestEngineMaintenanceDoesNotWaitForAStoppedEngine(t *testing.T) {
	clt, recorder, _ := startTestEngine(t)
	clt.currentRun().app.Stop(svcutil.ExitSuccess)
	recorder.waitFor(t, EngineStateShutdownRequested)
	done := make(chan error, 1)
	go func() { done <- clt.PerformMaintenanceBlocking() }()
	select {
	case err := <-done:
		if !errors.Is(err, ErrEngineStopped) {
			t.Fatalf("got %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("maintenance waited for a stopped engine")
	}
}

func TestEngineStopIsNotAShutdownRequest(t *testing.T) {
	clt, recorder, _ := startTestEngine(t)
	clt.Stop()
	clt.Stop()
	recorder.waitFor(t, EngineStateStopped)
	time.Sleep(200 * time.Millisecond)
	recorder.mutex.Lock()
	defer recorder.mutex.Unlock()
	for _, state := range recorder.states {
		if state == EngineStateShutdownRequested {
			t.Fatal("stopping was reported as a shutdown request")
		}
	}
}

// folderConfigurationRecorder is a file provider delegate that counts folder configuration changes
type folderConfigurationRecorder struct {
	changes chan struct{}
}

func (f *folderConfigurationRecorder) OnFolderIndexChanged(string, bool) {}
func (f *folderConfigurationRecorder) OnFolderConfigurationChanged() {
	select {
	case f.changes <- struct{}{}:
	default:
	}
}

func TestEngineRestartKeepsTheFileProviderWatcher(t *testing.T) {
	clt, _, _ := startTestEngine(t)
	delegate := &folderConfigurationRecorder{changes: make(chan struct{}, 1)}
	clt.SetFileProviderDelegate(delegate)
	defer clt.SetFileProviderDelegate(nil)

	if err := clt.RestartEngine(); err != nil {
		t.Fatal(err)
	}
	// After the restart the delegate hears of every folder once, and then of changes
	select {
	case <-delegate.changes:
	case <-time.After(30 * time.Second):
		t.Fatal("the file provider delegate was not told about the restart")
	}
	if err := clt.FolderWithID("test").SetLabel("Renamed"); err != nil {
		t.Fatal(err)
	}
	select {
	case <-delegate.changes:
	case <-time.After(30 * time.Second):
		t.Fatal("the file provider delegate was not told about the change after the restart")
	}
}

// Calls made while the engine restarts fail or succeed, but never crash or hang
func TestEngineCanBeUsedWhileRestarting(t *testing.T) {
	clt, _, _ := startTestEngine(t)
	stop := make(chan struct{})
	var wg sync.WaitGroup
	for range 4 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				_ = clt.Folders()
				if folder := clt.FolderWithID("test"); folder != nil {
					_, _ = folder.State()
					_ = folder.SetLabel("Label")
				}
				_ = clt.ConnectedPeerCount()
			}
		}()
	}
	for range 3 {
		if err := clt.RestartEngine(); err != nil {
			t.Fatal(err)
		}
	}
	close(stop)
	wg.Wait()
}

func TestEngineServesTheRESTAPI(t *testing.T) {
	address := freeGUIAddress(t)
	clt, _, _ := startTestEngineWith(t, func(cfg *config.Configuration) {
		cfg.GUI.Enabled = true
		cfg.GUI.RawAddress = address
		protectGUI(&cfg.GUI)
	})
	if !clt.IsGUIEnabled() || clt.GUIAddress() != address || clt.GUIAPIKey() == "" {
		t.Fatalf("GUI on %v at %s", clt.IsGUIEnabled(), clt.GUIAddress())
	}
	client := &http.Client{Timeout: 5 * time.Second}
	get := func(path string, header ...string) (*http.Response, error) {
		request, err := http.NewRequest(http.MethodGet, clt.GUIURL()+path, nil)
		if err != nil {
			t.Fatal(err)
		}
		for i := 0; i+1 < len(header); i += 2 {
			request.Header.Set(header[i], header[i+1])
		}
		return client.Do(request)
	}
	response, err := get("rest/system/status", "X-API-Key", clt.GUIAPIKey())
	if err != nil {
		t.Fatal(err)
	}
	var body map[string]any
	_ = json.NewDecoder(response.Body).Decode(&body)
	response.Body.Close()
	if response.StatusCode != http.StatusOK || body["myID"] != clt.DeviceID() {
		t.Fatalf("status %d, %v", response.StatusCode, body)
	}
	for name, header := range map[string][]string{"a wrong key": {"X-API-Key", "wrong"}} {
		response, err := get("rest/system/status", header...)
		if err != nil {
			t.Fatal(err)
		}
		response.Body.Close()
		if response.StatusCode != http.StatusForbidden && response.StatusCode != http.StatusUnauthorized {
			t.Fatalf("%s got %d", name, response.StatusCode)
		}
	}

	// With a password, the web pages' CSRF token alone (which any local user can get) opens nothing
	page, err := get("")
	if err != nil {
		t.Fatal(err)
	}
	page.Body.Close()
	for _, cookie := range page.Cookies() {
		if strings.HasPrefix(cookie.Name, "CSRF-Token-") {
			response, err := get("rest/system/status", "X-"+cookie.Name, cookie.Value)
			if err != nil {
				t.Fatal(err)
			}
			response.Body.Close()
			if response.StatusCode == http.StatusOK {
				t.Fatal("the CSRF token alone opened the REST API")
			}
		}
	}

	// Turned off, they stop with the next start
	if err := clt.SetGUIEnabled(false); err != nil {
		t.Fatal(err)
	}
	if err := clt.RestartEngine(); err != nil {
		t.Fatal(err)
	}
	if _, err := get("rest/system/status", "X-API-Key", clt.GUIAPIKey()); err == nil {
		t.Fatal("still listening after a restart with the GUI off")
	}
}
