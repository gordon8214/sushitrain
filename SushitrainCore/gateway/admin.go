// Copyright (C) 2026 Gordon
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this file,
// You can obtain one at https://mozilla.org/MPL/2.0/.

package gateway

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

// The admin socket is how the command-line interface (and other local tools) manage a running service: one engine
// owns Syncthing's database, so management goes through the process that runs it rather than opening the database
// a second time. Access is controlled by the socket's file permissions.

// AdminRequest is one line of JSON sent to the admin socket
type AdminRequest struct {
	Method string          `json:"method"`
	Params json.RawMessage `json:"params,omitempty"`
}

// AdminResponse is the line sent back
type AdminResponse struct {
	Result json.RawMessage `json:"result,omitempty"`
	Error  string          `json:"error,omitempty"`
}

type AdminMethod func(ctx context.Context, params json.RawMessage) (any, error)

// AdminServer serves registered methods on a Unix socket
type AdminServer struct {
	mutex    sync.Mutex
	methods  map[string]AdminMethod
	listener net.Listener
}

func NewAdminServer() *AdminServer {
	return &AdminServer{methods: make(map[string]AdminMethod)}
}

func (a *AdminServer) Register(name string, method AdminMethod) {
	a.mutex.Lock()
	defer a.mutex.Unlock()
	a.methods[name] = method
}

