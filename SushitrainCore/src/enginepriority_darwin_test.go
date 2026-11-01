// Copyright (C) 2026 Gordon
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this file,
// You can obtain one at https://mozilla.org/MPL/2.0/.

//go:build cgo

package sushitrain

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"runtime"
	"testing"
)

// From sys/resource.h, as a test can't use cgo
const (
	ioPolicyDefault  = 0
	ioPolicyUtility  = 4
	ioPolicyStandard = 5
)

func setEnginePriorityForTest(t *testing.T, priority string) {
	t.Helper()
	if err := SetEnginePriority(priority); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = SetEnginePriority(enginePriorityNormal) })
}

// Every level, in pieces that cross what the hash buffers: the block size, and the size of a piece of a file
func TestPriorityHashIsSHA256(t *testing.T) {
	data := make([]byte, 3*hashPieceSize+123)
	if _, err := rand.Read(data); err != nil {
		t.Fatal(err)
	}
	for _, priority := range []string{enginePriorityNormal, enginePriorityUtility, enginePriorityBackground} {
		for _, size := range []int{0, 1, sha256.BlockSize - 1, sha256.BlockSize, sha256.BlockSize + 1, hashPieceSize, len(data)} {
			t.Run(fmt.Sprintf("%s/%d", priority, size), func(t *testing.T) {
				setEnginePriorityForTest(t, priority)
				want := sha256.Sum256(data[:size])
				h := newPriorityHash(t.Context())
				written := 0
				for start := 0; start < size; start += 777 {
					n, err := h.Write(data[start:min(start+777, size)])
					if err != nil {
						t.Fatal(err)
					}
					written += n
				}
				if written != size {
					t.Errorf("wrote %d of %d", written, size)
				}
				if got := h.Sum(nil); !bytes.Equal(got, want[:]) {
					t.Errorf("got %x, want %x", got, want)
				}
				// Sum leaves the state alone, and appends
				if got := h.Sum([]byte("x")); !bytes.Equal(got[1:], want[:]) || got[0] != 'x' {
					t.Errorf("a second Sum gave %x", got)
				}

				// One Write of all of it, which goes through in pieces
				h.Reset()
				if n, err := h.Write(data[:size]); err != nil || n != size {
					t.Fatalf("wrote %d of %d: %v", n, size, err)
				}
				if got := h.Sum(nil); !bytes.Equal(got, want[:]) {
					t.Errorf("through one Write: got %x, want %x", got, want)
				}

				h.Reset()
				n, err := h.(io.ReaderFrom).ReadFrom(bytes.NewReader(data[:size]))
				if err != nil || n != int64(size) {
					t.Fatalf("read %d of %d: %v", n, size, err)
				}
				if got := h.Sum(nil); !bytes.Equal(got, want[:]) {
					t.Errorf("through ReadFrom: got %x, want %x", got, want)
				}
			})
		}
	}
}

func TestPriorityHashSizes(t *testing.T) {
	h := newPriorityHash(t.Context())
	if h.Size() != sha256.Size || h.BlockSize() != sha256.BlockSize {
		t.Errorf("size %d, block size %d", h.Size(), h.BlockSize())
	}
}

func TestSetEnginePriorityRefusesUnknown(t *testing.T) {
	setEnginePriorityForTest(t, enginePriorityUtility)
	if err := SetEnginePriority("high"); err == nil {
		t.Error("expected an error")
	}
	if got := enginePriority.Load().name; got != enginePriorityUtility {
		t.Errorf("the priority changed to %s", got)
	}
}

// A reader that notes the thread's disk policy, which is what the read would run under, and then does what a test asks
type policyReader struct {
	remaining int
	seen      []int
	then      func()
}

func (r *policyReader) Read(p []byte) (int, error) {
	r.seen = append(r.seen, threadIOPolicy())
	if r.then != nil {
		r.then()
		r.then = nil
	}
	if r.remaining == 0 {
		return 0, io.EOF
	}
	n := min(len(p), r.remaining)
	r.remaining -= n
	return n, nil
}

