// Copyright (C) 2026 Gordon
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this file,
// You can obtain one at https://mozilla.org/MPL/2.0/.
package sushitrain

import (
	"errors"
	"fmt"
	"math"
	"net/netip"
	"strconv"
	"strings"

	"github.com/syncthing/syncthing/lib/config"
)

const (
	// The engine uses this instead of anything shorter
	minReconnectionIntervalS = 5
	maxIntervalSeconds       = config.MaxRescanIntervalS
	maxIntervalMinutes       = config.MaxRescanIntervalS / secondsPerMinute
)

var (
	errPriorityTCP      = errors.New("TCP over the internet has to have a higher number than TCP on the local network")
	errPriorityQUIC     = errors.New("QUIC over the internet has to have a higher number than QUIC on the local network")
	errMulticastAddress = errors.New("enter an IPv6 multicast address with its port, such as [ff12::8384]:21027")
)

// ConnectionOptions are how the engine connects to other devices, for what has no setting of its own in the app, which it
// shows and saves together. Numbers are text, as they were entered, for SetConnectionOptions to check.
type ConnectionOptions struct {
	ConnectionLimitMax       string
	PriorityTCPLAN           string
	PriorityQUICLAN          string
	PriorityTCPWAN           string
	PriorityQUICWAN          string
	PriorityRelay            string
	PriorityUpgradeThreshold string
	LocalAnnouncePort        string
	LocalAnnounceMCAddr      string
	ReconnectionIntervalS    string
	RelayReconnectIntervalM  string
	NATLeaseMinutes          string
	NATRenewalMinutes        string
	NATTimeoutSeconds        string
	StunKeepaliveStartS      string
	// One per line
	AlwaysLocalNets            string
	OverwriteRemoteDeviceNames bool

	// The options as they were read, which tell what was edited
	saved *ConnectionOptions
}

func newConnectionOptions(opts config.OptionsConfiguration) *ConnectionOptions {
	options := &ConnectionOptions{
		ConnectionLimitMax:         strconv.Itoa(opts.ConnectionLimitMax),
		PriorityTCPLAN:             strconv.Itoa(opts.ConnectionPriorityTCPLAN),
		PriorityQUICLAN:            strconv.Itoa(opts.ConnectionPriorityQUICLAN),
		PriorityTCPWAN:             strconv.Itoa(opts.ConnectionPriorityTCPWAN),
		PriorityQUICWAN:            strconv.Itoa(opts.ConnectionPriorityQUICWAN),
		PriorityRelay:              strconv.Itoa(opts.ConnectionPriorityRelay),
		PriorityUpgradeThreshold:   strconv.Itoa(opts.ConnectionPriorityUpgradeThreshold),
		LocalAnnouncePort:          strconv.Itoa(opts.LocalAnnPort),
		LocalAnnounceMCAddr:        opts.LocalAnnMCAddr,
		ReconnectionIntervalS:      strconv.Itoa(opts.ReconnectIntervalS),
		RelayReconnectIntervalM:    strconv.Itoa(opts.RelayReconnectIntervalM),
		NATLeaseMinutes:            strconv.Itoa(opts.NATLeaseM),
		NATRenewalMinutes:          strconv.Itoa(opts.NATRenewalM),
		NATTimeoutSeconds:          strconv.Itoa(opts.NATTimeoutS),
		StunKeepaliveStartS:        strconv.Itoa(opts.StunKeepaliveStartS),
		AlwaysLocalNets:            strings.Join(opts.AlwaysLocalNets, "\n"),
		OverwriteRemoteDeviceNames: opts.OverwriteRemoteDevNames,
	}
	read := *options
	options.saved = &read
	return options
}

// Equal reports whether both hold the same options
func (o *ConnectionOptions) Equal(other *ConnectionOptions) bool {
	first, second := *o, *other
	first.saved, second.saved = nil, nil
	return first == second
}

