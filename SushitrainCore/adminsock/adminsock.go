// Copyright (C) 2026 Gordon
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this file,
// You can obtain one at https://mozilla.org/MPL/2.0/.

// Package adminsock is the admin socket of Synctrain's daemons: one line of JSON per request and per response over a
// Unix socket that only its owner (or group) can reach. The command-line interfaces and the Linux window manage a
// running daemon through it, since the daemon owns its databases.
package adminsock

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"sort"
	"sync"
	"time"
)

// Request is one line of JSON sent to the admin socket
type Request struct {
	Method   string          `json:"method"`
	Deadline int64           `json:"deadline,omitempty"`
	Params   json.RawMessage `json:"params,omitempty"`
}

// Response is the line sent back
type Response struct {
	Result    json.RawMessage `json:"result,omitempty"`
	Error     string          `json:"error,omitempty"`
	Length    int64           `json:"length,omitempty"`
	Streaming bool            `json:"stream,omitempty"`
}

// Stream is a bounded binary response following its JSON header. Its reader is closed after delivery.
// Ordinary Call methods keep their original one-line request/response contract.
type Stream struct {
	Reader io.ReadCloser
	Length int64
}

type Method func(ctx context.Context, params json.RawMessage) (any, error)

// Server serves registered methods on a Unix socket
type Server struct {
	ctx        context.Context
	cancel     context.CancelFunc
	acceptDone chan struct{}
	workers    sync.WaitGroup
	mutex      sync.Mutex
	methods    map[string]Method
	listener   net.Listener
	closeOnce  sync.Once
	cleanup    []func()
}

func NewServer() *Server {
	ctx, cancel := context.WithCancel(context.Background())
	return &Server{methods: make(map[string]Method), ctx: ctx, cancel: cancel}
}

func (a *Server) Register(name string, method Method) {
	a.mutex.Lock()
	defer a.mutex.Unlock()
	a.methods[name] = method
}

