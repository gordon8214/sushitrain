// Copyright (C) 2026 Gordon
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this file,
// You can obtain one at https://mozilla.org/MPL/2.0/.
package sushitrain

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/syncthing/syncthing/lib/config"
	"github.com/syncthing/syncthing/lib/osutil"
	"github.com/syncthing/syncthing/lib/protocol"
)

const (
	fileProviderHeaderBlocksHash = "X-Synctrain-Blocks-Hash"
	fileProviderHeaderSize       = "X-Synctrain-Size"
	fileProviderHeaderModNs      = "X-Synctrain-Mod-Ns"
	fileProviderHeaderModifiedBy = "X-Synctrain-Modified-By"
	fileProviderRetryAfter       = "30"
	fileProviderBlockTimeout     = 60 * time.Second
)

// What the file provider server needs from the client, so it can be tested without a running Syncthing instance
type fileProviderBackend interface {
	// The native path of the folder (empty when it has no local files) and the modification time window
	folderInfo(folderID string) (localPath string, modTimeWindow time.Duration, ok bool)
	globalFileInfo(folderID string, path string) (protocol.FileInfo, bool, error)
	// Whether a connected device can provide the given block
	isAvailable(folderID string, file protocol.FileInfo, blockIndex int) (bool, error)
	fetchBlock(ctx context.Context, folderID string, file protocol.FileInfo, blockIndex int) ([]byte, error)
}

// Serves file contents to the File Provider extension. It only listens on the loopback interface and requires a bearer
// token that changes every time it starts listening.
type FileProviderServer struct {
	backend      fileProviderBackend
	blockTimeout time.Duration

	mutex    sync.Mutex
	listener net.Listener
	server   *http.Server
	token    string
}

func NewFileProviderServer(client *Client) *FileProviderServer {
	return newFileProviderServer(&clientFileProviderBackend{client: client})
}

func newFileProviderServer(backend fileProviderBackend) *FileProviderServer {
	return &FileProviderServer{backend: backend, blockTimeout: fileProviderBlockTimeout}
}

// Starts listening on a new port with a new token, closing any previous listener
func (s *FileProviderServer) Listen() error {
	s.Shutdown()

	tokenBytes := make([]byte, 32)
	if _, err := rand.Read(tokenBytes); err != nil {
		return err
	}

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return err
	}

	server := &http.Server{
		Handler:           s.handler(),
		ReadHeaderTimeout: 10 * time.Second,
	}

	s.mutex.Lock()
	s.listener = listener
	s.server = server
	s.token = base64.RawURLEncoding.EncodeToString(tokenBytes)
	s.mutex.Unlock()

	go func() {
		if err := server.Serve(listener); err != nil && !errors.Is(err, http.ErrServerClosed) {
			slog.Warn("file provider server stopped", "cause", err)
		}
	}()
	slog.Info("file provider server listening", "address", listener.Addr().String())
	return nil
}

func (s *FileProviderServer) Port() int {
	s.mutex.Lock()
	defer s.mutex.Unlock()
	if s.listener == nil {
		return 0
	}
	return s.listener.Addr().(*net.TCPAddr).Port
}

func (s *FileProviderServer) Token() string {
	s.mutex.Lock()
	defer s.mutex.Unlock()
	return s.token
}

func (s *FileProviderServer) Shutdown() {
	s.mutex.Lock()
	server := s.server
	s.server = nil
	s.listener = nil
	s.token = ""
	s.mutex.Unlock()

	if server != nil {
		server.Close()
	}
}

// The token is presented as a bearer token, or as the "token" query parameter for clients that cannot set headers
// (such as AVFoundation streaming a video to make a thumbnail)
func (s *FileProviderServer) authorized(r *http.Request) bool {
	s.mutex.Lock()
	token := s.token
	s.mutex.Unlock()

	presented, found := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	if !found {
		presented = r.URL.Query().Get("token")
	}
	if presented == "" || token == "" {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(presented), []byte(token)) == 1
}

func (s *FileProviderServer) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/ping", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		w.Write([]byte("ok"))
	})
	mux.HandleFunc("/file", s.serveFile)

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !s.authorized(r) {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		mux.ServeHTTP(w, r)
	})
}

