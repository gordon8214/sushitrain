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
	"crypto/sha256"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"t-shaped.nl/sushitrain/v2/gateway/gatewaytest"
	"t-shaped.nl/sushitrain/v2/remoteapi"
)

func TestParseInvitation(t *testing.T) {
	inv, err := ParseInvitation("synctrain+pair://nas.local:20920/?service=svc_abc&secret=s3cret&spki=pin")
	if err != nil {
		t.Fatal(err)
	}
	if inv != (Invitation{Host: "nas.local", Port: 20920, ServiceID: "svc_abc", Secret: "s3cret", SPKI: "pin"}) {
		t.Fatalf("parsed %+v", inv)
	}
	if e := inv.Endpoint(); e.BaseURL() != "https://nas.local:20920" || len(e.Pins) != 1 || e.Pins[0] != "pin" {
		t.Fatalf("endpoint %+v", e)
	}
	inv, err = ParseInvitation(" SYNCTRAIN+PAIR://[fd00::1]:443/?service=svc_x&secret=y ")
	if err != nil || inv.Host != "fd00::1" || inv.Port != 443 || inv.SPKI != "" {
		t.Fatalf("IPv6: %+v, %v", inv, err)
	}
	if got := inv.Endpoint().BaseURL(); got != "https://[fd00::1]:443" {
		t.Fatalf("IPv6 base URL %s", got)
	}
	for _, bad := range []string{
		"https://nas.local:20920/?service=svc_abc&secret=s",
		"synctrain+pair://nas.local/?service=svc_abc&secret=s",
		"synctrain+pair://nas.local:0/?service=svc_abc&secret=s",
		"synctrain+pair://nas.local:20920/?service=svc_abc",
		"synctrain+pair://nas.local:20920/?secret=s",
		"synctrain+pair://:20920/?service=svc_abc&secret=s",
	} {
		if _, err := ParseInvitation(bad); err == nil {
			t.Errorf("%s parsed", bad)
		}
	}
}

// pair pairs a new client with the gateway, granting the folders, and returns a client with the credential
func pair(t *testing.T, g *gatewaytest.Gateway, grants map[string]string) *Client {
	t.Helper()
	inv, err := ParseInvitation(g.PairingLink())
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	type result struct {
		credential string
		err        error
	}
	done := make(chan result, 1)
	go func() {
		credential, err := Pair(ctx, inv, Pairing{ClientID: uuid.NewString(), ClientName: "test", Platform: "Linux"})
		done <- result{credential, err}
	}()
	request := g.ApproveNext(grants)
	if request.Platform != "Linux" || request.ClientName != "test" {
		t.Errorf("the gateway saw %+v", request)
	}
	r := <-done
	if r.err != nil {
		t.Fatal(r.err)
	}
	if !strings.HasPrefix(r.credential, remoteapi.PrefixCredential) {
		t.Fatalf("credential %q", r.credential)
	}
	client := New(inv.Endpoint(), func() (string, error) { return r.credential, nil })
	t.Cleanup(client.Close)
	return client
}

func newGateway(t *testing.T, write bool) *gatewaytest.Gateway {
	t.Helper()
	e := gatewaytest.NewEngine(t, "docs", nil)
	e.Write("readme.txt", "hello")
	e.Write("sub/a.txt", "a")
	e.Write("sub/b.txt", "b")
	g := gatewaytest.NewGateway(t, e, "")
	g.Export(write)
	return g
}

