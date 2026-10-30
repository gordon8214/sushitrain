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
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/syncthing/syncthing/lib/config"
	"github.com/syncthing/syncthing/lib/protocol"
)

type fakeFileProviderBackend struct {
	localPath string
	files     map[string]protocol.FileInfo
	blocks    map[string][][]byte
	available bool
	// The engine restarts or stopped
	stopped bool

	mutex   sync.Mutex
	fetches int
}

func (b *fakeFileProviderBackend) running() bool {
	return !b.stopped
}

func (b *fakeFileProviderBackend) folderInfo(folderID string) (fileProviderFolder, bool) {
	return fileProviderFolder{localPath: b.localPath, markerName: config.DefaultMarkerName}, folderID == "folder"
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
	if server.server.IdleTimeout <= 0 {
		t.Fatal("idle connections are kept forever")
	}

	// Files can also be requested with the token as a query parameter, but nothing else can
	fileURL := fmt.Sprintf("http://127.0.0.1:%d/file?folder=other&path=x", server.Port())
	for _, tc := range []struct {
		method, url, token string
		status             int
	}{
		{http.MethodGet, fileURL, "wrong", http.StatusUnauthorized},
		{http.MethodGet, fileURL, "", http.StatusUnauthorized},
		{http.MethodGet, fileURL, server.Token(), http.StatusNotFound},
		{http.MethodHead, fileURL, server.Token(), http.StatusNotFound},
		{http.MethodPost, fileURL, server.Token(), http.StatusUnauthorized},
		{http.MethodGet, url + "?", server.Token(), http.StatusUnauthorized},
	} {
		req, _ := http.NewRequest(tc.method, tc.url+"&token="+tc.token, nil)
		res, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		res.Body.Close()
		if res.StatusCode != tc.status {
			t.Fatalf("%s %s with token parameter %q: status %d, expected %d", tc.method, tc.url, tc.token, res.StatusCode, tc.status)
		}
		// Unauthorized clients do not get to keep the connection
		if tc.status == http.StatusUnauthorized && !res.Close {
			t.Fatalf("%s %s: connection kept open after 401", tc.method, tc.url)
		}
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

// Escapes everything except unreserved characters, as the File Provider extension does
func strictQueryEscape(value string) string {
	var b strings.Builder
	for _, c := range []byte(value) {
		if 'a' <= c && c <= 'z' || 'A' <= c && c <= 'Z' || '0' <= c && c <= '9' || strings.IndexByte("-._~/", c) >= 0 {
			b.WriteByte(c)
		} else {
			fmt.Fprintf(&b, "%%%02X", c)
		}
	}
	return b.String()
}

func TestFileProviderServerQueryEncoding(t *testing.T) {
	backend := &fakeFileProviderBackend{available: true}
	names := []string{"C++ notes.txt", "a;b.txt", "x & y.txt", "#hash.txt", "100% done.txt", "a=b.txt", "dir/Cafe\u0301.txt"}
	for _, name := range names {
		backend.addFile(name, []byte(name), time.Now())
	}
	server := startTestFileProviderServer(t, backend)

	for _, name := range names {
		res, body := fileProviderRequest(t, server, http.MethodGet, "/file?folder=folder&path="+strictQueryEscape(name), nil)
		if res.StatusCode != http.StatusOK || string(body) != name {
			t.Errorf("%q: %d %q", name, res.StatusCode, body)
		}
	}

	// "+" is never a space, and ";" does not end a value, even when sent without escaping
	for query, name := range map[string]string{
		"folder=folder&path=C++%20notes.txt": "C++ notes.txt",
		"path=a;b.txt&folder=folder":         "a;b.txt",
	} {
		res, body := fileProviderRequest(t, server, http.MethodGet, "/file?"+query, nil)
		if res.StatusCode != http.StatusOK || string(body) != name {
			t.Errorf("%q: %d %q", query, res.StatusCode, body)
		}
	}

	for _, query := range []string{"folder=folder&path=%zz", "folder=folder&path=a%2", "folder%=folder&path=a"} {
		if res, _ := fileProviderRequest(t, server, http.MethodGet, "/file?"+query, nil); res.StatusCode != http.StatusBadRequest {
			t.Errorf("malformed query %q: status %d", query, res.StatusCode)
		}
	}
}

func TestFileProviderServerPrivatePaths(t *testing.T) {
	backend := &fakeFileProviderBackend{available: true}
	names := []string{".stignore", ".stfolder/x", ".stversions/a.txt", "dir/.stversions/a.txt", "dir/.syncthing.a.tmp", "~syncthing~a", "nul\x00name"}
	for _, name := range names {
		backend.addFile(name, []byte("secret"), time.Now())
	}
	server := startTestFileProviderServer(t, backend)

	for _, name := range names {
		if res, _ := fileProviderRequest(t, server, http.MethodGet, "/file?folder=folder&path="+strictQueryEscape(name), nil); res.StatusCode != http.StatusNotFound {
			t.Errorf("%q: expected 404, got %d", name, res.StatusCode)
		}
	}
	if backend.fetches != 0 {
		t.Fatalf("private files were fetched %d times", backend.fetches)
	}
}

func TestFileProviderServerInconsistentBlocks(t *testing.T) {
	blockSize := protocol.MinBlockSize
	for name, corrupt := range map[string]func(info *protocol.FileInfo){
		"no blocks":         func(info *protocol.FileInfo) { info.Blocks = nil },
		"extra block":       func(info *protocol.FileInfo) { info.Blocks = append(info.Blocks, info.Blocks[2]) },
		"missing block":     func(info *protocol.FileInfo) { info.Blocks = info.Blocks[:2] },
		"shifted block":     func(info *protocol.FileInfo) { info.Blocks[1].Offset += 10 },
		"oversized block":   func(info *protocol.FileInfo) { info.Blocks[0].Size = blockSize + 1 },
		"empty block":       func(info *protocol.FileInfo) { info.Blocks[1].Size = 0 },
		"short last block":  func(info *protocol.FileInfo) { info.Blocks[2].Size = 500 },
		"larger file":       func(info *protocol.FileInfo) { info.Size += 10 },
		"huge block size":   func(info *protocol.FileInfo) { info.RawBlockSize = protocol.MaxBlockSize * 2 },
		"smaller blocksize": func(info *protocol.FileInfo) { info.RawBlockSize = int32(blockSize * 2) },
	} {
		t.Run(name, func(t *testing.T) {
			backend := &fakeFileProviderBackend{available: true}
			backend.addFile("file.bin", testContents(blockSize*2+1000), time.Now())
			info := backend.files["file.bin"]
			info.Blocks = slices.Clone(info.Blocks)
			corrupt(&info)
			backend.files["file.bin"] = info
			server := startTestFileProviderServer(t, backend)

			res, _ := fileProviderRequest(t, server, http.MethodGet, "/file?folder=folder&path=file.bin", map[string]string{
				"Range": "bytes=0-10,200000-200010",
			})
			if res.StatusCode != http.StatusBadGateway || backend.fetches != 0 {
				t.Fatalf("inconsistent file information served: %d, %d fetches", res.StatusCode, backend.fetches)
			}
		})
	}
}

func TestRemoteBlockReaderInconsistentLayout(t *testing.T) {
	newReader := func(ctx context.Context, corrupt func(info *protocol.FileInfo)) *remoteBlockReader {
		backend := &fakeFileProviderBackend{available: true}
		backend.addFile("file.bin", testContents(protocol.MinBlockSize+1000), time.Now())
		info := backend.files["file.bin"]
		info.Blocks = slices.Clone(info.Blocks)
		corrupt(&info)
		return &remoteBlockReader{ctx: ctx, backend: backend, folderID: "folder", info: info, blockTimeout: time.Second, blockIndex: -1}
	}

	// A block that starts after the offset being read
	reader := newReader(context.Background(), func(info *protocol.FileInfo) { info.Blocks[1].Offset += 10 })
	if _, err := io.ReadAll(reader); !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatalf("expected an unexpected EOF for a shifted block, got %v", err)
	}

	// A file larger than its blocks must not make reads return nothing forever
	reader = newReader(context.Background(), func(info *protocol.FileInfo) { info.Size += 10 })
	done := make(chan error, 1)
	go func() {
		_, err := io.ReadAll(reader)
		done <- err
	}()
	select {
	case err := <-done:
		if !errors.Is(err, io.ErrUnexpectedEOF) {
			t.Fatalf("expected an unexpected EOF for a file larger than its blocks, got %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("reading a file larger than its blocks does not end")
	}

	// Reads stop when the request is gone
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	reader = newReader(ctx, func(info *protocol.FileInfo) {})
	if _, err := reader.Read(make([]byte, 10)); !errors.Is(err, context.Canceled) {
		t.Fatalf("expected the context error, got %v", err)
	}
}

func TestBlockFetchTimeout(t *testing.T) {
	cases := []struct {
		size int
		want time.Duration
	}{
		{128 << 10, fileProviderBlockTimeout},
		{2 << 20, 64 * time.Second},
		{16 << 20, 512 * time.Second},
	}
	for _, c := range cases {
		if got := blockFetchTimeout(fileProviderBlockTimeout, c.size); got != c.want {
			t.Errorf("blockFetchTimeout(%d) = %v, want %v", c.size, got, c.want)
		}
	}
}

// While the engine restarts, files are not found but unavailable for a while, which the extension retries
func TestFileProviderServerAnswersUnavailableWhileTheEngineIsDown(t *testing.T) {
	server := startTestFileProviderServer(t, &fakeFileProviderBackend{stopped: true})
	res, _ := fileProviderRequest(t, server, http.MethodGet, "/file?folder=folder&path=a.txt", nil)
	if res.StatusCode != http.StatusServiceUnavailable || res.Header.Get("Retry-After") == "" {
		t.Fatalf("status %d, Retry-After %q", res.StatusCode, res.Header.Get("Retry-After"))
	}
}
