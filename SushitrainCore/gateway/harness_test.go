// Copyright (C) 2026 Gordon
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this file,
// You can obtain one at https://mozilla.org/MPL/2.0/.

package gateway

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/syncthing/syncthing/lib/config"
	"github.com/syncthing/syncthing/lib/events"
	"github.com/syncthing/syncthing/lib/protocol"
	"github.com/syncthing/syncthing/lib/svcutil"
	"github.com/syncthing/syncthing/lib/syncthing"
)

// testEngine is a running Syncthing instance with one folder, for tests that need the real engine
type testEngine struct {
	t          *testing.T
	app        *syncthing.App
	cfg        config.Wrapper
	events     events.Logger
	id         protocol.DeviceID
	dir        string
	folderID   string
	folderPath string
	listen     string
	backend    *EngineBackend
}

func freePort(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port
}

func newTestEngine(t *testing.T, folderID string, modify func(*config.FolderConfiguration)) *testEngine {
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
	listen := fmt.Sprintf("tcp://127.0.0.1:%d", freePort(t))

	cfg := config.New(id)
	cfg.GUI.Enabled = false
	cfg.Options.GlobalAnnEnabled = false
	cfg.Options.LocalAnnEnabled = false
	cfg.Options.RelaysEnabled = false
	cfg.Options.NATEnabled = false
	cfg.Options.CREnabled = false
	cfg.Options.URAccepted = -1
	cfg.Options.RawListenAddresses = []string{listen}
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
	e := &testEngine{t: t, app: app, cfg: wrapper, events: evLogger, id: id, dir: dir, folderID: folderID,
		folderPath: folderPath, listen: listen}
	e.backend = &EngineBackend{App: app, Config: wrapper, Events: evLogger, ID: id}
	return e
}

func (e *testEngine) write(name, contents string) {
	e.t.Helper()
	p := filepath.Join(e.folderPath, filepath.FromSlash(name))
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		e.t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(contents), 0o644); err != nil {
		e.t.Fatal(err)
	}
}

func (e *testEngine) read(name string) (string, bool) {
	data, err := os.ReadFile(filepath.Join(e.folderPath, filepath.FromSlash(name)))
	return string(data), err == nil
}

func (e *testEngine) scan() {
	e.t.Helper()
	if err := e.app.Internals.ScanFolderSubdirs(e.folderID, nil); err != nil {
		e.t.Fatal(err)
	}
}

// connect makes two engines share their folder
func connect(t *testing.T, a, b *testEngine) {
	t.Helper()
	for _, pair := range [][2]*testEngine{{a, b}, {b, a}} {
		self, peer := pair[0], pair[1]
		waiter, err := self.cfg.Modify(func(c *config.Configuration) {
			c.SetDevice(config.DeviceConfiguration{DeviceID: peer.id, Name: "peer", Addresses: []string{peer.listen}})
			folder, _, _ := c.Folder(self.folderID)
			folder.Devices = append(folder.Devices, config.FolderDeviceConfiguration{DeviceID: peer.id})
			c.SetFolder(folder)
		})
		if err != nil {
			t.Fatal(err)
		}
		waiter.Wait()
	}
}

// testService is a service on a test engine, with a client that trusts its self-signed certificate
type testService struct {
	*Service
	t      *testing.T
	e      *testEngine
	base   string
	client *http.Client
	home   string
}

func newTestService(t *testing.T, e *testEngine, home string) *testService {
	t.Helper()
	if home == "" {
		home = t.TempDir()
	}
	s, err := New(Config{
		Home:          home,
		ListenAddress: "127.0.0.1:0",
		ServiceName:   "gateway.test",
		Logger:        slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn})),
		SpoolQuota:    64 << 20,
	}, e.backend)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Start(); err != nil {
		t.Fatal(err)
	}
	ts := &testService{Service: s, t: t, e: e, home: home}
	t.Cleanup(func() { ts.Close() })
	certPEM, err := os.ReadFile(filepath.Join(home, "https-cert.pem"))
	if err != nil {
		t.Fatal(err)
	}
	pool := x509.NewCertPool()
	pool.AppendCertsFromPEM(certPEM)
	ts.client = &http.Client{
		Transport: &http.Transport{TLSClientConfig: &tls.Config{RootCAs: pool, ServerName: "gateway.test"}},
		Timeout:   2 * time.Minute,
	}
	ts.base = "https://" + s.Addr().String()
	return ts
}

