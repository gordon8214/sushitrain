// Copyright (C) 2026 Gordon
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this file,
// You can obtain one at https://mozilla.org/MPL/2.0/.

package remoteclient

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"

	"t-shaped.nl/sushitrain/v2/remoteapi"
)

const (
	// CorrelationHeader carries an ID the gateway logs with a request, so that its log and a client's can be matched
	CorrelationHeader = "Synctrain-Correlation-ID"
	tusVersion        = "1.0.0"
	// The largest JSON body accepted from a gateway
	maxControlBody = 4 << 20
	// How long a control request may take, beyond any long poll it asks for
	controlTimeout = 60 * time.Second
)

// CredentialFunc returns the client's credential for the gateway: "" when there is none (the client must pair again),
// or an error when it can't be read just now (a locked keyring), which is worth trying again later
type CredentialFunc func() (string, error)

// Client talks to one gateway. It is safe for concurrent use, and its requests share connections.
type Client struct {
	endpoint   Endpoint
	credential CredentialFunc
	http       *http.Client
	// UserAgent is sent with every request
	UserAgent string
}

// New returns a client of the gateway at the endpoint
func New(endpoint Endpoint, credential CredentialFunc) *Client {
	dialer := &net.Dialer{Timeout: 15 * time.Second, KeepAlive: 30 * time.Second}
	transport := &http.Transport{
		Proxy:                 http.ProxyFromEnvironment,
		DialContext:           dialer.DialContext,
		TLSClientConfig:       tlsConfig(endpoint),
		ForceAttemptHTTP2:     true,
		TLSHandshakeTimeout:   15 * time.Second,
		IdleConnTimeout:       90 * time.Second,
		MaxIdleConnsPerHost:   6,
		ExpectContinueTimeout: time.Second,
	}
	return &Client{
		endpoint:   endpoint,
		credential: credential,
		http: &http.Client{
			Transport: transport,
			// The gateway never redirects /v1 paths: a redirect is answered as it is, and is a protocol violation
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		},
		UserAgent: "synctrain-client",
	}
}

// Endpoint is the gateway the client talks to
func (c *Client) Endpoint() Endpoint {
	return c.endpoint
}

// Close closes idle connections
func (c *Client) Close() {
	c.http.CloseIdleConnections()
}

var errUntrusted = errors.New("the gateway's certificate is not the one its pairing code pinned")

// tlsConfig trusts a certificate whose public key one of the pins names, without checking its name or issuer (the
// gateway's own certificate is self-signed), or, without pins, a certificate the system trusts for the host
func tlsConfig(e Endpoint) *tls.Config {
	cfg := &tls.Config{MinVersion: tls.VersionTLS12, ServerName: e.Host}
	if len(e.Pins) > 0 {
		pins := slices.Clone(e.Pins)
		// The pin check below replaces chain and name verification
		cfg.InsecureSkipVerify = true
		cfg.VerifyConnection = func(cs tls.ConnectionState) error {
			if len(cs.PeerCertificates) > 0 &&
				slices.Contains(pins, remoteapi.SPKIHash(cs.PeerCertificates[0].RawSubjectPublicKeyInfo)) {
				return nil
			}
			return errUntrusted
		}
	}
	return cfg
}

type contextKey int

const correlationKey contextKey = 0

// WithCorrelationID makes the requests made with ctx carry the ID, instead of one each
func WithCorrelationID(ctx context.Context, id string) context.Context {
	return context.WithValue(ctx, correlationKey, id)
}

func newCorrelationID() string {
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// How a request is authorized
type authorization struct {
	// A scoped token (of a lease, an upload or a pairing request) instead of the credential
	token string
	none  bool
}

var (
	withCredential = authorization{}
	withNothing    = authorization{none: true}
)

func withToken(token string) authorization {
	if token == "" {
		return withCredential
	}
	return authorization{token: token}
}

// url is the URL of a path below the gateway's origin, each segment escaped
func (c *Client) url(segments []string, query url.Values) string {
	escaped := make([]string, len(segments))
	for i, s := range segments {
		escaped[i] = url.PathEscape(s)
	}
	u := c.endpoint.BaseURL() + "/" + strings.Join(escaped, "/")
	if len(query) > 0 {
		u += "?" + query.Encode()
	}
	return u
}

func (c *Client) newRequest(ctx context.Context, method string, segments []string, query url.Values,
	auth authorization, body io.Reader) (*http.Request, error) {
	req, err := http.NewRequestWithContext(ctx, method, c.url(segments, query), body)
	if err != nil {
		return nil, protocolError("building a request: %v", err)
	}
	id, _ := ctx.Value(correlationKey).(string)
	if id == "" {
		id = newCorrelationID()
	}
	req.Header.Set(CorrelationHeader, id)
	req.Header.Set("User-Agent", c.UserAgent)
	switch {
	case auth.none:
	case auth.token != "":
		req.Header.Set("Authorization", "Bearer "+auth.token)
	default:
		credential, err := c.credential()
		if err != nil {
			// The credential is still there, it just can't be read now: worth trying again rather than pairing anew
			return nil, &Error{Kind: KindUnavailable, RetryAfter: 10 * time.Second, Message: "the credential can't be read",
				Err: err}
		}
		if credential == "" {
			return nil, &Error{Kind: KindUnauthenticated, Message: "no credential for the gateway"}
		}
		req.Header.Set("Authorization", "Bearer "+credential)
	}
	return req, nil
}

// do sends a request and checks the response: a redirect is a protocol violation, and a status of 400 or above an
// Error (gone is the kind of a 410 without a known code). The caller closes the body of a response it gets.
func (c *Client) do(req *http.Request, gone Kind) (*http.Response, error) {
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, transportError(req.Context(), err)
	}
	if resp.StatusCode >= 300 && resp.StatusCode < 400 {
		resp.Body.Close()
		return nil, protocolError("the gateway redirected a request (%d)", resp.StatusCode)
	}
	if resp.StatusCode >= 400 {
		defer resp.Body.Close()
		body, _ := io.ReadAll(io.LimitReader(resp.Body, maxControlBody))
		return nil, statusError(resp, body, gone)
	}
	return resp, nil
}

