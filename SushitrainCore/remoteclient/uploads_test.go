// Copyright (C) 2026 Gordon
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this file,
// You can obtain one at https://mozilla.org/MPL/2.0/.

package remoteclient

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync/atomic"
	"testing"
	"time"

	"t-shaped.nl/sushitrain/v2/remoteapi"
)

// tusServer is a gateway's upload endpoint that answers every append with status and body
func tusServer(t *testing.T, status int, body string) (*Client, *atomic.Int32) {
	t.Helper()
	var appends atomic.Int32
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case "HEAD":
			w.Header().Set("Upload-Offset", "0")
		case "PATCH":
			appends.Add(1)
			_, _ = io.Copy(io.Discard, r.Body)
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(status)
			_, _ = io.WriteString(w, body)
		}
	}))
	t.Cleanup(server.Close)
	host, port, _ := net.SplitHostPort(server.Listener.Addr().String())
	portNumber, _ := strconv.Atoi(port)
	pin := remoteapi.SPKIHash(server.Certificate().RawSubjectPublicKeyInfo)
	return New(Endpoint{ServiceID: "svc_x", Host: host, Port: portNumber, Pins: []string{pin}},
		func() (string, error) { return "stc_x", nil }), &appends
}

func TestUploadOfAShortSourceFails(t *testing.T) {
	client, appends := tusServer(t, http.StatusNoContent, "")
	err := client.SendUpload(context.Background(), remoteapi.Upload{Upload: "upl_x", Length: 10},
		bytes.NewReader([]byte("12345")), nil)
	if !errors.Is(err, io.ErrUnexpectedEOF) || appends.Load() != 0 {
		t.Fatalf("a source shorter than the upload: %v, %d appends", err, appends.Load())
	}
}

func TestUploadMismatchEndsTheUpload(t *testing.T) {
	client, appends := tusServer(t, StatusChecksumMismatch,
		`{"error":{"code":"upload_mismatch","message":"the upload doesn't match its digest"}}`)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	err := client.SendUpload(ctx, remoteapi.Upload{Upload: "upl_x", Length: 10},
		bytes.NewReader([]byte("0123456789")), nil)
	var e *Error
	if !errors.As(err, &e) || e.Code != remoteapi.CodeUploadMismatch || appends.Load() != 1 {
		t.Fatalf("a whole upload that doesn't match: %v, %d appends", err, appends.Load())
	}

	// A chunk's checksum mismatch is tried again, but not without end
	client, appends = tusServer(t, StatusChecksumMismatch, `{"error":{"code":"checksum_mismatch","message":"x"}}`)
	err = client.SendUpload(ctx, remoteapi.Upload{Upload: "upl_x", Length: 10},
		bytes.NewReader([]byte("0123456789")), nil)
	if err == nil || ctx.Err() != nil || appends.Load() != 6 {
		t.Fatalf("a chunk refused every time: %v, %d appends", err, appends.Load())
	}
}

func TestGatewayErrorsArePrintable(t *testing.T) {
	resp := &http.Response{StatusCode: http.StatusBadRequest, Header: http.Header{"Retry-After": {"99999999"}}}
	e := statusError(resp, []byte(`{"error":{"code":"bad","message":"\u001b]0;owned\u0007 ‮evil"}}`), KindOther)
	if bytes.ContainsAny([]byte(e.Message), "\x1b\x07‮") || e.RetryAfter != maxRetryAfter {
		t.Fatalf("%q, retry after %s", e.Message, e.RetryAfter)
	}
	finished := &Error{Kind: KindUnavailable, Status: 503, Operation: &remoteapi.Operation{State: remoteapi.OperationFailed}}
	if finished.Transient() {
		t.Fatal("an operation the gateway finished is transient")
	}
}