func TestPairAndBrowse(t *testing.T) {
	g := newGateway(t, true)
	client := pair(t, g, map[string]string{"docs": "write"})
	ctx := context.Background()

	caps, err := client.Capabilities(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if caps.APIVersion != remoteapi.APIVersion || caps.ServiceID != client.Endpoint().ServiceID ||
		caps.Certificate == nil || caps.Certificate.SPKI != client.Endpoint().Pins[0] {
		t.Fatalf("capabilities %+v", caps)
	}
	folders, err := client.Folders(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(folders) != 1 || folders[0].ID != "docs" || !folders[0].Permissions.Write {
		t.Fatalf("folders %+v", folders)
	}

	// A snapshot in pages of two, then the changes after its cursor
	page, err := client.Snapshot(ctx, "docs", "", "", 2)
	if err != nil {
		t.Fatal(err)
	}
	paths := map[string]remoteapi.Item{}
	for {
		for _, item := range page.Items {
			paths[item.Path] = item
		}
		if page.Next == "" {
			break
		}
		if page, err = client.Snapshot(ctx, "docs", page.Snapshot, page.Next, 2); err != nil {
			t.Fatal(err)
		}
	}
	for _, p := range []string{"", "readme.txt", "sub", "sub/a.txt", "sub/b.txt"} {
		if _, ok := paths[p]; !ok {
			t.Errorf("the snapshot has no %q: %v", p, paths)
		}
	}
	if item, err := client.Item(ctx, paths["readme.txt"].ID); err != nil || item.Size != 5 {
		t.Fatalf("item %+v, %v", item, err)
	}

	g.Engine.Write("new.txt", "new")
	g.Sync()
	changes, err := client.Changes(ctx, "docs", page.Cursor, 0, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, item := range changes.Changes {
		found = found || item.Path == "new.txt"
	}
	if !found || changes.Cursor == page.Cursor {
		t.Fatalf("changes %+v", changes)
	}

	// A cursor the gateway can't serve
	if _, err := client.Changes(ctx, "docs", "g999999.0", 0, 0); !errors.Is(err, ErrCursorExpired) {
		t.Fatalf("expired cursor: %v", err)
	}
}

func TestTrust(t *testing.T) {
	g := newGateway(t, false)
	client := pair(t, g, map[string]string{"docs": "read"})
	ctx := context.Background()
	credential := func() (string, error) { return "stc_whatever", nil }

	wrong := client.Endpoint()
	wrong.Pins = []string{remoteapi.SPKIHash([]byte("another key"))}
	if _, err := New(wrong, credential).Capabilities(ctx); !errors.Is(err, ErrUntrusted) {
		t.Fatalf("a wrong pin: %v", err)
	}
	unpinned := client.Endpoint()
	unpinned.Pins = nil
	if _, err := New(unpinned, credential).Capabilities(ctx); !errors.Is(err, ErrUntrusted) {
		t.Fatalf("a self-signed certificate without a pin: %v", err)
	}
	// The announced next pin is accepted as well as the current one
	both := client.Endpoint()
	both.Pins = append([]string{remoteapi.SPKIHash([]byte("next"))}, both.Pins...)
	if _, err := New(both, client.credential).Capabilities(ctx); err != nil {
		t.Fatalf("current pin among others: %v", err)
	}

	if _, err := New(client.Endpoint(), credential).Folders(ctx); !errors.Is(err, ErrUnauthenticated) {
		t.Fatalf("a wrong credential: %v", err)
	}
	none := New(client.Endpoint(), func() (string, error) { return "", nil })
	if _, err := none.Folders(ctx); !errors.Is(err, ErrUnauthenticated) {
		t.Fatalf("no credential: %v", err)
	}
	locked := New(client.Endpoint(), func() (string, error) { return "", errors.New("keyring locked") })
	if _, err := locked.Folders(ctx); !errors.Is(err, ErrUnavailable) || !IsTransient(err) {
		t.Fatalf("an unreadable credential: %v", err)
	}
}

func TestRedirectIsAProtocolViolation(t *testing.T) {
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "https://elsewhere.example/v1/capabilities", http.StatusFound)
	}))
	defer server.Close()
	host, port, _ := net.SplitHostPort(server.Listener.Addr().String())
	portNumber, _ := strconv.Atoi(port)
	pin := remoteapi.SPKIHash(server.Certificate().RawSubjectPublicKeyInfo)
	client := New(Endpoint{ServiceID: "svc_x", Host: host, Port: portNumber, Pins: []string{pin}},
		func() (string, error) { return "stc_x", nil })
	if _, err := client.Capabilities(context.Background()); !errors.Is(err, ErrProtocolViolation) {
		t.Fatalf("redirect: %v", err)
	}
}