func transportError(ctx context.Context, err error) *Error {
	var verification *tls.CertificateVerificationError
	var unknownAuthority x509.UnknownAuthorityError
	var hostname x509.HostnameError
	switch {
	case errors.Is(err, errUntrusted), errors.As(err, &verification), errors.As(err, &unknownAuthority),
		errors.As(err, &hostname):
		return &Error{Kind: KindUntrusted, Message: "the gateway's certificate is not trusted", Err: err}
	case errors.Is(ctx.Err(), context.Canceled):
		return &Error{Kind: KindCanceled, Message: "canceled", Err: err}
	default:
		return &Error{Kind: KindUnreachable, Message: "the gateway can't be reached", Err: err}
	}
}

// readBody reads a bounded body
func readBody(resp *http.Response, limit int64) ([]byte, error) {
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, limit+1))
	if err != nil {
		return nil, transportError(resp.Request.Context(), err)
	}
	if int64(len(data)) > limit {
		return nil, protocolError("the response is larger than allowed")
	}
	return data, nil
}

// call sends a control request with an optional JSON body and decodes the JSON answer into out
func (c *Client) call(ctx context.Context, method string, segments []string, query url.Values, auth authorization,
	in, out any, gone Kind, wait time.Duration) (int, error) {
	ctx, cancel := context.WithTimeout(ctx, controlTimeout+wait)
	defer cancel()
	var body io.Reader
	if in != nil {
		var encoded []byte
		switch v := in.(type) {
		case []byte:
			encoded = v
		default:
			var err error
			if encoded, err = json.Marshal(in); err != nil {
				return 0, protocolError("encoding a request: %v", err)
			}
		}
		body = bytes.NewReader(encoded)
	}
	req, err := c.newRequest(ctx, method, segments, query, auth, body)
	if err != nil {
		return 0, err
	}
	if in != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.do(req, gone)
	if err != nil {
		return 0, err
	}
	data, err := readBody(resp, maxControlBody)
	if err != nil {
		return resp.StatusCode, err
	}
	if out != nil {
		if err := json.Unmarshal(data, out); err != nil {
			return resp.StatusCode, protocolError("invalid %T: %v", out, err)
		}
	}
	return resp.StatusCode, nil
}

// Capabilities is what the gateway is and supports
func (c *Client) Capabilities(ctx context.Context) (remoteapi.Capabilities, error) {
	var caps remoteapi.Capabilities
	_, err := c.call(ctx, "GET", []string{"v1", "capabilities"}, nil, withCredential, nil, &caps, KindOther, 0)
	return caps, err
}

// Folders are the folders this client may access
func (c *Client) Folders(ctx context.Context) ([]remoteapi.Folder, error) {
	var list remoteapi.FoldersResponse
	_, err := c.call(ctx, "GET", []string{"v1", "folders"}, nil, withCredential, nil, &list, KindOther, 0)
	return list.Folders, err
}

// Snapshot returns a page of a folder's snapshot: with snapshot and page empty, the first page of a new session
func (c *Client) Snapshot(ctx context.Context, folderID, snapshot, page string, limit int) (remoteapi.SnapshotPage,
	error) {
	query := url.Values{}
	if snapshot != "" {
		query.Set("snapshot", snapshot)
	}
	if page != "" {
		query.Set("page", page)
	}
	if limit > 0 {
		query.Set("limit", strconv.Itoa(limit))
	}
	var result remoteapi.SnapshotPage
	_, err := c.call(ctx, "GET", []string{"v1", "folders", folderID, "snapshot"}, query, withCredential, nil, &result,
		KindSnapshotExpired, 0)
	return result, err
}

// Changes returns the changes after cursor; with wait, the gateway holds the request up to that long for one to arrive
func (c *Client) Changes(ctx context.Context, folderID, cursor string, limit int, wait time.Duration) (
	remoteapi.ChangesPage, error) {
	query := url.Values{"cursor": {cursor}}
	if limit > 0 {
		query.Set("limit", strconv.Itoa(limit))
	}
	if seconds := int(wait / time.Second); seconds > 0 {
		query.Set("wait", strconv.Itoa(seconds))
	}
	var result remoteapi.ChangesPage
	_, err := c.call(ctx, "GET", []string{"v1", "folders", folderID, "changes"}, query, withCredential, nil, &result,
		KindCursorExpired, wait)
	return result, err
}

// Item returns an item as it is now, with its availability
func (c *Client) Item(ctx context.Context, itemID string) (remoteapi.Item, error) {
	var item remoteapi.Item
	_, err := c.call(ctx, "GET", []string{"v1", "items", itemID}, nil, withCredential, nil, &item, KindOther, 0)
	return item, err
}

// Versions lists the versions the folder's versioning keeps of a file, newest first
func (c *Client) Versions(ctx context.Context, itemID string) (remoteapi.ItemVersionList, error) {
	var list remoteapi.ItemVersionList
	_, err := c.call(ctx, "GET", []string{"v1", "items", itemID, "versions"}, nil, withCredential, nil, &list,
		KindOther, 0)
	return list, err
}