func (a *AdminServer) Methods() []string {
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
func (a *AdminServer) Listen(ctx context.Context, path string, groupAccess bool) error {
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

func (a *AdminServer) Close() {
	if a.listener != nil {
		a.listener.Close()
	}
}

func (a *AdminServer) serve(ctx context.Context, conn net.Conn) {
	defer conn.Close()
	reader := bufio.NewReader(conn)
	for {
		_ = conn.SetReadDeadline(time.Now().Add(10 * time.Minute))
		line, err := reader.ReadBytes('\n')
		if err != nil {
			return
		}
		var req AdminRequest
		var resp AdminResponse
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

// AdminCall sends one request to an admin socket and decodes the result into result (if not nil)
func AdminCall(path, method string, params any, result any) error {
	conn, err := net.DialTimeout("unix", path, 5*time.Second)
	if err != nil {
		return fmt.Errorf("connecting to the service at %s: %w", path, err)
	}
	defer conn.Close()
	req := AdminRequest{Method: method}
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
	var resp AdminResponse
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

// Status summarizes the service for the host's status displays
type Status struct {
	ServiceID           string         `json:"serviceID"`
	ServiceName         string         `json:"serviceName"`
	DeviceID            string         `json:"deviceID"`
	Address             string         `json:"address"`
	SPKI                string         `json:"spki,omitempty"`
	AuthorityGeneration int64          `json:"authorityGeneration"`
	Exports             []ExportStatus `json:"exports"`
	Clients             int            `json:"clients"`
	PendingOperations   int            `json:"pendingOperations"`
	PendingPairings     int            `json:"pendingPairings"`
	ContentCacheBytes   int64          `json:"contentCacheBytes"`
	ContentCacheQuota   int64          `json:"contentCacheQuota"`
}

type ExportStatus struct {
	Export
	Label string `json:"label"`
	State string `json:"state"`
	Items int64  `json:"items"`
}

func (s *Service) Status() (Status, error) {
	st := Status{ServiceName: s.cfg.ServiceName, DeviceID: s.backend.DeviceID().String(), SPKI: s.SPKI(),
		ContentCacheQuota: s.cfg.SpoolQuota}
	var err error
	if st.ServiceID, err = s.store.serviceID(); err != nil {
		return st, err
	}
	if st.AuthorityGeneration, err = s.store.generation(); err != nil {
		return st, err
	}
	if addr := s.Addr(); addr != nil {
		st.Address = addr.String()
	}
	exports, err := s.Exports()
	if err != nil {
		return st, err
	}
	for _, e := range exports {
		es := ExportStatus{Export: e, State: s.backend.FolderState(e.FolderID)}
		if info, ok := s.backend.Folder(e.FolderID); ok {
			es.Label = info.Label
		}
		_ = s.store.db.QueryRow(`SELECT COUNT(*) FROM items WHERE folder_id = ? AND deleted = 0`, e.FolderID).Scan(&es.Items)
		st.Exports = append(st.Exports, es)
	}
	_ = s.store.db.QueryRow(`SELECT COUNT(*) FROM clients WHERE revoked_at IS NULL`).Scan(&st.Clients)
	_ = s.store.db.QueryRow(`SELECT COUNT(*) FROM operations WHERE state = 'pending'`).Scan(&st.PendingOperations)
	pending, _ := s.PendingPairings()
	st.PendingPairings = len(pending)
	s.content.mutex.Lock()
	st.ContentCacheBytes = s.content.reserved
	s.content.mutex.Unlock()
	return st, nil
}

// PendingOperation describes an operation that has not finished, for the host's display. File names are left out.
type PendingOperation struct {
	Operation string    `json:"operation"`
	ClientID  string    `json:"clientID"`
	Kind      string    `json:"kind"`
	FolderID  string    `json:"folderID"`
	Journal   string    `json:"journal"`
	Created   time.Time `json:"created"`
}

func (s *Service) PendingOperations() ([]PendingOperation, error) {
	rows, err := s.store.db.Query(`SELECT op_id, client_id, kind, folder_id, journal, created_at FROM operations
		WHERE state = 'pending' ORDER BY created_at`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var result []PendingOperation
	for rows.Next() {
		var p PendingOperation
		var created int64
		if err := rows.Scan(&p.Operation, &p.ClientID, &p.Kind, &p.FolderID, &p.Journal, &created); err != nil {
			return nil, err
		}
		p.Created = time.Unix(created, 0)
		result = append(result, p)
	}
	return result, rows.Err()
}

// RegisterAdmin adds the service's management methods to an admin server. pairHost and pairPort are where clients
// reach the service, for pairing codes.
func (s *Service) RegisterAdmin(a *AdminServer, pairHost string, pairPort int) {
	decode := func(params json.RawMessage, v any) error {
		if len(params) == 0 {
			return errors.New("missing parameters")
		}
		return json.Unmarshal(params, v)
	}
	a.Register("status", func(context.Context, json.RawMessage) (any, error) { return s.Status() })
	a.Register("exports.list", func(context.Context, json.RawMessage) (any, error) { return s.Exports() })
	a.Register("exports.set", func(_ context.Context, params json.RawMessage) (any, error) {
		var e Export
		if err := decode(params, &e); err != nil {
			return nil, err
		}
		return nil, s.SetExport(e.FolderID, e.AllowWrite)
	})
	a.Register("exports.remove", func(_ context.Context, params json.RawMessage) (any, error) {
		var e Export
		if err := decode(params, &e); err != nil {
			return nil, err
		}
		return nil, s.RemoveExport(e.FolderID)
	})
	a.Register("clients.list", func(context.Context, json.RawMessage) (any, error) { return s.Clients() })
	a.Register("clients.revoke", func(_ context.Context, params json.RawMessage) (any, error) {
		var p struct {
			ClientID string `json:"clientID"`
		}
		if err := decode(params, &p); err != nil {
			return nil, err
		}
		return nil, s.RevokeClient(p.ClientID)
	})
	a.Register("clients.permission", func(_ context.Context, params json.RawMessage) (any, error) {
		var p struct {
			ClientID string `json:"clientID"`
			FolderID string `json:"folderID"`
			Access   string `json:"access"`
		}
		if err := decode(params, &p); err != nil {
			return nil, err
		}
		return nil, s.SetPermission(p.ClientID, p.FolderID, p.Access)
	})
	a.Register("pairing.start", func(context.Context, json.RawMessage) (any, error) {
		spki := ""
		if s.UsesSelfSignedCertificate() {
			spki = s.SPKI()
		}
		return s.StartPairing(pairHost, pairPort, spki)
	})
	a.Register("pairing.pending", func(context.Context, json.RawMessage) (any, error) { return s.PendingPairings() })
	a.Register("pairing.wait", func(ctx context.Context, params json.RawMessage) (any, error) {
		// Waits up to a minute for a request of the given session
		var p struct {
			SessionID string `json:"sessionID"`
		}
		_ = json.Unmarshal(params, &p)
		deadline := time.Now().Add(time.Minute)
		for {
			changed := s.pairingChanged.wait("")
			pending, err := s.PendingPairings()
			if err != nil {
				return nil, err
			}
			for _, req := range pending {
				if p.SessionID == "" || req.SessionID == p.SessionID {
					return req, nil
				}
			}
			select {
			case <-changed:
			case <-time.After(time.Until(deadline)):
				return nil, errors.New("no pairing request yet")
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		}
	})
	a.Register("pairing.approve", func(_ context.Context, params json.RawMessage) (any, error) {
		var p struct {
			RequestID string            `json:"requestID"`
			Grants    map[string]string `json:"grants"`
		}
		if err := decode(params, &p); err != nil {
			return nil, err
		}
		return nil, s.ApprovePairing(p.RequestID, p.Grants)
	})
	a.Register("pairing.deny", func(_ context.Context, params json.RawMessage) (any, error) {
		var p struct {
			RequestID string `json:"requestID"`
		}
		if err := decode(params, &p); err != nil {
			return nil, err
		}
		return nil, s.DenyPairing(p.RequestID)
	})
	a.Register("operations.pending", func(context.Context, json.RawMessage) (any, error) { return s.PendingOperations() })
}