func TestReadContent(t *testing.T) {
	contents := make([]byte, 300<<10)
	_, _ = rand.Read(contents)
	e := gatewaytest.NewEngine(t, "docs", nil)
	e.Write("blob.bin", string(contents))
	g := gatewaytest.NewGateway(t, e, "")
	g.Export(false)
	client := pair(t, g, map[string]string{"docs": "read"})
	ctx := context.Background()
	item := itemAt(t, client, "docs", "blob.bin")

	lease, err := client.CreateReadLease(ctx, item.ID, item.Revision)
	if err != nil {
		t.Fatal(err)
	}
	if lease.ContentID != item.ContentID || lease.Size != int64(len(contents)) || lease.BlockCount < 3 {
		t.Fatalf("lease %+v", lease)
	}
	blocks, err := client.Manifest(ctx, lease)
	if err != nil {
		t.Fatal(err)
	}
	// Block by block, from the end, resuming with the first ETag
	var etag string
	got := make([]byte, len(contents))
	for i := len(blocks) - 1; i >= 0; i-- {
		b := blocks[i]
		r, err := client.ReadRange(ctx, lease.Lease, lease.Token, b.Offset, b.Offset+int64(b.Size)-1, etag)
		if err != nil {
			t.Fatal(err)
		}
		data, err := io.ReadAll(r.Body)
		r.Body.Close()
		if err != nil {
			t.Fatal(err)
		}
		if err := VerifyBlock(b, data); err != nil {
			t.Fatal(err)
		}
		if etag == "" {
			etag = r.ETag
		} else if r.ETag != etag {
			t.Fatalf("ETag changed from %s to %s", etag, r.ETag)
		}
		if r.CompleteLength != int64(len(contents)) {
			t.Fatalf("complete length %d", r.CompleteLength)
		}
		copy(got[b.Offset:], data)
	}
	if !bytes.Equal(got, contents) {
		t.Fatal("the blocks don't make up the file")
	}
	// A lease for a revision that is no longer current
	e.Write("blob.bin", "changed")
	g.Sync()
	if _, err := client.CreateReadLease(ctx, item.ID, item.Revision); !errors.Is(err, ErrRevisionMismatch) {
		t.Fatalf("stale revision: %v", err)
	} else if current := err.(*Error).Item; current == nil || current.Revision == item.Revision {
		t.Fatalf("the 412 has no current item: %+v", current)
	}
	// The gateway serves the old content while it has it prepared…
	if r, err := client.ReadRange(ctx, lease.Lease, "", 0, 10, etag); err != nil {
		t.Fatalf("reading a superseded lease the gateway still has prepared: %v", err)
	} else {
		r.Body.Close()
	}
	// …and after a restart, when it doesn't, the lease is gone (the client then leases the new revision)
	g.Stop()
	g.Start()
	if _, err := client.ReadRange(ctx, lease.Lease, "", 0, 10, etag); !errors.Is(err, ErrLeaseExpired) {
		t.Fatalf("reading a superseded lease after a restart: %v", err)
	}
}

func TestManifestVerifier(t *testing.T) {
	hashes := make([][]byte, 3)
	var blocks []remoteapi.ManifestBlock
	for i := range hashes {
		sum := sha256.Sum256([]byte{byte(i)})
		hashes[i] = sum[:]
		blocks = append(blocks, remoteapi.ManifestBlock{Offset: int64(i) * 100, Size: 100,
			Hash: remoteapi.B64.EncodeToString(sum[:])})
	}
	contentID := remoteapi.FormatContentID(remoteapi.BlocksHash(hashes), 300)
	lease := remoteapi.ReadLease{ContentID: contentID, Size: 300, BlockCount: 3}
	page := remoteapi.ManifestPage{ContentID: contentID, Blocks: blocks}

	v, _ := NewManifestVerifier(lease)
	if _, err := v.Add(page, 0); err != nil {
		t.Fatal(err)
	}
	if err := v.Finish(); err != nil {
		t.Fatal(err)
	}
	cases := map[string]func(p *remoteapi.ManifestPage, l *remoteapi.ReadLease) int{
		"another content ID": func(p *remoteapi.ManifestPage, l *remoteapi.ReadLease) int {
			p.ContentID = remoteapi.FormatContentID(hashes[0], 300)
			return 0
		},
		"out of order": func(p *remoteapi.ManifestPage, l *remoteapi.ReadLease) int { return 1 },
		"a gap": func(p *remoteapi.ManifestPage, l *remoteapi.ReadLease) int {
			p.Blocks[1].Offset = 150
			return 0
		},
		"a swapped hash": func(p *remoteapi.ManifestPage, l *remoteapi.ReadLease) int {
			p.Blocks[0].Hash, p.Blocks[1].Hash = p.Blocks[1].Hash, p.Blocks[0].Hash
			return 0
		},
		"a short hash": func(p *remoteapi.ManifestPage, l *remoteapi.ReadLease) int {
			p.Blocks[2].Hash = "AAAA"
			return 0
		},
		"too few blocks": func(p *remoteapi.ManifestPage, l *remoteapi.ReadLease) int {
			p.Blocks = p.Blocks[:2]
			return 0
		},
		"a block too large": func(p *remoteapi.ManifestPage, l *remoteapi.ReadLease) int {
			p.Blocks[2].Size = MaxBlockSize + 1
			return 0
		},
	}
	for name, change := range cases {
		p := page
		p.Blocks = append([]remoteapi.ManifestBlock(nil), page.Blocks...)
		l := lease
		from := change(&p, &l)
		v, err := NewManifestVerifier(l)
		if err == nil {
			if _, err = v.Add(p, from); err == nil {
				err = v.Finish()
			}
		}
		if !errors.Is(err, ErrProtocolViolation) {
			t.Errorf("%s: %v", name, err)
		}
	}
	if err := VerifyBlock(Block{Size: 2, Hash: sha256.Sum256([]byte("ab"))}, []byte("ac")); err == nil {
		t.Error("a block with other bytes verified")
	}
}

