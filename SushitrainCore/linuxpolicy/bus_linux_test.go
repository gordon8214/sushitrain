// Copyright (C) 2026 Gordon
// SPDX-License-Identifier: MPL-2.0
package linuxpolicy

import (
	"context"
	"errors"
	"net"
	"path/filepath"
	"testing"
	"time"
)

func TestBusHandshakeCancellation(t *testing.T) {
	for _, bus := range []string{"system", "session"} {
		t.Run(bus, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "bus")
			listener, err := net.Listen("unix", path)
			if err != nil {
				t.Fatal(err)
			}
			accepted := make(chan net.Conn, 1)
			go func() {
				if conn, err := listener.Accept(); err == nil {
					accepted <- conn
				}
			}()
			t.Cleanup(func() {
				listener.Close()
				select {
				case conn := <-accepted:
					conn.Close()
				default:
				}
			})
			variable := "DBUS_SYSTEM_BUS_ADDRESS"
			if bus == "session" {
				variable = "DBUS_SESSION_BUS_ADDRESS"
			}
			t.Setenv(variable, "unix:path="+path)
			ctx, cancel := context.WithTimeout(t.Context(), 100*time.Millisecond)
			defer cancel()
			done := make(chan error, 1)
			go func() {
				if bus == "system" {
					if environment := ReadEnvironment(ctx); environment != (Environment{}) {
						done <- errors.New("stalled bus returned a known policy")
						return
					}
					done <- nil
				} else {
					done <- SetResourcePriority(ctx, "normal")
				}
			}()
			select {
			case err = <-done:
				if bus == "session" && !errors.Is(err, context.DeadlineExceeded) || bus == "system" && err != nil {
					t.Fatal(err)
				}
			case <-time.After(time.Second):
				t.Fatal("bus authentication ignored cancellation")
			}
		})
	}
}
