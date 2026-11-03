// Copyright (C) 2026 Gordon
// SPDX-License-Identifier: MPL-2.0

package adminsock

import (
	"bufio"
	"context"
	"encoding/json"
	"github.com/google/uuid"
	"io"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
)

func testEndpoint(t *testing.T) string {
	t.Helper()
	endpoint := `\\.\pipe\Synctrain-test-` + uuid.NewString()
	if runtime.GOOS != "windows" {
		dir, err := os.MkdirTemp("/tmp", "stadmin-")
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { os.RemoveAll(dir) })
		endpoint = filepath.Join(dir, "s")
	}
	return endpoint
}
func TestCallRejectsBinaryResponse(t *testing.T) {
	endpoint := testEndpoint(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	server := NewServer()
	server.Register("binary", func(context.Context, json.RawMessage) (any, error) {
		return &Stream{Reader: io.NopCloser(strings.NewReader("content")), Length: 7}, nil
	})
	if err := server.Listen(ctx, endpoint, false); err != nil {
		t.Fatal(err)
	}
	defer server.Close()
	if err := CallContext(ctx, endpoint, "binary", nil, nil); err == nil || !strings.Contains(err.Error(), "streaming client") {
		t.Fatalf("binary response falsely accepted: %v", err)
	}
}

type closeReader struct {
	io.Reader
	done chan struct{}
	once sync.Once
}

func (r *closeReader) Close() error { r.once.Do(func() { close(r.done) }); return nil }
func TestCancelClosesStalledStream(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	reader := &closeReader{Reader: strings.NewReader(strings.Repeat("x", 1<<20)), done: make(chan struct{})}
	server := NewServer()
	server.Register("binary", func(context.Context, json.RawMessage) (any, error) {
		return &Stream{Reader: reader, Length: 1 << 20}, nil
	})
	client, peer := net.Pipe()
	defer client.Close()
	done := make(chan struct{})
	go func() { server.serve(ctx, peer); close(done) }()
	_ = client.SetDeadline(time.Now().Add(2 * time.Second))
	if _, err := io.WriteString(client, "{\"method\":\"binary\"}\n"); err != nil {
		t.Fatal(err)
	}
	if _, err := bufio.NewReader(client).ReadBytes('\n'); err != nil {
		t.Fatal(err)
	}
	// Leave the binary body unread, as a stalled or disconnected Explorer request can.
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("server retained a stalled stream after cancellation")
	}
	select {
	case <-reader.done:
	default:
		t.Fatal("stream reader was not closed")
	}
}
