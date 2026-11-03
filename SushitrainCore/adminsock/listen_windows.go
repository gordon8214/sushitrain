// Copyright (C) 2026 Gordon
// SPDX-License-Identifier: MPL-2.0

package adminsock

import (
	"context"
	"errors"
	"net"
	"strings"

	"github.com/Microsoft/go-winio"
	"golang.org/x/sys/windows"
	"t-shaped.nl/sushitrain/v2/winplatform"
)

func listenLocal(path string, groupAccess bool) (net.Listener, error) {
	if groupAccess {
		return nil, errors.New("Windows admin pipes only allow their current user")
	}
	if !strings.HasPrefix(path, `\\.\pipe\Synctrain-`) {
		return nil, errors.New("expected a Synctrain named pipe")
	}
	sid, err := winplatform.UserSID()
	if err != nil {
		return nil, err
	}
	return winio.ListenPipe(path, &winio.PipeConfig{SecurityDescriptor: "D:P(A;;GA;;;" + sid + ")", InputBufferSize: 65536, OutputBufferSize: 65536})
}

func dialLocal(ctx context.Context, path string) (net.Conn, error) {
	conn, err := winio.DialPipeContext(ctx, path)
	if err != nil {
		return nil, err
	}
	file, ok := conn.(interface{ Fd() uintptr })
	if !ok {
		conn.Close()
		return nil, errors.New("cannot authenticate the named-pipe server")
	}
	if err := winplatform.VerifyPipeServer(windows.Handle(file.Fd())); err != nil {
		conn.Close()
		return nil, err
	}
	return conn, nil
}
