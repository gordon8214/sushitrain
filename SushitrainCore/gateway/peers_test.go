// Copyright (C) 2026 Gordon
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this file,
// You can obtain one at https://mozilla.org/MPL/2.0/.

//go:build gwintegration

package gateway

import (
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/syncthing/syncthing/lib/config"
)

// These tests connect the gateway's engine to an ordinary Syncthing peer over loopback. Run them with
// -tags noassets,gwintegration; they take a few seconds to tens of seconds.

const peerTimeout = 60 * time.Second

func setDevicePaused(t *testing.T, e *testEngine, peer *testEngine, paused bool) {
	t.Helper()
	waiter, err := e.cfg.Modify(func(c *config.Configuration) {
		d, _, _ := c.Device(peer.id)
		d.Paused = paused
		c.SetDevice(d)
	})
	if err != nil {
		t.Fatal(err)
	}
	waiter.Wait()
}

func TestPeersPropagateThroughGateway(t *testing.T) {
	gw := newTestEngine(t, "shared", nil)
	peer := newTestEngine(t, "shared", nil)
	connect(t, gw, peer)
	waitFor(t, peerTimeout, "connection", func() bool { return gw.app.Internals.IsConnectedTo(peer.id) })

	ts := newTestService(t, gw, "")
	ts.export(true)
	token := ts.pair(map[string]string{"shared": "write"})

	// A file written on the peer reaches the gateway's change log and can be read through it
	peer.write("from-peer.txt", "written on the peer")
	peer.scan()
	waitFor(t, peerTimeout, "the peer's file on the gateway", func() bool {
		got, ok := gw.read("from-peer.txt")
		return ok && got == "written on the peer"
	})
	item := ts.itemAt(token, "from-peer.txt")
	var lease ReadLease
	ts.json("POST", "/v1/items/"+item.ID+"/read-leases", token, ReadLeaseRequest{Revision: item.Revision},
		http.StatusCreated, &lease)
	if _, data := ts.do("GET", "/v1/content/"+lease.Lease, lease.Token, nil); string(data) != "written on the peer" {
		t.Fatalf("read %q", data)
	}

	// Files created and replaced through the gateway reach the peer
	root := ts.itemAt(token, "")
	op := createFile(ts, token, root.ID, "from-client.txt", []byte("written through the gateway"))
	if op.State != stateResultCommitted {
		t.Fatalf("create: %+v", op)
	}
	waitFor(t, peerTimeout, "the client's file on the peer", func() bool {
		got, ok := peer.read("from-client.txt")
		return ok && got == "written through the gateway"
	})
	item = ts.itemAt(token, "from-peer.txt")
	upload, digest := ts.upload(token, []byte("replaced through the gateway"))
	var replaced Operation
	ts.json("POST", "/v1/operations", token, OperationRequest{ID: newUUID(), Kind: opReplace, FolderID: "shared",
		ItemID: item.ID, BaseRevision: item.Revision, Upload: upload, UploadDigest: digest}, http.StatusCreated, &replaced)
	if replaced.State != stateResultCommitted {
		t.Fatalf("replace: %+v", replaced)
	}
	waitFor(t, peerTimeout, "the replacement on the peer", func() bool {
		got, _ := peer.read("from-peer.txt")
		return got == "replaced through the gateway"
	})
	// The peer saw an ordinary new version, not a conflict
	entries, _ := os.ReadDir(peer.folderPath)
	for _, entry := range entries {
		if strings.Contains(entry.Name(), ".sync-conflict-") {
			t.Fatalf("unexpected conflict copy %s on the peer", entry.Name())
		}
	}
}

