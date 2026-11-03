// Copyright (C) 2026 Gordon
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this file,
// You can obtain one at https://mozilla.org/MPL/2.0/.

//go:build !windows

package adminsock

import (
	"context"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"sync"
	"time"
)

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

func dialLocal(ctx context.Context, path string) (net.Conn, error) {
	return (&net.Dialer{}).DialContext(ctx, "unix", path)
}

func listenLocal(path string, groupAccess bool) (net.Listener, error) {
	if err := checkPrivateDirectory(filepath.Dir(path), groupAccess); err != nil {
		return nil, err
	}
	mode := os.FileMode(0600)
	if groupAccess {
		mode = 0660
	}
	if info, err := os.Lstat(path); err == nil {
		if info.Mode().Type() != os.ModeSocket {
			return nil, fmt.Errorf("%s exists and is not a socket", path)
		}
		if conn, err := net.DialTimeout("unix", path, time.Second); err == nil {
			conn.Close()
			return nil, fmt.Errorf("another process serves %s", path)
		}
		if err := os.Remove(path); err != nil {
			return nil, err
		}
	}
	return listenPrivately(path, mode)
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