// Stops at the next piece, not at the end of a block
func TestPriorityHashStopsWithItsContext(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	h := newPriorityHash(ctx)
	reader := &policyReader{remaining: 3 * hashPieceSize, then: cancel}
	n, err := h.(io.ReaderFrom).ReadFrom(reader)
	if !errors.Is(err, context.Canceled) || n != hashPieceSize {
		t.Errorf("read %d, %v", n, err)
	}
}

func TestPriorityHashReadsAtItsPolicy(t *testing.T) {
	for _, test := range []struct {
		priority string
		policy   int
	}{
		{enginePriorityNormal, ioPolicyDefault},
		{enginePriorityUtility, ioPolicyStandard},
		{enginePriorityBackground, ioPolicyUtility},
	} {
		t.Run(test.priority, func(t *testing.T) {
			setEnginePriorityForTest(t, test.priority)
			reader := &policyReader{remaining: hashPieceSize}
			if _, err := newPriorityHash(t.Context()).(io.ReaderFrom).ReadFrom(reader); err != nil {
				t.Fatal(err)
			}
			if reader.seen[0] != test.policy {
				t.Errorf("read under policy %d, want %d", reader.seen[0], test.policy)
			}
		})
	}
}

// The policy is for the read alone. A hasher goes on to open files and read their metadata under locks that the app
// takes too, and without a lock on the thread that would leave it throttled for whichever goroutine ran there next.
func TestPriorityHashRestoresThePolicyOfItsThread(t *testing.T) {
	setEnginePriorityForTest(t, enginePriorityBackground)
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	reader := &policyReader{remaining: 2 * hashPieceSize}
	if _, err := newPriorityHash(t.Context()).(io.ReaderFrom).ReadFrom(reader); err != nil {
		t.Fatal(err)
	}
	if reader.seen[0] != ioPolicyUtility {
		t.Errorf("the read ran under policy %d", reader.seen[0])
	}
	if got := threadIOPolicy(); got != ioPolicyDefault {
		t.Errorf("the thread was left at policy %d", got)
	}
}

// A new priority reaches the next piece
func TestPriorityHashFollowsAChange(t *testing.T) {
	setEnginePriorityForTest(t, enginePriorityBackground)
	reader := &policyReader{remaining: 3 * hashPieceSize}
	reader.then = func() { _ = SetEnginePriority(enginePriorityNormal) }
	if _, err := newPriorityHash(t.Context()).(io.ReaderFrom).ReadFrom(reader); err != nil {
		t.Fatal(err)
	}
	if first, last := reader.seen[0], reader.seen[len(reader.seen)-1]; first != ioPolicyUtility || last != ioPolicyDefault {
		t.Errorf("read under policy %d, then %d", first, last)
	}
}

// The reason for the queues: the work runs at the level's class, and the thread that asked for it, which runs Go, doesn't
func TestPriorityHashRunsOnAQueueOfItsClass(t *testing.T) {
	for _, priority := range []string{enginePriorityUtility, enginePriorityBackground} {
		t.Run(priority, func(t *testing.T) {
			setEnginePriorityForTest(t, priority)
			runtime.LockOSThread()
			defer runtime.UnlockOSThread()
			before := threadQoS()
			h := newPriorityHash(t.Context()).(*priorityHash)
			if _, err := h.Write(make([]byte, 1024)); err != nil {
				t.Fatal(err)
			}
			if want := enginePriority.Load().qos; h.ranAt != want {
				t.Errorf("the piece ran at QoS %#x, want %#x", h.ranAt, want)
			}
			if after := threadQoS(); after != before {
				t.Errorf("the thread went from QoS %#x to %#x", before, after)
			}
		})
	}
}

// At Normal nothing moves: hashing stays on the hasher's thread, like Go's own
func TestPriorityHashStaysOnItsThreadAtNormal(t *testing.T) {
	setEnginePriorityForTest(t, enginePriorityNormal)
	h := newPriorityHash(t.Context()).(*priorityHash)
	if _, err := h.Write(make([]byte, 1024)); err != nil {
		t.Fatal(err)
	}
	if h.ranAt != 0 {
		t.Errorf("the piece went to a queue of QoS %#x", h.ranAt)
	}
}