func itemAt(t *testing.T, client *Client, folderID, path string) remoteapi.Item {
	t.Helper()
	page, err := client.Snapshot(context.Background(), folderID, "", "", 1000)
	if err != nil {
		t.Fatal(err)
	}
	for _, item := range page.Items {
		if item.Path == path {
			return item
		}
	}
	t.Fatalf("no item at %q", path)
	return remoteapi.Item{}
}

func upload(t *testing.T, client *Client, folderID string, contents []byte) (remoteapi.Upload, string) {
	t.Helper()
	sum := sha256.Sum256(contents)
	u, err := client.CreateUpload(context.Background(), folderID, int64(len(contents)), sum)
	if err != nil {
		t.Fatal(err)
	}
	if err := client.SendUpload(context.Background(), u, bytes.NewReader(contents), nil); err != nil {
		t.Fatal(err)
	}
	return u, UploadDigest(sum)
}

func submit(t *testing.T, client *Client, op remoteapi.OperationRequest) (remoteapi.Operation, []byte, error) {
	t.Helper()
	body, err := EncodeOperation(op)
	if err != nil {
		t.Fatal(err)
	}
	result, err := client.SubmitOperation(context.Background(), body)
	for err == nil && result.State == remoteapi.OperationPending {
		time.Sleep(100 * time.Millisecond)
		result, err = client.Operation(context.Background(), result.Operation)
	}
	return result, body, err
}

