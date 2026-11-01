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
	"net"
	"strconv"
	"strings"

	"github.com/syncthing/syncthing/lib/config"
)

const (
	maxPort = 65535
	// The highest limit that still fits the engine's KiB/s in 32 bits
	maxRateMbits = 17_592_186
)

var (
	deviceCompressions = []string{"metadata", "always", "never"}

	errDeviceConfigNotFound = errors.New("device not found")
	errAutoAcceptUntrusted  = errors.New("folders can't be accepted automatically from an untrusted device")
)

// DeviceSettings are a device's settings that have no control of their own in the app, which it shows and saves together.
// Numbers are text, as they were entered, for SetSettings to check.
type DeviceSettings struct {
	Compression       string
	AutoAcceptFolders bool
	// In Mbit/s, as the app's own limits are, and 0 for no limit
	MaxSendMbits string
	MaxRecvMbits string
	// One per line, tried in order; ! in front of one refuses it
	AllowedNetworks          string
	NumConnections           string
	SkipIntroductionRemovals bool
	MaxRequestKiB            string
	CertName                 string
	RemoteGUIPort            string
	// Whether the device is untrusted, for what doesn't apply then. It has a control of its own.
	Untrusted bool

	// The settings as they were read, which tell what was edited
	saved *DeviceSettings
}

func newDeviceSettings(device config.DeviceConfiguration) *DeviceSettings {
	compression, _ := device.Compression.MarshalText()
	settings := &DeviceSettings{
		Compression:              string(compression),
		AutoAcceptFolders:        device.AutoAcceptFolders,
		MaxSendMbits:             strconv.Itoa(kibPerSecondToMbitsPerSecond(device.MaxSendKbps)),
		MaxRecvMbits:             strconv.Itoa(kibPerSecondToMbitsPerSecond(device.MaxRecvKbps)),
		AllowedNetworks:          strings.Join(device.AllowedNetworks, "\n"),
		NumConnections:           strconv.Itoa(device.RawNumConnections),
		SkipIntroductionRemovals: device.SkipIntroductionRemovals,
		MaxRequestKiB:            strconv.Itoa(device.MaxRequestKiB),
		CertName:                 device.CertName,
		RemoteGUIPort:            strconv.Itoa(device.RemoteGUIPort),
		Untrusted:                device.Untrusted,
	}
	read := *settings
	settings.saved = &read
	return settings
}

// Equal reports whether both hold the same settings
func (s *DeviceSettings) Equal(other *DeviceSettings) bool {
	first, second := *s, *other
	first.saved, second.saved = nil, nil
	return first == second
}

// applyTo checks the settings that were edited, and puts them in device, which is a copy to throw away if this fails
func (s *DeviceSettings) applyTo(device *config.DeviceConfiguration) error {
	saved := s.saved
	if err := errors.Join(
		setChoiceOption("compression", s.Compression, saved.Compression, deviceCompressions, &device.Compression),
		setRateOption("upload limit", s.MaxSendMbits, saved.MaxSendMbits, &device.MaxSendKbps),
		setRateOption("download limit", s.MaxRecvMbits, saved.MaxRecvMbits, &device.MaxRecvKbps),
		setNumberOption("connections", s.NumConnections, saved.NumConnections, &device.RawNumConnections, 0, math.MaxInt32),
		setNumberOption("request size", s.MaxRequestKiB, saved.MaxRequestKiB, &device.MaxRequestKiB, 0, math.MaxInt32),
		setNumberOption("web GUI port", s.RemoteGUIPort, saved.RemoteGUIPort, &device.RemoteGUIPort, 0, maxPort),
	); err != nil {
		return err
	}
	if s.AllowedNetworks != saved.AllowedNetworks {
		networks, err := parseNetworks("allowed networks", s.AllowedNetworks, true)
		if err != nil {
			return err
		}
		device.AllowedNetworks = networks
	}
	if s.CertName != saved.CertName {
		device.CertName = strings.TrimSpace(s.CertName)
	}
	setBoolOption(s.SkipIntroductionRemovals, saved.SkipIntroductionRemovals, &device.SkipIntroductionRemovals)
	if s.AutoAcceptFolders != saved.AutoAcceptFolders {
		// The engine would quietly turn this off again
		if s.AutoAcceptFolders && device.Untrusted {
			return errAutoAcceptUntrusted
		}
		device.AutoAcceptFolders = s.AutoAcceptFolders
	}
	return nil
}

// setRateOption puts an edited limit in Mbit/s in its option in KiB/s. A limit that wasn't edited stays as it is, also
// when it isn't a whole number of Mbit/s.
func setRateOption(label string, text string, saved string, option *int) error {
	if text == saved {
		return nil
	}
	mbits, err := parseNumberOption(label, text, 0, maxRateMbits)
	if err != nil {
		return err
	}
	*option = mbitsPerSecondToKiBPerSecond(mbits)
	return nil
}

// parseNetworks reads networks in CIDR notation, one per line. The engine skips what it can't read, so a mistake would
// leave a device refused without saying why.
func parseNetworks(label string, text string, allowNegation bool) ([]string, error) {
	networks := advancedListItems(text, false)
	for _, network := range networks {
		address := network
		if allowNegation {
			address = strings.TrimPrefix(address, "!")
		}
		if _, _, err := net.ParseCIDR(address); err != nil {
			return nil, fmt.Errorf("%s: %q is not a network such as 192.168.1.0/24", label, network)
		}
	}
	return networks, nil
}

// Settings are the device's settings the app has a page for; nil when the device is gone
func (peer *Peer) Settings() *DeviceSettings {
	wrapper := peer.client.cfg()
	if wrapper == nil {
		return nil
	}
	device, ok := wrapper.Device(peer.deviceID)
	if !ok {
		return nil
	}
	return newDeviceSettings(device)
}

// SetSettings saves the settings that were edited. When one can't be used, nothing changes and the error says why.
func (peer *Peer) SetSettings(settings *DeviceSettings) error {
	if settings.saved == nil {
		current := peer.Settings()
		if current == nil {
			return errDeviceConfigNotFound
		}
		settings.saved = current
	}
	return peer.changeDeviceCopy(func(device *config.DeviceConfiguration) error {
		return settings.applyTo(device)
	})
}