func (s *FileProviderServer) serveFile(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	folderID := r.URL.Query().Get("folder")
	name := strings.TrimPrefix(r.URL.Query().Get("path"), "/")
	localPath, modTimeWindow, ok := s.backend.folderInfo(folderID)
	if !ok || name == "" {
		http.NotFound(w, r)
		return
	}

	info, ok, err := s.backend.globalFileInfo(folderID, name)
	if err != nil {
		slog.Warn("file provider server could not get file information", "folderID", folderID, "cause", err)
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if !ok || info.IsDeleted() || info.IsInvalid() || info.Type != protocol.FileInfoTypeFile {
		http.NotFound(w, r)
		return
	}

	// Always describe the version that is being served, which may be newer than the one the caller asked for
	headers := w.Header()
	headers.Set("Cache-Control", "no-store")
	headers.Set("Content-Type", "application/octet-stream")
	headers.Set(fileProviderHeaderBlocksHash, base64.StdEncoding.EncodeToString(info.BlocksHash))
	headers.Set(fileProviderHeaderSize, strconv.FormatInt(info.Size, 10))
	headers.Set(fileProviderHeaderModNs, strconv.FormatInt(info.ModTime().UnixNano(), 10))
	headers.Set(fileProviderHeaderModifiedBy, info.FileModifiedBy().String())

	if info.Size == 0 {
		headers.Set("Content-Length", "0")
		w.WriteHeader(http.StatusOK)
		return
	}

	// Serve from disk only when the local copy is the global version. The opened file stays the same even when the
	// puller replaces it (which it always does by renaming), so checking the opened file is free of races.
	if localPath != "" {
		if file := openMatchingLocalFile(localPath, info, modTimeWindow); file != nil {
			defer file.Close()
			http.ServeContent(w, r, "", info.ModTime(), file)
			return
		}
	}

	available, err := s.backend.isAvailable(folderID, info, 0)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if !available {
		headers.Set("Retry-After", fileProviderRetryAfter)
		http.Error(w, "no connected device has this file", http.StatusServiceUnavailable)
		return
	}

	reader := &remoteBlockReader{
		ctx:          r.Context(),
		backend:      s.backend,
		folderID:     folderID,
		info:         info,
		blockTimeout: s.blockTimeout,
		blockIndex:   -1,
	}
	http.ServeContent(w, r, "", info.ModTime(), reader)
}

func openMatchingLocalFile(localPath string, info protocol.FileInfo, modTimeWindow time.Duration) *os.File {
	file, err := os.Open(filepath.Join(localPath, osutil.NativeFilename(info.Name)))
	if err != nil {
		return nil
	}
	stat, err := file.Stat()
	if err != nil || !stat.Mode().IsRegular() || stat.Size() != info.Size {
		file.Close()
		return nil
	}
	difference := stat.ModTime().Sub(info.ModTime()).Abs()
	if difference > modTimeWindow {
		file.Close()
		return nil
	}
	return file
}

// Reads a file from other devices block by block. Seeking is supported so that range requests can be served.
type remoteBlockReader struct {
	ctx          context.Context
	backend      fileProviderBackend
	folderID     string
	info         protocol.FileInfo
	blockTimeout time.Duration
	offset       int64

	// The most recently fetched block, as reads are typically much smaller than a block
	blockIndex int
	block      []byte
}

func (rd *remoteBlockReader) Seek(offset int64, whence int) (int64, error) {
	var target int64
	switch whence {
	case io.SeekStart:
		target = offset
	case io.SeekCurrent:
		target = rd.offset + offset
	case io.SeekEnd:
		target = rd.info.Size + offset
	default:
		return rd.offset, errors.New("invalid whence")
	}
	if target < 0 {
		return rd.offset, errors.New("negative offset")
	}
	rd.offset = target
	return target, nil
}

func (rd *remoteBlockReader) Read(p []byte) (int, error) {
	if rd.offset >= rd.info.Size {
		return 0, io.EOF
	}
	if len(p) == 0 {
		return 0, nil
	}

	blockSize := int64(rd.info.BlockSize())
	index := int(rd.offset / blockSize)
	if index >= len(rd.info.Blocks) {
		return 0, fmt.Errorf("offset %d is beyond the last block", rd.offset)
	}

	if index != rd.blockIndex {
		ctx, cancel := context.WithTimeout(rd.ctx, rd.blockTimeout)
		block, err := rd.backend.fetchBlock(ctx, rd.folderID, rd.info, index)
		cancel()
		if err != nil {
			return 0, err
		}
		if len(block) != int(rd.info.Blocks[index].Size) {
			return 0, fmt.Errorf("block %d has size %d, expected %d", index, len(block), rd.info.Blocks[index].Size)
		}
		rd.blockIndex = index
		rd.block = block
	}

	start := rd.offset - rd.info.Blocks[index].Offset
	n := copy(p, rd.block[start:])
	rd.offset += int64(n)
	return n, nil
}

type clientFileProviderBackend struct {
	client *Client
}

func (b *clientFileProviderBackend) folderInfo(folderID string) (string, time.Duration, bool) {
	// Measurements are only available once the client has started
	if b.client.app == nil || b.client.app.Internals == nil || b.client.Measurements == nil {
		return "", 0, false
	}
	folder := b.client.FolderWithID(folderID)
	if folder == nil {
		return "", 0, false
	}
	fc := folder.folderConfiguration()
	if fc == nil || fc.Type == config.FolderTypeReceiveEncrypted {
		return "", 0, false
	}
	localPath := ""
	if isNativeFilesystem(fc.FilesystemType) {
		if p, err := folder.LocalNativePath(); err == nil {
			localPath = p
		}
	}
	return localPath, fc.ModTimeWindow(), true
}

func (b *clientFileProviderBackend) globalFileInfo(folderID string, path string) (protocol.FileInfo, bool, error) {
	return b.client.app.Internals.GlobalFileInfo(folderID, path)
}

func (b *clientFileProviderBackend) isAvailable(folderID string, file protocol.FileInfo, blockIndex int) (bool, error) {
	internals := b.client.app.Internals
	availables, err := internals.BlockAvailability(folderID, file, file.Blocks[blockIndex])
	if err != nil {
		return false, err
	}
	for _, available := range availables {
		if internals.IsConnectedTo(available.ID) {
			return true, nil
		}
	}
	return false, nil
}

func (b *clientFileProviderBackend) fetchBlock(ctx context.Context, folderID string, file protocol.FileInfo, blockIndex int) ([]byte, error) {
	puller := newMiniPuller(b.client.Measurements, b.client.app.Internals)
	return puller.downloadBlock(ctx, folderID, blockIndex, file)
}
