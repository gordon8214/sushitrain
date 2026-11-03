// Copyright (C) 2026 Gordon
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this file,
// You can obtain one at https://mozilla.org/MPL/2.0/.

package gateway

import (
	"bytes"
	"crypto/sha256"
	"testing"

	"github.com/syncthing/syncthing/lib/protocol"
	"t-shaped.nl/sushitrain/v2/remoteapi"
)

// Clients verify manifests with remoteapi.BlocksHash, so it must be the engine's hash of a block list
func TestClientBlocksHashIsTheEngines(t *testing.T) {
	var blocks []protocol.BlockInfo
	var hashes [][]byte
	for i := range 3 {
		sum := sha256.Sum256([]byte{byte(i)})
		blocks = append(blocks, protocol.BlockInfo{Hash: sum[:], Offset: int64(i) * 128 << 10, Size: 128 << 10})
		hashes = append(hashes, sum[:])
	}
	if got, want := remoteapi.BlocksHash(hashes), protocol.BlocksHash(blocks); !bytes.Equal(got, want) {
		t.Fatalf("remoteapi.BlocksHash = %x, protocol.BlocksHash = %x", got, want)
	}
	fi := protocol.FileInfo{Name: "f", Type: protocol.FileInfoTypeFile, Size: 3 * 128 << 10, Blocks: blocks}
	hash, size, ok := remoteapi.ParseContentID(contentIDOf(fi))
	if !ok || size != fi.Size || !bytes.Equal(hash, remoteapi.BlocksHash(hashes)) {
		t.Fatalf("content ID %s doesn't name the block list", contentIDOf(fi))
	}
}
