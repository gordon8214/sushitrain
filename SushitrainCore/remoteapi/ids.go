// Copyright (C) 2026 Gordon
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this file,
// You can obtain one at https://mozilla.org/MPL/2.0/.

package remoteapi

import (
	"crypto/sha256"
	"encoding/base64"
	"strconv"
	"strings"
)

// B64 is the encoding of identifiers, tokens, hashes and pins: base64url without padding
var B64 = base64.RawURLEncoding

// Prefixes of identifiers and tokens (see Docs/remote-api/invariants.md)
const (
	PrefixService   = "svc_"
	PrefixItem      = "itm_"
	PrefixLease     = "lse_"
	PrefixUpload    = "upl_"
	PrefixSnapshot  = "snp_"
	PrefixPage      = "pg_"
	PrefixPairing   = "prq_"
	PrefixOperation = "op_"
	PrefixVersion   = "ver_"

	PrefixCredential  = "stc_"
	PrefixLeaseToken  = "stl_"
	PrefixUploadToken = "stu_"
	PrefixPollToken   = "stp_"
)

// Values of ItemVersionList.Versioning
const (
	VersioningNone      = "none"
	VersioningSimple    = "simple"
	VersioningStaggered = "staggered"
	VersioningTrashcan  = "trashcan"
	VersioningExternal  = "external"
	// Versions kept somewhere other than the folder's default .stversions, which the service doesn't serve
	VersioningCustomPath = "customPath"
)

// HasIDFormat reports whether id is a 128-bit identifier with the given prefix
func HasIDFormat(id, prefix string) bool {
	rest, ok := strings.CutPrefix(id, prefix)
	if !ok || len(rest) != 22 {
		return false
	}
	_, err := B64.DecodeString(rest)
	return err == nil
}

// FormatContentID names a regular file's content by the hash of its block list and its length
func FormatContentID(blocksHash []byte, size int64) string {
	return "c1." + B64.EncodeToString(blocksHash) + "." + strconv.FormatInt(size, 10)
}

// ParseContentID returns the block list hash and length a content ID names
func ParseContentID(id string) (blocksHash []byte, size int64, ok bool) {
	rest, found := strings.CutPrefix(id, "c1.")
	if !found {
		return nil, 0, false
	}
	hash, length, found := strings.Cut(rest, ".")
	if !found {
		return nil, 0, false
	}
	blocksHash, err := B64.DecodeString(hash)
	if err != nil {
		return nil, 0, false
	}
	size, err = strconv.ParseInt(length, 10, 64)
	if err != nil || size < 0 {
		return nil, 0, false
	}
	return blocksHash, size, true
}

// BlocksHash is the hash a content ID names: SHA-256 over the raw block hashes in order, as Syncthing computes it
// (lib/protocol BlocksHash)
func BlocksHash(blockHashes [][]byte) []byte {
	h := sha256.New()
	for _, b := range blockHashes {
		h.Write(b)
	}
	return h.Sum(nil)
}

// SPKIHash is how a pairing code pins a certificate: base64url of the SHA-256 of its DER SubjectPublicKeyInfo
func SPKIHash(rawSubjectPublicKeyInfo []byte) string {
	sum := sha256.Sum256(rawSubjectPublicKeyInfo)
	return B64.EncodeToString(sum[:])
}
