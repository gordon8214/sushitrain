// Copyright (C) 2026 Gordon
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this file,
// You can obtain one at https://mozilla.org/MPL/2.0/.

package files

import (
	"context"
	"errors"
	"net"
	"strconv"
	"strings"
	"testing"
	"time"

	"t-shaped.nl/sushitrain/v2/gateway/gatewaytest"
	"t-shaped.nl/sushitrain/v2/remoteclient"
)

// A gateway that moved is reached again once its address is changed here, without pairing again
func TestChangingAGatewaysAddress(t *testing.T) {
	e := gatewaytest.NewEngine(t, "docs", nil)
	e.Write("readme.txt", "hello")
	g := gatewaytest.NewGateway(t, e, "")
	g.Export(true)
	ts := newTestService(t, g, 0)
	serviceID := ts.folder.ServiceID
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()

	// The gateway comes back on another port: the stored address no longer reaches it
	oldPort := g.Port
	g.Stop()
	g.Port = 0
	g.Start()
	if g.Port == oldPort {
		t.Fatal("the gateway came back on the same port")
	}
	e.Write("moved.txt", "after the move")
	g.Sync()
	if err := ts.Refresh(ctx, ts.folder.ID); remoteclient.KindOf(err) != remoteclient.KindUnreachable {
		t.Fatalf("refreshing at the old address: %v", err)
	}

	change, err := ts.SetGatewayAddress(ctx, serviceID, "127.0.0.1:"+strconv.Itoa(g.Port))
	if err != nil || change.Problem != "" {
		t.Fatalf("changing the address: %+v, %v", change, err)
	}
	want := remoteclient.FormatAddress("127.0.0.1", g.Port)
	if change.Gateway.Port != g.Port || change.Gateway.Address != want || !change.Gateway.Pinned {
		t.Fatalf("gateway after the change: %+v", change.Gateway)
	}
	ts.refresh()
	if f := ts.mustLookup("docs/moved.txt"); string(ts.read(f)) != "after the move" {
		t.Fatal("the folder didn't follow the gateway to its new address")
	}

	// A host without a port keeps the port
	if change, err = ts.SetGatewayAddress(ctx, serviceID, "localhost"); err != nil || change.Gateway.Port != g.Port ||
		change.Gateway.Host != "localhost" || change.Problem != "" {
		t.Fatalf("a host without a port: %+v, %v", change, err)
	}
}

// An address is kept even where nothing answers (it may work only away from the gateway's network), and the problem
// is told
func TestAnAddressNothingAnswersAtIsKept(t *testing.T) {
	e := gatewaytest.NewEngine(t, "docs", nil)
	g := gatewaytest.NewGateway(t, e, "")
	g.Export(true)
	ts := newTestService(t, g, 0)
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()

	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	closed := l.Addr().(*net.TCPAddr).Port
	l.Close()
	change, err := ts.SetGatewayAddress(ctx, ts.folder.ServiceID, "127.0.0.1:"+strconv.Itoa(closed))
	if err != nil || change.Gateway.Port != closed || !strings.Contains(change.Problem, "can't be reached") {
		t.Fatalf("an address nothing answers at: %+v, %v", change, err)
	}

	// Another gateway there has another key: it's refused, said so, and kept
	other := gatewaytest.NewGateway(t, gatewaytest.NewEngine(t, "other", nil), "")
	change, err = ts.SetGatewayAddress(ctx, ts.folder.ServiceID, "127.0.0.1:"+strconv.Itoa(other.Port))
	if err != nil || change.Gateway.Port != other.Port || change.Gateway.State != "untrusted" ||
		!strings.Contains(change.Problem, "certificate this machine paired with") {
		t.Fatalf("an address where another gateway answers: %+v, %v", change, err)
	}
	if err := ts.Refresh(ctx, ts.folder.ID); remoteclient.KindOf(err) != remoteclient.KindUntrusted {
		t.Fatalf("refreshing from another gateway: %v", err)
	}
}

