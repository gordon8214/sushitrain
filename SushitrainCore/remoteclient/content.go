// Copyright (C) 2026 Gordon
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this file,
// You can obtain one at https://mozilla.org/MPL/2.0/.

package remoteclient

import (
	"bytes"
	"context"
	"crypto/sha256"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"t-shaped.nl/sushitrain/v2/remoteapi"
)

// MaxBlockSize is the largest block a manifest may describe (Syncthing's largest block size)
const MaxBlockSize = 16 << 20

// CreateReadLease creates a read lease for the item's revision, or returns the one this client has, with its
// preparation progress. A 412 means the revision is no longer current, and carries the current item.
func (c *Client) CreateReadLease(ctx context.Context, itemID, revision string) (remoteapi.ReadLease, error) {
	var lease remoteapi.ReadLease
	_, err := c.call(ctx, "POST", []string{"v1", "items", itemID, "read-leases"}, nil, withCredential,
		remoteapi.ReadLeaseRequest{Revision: revision}, &lease, KindLeaseExpired, 0)
	return lease, err
}

// ManifestPage returns a page of the lease's block list from block index from; token is the lease's token, or ""
// for the credential
func (c *Client) ManifestPage(ctx context.Context, leaseID, token string, from, limit int) (remoteapi.ManifestPage,
	error) {
	query := url.Values{"from": {strconv.Itoa(from)}}
	if limit > 0 {
		query.Set("limit", strconv.Itoa(limit))
	}
	var page remoteapi.ManifestPage
	_, err := c.call(ctx, "GET", []string{"v1", "content", leaseID, "manifest"}, query, withToken(token), nil, &page,
		KindLeaseExpired, 0)
	return page, err
}

// Block is one verified entry of a manifest
type Block struct {
	Index  int
	Offset int64
	Size   int
	Hash   [sha256.Size]byte
}

// ManifestVerifier checks a lease's manifest, page by page, against its content ID: blocks are contiguous from 0,
// cover exactly the content, and their hashes together are the hash the content ID names. It is the Swift
// ManifestVerifier.
type ManifestVerifier struct {
	contentID  string
	identity   []byte
	size       int64
	blockCount int
	hasher     hashWriter
	nextOffset int64
	count      int
}

type hashWriter interface {
	io.Writer
	Sum([]byte) []byte
}

// NewManifestVerifier starts checking the manifest of a lease
func NewManifestVerifier(lease remoteapi.ReadLease) (*ManifestVerifier, error) {
	identity, size, ok := remoteapi.ParseContentID(lease.ContentID)
	if !ok || size != lease.Size || lease.BlockCount < 0 {
		return nil, protocolError("the lease's content ID %q doesn't describe its content", lease.ContentID)
	}
	return &ManifestVerifier{contentID: lease.ContentID, identity: identity, size: size, blockCount: lease.BlockCount,
		hasher: sha256.New()}, nil
}

// Add checks a page that was asked for from block index from, and returns its blocks
func (v *ManifestVerifier) Add(page remoteapi.ManifestPage, from int) ([]Block, error) {
	if page.ContentID != v.contentID {
		return nil, protocolError("a manifest page is for another content ID")
	}
	if from != v.count {
		return nil, protocolError("manifest pages out of order")
	}
	blocks := make([]Block, 0, len(page.Blocks))
	for _, b := range page.Blocks {
		hash, err := remoteapi.B64.DecodeString(b.Hash)
		if err != nil || len(hash) != sha256.Size {
			return nil, protocolError("block %d's hash is not a SHA-256", v.count)
		}
		if b.Offset != v.nextOffset || b.Size < 0 || b.Size > MaxBlockSize || (b.Size == 0 && v.size != 0) {
			return nil, protocolError("block %d does not continue the content", v.count)
		}
		v.hasher.Write(hash)
		block := Block{Index: v.count, Offset: b.Offset, Size: b.Size}
		copy(block.Hash[:], hash)
		blocks = append(blocks, block)
		v.nextOffset += int64(b.Size)
		v.count++
		if v.count > v.blockCount || v.nextOffset > v.size {
			return nil, protocolError("the manifest describes more than the content")
		}
	}
	return blocks, nil
}

// Next is the index of the first block not added yet
func (v *ManifestVerifier) Next() int {
	return v.count
}

// Finish checks that the blocks cover the content exactly and match the content ID
func (v *ManifestVerifier) Finish() error {
	if v.count != v.blockCount || v.nextOffset != v.size {
		return protocolError("the manifest does not cover the content")
	}
	if !bytes.Equal(v.hasher.Sum(nil), v.identity) {
		return protocolError("the manifest does not match the content ID")
	}
	return nil
}

