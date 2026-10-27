// Copyright (C) 2026 Gordon
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this file,
// You can obtain one at https://mozilla.org/MPL/2.0/.
package sushitrain

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/syncthing/syncthing/lib/protocol"
)

type fakeFileProviderBackend struct {
	localPath string
	files     map[string]protocol.FileInfo
	blocks    map[string][][]byte
	available bool

	mutex   sync.Mutex
	fetches int
}

func (b *fakeFileProviderBackend) folderInfo(folderID string) (string, time.Duration, bool) {
	return b.localPath, 0, folderID == "folder"
}

func (b *fakeFileProviderBackend) globalFileInfo(folderID string, path string) (protocol.FileInfo, bool, error) {
	info, ok := b.files[path]
	return info, ok, nil
}

func (b *fakeFileProviderBackend) isAvailable(folderID string, file protocol.FileInfo, blockIndex int) (bool, error) {
	return b.available, nil
}

func (b *fakeFileProviderBackend) fetchBlock(ctx context.Context, folderID string, file protocol.FileInfo, blockIndex int) ([]byte, error) {
	b.mutex.Lock()
	b.fetches++
	b.mutex.Unlock()
	blocks, ok := b.blocks[file.Name]
	if !ok || blockIndex >= len(blocks) {
		return nil, errors.New("no such block")
	}
	return blocks[blockIndex], nil
}

// Adds a file with the given contents split into blocks of the minimum block size
func (b *fakeFileProviderBackend) addFile(name string, contents []byte, modTime time.Time) {
	blockSize := protocol.MinBlockSize
	info := protocol.FileInfo{
		Name:         name,
		Size:         int64(len(contents)),
		ModifiedS:    modTime.Unix(),
		ModifiedNs:   int32(modTime.Nanosecond()),
		RawBlockSize: int32(blockSize),
		BlocksHash:   []byte(name),
		ModifiedBy:   protocol.ShortID(7),
	}
	var blocks [][]byte
	for offset := 0; offset < len(contents); offset += blockSize {
		end := min(offset+blockSize, len(contents))
		info.Blocks = append(info.Blocks, protocol.BlockInfo{Offset: int64(offset), Size: end - offset})
		blocks = append(blocks, contents[offset:end])
	}
	if b.files == nil {
		b.files = map[string]protocol.FileInfo{}
		b.blocks = map[string][][]byte{}
	}
	b.files[name] = info
	b.blocks[name] = blocks
}

