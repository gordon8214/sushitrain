// Copyright (C) 2026 Gordon
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this file,
// You can obtain one at https://mozilla.org/MPL/2.0/.

package gateway

import (
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

type failedPairingResponse struct {
	http.ResponseWriter
}

func (failedPairingResponse) Write([]byte) (int, error) {
	return 0, io.ErrClosedPipe
}

func TestPairingRetryAfterLostResponse(t *testing.T) {
	e := newTestEngine(t, "docs", nil)
	ts := newTestService(t, e, "")
	ts.export(true)
	clientID := newUUID()
	request := func() PairingPending {
		t.Helper()
		session, err := ts.StartPairing("127.0.0.1", 443, "")
		if err != nil {
			t.Fatal(err)
		}
		var pending PairingPending
		ts.json("POST", "/v1/pairing", "", PairingRequest{
			Secret: session.Secret, ClientID: clientID, ClientName: "phone",
		}, http.StatusAccepted, &pending)
		return pending
	}
	approve := func(pending PairingPending) {
		t.Helper()
		if err := ts.ApprovePairing(pending.Request, map[string]string{"docs": "write"}); err != nil {
			t.Fatal(err)
		}
	}
	pending := request()
	path := "/v1/pairing/" + pending.Request
	// The transaction commits before the response is written. Lose that response, as when a long poll's
	// connection goes away just as the host approves, then retry with the original poll token.
	w := failedPairingResponse{httptest.NewRecorder()}
	r := httptest.NewRequest("GET", path+"?wait=25", nil)
	r.SetPathValue("requestID", pending.Request)
	completed := make(chan error, 1)
	go func() {
		completed <- ts.handlePairingPoll(w, r, principal{kind: principalPairingPoll, resource: pending.Request})
	}()
	waitFor(t, 3*time.Second, "the pairing poll to wait for approval", func() bool {
		ts.pairingChanged.mutex.Lock()
		defer ts.pairingChanged.mutex.Unlock()
		return ts.pairingChanged.waiters[pending.Request] != nil
	})
	approve(pending)
	select {
	case err := <-completed:
		if !errors.Is(err, io.ErrClosedPipe) {
			t.Fatalf("lost response: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("approval did not wake the pairing poll")
	}
	var result PairingResult
	ts.json("GET", path, pending.PollToken, nil, http.StatusOK, &result)
	if result.State != pairingApproved || result.Credential == "" || result.ClientID != clientID {
		t.Fatal("retry did not recover the approved pairing")
	}
	credential := result.Credential
	if err := ts.Close(); err != nil {
		t.Fatal(err)
	}
	ts = newTestService(t, e, ts.home)
	// A retry must not rotate the credential, reapply grants, or record another pairing.
	if err := ts.SetPermission(clientID, "docs", "read"); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		ts.json("GET", path, pending.PollToken, nil, http.StatusOK, &result)
		if result.Credential != credential {
			t.Fatal("retry rotated the credential")
		}
	}
	var folders FoldersResponse
	ts.json("GET", "/v1/folders", credential, nil, http.StatusOK, &folders)
	if len(folders.Folders) != 1 || folders.Folders[0].Permissions.Write {
		t.Fatal("retry reapplied the original permissions")
	}
	var pairings int
	if err := ts.store.db.QueryRow(`SELECT COUNT(*) FROM audit WHERE client_id = ? AND action = 'pair'`, clientID).
		Scan(&pairings); err != nil || pairings != 1 {
		t.Fatalf("pairing audit count %d: %v", pairings, err)
	}
	// Re-pairing invalidates the earlier credential and its recovery path.
	replacement := request()
	approve(replacement)
	ts.json("GET", "/v1/pairing/"+replacement.Request, replacement.PollToken, nil, http.StatusOK, &result)
	if result.Credential == credential {
		t.Fatal("re-pairing reused the previous credential")
	}
	resp, _ := ts.do("GET", path, pending.PollToken, nil)
	if resp.StatusCode != http.StatusGone {
		t.Fatalf("superseded pairing: %d", resp.StatusCode)
	}
	resp, _ = ts.do("GET", "/v1/capabilities", credential, nil)
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("superseded credential: %d", resp.StatusCode)
	}
	// A revoked client cannot recover or reactivate its credential with a still-live poll token.
	if err := ts.RevokeClient(clientID); err != nil {
		t.Fatal(err)
	}
	resp, _ = ts.do("GET", "/v1/pairing/"+replacement.Request, replacement.PollToken, nil)
	if resp.StatusCode != http.StatusGone {
		t.Fatalf("revoked pairing: %d", resp.StatusCode)
	}
	// The poll token's original lifetime still bounds recovery; the issued credential stays valid afterward.
	pending = request()
	approve(pending)
	path = "/v1/pairing/" + pending.Request
	ts.json("GET", path, pending.PollToken, nil, http.StatusOK, &result)
	if _, err := ts.store.db.Exec(`UPDATE pairing_requests SET expires_at = ? WHERE request_id = ?`,
		ts.store.now().Unix()-1, pending.Request); err != nil {
		t.Fatal(err)
	}
	resp, _ = ts.do("GET", path, pending.PollToken, nil)
	if resp.StatusCode != http.StatusGone {
		t.Fatalf("expired pairing: %d", resp.StatusCode)
	}
	ts.json("GET", "/v1/capabilities", result.Credential, nil, http.StatusOK, nil)
}
