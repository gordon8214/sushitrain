// Copyright (C) 2026 Gordon
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this file,
// You can obtain one at https://mozilla.org/MPL/2.0/.
package sushitrain

import (
	"errors"
	"reflect"
	"slices"
	"testing"

	"github.com/syncthing/syncthing/lib/config"
)

func TestConnectionOptionsChangeNothingUnlessEdited(t *testing.T) {
	client, wrapper := advancedTestClient(t)
	before := wrapper.Options()
	if err := client.SetConnectionOptions(client.ConnectionOptions()); err != nil {
		t.Fatal(err)
	}
	if after := wrapper.Options(); !reflect.DeepEqual(before, after) {
		t.Fatalf("changed:\n%+v\n%+v", before, after)
	}
}

func TestConnectionOptionsAreSaved(t *testing.T) {
	client, wrapper := advancedTestClient(t)
	options := client.ConnectionOptions()
	options.ConnectionLimitMax = "12"
	options.PriorityTCPLAN = "1"
	options.PriorityQUICLAN = "2"
	options.PriorityTCPWAN = "3"
	options.PriorityQUICWAN = "4"
	options.PriorityRelay = "5"
	options.PriorityUpgradeThreshold = "1"
	options.LocalAnnouncePort = "21028"
	options.LocalAnnounceMCAddr = "[ff12::1234]:21028"
	options.ReconnectionIntervalS = "30"
	options.RelayReconnectIntervalM = "2"
	options.NATLeaseMinutes = "90"
	options.NATRenewalMinutes = "45"
	options.NATTimeoutSeconds = "20"
	options.StunKeepaliveStartS = "60"
	options.AlwaysLocalNets = "100.64.0.0/10"
	options.OverwriteRemoteDeviceNames = true
	if err := client.SetConnectionOptions(options); err != nil {
		t.Fatal(err)
	}

	after := wrapper.Options()
	if after.ConnectionLimitMax != 12 || after.ConnectionPriorityTCPLAN != 1 || after.ConnectionPriorityQUICLAN != 2 ||
		after.ConnectionPriorityTCPWAN != 3 || after.ConnectionPriorityQUICWAN != 4 || after.ConnectionPriorityRelay != 5 ||
		after.ConnectionPriorityUpgradeThreshold != 1 || after.LocalAnnPort != 21028 ||
		after.LocalAnnMCAddr != "[ff12::1234]:21028" || after.ReconnectIntervalS != 30 ||
		after.RelayReconnectIntervalM != 2 || after.NATLeaseM != 90 || after.NATRenewalM != 45 || after.NATTimeoutS != 20 ||
		after.StunKeepaliveStartS != 60 || !slices.Equal(after.AlwaysLocalNets, []string{"100.64.0.0/10"}) ||
		!after.OverwriteRemoteDevNames {
		t.Fatalf("got %+v", after)
	}
	if !client.ConnectionOptions().Equal(options) {
		t.Fatal("the page would have something left to save")
	}
}

func TestConnectionOptionsLeaveChangesFromElsewhereAlone(t *testing.T) {
	client, wrapper := advancedTestClient(t)
	options := client.ConnectionOptions()
	if err := client.changeConfiguration(func(cfg *config.Configuration) { cfg.Options.NATLeaseM = 15 }); err != nil {
		t.Fatal(err)
	}
	options.ReconnectionIntervalS = "60"
	if err := client.SetConnectionOptions(options); err != nil {
		t.Fatal(err)
	}
	if after := wrapper.Options(); after.NATLeaseM != 15 || after.ReconnectIntervalS != 60 {
		t.Fatalf("got %+v", after)
	}
}

func TestConnectionOptionsAreChecked(t *testing.T) {
	tests := []struct {
		name string
		edit func(*ConnectionOptions)
		want error
	}{
		{"shorter than the engine takes", func(o *ConnectionOptions) { o.ReconnectionIntervalS = "4" }, nil},
		{"negative limit", func(o *ConnectionOptions) { o.ConnectionLimitMax = "-1" }, nil},
		{"port", func(o *ConnectionOptions) { o.LocalAnnouncePort = "0" }, nil},
		{"no number", func(o *ConnectionOptions) { o.NATTimeoutSeconds = "soon" }, nil},
		{"address that isn't multicast", func(o *ConnectionOptions) { o.LocalAnnounceMCAddr = "[2001:db8::1]:21027" },
			errMulticastAddress},
		{"address without a port", func(o *ConnectionOptions) { o.LocalAnnounceMCAddr = "ff12::8384" }, errMulticastAddress},
		{"multicast over IPv4", func(o *ConnectionOptions) { o.LocalAnnounceMCAddr = "239.0.0.1:21027" }, errMulticastAddress},
		{"no time for a router to answer", func(o *ConnectionOptions) { o.NATTimeoutSeconds = "0" }, nil},
		{"no time for STUN, which has its own setting", func(o *ConnectionOptions) { o.StunKeepaliveStartS = "0" }, nil},
		{"network", func(o *ConnectionOptions) { o.AlwaysLocalNets = "lan" }, nil},
		{"negation means nothing here", func(o *ConnectionOptions) { o.AlwaysLocalNets = "!10.0.0.0/8" }, nil},
		{"LAN at the priority of WAN, TCP", func(o *ConnectionOptions) { o.PriorityTCPLAN = "30" }, errPriorityTCP},
		{"WAN below LAN, QUIC", func(o *ConnectionOptions) { o.PriorityQUICWAN = "19" }, errPriorityQUIC},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			client, wrapper := advancedTestClient(t)
			before := wrapper.Options()
			options := client.ConnectionOptions()
			options.NATLeaseMinutes = "5"
			tt.edit(options)
			err := client.SetConnectionOptions(options)
			if err == nil {
				t.Fatal("saved")
			}
			if tt.want != nil && !errors.Is(err, tt.want) {
				t.Fatalf("got %v, want %v", err, tt.want)
			}
			if after := wrapper.Options(); !reflect.DeepEqual(before, after) {
				t.Fatal("something was changed")
			}
		})
	}
}

func TestConnectionOptionsNotSavedWhileLoading(t *testing.T) {
	if err := (&Client{}).SetConnectionOptions(&ConnectionOptions{}); !errors.Is(err, ErrStillLoading) {
		t.Fatalf("got %v", err)
	}
	if (&Client{}).ConnectionOptions() != nil {
		t.Fatal("got options")
	}
}