func (a *Server) Methods() []string {
	a.mutex.Lock()
	defer a.mutex.Unlock()
	names := make([]string, 0, len(a.methods))
	for name := range a.methods {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// Listen serves on a Unix socket at path. Only its owner may use it, or with groupAccess also the members of its
// group (such as an administrators group on a server). The socket's directory must not let anyone else replace the
// socket: owned by this user, and writable by others only if it is sticky, or by the group only with groupAccess.
func (a *Server) Listen(ctx context.Context, path string, groupAccess bool) error {
	listener, err := listenLocal(path, groupAccess)
	if err != nil {
		return err
	}
	stopParent := context.AfterFunc(ctx, a.cancel)
	a.OnClose(func() { stopParent() })
	ctx = a.ctx
	a.acceptDone = make(chan struct{})
	a.listener = listener
	go func() {
		<-ctx.Done()
		listener.Close()
	}()
	go func() {
		defer close(a.acceptDone)
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			a.workers.Add(1)
			go func() { defer a.workers.Done(); a.serve(ctx, conn) }()
		}
	}()
	return nil
}

// OnClose registers resource cleanup before Listen. Close is idempotent.
func (a *Server) OnClose(fn func()) { a.cleanup = append(a.cleanup, fn) }

func (a *Server) Close() {
	a.closeOnce.Do(func() {
		if a.cancel != nil {
			a.cancel()
		}
		if a.listener != nil {
			a.listener.Close()
		}
		if a.acceptDone != nil {
			<-a.acceptDone
		}
		a.workers.Wait()
		for i := len(a.cleanup) - 1; i >= 0; i-- {
			a.cleanup[i]()
		}
	})
}

func (a *Server) serve(ctx context.Context, conn net.Conn) {
	defer conn.Close()
	stop := context.AfterFunc(ctx, func() { _ = conn.Close() })
	defer stop()
	reader := bufio.NewReader(conn)
	for {
		_ = conn.SetReadDeadline(time.Now().Add(10 * time.Minute))
		line, err := reader.ReadBytes('\n')
		if err != nil {
			return
		}
		callCtx, callCancel := context.WithCancel(ctx)
		readReady := make(chan struct{})
		// A client uses one request/response at a time. Peeking never consumes a
		// pipelined request, and disconnecting cancels network/rendering work.
		go func() {
			_, err := reader.Peek(1)
			if err != nil {
				callCancel()
			}
			close(readReady)
		}()
		var req Request
		var resp Response
		var stream *Stream
		var methodCancel context.CancelFunc = func() {}
		if err := json.Unmarshal(line, &req); err != nil {
			resp.Error = "malformed request"
		} else {
			a.mutex.Lock()
			method, ok := a.methods[req.Method]
			a.mutex.Unlock()
			if !ok {
				resp.Error = fmt.Sprintf("unknown method %q", req.Method)
			} else if result, err := invoke(callCtx, req, method, &methodCancel); err != nil {
				resp.Error = err.Error()
			} else if value, ok := result.(*Stream); ok {
				stream = value
				resp.Length = value.Length
				resp.Streaming = true
			} else if encoded, err := json.Marshal(result); err != nil {
				resp.Error = err.Error()
			} else {
				resp.Result = encoded
			}
		}
		encoded, _ := json.Marshal(resp)
		_ = conn.SetWriteDeadline(time.Now().Add(time.Minute))
		if _, err := conn.Write(append(encoded, '\n')); err != nil {
			if stream != nil {
				stream.Reader.Close()
			}
			methodCancel()
			callCancel()
			return
		}
		if stream != nil {
			_, err := io.CopyN(conn, stream.Reader, stream.Length)
			stream.Reader.Close()
			if err != nil {
				methodCancel()
				callCancel()
				return
			}
		}
		methodCancel()
		callCancel()
		<-readReady
	}
}

// Call sends one request to an admin socket and decodes the result into result (if not nil). It waits for the
// answer as long as the method takes; CallContext bounds that.
func Call(path, method string, params any, result any) error {
	return CallContext(context.Background(), path, method, params, result)
}

// CallContext is Call, giving up when ctx is done (its deadline bounds the whole exchange)
func CallContext(ctx context.Context, path, method string, params any, result any) error {
	dialCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	conn, err := dialLocal(dialCtx, path)
	cancel()
	if err != nil {
		return fmt.Errorf("connecting to the service at %s: %w", path, err)
	}
	defer conn.Close()
	if deadline, ok := ctx.Deadline(); ok {
		_ = conn.SetDeadline(deadline)
	}
	stop := context.AfterFunc(ctx, func() { _ = conn.SetDeadline(time.Now()) })
	defer stop()
	req := Request{Method: method}
	if deadline, ok := ctx.Deadline(); ok {
		req.Deadline = deadline.UnixNano()
	}
	if params != nil {
		if req.Params, err = json.Marshal(params); err != nil {
			return err
		}
	}
	encoded, err := json.Marshal(req)
	if err != nil {
		return err
	}
	if _, err := conn.Write(append(encoded, '\n')); err != nil {
		return err
	}
	line, err := bufio.NewReader(conn).ReadBytes('\n')
	if err != nil {
		return err
	}
	var resp Response
	if err := json.Unmarshal(line, &resp); err != nil {
		return err
	}
	if resp.Error != "" {
		return errors.New(resp.Error)
	}
	if resp.Streaming || resp.Length != 0 {
		return errors.New("this method returns binary content; use a streaming client")
	}
	if result != nil && len(resp.Result) > 0 {
		return json.Unmarshal(resp.Result, result)
	}
	return nil
}

// A client deadline bounds backend work as well as its local socket wait.
func invoke(ctx context.Context, req Request, method Method, cleanup *context.CancelFunc) (any, error) {
	if req.Deadline > 0 {
		deadline := time.Unix(0, req.Deadline)
		if deadline.After(time.Now().Add(11 * time.Minute)) {
			deadline = time.Now().Add(11 * time.Minute)
		}
		var cancel context.CancelFunc
		ctx, cancel = context.WithDeadline(ctx, deadline)
		*cleanup = cancel
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return method(ctx, req.Params)
}

// Context ends before Close drains handlers and releases per-run resources.
func (a *Server) Context() context.Context { return a.ctx }
