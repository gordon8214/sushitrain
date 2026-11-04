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

type contextualReader struct {
	ctx context.Context
	io.Reader
}

func (r contextualReader) Read(b []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	return r.Reader.Read(b)
}
func (r contextualReader) Close() error { return nil }
func TestDeadlineContextLivesThroughStream(t *testing.T) {
	endpoint := testEndpoint(t)
	server := NewServer()
	server.Register("binary", func(ctx context.Context, _ json.RawMessage) (any, error) {
		return &Stream{Reader: contextualReader{ctx, strings.NewReader("content")}, Length: 7}, nil
	})
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	defer cancel()
	if err := server.Listen(ctx, endpoint, false); err != nil {
		t.Fatal(err)
	}
	defer server.Close()
	conn, err := dialLocal(ctx, endpoint)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	conn.SetDeadline(time.Now().Add(2 * time.Second))
	encoded, _ := json.Marshal(Request{Method: "binary", Deadline: time.Now().Add(time.Second).UnixNano()})
	if _, err = conn.Write(append(encoded, '\n')); err != nil {
		t.Fatal(err)
	}
	reader := bufio.NewReader(conn)
	if _, err = reader.ReadBytes('\n'); err != nil {
		t.Fatal(err)
	}
	output := make([]byte, 7)
	if _, err = io.ReadFull(reader, output); err != nil {
		t.Fatal(err)
	}
	if string(output) != "content" {
		t.Fatal(string(output))
	}
}
func TestDisconnectCancelsHandler(t *testing.T) {
	endpoint := testEndpoint(t)
	server := NewServer()
	entered := make(chan struct{})
	finished := make(chan struct{})
	server.Register("wait", func(ctx context.Context, _ json.RawMessage) (any, error) {
		close(entered)
		<-ctx.Done()
		close(finished)
		return nil, ctx.Err()
	})
	if err := server.Listen(t.Context(), endpoint, false); err != nil {
		t.Fatal(err)
	}
	defer server.Close()
	conn, err := dialLocal(t.Context(), endpoint)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = io.WriteString(conn, "{\"method\":\"wait\"}\n"); err != nil {
		t.Fatal(err)
	}
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("handler never entered")
	}
	conn.Close()
	select {
	case <-finished:
	case <-time.After(time.Second):
		t.Fatal("disconnect left handler running")
	}
}
func TestCloseDrainsHandlersBeforeResources(t *testing.T) {
	endpoint := testEndpoint(t)
	server := NewServer()
	entered := make(chan struct{})
	exited := make(chan struct{})
	resources := make(chan struct{})
	server.Register("wait", func(ctx context.Context, _ json.RawMessage) (any, error) {
		close(entered)
		<-ctx.Done()
		close(exited)
		return nil, ctx.Err()
	})
	server.OnClose(func() {
		select {
		case <-exited:
		default:
			t.Error("resource closed before handler exited")
		}
		close(resources)
	})
	if err := server.Listen(t.Context(), endpoint, false); err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() { defer close(done); CallContext(t.Context(), endpoint, "wait", nil, nil) }()
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("handler never entered")
	}
	server.Close()
	select {
	case <-resources:
	default:
		t.Fatal("resource cleanup omitted")
	}
	<-done
}