func TestUploadsAndOperations(t *testing.T) {
	g := newGateway(t, true)
	client := pair(t, g, map[string]string{"docs": "write"})
	ctx := context.Background()
	root := itemAt(t, client, "docs", "")
	readme := itemAt(t, client, "docs", "readme.txt")

	// Create a file: the upload, then the operation; a replay of the same body gets the same result
	u, digest := upload(t, client, "docs", []byte("created"))
	created, body, err := submit(t, client, remoteapi.OperationRequest{ID: uuid.NewString(), Kind: remoteapi.OpCreate,
		FolderID: "docs", ParentID: root.ID, Name: "created.txt", ItemKind: remoteapi.KindFile, Upload: u.Upload,
		UploadDigest: digest})
	if err != nil || created.State != remoteapi.OperationCommitted || created.Item == nil {
		t.Fatalf("create: %+v, %v", created, err)
	}
	if got, _ := g.Engine.Read("created.txt"); got != "created" {
		t.Fatalf("the gateway has %q", got)
	}
	replay, err := client.SubmitOperation(ctx, body)
	if err != nil || replay.Operation != created.Operation || replay.Item.ID != created.Item.ID {
		t.Fatalf("replay: %+v, %v", replay, err)
	}
	// The same ID with another payload
	var other remoteapi.OperationRequest
	_ = json.Unmarshal(body, &other)
	other.Name = "elsewhere.txt"
	otherBody, _ := EncodeOperation(other)
	if _, err := client.SubmitOperation(ctx, otherBody); KindOf(err) != KindOperationMismatch {
		t.Fatalf("a changed payload under the same ID: %v", err)
	}

	// Replace with the current base revision
	u, digest = upload(t, client, "docs", []byte("hello again"))
	replaced, _, err := submit(t, client, remoteapi.OperationRequest{ID: uuid.NewString(), Kind: remoteapi.OpReplace,
		FolderID: "docs", ItemID: readme.ID, BaseRevision: readme.Revision, Upload: u.Upload, UploadDigest: digest})
	if err != nil || replaced.State != remoteapi.OperationCommitted {
		t.Fatalf("replace: %+v, %v", replaced, err)
	}
	if got, _ := g.Engine.Read("readme.txt"); got != "hello again" {
		t.Fatalf("the gateway has %q", got)
	}

	// Replacing from a stale base keeps both, as a conflict copy
	u, digest = upload(t, client, "docs", []byte("from a stale base"))
	conflict, _, err := submit(t, client, remoteapi.OperationRequest{ID: uuid.NewString(), Kind: remoteapi.OpReplace,
		FolderID: "docs", ItemID: readme.ID, BaseRevision: readme.Revision, Upload: u.Upload, UploadDigest: digest,
		OnConflict: remoteapi.OnConflictKeepBoth})
	if err != nil || conflict.State != remoteapi.OperationConflict || conflict.ConflictItem == nil {
		t.Fatalf("stale replace: %+v, %v", conflict, err)
	}
	if got, _ := g.Engine.Read("readme.txt"); got != "hello again" {
		t.Fatalf("a stale replace overwrote the file: %q", got)
	}
	if got, _ := g.Engine.Read(conflict.ConflictItem.Path); got != "from a stale base" {
		t.Fatalf("the conflict copy has %q", got)
	}

	// Deleting a version that is no longer current is refused, with the current item
	_, _, err = submit(t, client, remoteapi.OperationRequest{ID: uuid.NewString(), Kind: remoteapi.OpDelete,
		FolderID: "docs", ItemID: readme.ID, BaseRevision: readme.Revision})
	var e *Error
	if !errors.As(err, &e) || e.Kind != KindRevisionMismatch || e.Item == nil || e.Item.Revision == readme.Revision {
		t.Fatalf("stale delete: %v", err)
	}
	if _, ok := g.Engine.Read("readme.txt"); !ok {
		t.Fatal("a stale delete removed the file")
	}
}

func TestUploadResumes(t *testing.T) {
	g := newGateway(t, true)
	client := pair(t, g, map[string]string{"docs": "write"})
	ctx := context.Background()
	contents := make([]byte, UploadChunkSize+12345)
	_, _ = rand.Read(contents)
	sum := sha256.Sum256(contents)
	u, err := client.CreateUpload(ctx, "docs", int64(len(contents)), sum)
	if err != nil {
		t.Fatal(err)
	}
	// Part of it, then an append at the wrong offset, then the rest from where the gateway is
	if _, err := client.AppendUpload(ctx, u.Upload, u.Token, 0, contents[:1000]); err != nil {
		t.Fatal(err)
	}
	var e *Error
	if _, err := client.AppendUpload(ctx, u.Upload, u.Token, 0, contents[:10]); !errors.As(err, &e) ||
		e.Status != http.StatusConflict {
		t.Fatalf("append at a stale offset: %v", err)
	}
	var offsets []int64
	if err := client.SendUpload(ctx, u, bytes.NewReader(contents), func(o int64) { offsets = append(offsets, o) }); err != nil {
		t.Fatal(err)
	}
	if len(offsets) != 2 || offsets[len(offsets)-1] != int64(len(contents)) {
		t.Fatalf("offsets %v", offsets)
	}
	if offset, err := client.UploadOffset(ctx, u.Upload, u.Token); err != nil || offset != int64(len(contents)) {
		t.Fatalf("offset %d, %v", offset, err)
	}
	// A terminated upload is gone
	if err := client.TerminateUpload(ctx, u.Upload, u.Token); err != nil {
		t.Fatal(err)
	}
	if _, err := client.UploadOffset(ctx, u.Upload, ""); !errors.Is(err, ErrNotFound) {
		t.Fatalf("a terminated upload: %v", err)
	}
	// Its scoped token is no longer valid
	if _, err := client.UploadOffset(ctx, u.Upload, u.Token); !errors.Is(err, ErrUnauthenticated) {
		t.Fatalf("the token of a terminated upload: %v", err)
	}
	options, err := client.UploadOptions(ctx)
	if err != nil || len(options.Versions) == 0 || options.Versions[0] != tusVersion {
		t.Fatalf("options %+v, %v", options, err)
	}
}
