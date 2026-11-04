// Copyright (C) 2026 Gordon
// SPDX-License-Identifier: MPL-2.0
package linuxpolicy

import (
	"context"
	"errors"
	"github.com/godbus/dbus/v5"
	"time"
)

// SetResourcePriority changes the user's service cgroup, rather than claiming
// Linux can apply macOS QoS exclusively to scanner threads.
func SetResourcePriority(ctx context.Context, priority string) error {
	weight := uint64(100)
	switch priority {
	case "normal":
	case "utility":
		weight = 50
	case "background":
		weight = 10
	default:
		return errors.New("choose Normal, Utility or Background")
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	conn, err := connectBus(ctx, dbus.SessionBusPrivateNoAutoStartup)
	if err != nil {
		return err
	}
	defer conn.Close()
	properties := []struct {
		Name  string
		Value dbus.Variant
	}{{"CPUWeight", dbus.MakeVariant(weight)}, {"IOWeight", dbus.MakeVariant(weight)}}
	return conn.Object("org.freedesktop.systemd1", "/org/freedesktop/systemd1").CallWithContext(ctx, "org.freedesktop.systemd1.Manager.SetUnitProperties", 0, "synctrain-gateway.service", true, properties).Err
}

// The connection context closes its private transport if Auth or Hello stalls.
// Callers bound this context for the connection's entire operation.
func connectBus(ctx context.Context, dial func(...dbus.ConnOption) (*dbus.Conn, error)) (*dbus.Conn, error) {
	conn, err := dial(dbus.WithContext(ctx))
	if err != nil {
		return nil, err
	}
	if err = conn.Auth(nil); err == nil {
		err = conn.Hello()
	}
	if err != nil || ctx.Err() != nil {
		conn.Close()
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, err
	}
	return conn, nil
}
