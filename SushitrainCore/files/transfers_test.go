// Copyright (C) 2026 Gordon
// SPDX-License-Identifier: MPL-2.0
package files

import (
	"bytes"
	"context"
	"crypto/rand"
	"errors"
	"testing"
	"time"

	"t-shaped.nl/sushitrain/v2/gateway/gatewaytest"
	"t-shaped.nl/sushitrain/v2/remoteclient"
)

func TestTransfersListPartialDownloads(t *testing.T) {
	contents := make([]byte, 3<<20)
	_, _ = rand.Read(contents)
	e := gatewaytest.NewEngine(t, "docs", nil)
	e.Write("sub/big.bin", string(contents))
	g := gatewaytest.NewGateway(t, e, "")
	g.Export(false)
	ts := newTestService(t, g, 0)
	entry := ts.mustLookup("docs/sub/big.bin")
	if list, err := ts.Transfers(context.Background()); err != nil || len(list) != 0 {
		t.Fatalf("transfers before reading: %+v, %v", list, err)
	}

	r, err := ts.OpenFile(entry.Item)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.ReadAt(context.Background(), make([]byte, 4096), 0); err != nil {
		t.Fatal(err)
	}
	r.Close()
	var paused TransferProgress
	gatewaytest.WaitFor(t, 30*time.Second, "the download to stop", func() bool {
		list, err := ts.Transfers(context.Background())
		if err != nil || len(list) != 1 || list[0].State != TransferPaused {
			return false
		}
		paused = list[0]
		return true
	})
	check := func(when string, got TransferProgress) {
		t.Helper()
		// Blocks a stopped range verified may not be recorded, so a record can have none
		if got.Item != entry.Item || got.Path != "docs/sub/big.bin" || got.TotalBytes != int64(len(contents)) ||
			got.TotalBlocks == 0 || got.VerifiedBlocks >= got.TotalBlocks || got.Error != "" {
			t.Fatalf("%s: %+v", when, got)
		}
		if want := int64(float64(got.TotalBytes) * float64(got.VerifiedBlocks) / float64(got.TotalBlocks)); got.VerifiedBytes != want {
			t.Fatalf("%s: %d verified bytes, want %d", when, got.VerifiedBytes, want)
		}
	}
	check("after reading the start", paused)

	// From its record alone after a restart
	ts.restart()
	list, err := ts.Transfers(context.Background())
	if err != nil || len(list) != 1 {
		t.Fatalf("transfers after a restart: %+v, %v", list, err)
	}
	check("after a restart", list[0])
	if list[0].State != TransferPaused || list[0].VerifiedBlocks != paused.VerifiedBlocks {
		t.Fatalf("after a restart: %+v, before: %+v", list[0], paused)
	}

	// A record of content no item has any more is never resumed, so it isn't listed
	if _, err := ts.store.db.Exec(`INSERT INTO transfers (content_id, item, size, block_count, verified, updated_at)
		VALUES ('stale', ?, 10, 1, x'01', 0)`, entry.Item); err != nil {
		t.Fatal(err)
	}

	// A download in memory is ahead of its record, and those without a record yet follow, by ID
	ts.cache.mutex.Lock()
	ts.cache.transfers[paused.ID] = &transfer{item: entry.Item, size: paused.TotalBytes, running: true,
		blocks: make([]remoteclient.Block, paused.TotalBlocks), count: paused.TotalBlocks - 1}
	ts.cache.transfers["failed"] = &transfer{item: entry.Item, size: 10, err: errors.New("the gateway is gone")}
	ts.cache.transfers["another"] = &transfer{item: entry.Item, size: 10, running: true,
		retryErr: errors.New("the gateway is out of reach")}
	ts.cache.transfers["stopped"] = &transfer{item: entry.Item, size: 10}
	ts.cache.mutex.Unlock()
	list, err = ts.Transfers(context.Background())
	ts.cache.mutex.Lock()
	clear(ts.cache.transfers)
	ts.cache.mutex.Unlock()
	if err != nil || len(list) != 3 {
		t.Fatalf("transfers in memory: %+v, %v", list, err)
	}
	if got := list[0]; got.State != TransferDownloading || got.VerifiedBlocks != paused.TotalBlocks-1 ||
		got.TotalBlocks != paused.TotalBlocks || got.VerifiedBytes >= got.TotalBytes || got.VerifiedBytes == 0 ||
		got.Error != "" {
		t.Fatalf("a running download: %+v", got)
	}
	if got := list[1]; got.ID != "another" || got.State != TransferDownloading ||
		got.Error != "the gateway is out of reach" {
		t.Fatalf("a download waiting to try again: %+v", got)
	}
	if got := list[2]; got.ID != "failed" || got.State != TransferFailed || got.Error != "the gateway is gone" ||
		got.Path != "docs/sub/big.bin" || got.TotalBlocks != 0 || got.VerifiedBytes != 0 {
		t.Fatalf("a failed download without a record: %+v", got)
	}
	if _, err := ts.store.db.Exec(`DELETE FROM transfers WHERE content_id = 'stale'`); err != nil {
		t.Fatal(err)
	}

	if got := ts.read(ts.mustLookup("docs/sub/big.bin")); !bytes.Equal(got, contents) {
		t.Fatal("read other bytes than the gateway's")
	}
	gatewaytest.WaitFor(t, 30*time.Second, "the finished download to leave the list", func() bool {
		list, err := ts.Transfers(context.Background())
		return err == nil && len(list) == 0
	})
}