// applyTo checks the options that were edited, and puts them in opts, which is a copy to throw away if this fails. What
// the engine would silently replace is refused, so that the page doesn't end up showing another value than was entered.
func (o *ConnectionOptions) applyTo(opts *config.OptionsConfiguration) error {
	saved := o.saved
	if err := errors.Join(
		setNumberOption("maximum connections", o.ConnectionLimitMax, saved.ConnectionLimitMax, &opts.ConnectionLimitMax,
			0, math.MaxInt32),
		setNumberOption("TCP on the local network", o.PriorityTCPLAN, saved.PriorityTCPLAN,
			&opts.ConnectionPriorityTCPLAN, math.MinInt32, math.MaxInt32),
		setNumberOption("QUIC on the local network", o.PriorityQUICLAN, saved.PriorityQUICLAN,
			&opts.ConnectionPriorityQUICLAN, math.MinInt32, math.MaxInt32),
		setNumberOption("TCP over the internet", o.PriorityTCPWAN, saved.PriorityTCPWAN,
			&opts.ConnectionPriorityTCPWAN, math.MinInt32, math.MaxInt32),
		setNumberOption("QUIC over the internet", o.PriorityQUICWAN, saved.PriorityQUICWAN,
			&opts.ConnectionPriorityQUICWAN, math.MinInt32, math.MaxInt32),
		setNumberOption("relays", o.PriorityRelay, saved.PriorityRelay, &opts.ConnectionPriorityRelay,
			math.MinInt32, math.MaxInt32),
		setNumberOption("upgrade threshold", o.PriorityUpgradeThreshold, saved.PriorityUpgradeThreshold,
			&opts.ConnectionPriorityUpgradeThreshold, 0, math.MaxInt32),
		setNumberOption("local announcement port", o.LocalAnnouncePort, saved.LocalAnnouncePort, &opts.LocalAnnPort,
			1, maxPort),
		setNumberOption("reconnection interval", o.ReconnectionIntervalS, saved.ReconnectionIntervalS,
			&opts.ReconnectIntervalS, minReconnectionIntervalS, maxIntervalSeconds),
		setNumberOption("relay reconnection interval", o.RelayReconnectIntervalM, saved.RelayReconnectIntervalM,
			&opts.RelayReconnectIntervalM, 0, maxIntervalMinutes),
		setNumberOption("port mapping lease", o.NATLeaseMinutes, saved.NATLeaseMinutes, &opts.NATLeaseM,
			0, maxIntervalMinutes),
		setNumberOption("port mapping renewal", o.NATRenewalMinutes, saved.NATRenewalMinutes, &opts.NATRenewalM,
			0, maxIntervalMinutes),
		setNumberOption("port mapping timeout", o.NATTimeoutSeconds, saved.NATTimeoutSeconds, &opts.NATTimeoutS,
			1, maxIntervalSeconds),
		setNumberOption("STUN keepalive", o.StunKeepaliveStartS, saved.StunKeepaliveStartS, &opts.StunKeepaliveStartS,
			1, maxIntervalSeconds),
	); err != nil {
		return err
	}
	if o.LocalAnnounceMCAddr != saved.LocalAnnounceMCAddr {
		address, err := parseMulticastAddress(o.LocalAnnounceMCAddr)
		if err != nil {
			return err
		}
		opts.LocalAnnMCAddr = address
	}
	if o.AlwaysLocalNets != saved.AlwaysLocalNets {
		networks, err := parseNetworks("always local networks", o.AlwaysLocalNets, false)
		if err != nil {
			return err
		}
		opts.AlwaysLocalNets = networks
	}
	setBoolOption(o.OverwriteRemoteDeviceNames, saved.OverwriteRemoteDeviceNames, &opts.OverwriteRemoteDevNames)
	// On what the configuration has by now, as only one side of a pair may have been edited
	if opts.ConnectionPriorityTCPWAN <= opts.ConnectionPriorityTCPLAN {
		return errPriorityTCP
	}
	if opts.ConnectionPriorityQUICWAN <= opts.ConnectionPriorityQUICLAN {
		return errPriorityQUIC
	}
	return nil
}

func parseMulticastAddress(text string) (string, error) {
	address, err := netip.ParseAddrPort(strings.TrimSpace(text))
	// IPv4 has broadcasts, so the engine only takes IPv6 here
	if err != nil || !address.Addr().Is6() || !address.Addr().IsMulticast() || address.Port() == 0 {
		return "", fmt.Errorf("local announcement address: %w", errMulticastAddress)
	}
	return address.String(), nil
}

// ConnectionOptions are the options the app has a page for; nil while the configuration is not loaded
func (clt *Client) ConnectionOptions() *ConnectionOptions {
	cfg := clt.cfg()
	if cfg == nil {
		return nil
	}
	return newConnectionOptions(cfg.Options())
}

// SetConnectionOptions saves the options that were edited. When one can't be used, nothing changes and the error says why.
func (clt *Client) SetConnectionOptions(options *ConnectionOptions) error {
	wrapper := clt.cfg()
	if wrapper == nil {
		return ErrStillLoading
	}
	if options.saved == nil {
		options.saved = newConnectionOptions(wrapper.Options())
	}
	var invalid error
	err := clt.changeConfiguration(func(cfg *config.Configuration) {
		opts := cfg.Options.Copy()
		if invalid = options.applyTo(&opts); invalid == nil {
			cfg.Options = opts
		}
	})
	if invalid != nil {
		return invalid
	}
	return err
}