func (ts *testService) do(method, path, token string, body any, headers ...string) (*http.Response, []byte) {
	ts.t.Helper()
	var reader io.Reader
	switch b := body.(type) {
	case nil:
	case []byte:
		reader = bytes.NewReader(b)
	default:
		encoded, err := json.Marshal(b)
		if err != nil {
			ts.t.Fatal(err)
		}
		reader = bytes.NewReader(encoded)
	}
	req, err := http.NewRequest(method, ts.base+path, reader)
	if err != nil {
		ts.t.Fatal(err)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	if _, raw := body.([]byte); body != nil && !raw {
		req.Header.Set("Content-Type", "application/json")
	}
	for i := 0; i+1 < len(headers); i += 2 {
		req.Header.Set(headers[i], headers[i+1])
	}
	resp, err := ts.client.Do(req)
	if err != nil {
		ts.t.Fatal(err)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		ts.t.Fatal(err)
	}
	return resp, data
}

func (ts *testService) json(method, path, token string, body any, want int, out any) {
	ts.t.Helper()
	resp, data := ts.do(method, path, token, body)
	if resp.StatusCode != want {
		ts.t.Fatalf("%s %s: status %d, want %d: %s", method, path, resp.StatusCode, want, data)
	}
	if out != nil {
		if err := json.Unmarshal(data, out); err != nil {
			ts.t.Fatalf("%s %s: %v: %s", method, path, err, data)
		}
	}
}

// pair pairs a client with the given folder grants and returns its credential
func (ts *testService) pair(grants map[string]string) string {
	ts.t.Helper()
	session, err := ts.StartPairing("127.0.0.1", 0, ts.SPKI())
	if err != nil {
		ts.t.Fatal(err)
	}
	var pending PairingPending
	ts.json("POST", "/v1/pairing", "", PairingRequest{Secret: session.Secret, ClientID: newUUID(), ClientName: "test"},
		http.StatusAccepted, &pending)
	if err := ts.ApprovePairing(pending.Request, grants); err != nil {
		ts.t.Fatal(err)
	}
	var result PairingResult
	ts.json("GET", "/v1/pairing/"+pending.Request, pending.PollToken, nil, http.StatusOK, &result)
	if result.Credential == "" {
		ts.t.Fatal("no credential")
	}
	return result.Credential
}

func newUUID() string {
	b := randomBytes(16)
	b[6] = b[6]&0x0f | 0x40
	b[8] = b[8]&0x3f | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:])
}

// export exports the engine's folder and reconciles it
func (ts *testService) export(write bool) {
	ts.t.Helper()
	if err := ts.SetExport(ts.e.folderID, write); err != nil {
		ts.t.Fatal(err)
	}
	ts.sync()
}

// sync scans the engine's folder and brings the change log up to date
func (ts *testService) sync() {
	ts.t.Helper()
	ts.e.scan()
	lock := ts.folderLock(ts.e.folderID)
	lock.Lock()
	if err := ts.reconcileLocked(context.Background(), ts.e.folderID); err != nil {
		ts.t.Fatal(err)
	}
	lock.Unlock()
	ts.retryPendingOperations(ts.e.folderID)
}

func (ts *testService) itemAt(token, p string) Item {
	ts.t.Helper()
	ts.sync()
	row, ok, err := liveItemByPath(ts.store.db, ts.e.folderID, p)
	if err != nil || !ok {
		ts.t.Fatalf("no item at %q (%v)", p, err)
	}
	var item Item
	ts.json("GET", "/v1/items/"+row.id, token, nil, http.StatusOK, &item)
	return item
}

// upload stages contents with tus and returns the upload ID and the digest operations name it by
func (ts *testService) upload(token string, contents []byte) (string, string) {
	ts.t.Helper()
	sum := sha256.Sum256(contents)
	meta := "folder " + base64.StdEncoding.EncodeToString([]byte(ts.e.folderID)) + ",digest " +
		base64.StdEncoding.EncodeToString(sum[:])
	resp, data := ts.do("POST", "/v1/uploads", token, []byte{}, "Tus-Resumable", tusVersion, "Upload-Length",
		strconv.Itoa(len(contents)), "Upload-Metadata", meta)
	if resp.StatusCode != http.StatusCreated {
		ts.t.Fatalf("create upload: %d %s", resp.StatusCode, data)
	}
	var u Upload
	if err := json.Unmarshal(data, &u); err != nil {
		ts.t.Fatal(err)
	}
	if len(contents) > 0 {
		resp, data = ts.do("PATCH", "/v1/uploads/"+u.Upload, u.Token, contents, "Tus-Resumable", tusVersion,
			"Content-Type", "application/offset+octet-stream", "Upload-Offset", "0")
		if resp.StatusCode != http.StatusNoContent {
			ts.t.Fatalf("patch upload: %d %s", resp.StatusCode, data)
		}
	}
	return u.Upload, b64.EncodeToString(sum[:])
}

func waitFor(t *testing.T, timeout time.Duration, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func hasPrefixItem(items []Item, prefix string) bool {
	for _, item := range items {
		if strings.HasPrefix(item.Path, prefix) {
			return true
		}
	}
	return false
}

// addFolder adds another folder of this device, shared with nobody, and returns its path
func (e *testEngine) addFolder(id string) string {
	e.t.Helper()
	path := filepath.Join(e.dir, id)
	if err := os.MkdirAll(filepath.Join(path, ".stfolder"), 0o700); err != nil {
		e.t.Fatal(err)
	}
	waiter, err := e.cfg.Modify(func(c *config.Configuration) {
		folder := c.Defaults.Folder.Copy()
		folder.ID, folder.Label, folder.Path = id, id, path
		folder.FilesystemType = config.FilesystemTypeBasic
		folder.RescanIntervalS = 0
		folder.FSWatcherEnabled = false
		folder.Devices = []config.FolderDeviceConfiguration{{DeviceID: e.id}}
		c.SetFolder(folder)
	})
	if err != nil {
		e.t.Fatal(err)
	}
	waiter.Wait()
	waitFor(e.t, 30*time.Second, "folder "+id, func() bool {
		return e.app.Internals.ScanFolderSubdirs(id, nil) == nil
	})
	return path
}

// syncFolder scans a folder and brings its change log up to date
func (ts *testService) syncFolder(folderID string) {
	ts.t.Helper()
	if err := ts.e.app.Internals.ScanFolderSubdirs(folderID, nil); err != nil {
		ts.t.Fatal(err)
	}
	lock := ts.folderLock(folderID)
	lock.Lock()
	defer lock.Unlock()
	if err := ts.reconcileLocked(context.Background(), folderID); err != nil {
		ts.t.Fatal(err)
	}
}
