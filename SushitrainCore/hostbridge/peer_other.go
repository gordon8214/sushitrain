// Copyright (C) 2026 Gordon
// SPDX-License-Identifier: MPL-2.0
//go:build !darwin || !cgo

package hostbridge

import (
	"errors"
	"net"
)

func authorizePeer(net.Conn) error {
	return errors.New("the native host bridge requires signed macOS processes")
}