// Changes that waited while the old address failed go to the new one at once, not after their backoff
func TestWaitingChangesTryANewAddressAtOnce(t *testing.T) {
	e := gatewaytest.NewEngine(t, "docs", nil)
	g := gatewaytest.NewGateway(t, e, "")
	g.Export(true)
	ts := newTestService(t, g, 0)
	g.Stop()
	g.Port = 0
	ts.create("docs", "while-away.txt", "made while the gateway was away")
	gatewaytest.WaitFor(t, 30*time.Second, "a failed attempt to send the change", func() bool {
		var n int
		_ = ts.store.db.QueryRow(`SELECT COUNT(*) FROM outbox WHERE state IN ` + activeStates + ` AND attempts > 0`).Scan(&n)
		return n > 0
	})
	// However long the backoff, the address change ends it
	if _, err := ts.store.db.Exec(`UPDATE outbox SET not_before = ? WHERE state IN `+activeStates,
		nowMillis()+time.Hour.Milliseconds()); err != nil {
		t.Fatal(err)
	}
	g.Start()
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	if change, err := ts.SetGatewayAddress(ctx, ts.folder.ServiceID, "127.0.0.1:"+strconv.Itoa(g.Port)); err != nil ||
		change.Problem != "" {
		t.Fatalf("changing the address: %+v, %v", change, err)
	}
	ts.settled()
	ts.gatewayHas("while-away.txt", "made while the gateway was away")
}

func TestAddressChangesThatAreRefused(t *testing.T) {
	e := gatewaytest.NewEngine(t, "docs", nil)
	g := gatewaytest.NewGateway(t, e, "")
	g.Export(true)
	ts := newTestService(t, g, 0)
	ctx := context.Background()
	serviceID := ts.folder.ServiceID

	for _, text := range []string{"", "studio example.org", "studio.example.org/path", "studio.example.org:70000"} {
		if _, err := ts.SetGatewayAddress(ctx, serviceID, text); !errors.Is(err, remoteclient.ErrNotAnAddress) {
			t.Errorf("%q: %v", text, err)
		}
	}
	if _, err := ts.SetGatewayAddress(ctx, "svc_unknown", "studio.example.org"); err == nil ||
		err.Error() != "no such gateway" {
		t.Errorf("an unknown gateway's address: %v", err)
	}
	ts.Service.cfg.AllowLocalGateway = false
	if _, err := ts.SetGatewayAddress(ctx, serviceID, "127.0.0.1:1"); !errors.Is(err, ErrLocalGateway) {
		t.Errorf("an address on this machine: %v", err)
	}
	// A gateway paired without a pin is trusted by the name in its certificate: only its port changes here
	if _, err := ts.store.db.Exec(`UPDATE gateways SET pins = '' WHERE service_id = ?`, serviceID); err != nil {
		t.Fatal(err)
	}
	if _, err := ts.SetGatewayAddress(ctx, serviceID, "studio.example.org"); !errors.Is(err, ErrUnpinnedHost) {
		t.Errorf("another host for a gateway without a pin: %v", err)
	}
	ts.Service.cfg.AllowLocalGateway = true
	change, err := ts.SetGatewayAddress(ctx, serviceID, strings.ToUpper(g.Host)+":"+strconv.Itoa(g.Port+1))
	if err != nil || change.Gateway.Port != g.Port+1 {
		t.Errorf("another port for a gateway without a pin: %+v, %v", change, err)
	}
	if _, err := ts.SetGatewayAddress(ctx, serviceID, g.Host+":"+strconv.Itoa(g.Port)); err != nil {
		t.Fatal(err)
	}
	ts.Service.cfg.AllowLocalGateway = false

	// Nothing refused was stored
	list, err := ts.Gateways()
	if err != nil || len(list) != 1 || list[0].Port != g.Port || list[0].Host != g.Host {
		t.Fatalf("gateways after refused changes: %+v, %v", list, err)
	}
}
