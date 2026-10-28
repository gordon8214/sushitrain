// Copyright (C) 2026 Gordon
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this file,
// You can obtain one at https://mozilla.org/MPL/2.0/.

// Package gateway implements the remote file service: an HTTPS API through which paired clients (the iOS Files
// extension and app) browse, download and edit files in Syncthing folders of the device it runs on. See
// Docs/remote-api for the contract and Docs/remote-file-provider-plan.md for the design.
package gateway

import (
	"cmp"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"fmt"
	"slices"
	"strconv"
	"strings"

	"github.com/syncthing/syncthing/lib/protocol"
)

var b64 = base64.RawURLEncoding

// Prefixes of identifiers and tokens (see Docs/remote-api/invariants.md)
const (
	prefixService   = "svc_"
	prefixItem      = "itm_"
	prefixLease     = "lse_"
	prefixUpload    = "upl_"
	prefixSnapshot  = "snp_"
	prefixPage      = "pg_"
	prefixPairing   = "prq_"
	prefixOperation = "op_"

	prefixCredential = "stc_"
	prefixLeaseToken = "stl_"
	prefixUploadTok  = "stu_"
	prefixPollToken  = "stp_"
)

func randomBytes(n int) []byte {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		// crypto/rand does not fail on supported platforms
		panic(err)
	}
	return b
}

// newID returns an unguessable 128-bit identifier with the given prefix
func newID(prefix string) string {
	return prefix + b64.EncodeToString(randomBytes(16))
}

// newToken returns a 256-bit secret with the given prefix
func newToken(prefix string) string {
	return prefix + b64.EncodeToString(randomBytes(32))
}

// newSecret returns a 128-bit pairing secret
func newSecret() string {
	return b64.EncodeToString(randomBytes(16))
}

// secretHash is what the service stores of a credential, token or secret
func secretHash(secret string) []byte {
	sum := sha256.Sum256([]byte(secret))
	return sum[:]
}

func hasIDFormat(id, prefix string) bool {
	rest, ok := strings.CutPrefix(id, prefix)
	if !ok || len(rest) != 22 {
		return false
	}
	_, err := b64.DecodeString(rest)
	return err == nil
}

// blocksHashOf returns the hash naming a file's block list, computing it where the index leaves it out
func blocksHashOf(fi protocol.FileInfo) []byte {
	if len(fi.BlocksHash) > 0 {
		return fi.BlocksHash
	}
	if len(fi.Blocks) > 0 {
		return protocol.BlocksHash(fi.Blocks)
	}
	return nil
}

// contentIDOf names the block list of a regular file together with its length, or returns "" for other items
func contentIDOf(fi protocol.FileInfo) string {
	if fi.Type != protocol.FileInfoTypeFile || fi.Deleted {
		return ""
	}
	return formatContentID(blocksHashOf(fi), fi.Size)
}

func formatContentID(blocksHash []byte, size int64) string {
	return "c1." + b64.EncodeToString(blocksHash) + "." + strconv.FormatInt(size, 10)
}

func parseContentID(id string) (blocksHash []byte, size int64, ok bool) {
	rest, found := strings.CutPrefix(id, "c1.")
	if !found {
		return nil, 0, false
	}
	hash, length, found := strings.Cut(rest, ".")
	if !found {
		return nil, 0, false
	}
	blocksHash, err := b64.DecodeString(hash)
	if err != nil {
		return nil, 0, false
	}
	size, err = strconv.ParseInt(length, 10, 64)
	if err != nil || size < 0 {
		return nil, 0, false
	}
	return blocksHash, size, true
}

// revisionOf derives an item's opaque revision from what a client can observe of it: the version vector, content
// identity, kind, permissions, modification time and deletion state. Size and modification time alone would not be
// enough, as they don't change for every edit.
func revisionOf(fi protocol.FileInfo) string {
	h := sha256.New()
	var buf [8]byte
	put := func(v uint64) {
		binary.BigEndian.PutUint64(buf[:], v)
		h.Write(buf[:])
	}
	counters := slices.Clone(fi.Version.Counters)
	slices.SortFunc(counters, func(a, b protocol.Counter) int {
		return cmp.Compare(a.ID, b.ID)
	})
	put(uint64(len(counters)))
	for _, c := range counters {
		put(uint64(c.ID))
		put(c.Value)
	}
	put(uint64(fi.Type))
	if fi.Deleted {
		put(1)
	} else {
		put(0)
		h.Write([]byte(contentIDOf(fi)))
		h.Write([]byte{0})
		h.Write(fi.SymlinkTarget)
		h.Write([]byte{0})
		if !fi.NoPermissions {
			put(uint64(fi.Permissions))
		}
		put(uint64(fi.ModifiedS))
		put(uint64(fi.ModifiedNs))
	}
	return "r1." + b64.EncodeToString(h.Sum(nil))
}

// A position in an authority generation's change log
type cursor struct {
	generation int64
	sequence   int64
}

func (c cursor) String() string {
	return fmt.Sprintf("g%d.%d", c.generation, c.sequence)
}

func parseCursor(s string) (cursor, bool) {
	rest, ok := strings.CutPrefix(s, "g")
	if !ok {
		return cursor{}, false
	}
	gen, seq, ok := strings.Cut(rest, ".")
	if !ok {
		return cursor{}, false
	}
	g, err1 := strconv.ParseInt(gen, 10, 64)
	q, err2 := strconv.ParseInt(seq, 10, 64)
	if err1 != nil || err2 != nil || g < 0 || q < 0 {
		return cursor{}, false
	}
	return cursor{generation: g, sequence: q}, true
}