// Manifest reads and verifies a lease's whole block list, with the credential
func (c *Client) Manifest(ctx context.Context, lease remoteapi.ReadLease) ([]Block, error) {
	verifier, err := NewManifestVerifier(lease)
	if err != nil {
		return nil, err
	}
	blocks := make([]Block, 0, lease.BlockCount)
	for verifier.Next() < lease.BlockCount {
		from := verifier.Next()
		page, err := c.ManifestPage(ctx, lease.Lease, "", from, 0)
		if err != nil {
			return nil, err
		}
		added, err := verifier.Add(page, from)
		if err != nil {
			return nil, err
		}
		if len(added) == 0 {
			return nil, protocolError("a manifest page has no blocks before the end")
		}
		blocks = append(blocks, added...)
	}
	if err := verifier.Finish(); err != nil {
		return nil, err
	}
	return blocks, nil
}

// VerifyBlock checks a block's bytes against its manifest entry
func VerifyBlock(block Block, data []byte) error {
	if len(data) != block.Size || sha256.Sum256(data) != block.Hash {
		return protocolError("block %d does not match the manifest", block.Index)
	}
	return nil
}

// Range is a response to a range request. Its bytes are not verified: the caller checks them block by block.
type Range struct {
	Body io.ReadCloser
	// The strong entity tag of the content, to resume with If-Range
	ETag string
	// The first byte, and the length of the whole content
	Start          int64
	CompleteLength int64
}

// ReadRange requests the bytes start to end (inclusive) of the lease's content. ifRange is the ETag the bytes read
// so far came with ("" at first). A response with other bytes, or the whole content, is a protocol violation.
func (c *Client) ReadRange(ctx context.Context, leaseID, token string, start, end int64, ifRange string) (*Range,
	error) {
	if start < 0 || end < start {
		return nil, fmt.Errorf("invalid range %d-%d", start, end)
	}
	req, err := c.newRequest(ctx, "GET", []string{"v1", "content", leaseID}, nil, withToken(token), nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Range", fmt.Sprintf("bytes=%d-%d", start, end))
	if ifRange != "" {
		req.Header.Set("If-Range", ifRange)
	}
	resp, err := c.do(req, KindLeaseExpired)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusPartialContent {
		resp.Body.Close()
		return nil, protocolError("the gateway answered a range request with status %d", resp.StatusCode)
	}
	first, last, complete, ok := parseContentRange(resp.Header.Get("Content-Range"))
	if !ok || first != start || last > end {
		resp.Body.Close()
		return nil, protocolError("the gateway answered with another range")
	}
	etag := resp.Header.Get("ETag")
	if etag == "" || strings.HasPrefix(etag, "W/") {
		resp.Body.Close()
		return nil, protocolError("the content has no strong entity tag")
	}
	return &Range{Body: &limitedBody{Reader: io.LimitReader(resp.Body, last-first+1), Closer: resp.Body}, ETag: etag,
		Start: first, CompleteLength: complete}, nil
}

type limitedBody struct {
	io.Reader
	io.Closer
}

// parseContentRange reads "bytes <first>-<last>/<complete or *>"
func parseContentRange(value string) (first, last, complete int64, ok bool) {
	rest, found := strings.CutPrefix(strings.TrimSpace(value), "bytes ")
	if !found {
		return 0, 0, 0, false
	}
	bounds, total, found := strings.Cut(rest, "/")
	if !found {
		return 0, 0, 0, false
	}
	a, b, found := strings.Cut(bounds, "-")
	if !found {
		return 0, 0, 0, false
	}
	var err1, err2 error
	first, err1 = strconv.ParseInt(a, 10, 64)
	last, err2 = strconv.ParseInt(b, 10, 64)
	if err1 != nil || err2 != nil || first < 0 || last < first {
		return 0, 0, 0, false
	}
	complete = -1
	if total != "*" {
		n, err := strconv.ParseInt(total, 10, 64)
		if err != nil || n <= last {
			return 0, 0, 0, false
		}
		complete = n
	}
	return first, last, complete, true
}

// VersionContent opens a previous version's bytes, which are not verified: they are only for previewing and copying,
// and never kept as an item's content (invariant 12). The caller closes the reader.
func (c *Client) VersionContent(ctx context.Context, itemID, versionID string) (io.ReadCloser, error) {
	req, err := c.newRequest(ctx, "GET", []string{"v1", "items", itemID, "versions", versionID, "content"}, nil,
		withCredential, nil)
	if err != nil {
		return nil, err
	}
	resp, err := c.do(req, KindOther)
	if err != nil {
		return nil, err
	}
	return resp.Body, nil
}

const maxThumbnail = 16 << 20

// Thumbnail returns a JPEG thumbnail of the item's revision, of at least size pixels on its longer side (the gateway
// rounds up to 64, 128, 256, 512 or 1024). A 404 means the gateway can't render this kind of file.
func (c *Client) Thumbnail(ctx context.Context, itemID, revision string, size int) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	query := url.Values{"revision": {revision}, "size": {strconv.Itoa(size)}}
	req, err := c.newRequest(ctx, "GET", []string{"v1", "items", itemID, "thumbnail"}, query, withCredential, nil)
	if err != nil {
		return nil, err
	}
	resp, err := c.do(req, KindOther)
	if err != nil {
		return nil, err
	}
	return readBody(resp, maxThumbnail)
}