func startTestFileProviderServer(t *testing.T, backend fileProviderBackend) *FileProviderServer {
	t.Helper()
	server := newFileProviderServer(backend)
	if err := server.Listen(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(server.Shutdown)
	return server
}

func fileProviderRequest(t *testing.T, server *FileProviderServer, method string, path string, headers map[string]string) (*http.Response, []byte) {
	t.Helper()
	req, err := http.NewRequest(method, fmt.Sprintf("http://127.0.0.1:%d%s", server.Port(), path), nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+server.Token())
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	body, err := io.ReadAll(res.Body)
	if err != nil {
		t.Fatal(err)
	}
	return res, body
}

func testContents(size int) []byte {
	contents := make([]byte, size)
	for i := range contents {
		contents[i] = byte(i % 251)
	}
	return contents
}

func TestFileProviderServerAuthorization(t *testing.T) {
	server := startTestFileProviderServer(t, &fakeFileProviderBackend{})
	addr := server.listener.Addr().(*net.TCPAddr)
	if !addr.IP.IsLoopback() {
		t.Fatalf("listening on non-loopback address %v", addr)
	}

	url := fmt.Sprintf("http://127.0.0.1:%d/ping", server.Port())
	for _, header := range []string{"", "Bearer wrong", server.Token()} {
		req, _ := http.NewRequest(http.MethodGet, url, nil)
		if header != "" {
			req.Header.Set("Authorization", header)
		}
		res, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		res.Body.Close()
		if res.StatusCode != http.StatusUnauthorized {
			t.Fatalf("authorization %q accepted with status %d", header, res.StatusCode)
		}
	}

	if res, body := fileProviderRequest(t, server, http.MethodGet, "/ping", nil); res.StatusCode != http.StatusOK || string(body) != "ok" {
		t.Fatalf("ping failed: %d %q", res.StatusCode, body)
	}

	// Listening again rotates the token
	oldToken := server.Token()
	if err := server.Listen(); err != nil {
		t.Fatal(err)
	}
	if server.Token() == oldToken || server.Token() == "" {
		t.Fatal("token was not rotated")
	}
}

func TestFileProviderServerRemoteFile(t *testing.T) {
	backend := &fakeFileProviderBackend{available: true}
	contents := testContents(protocol.MinBlockSize*2 + 1000)
	modTime := time.Unix(1_700_000_000, 123_456_789)
	backend.addFile("dir/remote.bin", contents, modTime)
	server := startTestFileProviderServer(t, backend)

	res, body := fileProviderRequest(t, server, http.MethodGet, "/file?folder=folder&path=dir/remote.bin", nil)
	if res.StatusCode != http.StatusOK || !bytes.Equal(body, contents) {
		t.Fatalf("unexpected response %d with %d bytes", res.StatusCode, len(body))
	}
	if res.Header.Get(fileProviderHeaderSize) != fmt.Sprint(len(contents)) ||
		res.Header.Get(fileProviderHeaderModNs) != fmt.Sprint(modTime.UnixNano()) ||
		res.Header.Get(fileProviderHeaderBlocksHash) == "" || res.Header.Get(fileProviderHeaderModifiedBy) == "" {
		t.Fatalf("missing version headers: %v", res.Header)
	}
	if backend.fetches != 3 {
		t.Fatalf("expected each block to be fetched once, got %d fetches", backend.fetches)
	}

	// A range spanning a block boundary
	start, end := protocol.MinBlockSize-10, protocol.MinBlockSize+20
	res, body = fileProviderRequest(t, server, http.MethodGet, "/file?folder=folder&path=dir/remote.bin", map[string]string{
		"Range": fmt.Sprintf("bytes=%d-%d", start, end),
	})
	if res.StatusCode != http.StatusPartialContent || !bytes.Equal(body, contents[start:end+1]) {
		t.Fatalf("unexpected range response %d with %d bytes", res.StatusCode, len(body))
	}

	res, body = fileProviderRequest(t, server, http.MethodHead, "/file?folder=folder&path=dir/remote.bin", nil)
	if res.StatusCode != http.StatusOK || len(body) != 0 || res.ContentLength != int64(len(contents)) {
		t.Fatalf("unexpected HEAD response %d, length %d", res.StatusCode, res.ContentLength)
	}

	backend.available = false
	res, _ = fileProviderRequest(t, server, http.MethodGet, "/file?folder=folder&path=dir/remote.bin", nil)
	if res.StatusCode != http.StatusServiceUnavailable || res.Header.Get("Retry-After") == "" {
		t.Fatalf("expected 503 when no device is available, got %d", res.StatusCode)
	}
}

func TestFileProviderServerLocalFile(t *testing.T) {
	root := t.TempDir()
	backend := &fakeFileProviderBackend{localPath: root, available: false}
	contents := testContents(5000)
	modTime := time.Unix(1_700_000_000, 987_654_321)
	backend.addFile("local.bin", contents, modTime)
	server := startTestFileProviderServer(t, backend)

	localFile := filepath.Join(root, "local.bin")
	if err := os.WriteFile(localFile, contents, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(localFile, modTime, modTime); err != nil {
		t.Fatal(err)
	}

	res, body := fileProviderRequest(t, server, http.MethodGet, "/file?folder=folder&path=local.bin", nil)
	if res.StatusCode != http.StatusOK || !bytes.Equal(body, contents) || backend.fetches != 0 {
		t.Fatalf("local file not served from disk: %d, %d bytes, %d fetches", res.StatusCode, len(body), backend.fetches)
	}

	// A stale local copy must not be served
	if err := os.Chtimes(localFile, modTime.Add(-time.Hour), modTime.Add(-time.Hour)); err != nil {
		t.Fatal(err)
	}
	res, _ = fileProviderRequest(t, server, http.MethodGet, "/file?folder=folder&path=local.bin", nil)
	if res.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("stale local copy served: %d", res.StatusCode)
	}
}

func TestFileProviderServerNotFound(t *testing.T) {
	backend := &fakeFileProviderBackend{available: true}
	backend.addFile("file.txt", []byte("x"), time.Now())
	backend.files["dir"] = protocol.FileInfo{Name: "dir", Type: protocol.FileInfoTypeDirectory}
	backend.files["deleted.txt"] = protocol.FileInfo{Name: "deleted.txt", Deleted: true}
	backend.files["empty.txt"] = protocol.FileInfo{Name: "empty.txt"}
	server := startTestFileProviderServer(t, backend)

	for _, path := range []string{
		"/file?folder=other&path=file.txt",
		"/file?folder=folder&path=missing.txt",
		"/file?folder=folder&path=dir",
		"/file?folder=folder&path=deleted.txt",
		"/file?folder=folder",
	} {
		if res, _ := fileProviderRequest(t, server, http.MethodGet, path, nil); res.StatusCode != http.StatusNotFound {
			t.Errorf("%s: expected 404, got %d", path, res.StatusCode)
		}
	}

	res, body := fileProviderRequest(t, server, http.MethodGet, "/file?folder=folder&path=empty.txt", nil)
	if res.StatusCode != http.StatusOK || len(body) != 0 {
		t.Fatalf("empty file: %d, %q", res.StatusCode, body)
	}

	if res, _ := fileProviderRequest(t, server, http.MethodPost, "/file?folder=folder&path=file.txt", nil); res.StatusCode != http.StatusMethodNotAllowed {
		t.Fatalf("POST accepted: %d", res.StatusCode)
	}
}

func TestRemoteBlockReaderFailure(t *testing.T) {
	backend := &fakeFileProviderBackend{available: true}
	backend.addFile("file.bin", testContents(protocol.MinBlockSize+10), time.Now())
	backend.blocks["file.bin"] = backend.blocks["file.bin"][:1]
	reader := &remoteBlockReader{
		ctx:          context.Background(),
		backend:      backend,
		folderID:     "folder",
		info:         backend.files["file.bin"],
		blockTimeout: time.Second,
		blockIndex:   -1,
	}
	_, err := io.ReadAll(reader)
	if err == nil || !strings.Contains(err.Error(), "no such block") {
		t.Fatalf("expected failure for missing block, got %v", err)
	}
}
