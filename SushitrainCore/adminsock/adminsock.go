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
	"net"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"syscall"
	"time"
)

// Request is one line of JSON sent to the admin socket
type Request struct {
	Method string          `json:"method"`
	Params json.RawMessage `json:"params,omitempty"`
}

// Response is the line sent back
type Response struct {
	Result json.RawMessage `json:"result,omitempty"`
	Error  string          `json:"error,omitempty"`
}

type Method func(ctx context.Context, params json.RawMessage) (any, error)

// Server serves registered methods on a Unix socket
type Server struct {
	mutex    sync.Mutex
	methods  map[string]Method
	listener net.Listener
}

func NewServer() *Server {
	return &Server{methods: make(map[string]Method)}
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
	if err := checkPrivateDirectory(filepath.Dir(path), groupAccess); err != nil {
		return err
	}
	mode := os.FileMode(0o600)
	if groupAccess {
		mode = 0o660
	}
	// Replace only a socket that nothing serves any more (left by a previous run)
	if info, err := os.Lstat(path); err == nil {
		if info.Mode().Type() != os.ModeSocket {
			return fmt.Errorf("%s exists and is not a socket", path)
		}
		if conn, err := net.DialTimeout("unix", path, time.Second); err == nil {
			conn.Close()
			return fmt.Errorf("another process serves %s", path)
		}
		if err := os.Remove(path); err != nil {
			return err
		}
	}
	listener, err := listenPrivately(path, mode)
	if err != nil {
		return err
	}
	a.listener = listener
	go func() {
		<-ctx.Done()
		listener.Close()
	}()
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			go a.serve(ctx, conn)
		}
	}()
	return nil
}

// listenPrivately creates the socket in a new directory only this user can enter, gives it its mode there and then
// moves it into place, so that nobody else can reach it, or swap it for a link whose target would get that mode, before
// it is ready
func listenPrivately(path string, mode os.FileMode) (net.Listener, error) {
	dir, err := os.MkdirTemp(filepath.Dir(path), ".adm")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(dir)
	private := filepath.Join(dir, "s")
	listener, err := net.Listen("unix", private)
	if err != nil {
		return nil, err
	}
	// The listener must not remove the socket's old name when it closes
	listener.(*net.UnixListener).SetUnlinkOnClose(false)
	if err := os.Chmod(private, mode); err != nil {
		listener.Close()
		return nil, err
	}
	if err := os.Rename(private, path); err != nil {
		listener.Close()
		return nil, err
	}
	return &unlinkingListener{Listener: listener, path: path}, nil
}

// checkPrivateDirectory refuses a directory that someone else could change entries of: one owned by another user
// (other than root), writable by everyone without the sticky bit, or writable by its group unless groupWritable
func checkPrivateDirectory(dir string, groupWritable bool) error {
	info, err := os.Stat(dir)
	if err != nil {
		return err
	}
	if !info.IsDir() {
		return fmt.Errorf("%s is not a directory", dir)
	}
	mode := info.Mode()
	if st, ok := info.Sys().(*syscall.Stat_t); ok && int(st.Uid) != os.Geteuid() && st.Uid != 0 {
		return fmt.Errorf("%s belongs to another user (%d)", dir, st.Uid)
	}
	switch {
	case mode&0o002 != 0 && mode&os.ModeSticky == 0:
		return fmt.Errorf("%s is writable by everyone; restrict it (chmod o-w) or set its sticky bit", dir)
	case mode&0o020 != 0 && !groupWritable && mode&os.ModeSticky == 0:
		return fmt.Errorf("%s is writable by its group; restrict it (chmod g-w), or allow the group to use the "+
			"admin socket", dir)
	}
	return nil
}

// unlinkingListener removes the socket at its final path when closed
type unlinkingListener struct {
	net.Listener
	path string
	once sync.Once
}

func (l *unlinkingListener) Close() error {
	err := l.Listener.Close()
	l.once.Do(func() { _ = os.Remove(l.path) })
	return err
}

func (a *Server) Close() {
	if a.listener != nil {
		a.listener.Close()
	}
}

func (a *Server) serve(ctx context.Context, conn net.Conn) {
	defer conn.Close()
	reader := bufio.NewReader(conn)
	for {
		_ = conn.SetReadDeadline(time.Now().Add(10 * time.Minute))
		line, err := reader.ReadBytes('\n')
		if err != nil {
			return
		}
		var req Request
		var resp Response
		if err := json.Unmarshal(line, &req); err != nil {
			resp.Error = "malformed request"
		} else {
			a.mutex.Lock()
			method, ok := a.methods[req.Method]
			a.mutex.Unlock()
			if !ok {
				resp.Error = fmt.Sprintf("unknown method %q", req.Method)
			} else if result, err := method(ctx, req.Params); err != nil {
				resp.Error = err.Error()
			} else if encoded, err := json.Marshal(result); err != nil {
				resp.Error = err.Error()
			} else {
				resp.Result = encoded
			}
		}
		encoded, _ := json.Marshal(resp)
		if _, err := conn.Write(append(encoded, '\n')); err != nil {
			return
		}
	}
}

// Call sends one request to an admin socket and decodes the result into result (if not nil). It waits for the
// answer as long as the method takes; CallContext bounds that.
func Call(path, method string, params any, result any) error {
	return CallContext(context.Background(), path, method, params, result)
}

// CallContext is Call, giving up when ctx is done (its deadline bounds the whole exchange)
func CallContext(ctx context.Context, path, method string, params any, result any) error {
	var dialer net.Dialer
	dialCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	conn, err := dialer.DialContext(dialCtx, "unix", path)
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
	if result != nil && len(resp.Result) > 0 {
		return json.Unmarshal(resp.Result, result)
	}
	return nil
}