func TestGatewayStreamsFromPeers(t *testing.T) {
	gw := newTestEngine(t, "shared", nil)
	peer := newTestEngine(t, "shared", nil)
	// The gateway doesn't keep this file, so reading it must stream verified blocks from the peer
	if err := os.WriteFile(filepath.Join(gw.folderPath, ".stignore"), []byte("remote-only.bin\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	connect(t, gw, peer)
	waitFor(t, peerTimeout, "connection", func() bool { return gw.app.Internals.IsConnectedTo(peer.id) })
	contents := strings.Repeat("streamed from the peer ", 50000)
	peer.write("remote-only.bin", contents)
	peer.scan()

	ts := newTestService(t, gw, "")
	ts.export(false)
	token := ts.pair(map[string]string{"shared": "read"})
	var item Item
	waitFor(t, peerTimeout, "the peer's index on the gateway", func() bool {
		ts.sync()
		row, ok, _ := liveItemByPath(ts.store.db, "shared", "remote-only.bin")
		if ok {
			ts.json("GET", "/v1/items/"+row.id, token, nil, http.StatusOK, &item)
		}
		return ok
	})
	if _, ok := gw.read("remote-only.bin"); ok {
		t.Fatal("the gateway pulled a file it ignores")
	}
	if item.Availability != availabilityPeers {
		t.Fatalf("availability %q", item.Availability)
	}
	var lease ReadLease
	ts.json("POST", "/v1/items/"+item.ID+"/read-leases", token, ReadLeaseRequest{Revision: item.Revision},
		http.StatusCreated, &lease)
	resp, data := ts.do("GET", "/v1/content/"+lease.Lease, lease.Token, nil, "Range", "bytes=500000-700000")
	if resp.StatusCode != http.StatusPartialContent || string(data) != contents[500000:700001] {
		t.Fatalf("streamed range: %d, %d bytes", resp.StatusCode, len(data))
	}
}

func TestOfflinePeerEditKeepsBothVersions(t *testing.T) {
	gw := newTestEngine(t, "shared", nil)
	peer := newTestEngine(t, "shared", nil)
	connect(t, gw, peer)
	waitFor(t, peerTimeout, "connection", func() bool { return gw.app.Internals.IsConnectedTo(peer.id) })
	peer.write("doc.txt", "original")
	peer.scan()
	waitFor(t, peerTimeout, "the original on the gateway", func() bool {
		got, _ := gw.read("doc.txt")
		return got == "original"
	})

	ts := newTestService(t, gw, "")
	ts.export(true)
	token := ts.pair(map[string]string{"shared": "write"})
	item := ts.itemAt(token, "doc.txt")

	// The peer goes offline and edits the file; the client edits it through the gateway meanwhile
	setDevicePaused(t, gw, peer, true)
	waitFor(t, peerTimeout, "disconnection", func() bool { return !gw.app.Internals.IsConnectedTo(peer.id) })
	time.Sleep(1100 * time.Millisecond)
	peer.write("doc.txt", "edited on the offline peer")
	peer.scan()
	upload, digest := ts.upload(token, []byte("edited through the gateway"))
	var op Operation
	ts.json("POST", "/v1/operations", token, OperationRequest{ID: newUUID(), Kind: opReplace, FolderID: "shared",
		ItemID: item.ID, BaseRevision: item.Revision, Upload: upload, UploadDigest: digest}, http.StatusCreated, &op)
	if op.State != stateResultCommitted {
		t.Fatalf("gateway edit: %+v", op)
	}

	// Back online, Syncthing's conflict handling keeps both edits
	setDevicePaused(t, gw, peer, false)
	waitFor(t, peerTimeout, "both versions on the gateway", func() bool {
		entries, _ := os.ReadDir(gw.folderPath)
		var contents []string
		for _, entry := range entries {
			if strings.HasPrefix(entry.Name(), "doc") {
				data, _ := os.ReadFile(filepath.Join(gw.folderPath, entry.Name()))
				contents = append(contents, string(data))
			}
		}
		joined := strings.Join(contents, "|")
		return strings.Contains(joined, "edited on the offline peer") && strings.Contains(joined, "edited through the gateway")
	})
}

func TestGlobalVersionFallsBack(t *testing.T) {
	// When the device with the newest version leaves the folder, the global version falls back to this device's
	// older one, without a new index sequence; the change log follows it
	gw := newTestEngine(t, "shared", func(f *config.FolderConfiguration) { f.Type = config.FolderTypeSendOnly })
	peer := newTestEngine(t, "shared", nil)
	gw.write("doc.txt", "old")
	gw.scan()
	connect(t, gw, peer)
	waitFor(t, peerTimeout, "connection", func() bool { return gw.app.Internals.IsConnectedTo(peer.id) })
	waitFor(t, peerTimeout, "the file on the peer", func() bool {
		got, ok := peer.read("doc.txt")
		return ok && got == "old"
	})

	ts := newTestService(t, gw, "")
	ts.export(false)
	token := ts.pair(map[string]string{"shared": "read"})
	old := ts.itemAt(token, "doc.txt")

	// A newer version on the peer is global; the send-only gateway doesn't pull it
	peer.write("doc.txt", "newer on the peer")
	peer.scan()
	waitFor(t, peerTimeout, "the peer's version in the change log", func() bool {
		return ts.itemAt(token, "doc.txt").Revision != old.Revision
	})

	waiter, err := gw.cfg.Modify(func(c *config.Configuration) {
		folder, _, _ := c.Folder("shared")
		folder.Devices = slices.DeleteFunc(folder.Devices, func(d config.FolderDeviceConfiguration) bool {
			return d.DeviceID == peer.id
		})
		c.SetFolder(folder)
	})
	if err != nil {
		t.Fatal(err)
	}
	waiter.Wait()
	waitFor(t, peerTimeout, "the gateway's own version in the change log again", func() bool {
		return ts.itemAt(token, "doc.txt").Revision == old.Revision
	})
}
