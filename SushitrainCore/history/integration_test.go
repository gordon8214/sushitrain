// Copyright (C) 2026 Gordon
// SPDX-License-Identifier: MPL-2.0
package history

import (
	"context"
	"encoding/json"
	"fmt"
	"github.com/syncthing/syncthing/lib/config"
	"github.com/syncthing/syncthing/lib/versioner"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"t-shaped.nl/sushitrain/v2/files"
	"t-shaped.nl/sushitrain/v2/gateway/gatewaytest"
	"t-shaped.nl/sushitrain/v2/remoteapi"
	"testing"
	"time"
)

func testStore(t *testing.T) *Store {
	t.Helper()
	state := t.TempDir()
	s, err := openWithCredentials(state, t.TempDir(), &files.FileCredentials{Path: filepath.Join(state, "credentials.json")}, func() error { return nil })
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.Close)
	return s
}
func testPairServer(t *testing.T, handler http.HandlerFunc) string {
	t.Helper()
	server := httptest.NewTLSServer(handler)
	t.Cleanup(server.Close)
	host, port, _ := net.SplitHostPort(server.Listener.Addr().String())
	return fmt.Sprintf("synctrain+pair://%s:%s/?service=svc_history&secret=test&spki=%s", host, port, remoteapi.SPKIHash(server.Certificate().RawSubjectPublicKeyInfo))
}
func pairResponse(w http.ResponseWriter, r *http.Request) bool {
	w.Header().Set("Content-Type", "application/json")
	switch r.URL.Path {
	case "/v1/pairing":
		json.NewEncoder(w).Encode(remoteapi.PairingPending{Request: "req_1", PollToken: "poll", ServiceID: "svc_history"})
		return true
	case "/v1/pairing/req_1":
		json.NewEncoder(w).Encode(remoteapi.PairingResult{State: remoteapi.PairingStateApproved, Credential: "stc_approved", ServiceID: "svc_history"})
		return true
	}
	return false
}
func TestApprovedCredentialSurvivesCapabilitiesFailure(t *testing.T) {
	s := testStore(t)
	link := testPairServer(t, func(w http.ResponseWriter, r *http.Request) {
		if !pairResponse(w, r) {
			http.Error(w, "unavailable", 503)
		}
	})
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	source, err := s.Pair(ctx, link)
	if err != nil {
		t.Fatal(err)
	}
	if source.Endpoint.ServiceID != "svc_history" {
		t.Fatal(source)
	}
	if credential, err := s.credentials.Credential("svc_history"); err != nil || credential != "stc_approved" {
		t.Fatal("one-time credential lost", err)
	}
	sources, err := s.Sources()
	if err != nil || len(sources) != 1 {
		t.Fatal(sources, err)
	}
}
func TestStoreCloseCancelsPendingPair(t *testing.T) {
	s := testStore(t)
	entered := make(chan struct{})
	var once sync.Once
	link := testPairServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/pairing/req_1" {
			once.Do(func() { close(entered) })
			<-r.Context().Done()
			return
		}
		pairResponse(w, r)
	})
	done := make(chan error, 1)
	go func() { _, err := s.Pair(t.Context(), link); done <- err }()
	select {
	case <-entered:
	case <-time.After(2 * time.Second):
		t.Fatal("pairing did not begin")
	}
	s.Close()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("canceled pairing succeeded")
		}
	case <-time.After(time.Second):
		t.Fatal("pairing survived store close")
	}
}
func TestForgetCannotResurrectPendingPair(t *testing.T) {
	s := testStore(t)
	entered := make(chan struct{})
	approve := make(chan struct{})
	var once sync.Once
	link := testPairServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/pairing/req_1" {
			once.Do(func() { close(entered) })
			select {
			case <-approve:
			case <-r.Context().Done():
				return
			}
		}
		if !pairResponse(w, r) {
			http.Error(w, "unavailable", 503)
		}
	})
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() { _, err := s.Pair(ctx, link); done <- err }()
	select {
	case <-entered:
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	if err := s.update(ctx, Request{ID: "svc_history"}, true); err != nil {
		t.Fatal(err)
	}
	close(approve)
	if err := <-done; err == nil {
		t.Fatal("forgotten pairing succeeded")
	}
	list, err := s.Sources()
	if err != nil || len(list) != 0 {
		t.Fatal("forgotten source resurrected", list, err)
	}
}
func TestHistoryWithRealReadOnlyGateway(t *testing.T) {
	e := gatewaytest.NewEngine(t, "docs", func(f *config.FolderConfiguration) {
		f.Versioning = config.VersioningConfiguration{Type: "simple", Params: map[string]string{"keep": "10"}}
	})
	e.Write("report.txt", "current")
	archived := time.Now().Add(-time.Hour).Truncate(time.Second)
	path := filepath.Join(e.FolderPath, versioner.DefaultPath, versioner.TagFilename("report.txt", archived.In(time.Local).Format(versioner.TimeFormat)))
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("archived content"), 0600); err != nil {
		t.Fatal(err)
	}
	os.Chtimes(path, archived.Add(-time.Hour), archived.Add(-time.Hour))
	g := gatewaytest.NewGateway(t, e, "")
	g.Export(false)
	s := testStore(t)
	link := g.PairingLink()
	ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() { _, err := s.Pair(ctx, link); done <- err }()
	g.ApproveNext(map[string]string{"docs": "read"})
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	list, err := s.List(ctx, "docs", "report.txt")
	if err != nil || len(list) != 1 || list[0].Error != "" || len(list[0].Versions) != 1 {
		t.Fatalf("history: %#v %v", list, err)
	}
	v := list[0].Versions[0]
	request := Request{ID: list[0].Source.Endpoint.ServiceID, Folder: "docs", Path: "report.txt", Version: v.ID, ExpectedTag: v.EntityTag}
	local, err := s.Download(ctx, request)
	if err != nil {
		t.Fatal(err)
	}
	bytes, err := os.ReadFile(local)
	if err != nil || string(bytes) != "archived content" {
		t.Fatal(string(bytes), err)
	}
	request.Destination = filepath.Join(t.TempDir(), "copy.txt")
	if err = s.Export(ctx, request); err != nil {
		t.Fatal(err)
	}
	if err = s.Export(ctx, request); err == nil {
		t.Fatal("history export replaced an existing destination")
	}
	request.ExpectedTag = ""
	if _, err = s.Download(ctx, request); err == nil {
		t.Fatal("unbound archived version was accepted")
	}
	request.ExpectedTag = v.EntityTag
	if err = os.WriteFile(path, []byte("changed archive bytes"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err = s.Download(ctx, request); err == nil {
		t.Fatal("stale selected archive was accepted")
	}
}
