// Copyright (C) 2026 Gordon
// SPDX-License-Identifier: MPL-2.0
package desktoptray

import (
	"bufio"
	"context"
	"fmt"
	"github.com/godbus/dbus/v5"
	"os/exec"
	"testing"
	"time"
)

type testWatcher struct{ registered chan string }

func (w *testWatcher) RegisterStatusNotifierItem(service string) *dbus.Error {
	select {
	case w.registered <- service:
	default:
	}
	return nil
}
func startWatcher(t *testing.T, address string) (*dbus.Conn, *testWatcher) {
	t.Helper()
	conn, err := dbus.Connect(address)
	if err != nil {
		t.Fatal(err)
	}
	watcher := &testWatcher{registered: make(chan string, 4)}
	if err = conn.Export(watcher, "/StatusNotifierWatcher", "org.kde.StatusNotifierWatcher"); err != nil {
		t.Fatal(err)
	}
	if _, err = conn.RequestName("org.kde.StatusNotifierWatcher", dbus.NameFlagDoNotQueue); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close() })
	return conn, watcher
}
func TestTrayWithoutHostThenHostRestart(t *testing.T) {
	executable, err := exec.LookPath("dbus-daemon")
	if err != nil {
		t.Fatal("install dbus-daemon for the Linux tray integration test")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
	defer cancel()
	daemon := exec.CommandContext(ctx, executable, "--session", "--nofork", "--print-address=1")
	out, err := daemon.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err = daemon.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { daemon.Process.Kill(); daemon.Wait() }()
	address, err := bufio.NewReader(out).ReadString('\n')
	if err != nil {
		t.Fatal(err)
	}
	address = address[:len(address)-1]
	t.Setenv("DBUS_SESSION_BUS_ADDRESS", address)
	clicked := make(chan string, 4)
	tray, err := New([]Action{{Label: "Show", Run: func() { clicked <- "show" }}, {Label: "Pause", Run: func() { clicked <- "pause" }}})
	if err != nil {
		t.Fatal(err)
	}
	defer tray.Close()
	host, watcher := startWatcher(t, address)
	var name string
	select {
	case name = <-watcher.registered:
	case <-ctx.Done():
		t.Fatal("tray did not register with a newly available host")
	}
	item := host.Object(name, "/StatusNotifierItem")
	if err = item.CallWithContext(ctx, itemInterface+".Activate", 0, int32(0), int32(0)).Err; err != nil {
		t.Fatal(err)
	}
	select {
	case got := <-clicked:
		if got != "show" {
			t.Fatal(got)
		}
	case <-ctx.Done():
		t.Fatal("tray activation did not dispatch its action")
	}
	menu := host.Object(name, "/MenuBar")
	var revision uint32
	var layout Layout
	if err = menu.CallWithContext(ctx, menuInterface+".GetLayout", 0, int32(0), int32(-1), []string{}).Store(&revision, &layout); err != nil {
		t.Fatal(err)
	}
	if len(layout.Children) != 2 {
		t.Fatal("menu lost native actions", layout)
	}
	if err = menu.CallWithContext(ctx, menuInterface+".Event", 0, int32(2), "clicked", dbus.MakeVariant(int32(0)), uint32(0)).Err; err != nil {
		t.Fatal(err)
	}
	select {
	case got := <-clicked:
		if got != "pause" {
			t.Fatal(got)
		}
	case <-ctx.Done():
		t.Fatal("tray menu did not dispatch its action")
	}
	host.Close()
	_, replacement := startWatcher(t, address)
	select {
	case registered := <-replacement.registered:
		if registered != name {
			t.Fatal(fmt.Sprint(registered))
		}
	case <-ctx.Done():
		t.Fatal("tray did not reconnect after its host restarted")
	}
}
