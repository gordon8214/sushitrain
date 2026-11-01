// Copyright (C) 2026 Gordon
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this file,
// You can obtain one at https://mozilla.org/MPL/2.0/.
package sushitrain

/*
#include <CommonCrypto/CommonDigest.h>
#include <dispatch/dispatch.h>
#include <pthread/qos.h>
#include <sys/qos.h>
#include <sys/resource.h>

typedef struct {
	CC_SHA256_CTX *state;
	const void *data;
	CC_LONG length;
	dispatch_semaphore_t done;
	qos_class_t ranAt;
} hashPiece;

static void hashPieceOnQueue(void *argument) {
	hashPiece *piece = argument;
	CC_SHA256_Update(piece->state, piece->data, piece->length);
	piece->ranAt = qos_class_self();
	dispatch_semaphore_signal(piece->done);
}

// Hashes on a queue of this QoS class while the calling thread waits, at its own priority and outside Go's scheduler.
// dispatch_sync would run the block on the calling thread, at the caller's priority.
static qos_class_t hashAtQoS(qos_class_t class, CC_SHA256_CTX *state, const void *data, CC_LONG length) {
	hashPiece piece = {state, data, length, dispatch_semaphore_create(0), QOS_CLASS_UNSPECIFIED};
	dispatch_async_f(dispatch_get_global_queue(class, 0), &piece, hashPieceOnQueue);
	dispatch_semaphore_wait(piece.done, DISPATCH_TIME_FOREVER);
	dispatch_release(piece.done);
	return piece.ranAt;
}
*/
import "C"

import (
	"context"
	"crypto/sha256"
	"fmt"
	"hash"
	"io"
	"log/slog"
	"runtime"
	"sync/atomic"
	"unsafe"

	"github.com/syncthing/syncthing/lib/scanner"
)

// The priorities SetEnginePriority takes, as in EnginePriority in the app
const (
	enginePriorityNormal     = "normal"
	enginePriorityUtility    = "utility"
	enginePriorityBackground = "background"
)

// How much of a file goes through the hash at a time, which is also how often it looks at the priority and whether it
// should stop. A piece costs a round trip to a queue: at this size that takes up to a fifth off the rate at Utility on an
// idle Mac, and nothing at Background, where 128 KiB did no better. On a Mac busy on every core a piece at Background
// takes up to a second (measured November 1, 2026), and the engine waits for that when it stops, so a larger one would be
// felt.
const hashPieceSize = 256 << 10

type enginePriorityLevel struct {
	name string
	// Where hashing runs. C.QOS_CLASS_DEFAULT stands for the calling thread: the engine's priority is the default one.
	qos C.qos_class_t
	// The disk policy for this QoS class's tier in XNU (thread_qos_policy_params), as the thread that reads the file can't
	// have the class itself (SetHasherHash)
	ioPolicy C.int
}

// read reads at the level's disk policy, which is set on the thread for this read alone. A hasher also opens files, under
// a lock of the filesystem layer that calls from the app take too, so a thread that gets throttled must not go on to that.
func (level *enginePriorityLevel) read(r io.Reader, piece []byte) (int, error) {
	if level.ioPolicy == C.IOPOL_DEFAULT {
		return r.Read(piece)
	}
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	setThreadIOPolicy(level.ioPolicy)
	defer setThreadIOPolicy(C.IOPOL_DEFAULT)
	return r.Read(piece)
}

func setThreadIOPolicy(policy C.int) {
	if result, err := C.setiopolicy_np(C.IOPOL_TYPE_DISK, C.IOPOL_SCOPE_THREAD, policy); result != 0 {
		slog.Warn("Could not set the disk policy of a hasher", "policy", int(policy), "error", err)
	}
}

var enginePriorityLevels = []enginePriorityLevel{
	{enginePriorityNormal, C.QOS_CLASS_DEFAULT, C.IOPOL_DEFAULT},
	{enginePriorityUtility, C.QOS_CLASS_UTILITY, C.IOPOL_STANDARD},
	{enginePriorityBackground, C.QOS_CLASS_BACKGROUND, C.IOPOL_UTILITY},
}

var enginePriority atomic.Pointer[enginePriorityLevel]

func init() {
	enginePriority.Store(&enginePriorityLevels[0])
	scanner.SetHasherHash(newPriorityHash)
}

// SetEnginePriority sets how hashing files competes with other work on the computer, from the next piece of each file
// being hashed. It doesn't lower the rest of the engine, which holds the locks that calls from the app wait for.
func SetEnginePriority(priority string) error {
	for i := range enginePriorityLevels {
		if enginePriorityLevels[i].name == priority {
			enginePriority.Store(&enginePriorityLevels[i])
			return nil
		}
	}
	return fmt.Errorf("unknown priority %q", priority)
}

// priorityHash is SHA-256 for the scanner's hashers, at the engine's priority. CommonCrypto does it, as fast as Go does,
// since no thread below the default priority can run Go: such a thread can be descheduled while it holds a P or a
// runtime lock, and on a Mac busy on every core, calls into Go from the main thread then waited for up to 5 seconds
// (Docs/macos.md, Scanning priority). A hasher waiting for a queue here holds neither.
type priorityHash struct {
	ctx   context.Context
	state C.CC_SHA256_CTX
	final C.CC_SHA256_CTX
	piece []byte
	// The class the last piece hashed on a queue ran at
	ranAt C.qos_class_t
}

func newPriorityHash(ctx context.Context) hash.Hash {
	h := &priorityHash{ctx: ctx, piece: make([]byte, hashPieceSize)}
	h.Reset()
	return h
}

// ReadFrom is how the scanner gets a file into the hash, as Blocks uses io.CopyBuffer
func (h *priorityHash) ReadFrom(r io.Reader) (int64, error) {
	var total int64
	for {
		if err := h.ctx.Err(); err != nil {
			return total, err
		}
		level := enginePriority.Load()
		n, err := level.read(r, h.piece)
		if n > 0 {
			h.update(level, h.piece[:n])
			total += int64(n)
		}
		if err == io.EOF {
			return total, nil
		}
		if err != nil {
			return total, err
		}
	}
}

func (h *priorityHash) Write(data []byte) (int, error) {
	level := enginePriority.Load()
	for rest := data; len(rest) > 0; {
		piece := rest[:min(len(rest), hashPieceSize)]
		h.update(level, piece)
		rest = rest[len(piece):]
	}
	return len(data), nil
}

func (h *priorityHash) update(level *enginePriorityLevel, piece []byte) {
	if level.qos == C.QOS_CLASS_DEFAULT {
		C.CC_SHA256_Update(&h.state, unsafe.Pointer(&piece[0]), C.CC_LONG(len(piece)))
		return
	}
	h.ranAt = C.hashAtQoS(level.qos, &h.state, unsafe.Pointer(&piece[0]), C.CC_LONG(len(piece)))
}

func (h *priorityHash) Sum(b []byte) []byte {
	h.final = h.state
	sum := make([]byte, sha256.Size)
	C.CC_SHA256_Final((*C.uchar)(unsafe.SliceData(sum)), &h.final)
	return append(b, sum...)
}

func (h *priorityHash) Reset() {
	C.CC_SHA256_Init(&h.state)
}

func (*priorityHash) Size() int {
	return sha256.Size
}

func (*priorityHash) BlockSize() int {
	return sha256.BlockSize
}

// The state of the calling thread, for tests, which can't use cgo
func threadIOPolicy() int {
	return int(C.getiopolicy_np(C.IOPOL_TYPE_DISK, C.IOPOL_SCOPE_THREAD))
}

func threadQoS() int {
	return int(C.qos_class_self())
}
