// Copyright (C) 2026 Gordon
// SPDX-License-Identifier: MPL-2.0
package files

import (
	"bytes"
	"context"
	"crypto/rand"
	"errors"
	"io"
	"slices"
	"sort"
	"sync"
	"sync/atomic"
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

// The next range is asked for while the current one is read, so that the link doesn't stand idle between ranges: the
// first range's bytes are held back until the second range is asked for
func TestDownloadAsksForTheNextRangeWhileReading(t *testing.T) {
	// More than one range, however large the first one is
	contents := make([]byte, rangeBytes+2<<20)
	_, _ = rand.Read(contents)
	e := gatewaytest.NewEngine(t, "docs", nil)
	e.Write("big.bin", string(contents))
	g := gatewaytest.NewGateway(t, e, "")
	g.Export(false)
	ts := newTestService(t, g, 0)
	entry := ts.mustLookup("docs/big.bin")

	second := make(chan struct{})
	var calls atomic.Int32
	var overlapped atomic.Bool
	ts.cache.readRange = func(ctx context.Context, client *remoteclient.Client, leaseID string, start, end int64,
		ifRange string) (*remoteclient.Range, error) {
		n := calls.Add(1)
		if n == 2 {
			close(second)
		}
		r, err := gatewayReadRange(ctx, client, leaseID, start, end, ifRange)
		if err == nil && n == 1 {
			r.Body = &heldBody{ReadCloser: r.Body, release: second, overlapped: &overlapped}
		}
		return r, err
	}

	r, err := ts.OpenFile(entry.Item)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	got := make([]byte, len(contents))
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	if n, err := r.ReadAt(ctx, got, 0); err != nil || n != len(contents) || !bytes.Equal(got, contents) {
		t.Fatalf("read %d bytes, %v", n, err)
	}
	if calls.Load() < 2 {
		t.Fatalf("%d range requests; the test needs several ranges", calls.Load())
	}
	if !overlapped.Load() {
		t.Fatal("the second range was asked for only after the first one was read")
	}
}

// heldBody holds back a response's bytes until release is closed (or a while has passed), and records which it was
type heldBody struct {
	io.ReadCloser
	release    <-chan struct{}
	overlapped *atomic.Bool
	waited     bool
}

func (b *heldBody) Read(p []byte) (int, error) {
	if !b.waited {
		b.waited = true
		select {
		case <-b.release:
			b.overlapped.Store(true)
		case <-time.After(5 * time.Second):
		}
	}
	return b.ReadCloser.Read(p)
}

// A seek doesn't wait for the range asked for next: its own range is read first
func TestSeekIsReadBeforeTheNextRange(t *testing.T) {
	contents := make([]byte, 2*rangeBytes+4<<20)
	_, _ = rand.Read(contents)
	e := gatewaytest.NewEngine(t, "docs", nil)
	e.Write("big.bin", string(contents))
	g := gatewaytest.NewGateway(t, e, "")
	g.Export(false)
	ts := newTestService(t, g, 0)
	entry := ts.mustLookup("docs/big.bin")
	offset := int64(len(contents) - 2<<20)

	var r *Reader
	// The transfer's state, under its lock
	inTransfer := func(f func(tr *transfer)) {
		ts.cache.mutex.Lock()
		tr := ts.cache.transfers[r.ContentID()]
		ts.cache.mutex.Unlock()
		if tr != nil {
			tr.mutex.Lock()
			defer tr.mutex.Unlock()
			f(tr)
		}
	}
	first := make(chan struct{})
	var calls atomic.Int32
	var nextReadEarly atomic.Bool
	ts.cache.readRange = func(ctx context.Context, client *remoteclient.Client, leaseID string, start, end int64,
		ifRange string) (*remoteclient.Range, error) {
		n := calls.Add(1)
		rng, err := gatewayReadRange(ctx, client, leaseID, start, end, ifRange)
		if err == nil {
			switch n {
			case 1:
				// Held until the seek waits
				rng.Body = &heldBody{ReadCloser: rng.Body, release: first, overlapped: new(atomic.Bool)}
			case 2:
				// The range asked for next: when it is read, the seek's block must be in already
				rng.Body = &firstReadHook{ReadCloser: rng.Body, hook: func() {
					inTransfer(func(tr *transfer) {
						i := sort.Search(len(tr.blocks), func(i int) bool {
							return tr.blocks[i].Offset+int64(tr.blocks[i].Size) > offset
						})
						nextReadEarly.Store(!tr.verified[i])
					})
				}}
			}
		}
		return rng, err
	}

	var err error
	if r, err = ts.OpenFile(entry.Item); err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	start := make(chan error, 1)
	go func() {
		_, err := r.ReadAt(ctx, make([]byte, 4096), 0)
		start <- err
	}()
	gatewaytest.WaitFor(t, 10*time.Second, "the range asked for next", func() bool { return calls.Load() >= 2 })

	seek := make(chan error, 1)
	go func() {
		got := make([]byte, 4096)
		_, err := r.ReadAt(ctx, got, offset)
		if err == nil && !bytes.Equal(got, contents[offset:offset+4096]) {
			err = errors.New("the seek read other bytes")
		}
		seek <- err
	}()
	// The seek waits for its block before the first range is let go
	gatewaytest.WaitFor(t, 10*time.Second, "the seek to wait", func() bool {
		waiting := false
		inTransfer(func(tr *transfer) {
			waiting = slices.ContainsFunc(tr.wanted, func(w int) bool { return tr.blocks[w].Offset >= rangeBytes })
		})
		return waiting
	})
	close(first)
	if err := <-start; err != nil {
		t.Fatal(err)
	}
	if err := <-seek; err != nil {
		t.Fatal(err)
	}
	gatewaytest.WaitFor(t, 30*time.Second, "the download to finish", func() bool {
		done := false
		inTransfer(func(tr *transfer) { done = tr.done })
		return done
	})
	if nextReadEarly.Load() {
		t.Fatal("the range asked for next was read before the seek's range")
	}
}

// firstReadHook calls hook before the body's first read
type firstReadHook struct {
	io.ReadCloser
	hook func()
	once sync.Once
}

func (b *firstReadHook) Read(p []byte) (int, error) {
	b.once.Do(b.hook)
	return b.ReadCloser.Read(p)
}

// Ranges in flight never overlap, and blocks of a range that failed, whether it was the one read or the one asked for
// next, are asked for again
func TestRangesInFlightSurviveFailures(t *testing.T) {
	for _, mode := range []string{"next expired", "next unreachable", "current cut off"} {
		t.Run(mode, func(t *testing.T) {
			contents := make([]byte, rangeBytes+4<<20)
			_, _ = rand.Read(contents)
			e := gatewaytest.NewEngine(t, "docs", nil)
			e.Write("big.bin", string(contents))
			g := gatewaytest.NewGateway(t, e, "")
			g.Export(false)
			ts := newTestService(t, g, 0)
			entry := ts.mustLookup("docs/big.bin")

			var mutex sync.Mutex
			var open [][2]int64
			var overlapped atomic.Bool
			var calls atomic.Int32
			closed := func(start, end int64) {
				mutex.Lock()
				defer mutex.Unlock()
				open = slices.DeleteFunc(open, func(o [2]int64) bool { return o == [2]int64{start, end} })
			}
			ts.cache.readRange = func(ctx context.Context, client *remoteclient.Client, leaseID string, start,
				end int64, ifRange string) (*remoteclient.Range, error) {
				n := calls.Add(1)
				mutex.Lock()
				for _, o := range open {
					if start <= o[1] && o[0] <= end {
						overlapped.Store(true)
					}
				}
				open = append(open, [2]int64{start, end})
				mutex.Unlock()
				var r *remoteclient.Range
				var err error
				switch {
				case n == 2 && mode == "next expired":
					err = remoteclient.ErrLeaseExpired
				case n == 2 && mode == "next unreachable":
					err = &remoteclient.Error{Kind: remoteclient.KindUnreachable, Message: "dropped"}
				default:
					r, err = gatewayReadRange(ctx, client, leaseID, start, end, ifRange)
				}
				if err != nil {
					closed(start, end)
					return nil, err
				}
				if n == 1 && mode == "current cut off" {
					r.Body = &cutBody{ReadCloser: r.Body, left: (end - start + 1) / 2}
				}
				r.Body = &closeHook{ReadCloser: r.Body, closed: func() { closed(start, end) }}
				return r, nil
			}

			r, err := ts.OpenFile(entry.Item)
			if err != nil {
				t.Fatal(err)
			}
			defer r.Close()
			got := make([]byte, len(contents))
			ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
			defer cancel()
			if n, err := r.ReadAt(ctx, got, 0); err != nil || n != len(contents) || !bytes.Equal(got, contents) {
				t.Fatalf("read %d bytes, %v", n, err)
			}
			if overlapped.Load() {
				t.Fatal("ranges in flight overlapped")
			}
		})
	}
}

// cutBody ends a response with an error after some of its bytes
type cutBody struct {
	io.ReadCloser
	left int64
}

func (b *cutBody) Read(p []byte) (int, error) {
	if b.left <= 0 {
		return 0, errors.New("cut off")
	}
	if int64(len(p)) > b.left {
		p = p[:b.left]
	}
	n, err := b.ReadCloser.Read(p)
	b.left -= int64(n)
	return n, err
}

// closeHook calls closed when the body is first closed
type closeHook struct {
	io.ReadCloser
	closed func()
	once   sync.Once
}

func (c *closeHook) Close() error {
	c.once.Do(c.closed)
	return c.ReadCloser.Close()
}
