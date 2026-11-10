// Copyright (C) 2026 Gordon
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this file,
// You can obtain one at https://mozilla.org/MPL/2.0/.

package remoteclient

import (
	"errors"
	"net"
	"strconv"
	"strings"
)

// Address is where a gateway is reached, as people write it: a host name or IP address, with or without a port
// (studio.example.org, studio.example.org:20920, 192.168.1.2:20920, [fd00::1]:20920 or fd00::1). It follows the
// Swift RemoteAddress, so that every client takes the same addresses.
type Address struct {
	// The host, an IPv6 address without its brackets
	Host string
	// The port, or 0 when none was written
	Port int
}

// ErrNotAnAddress refuses text that isn't a host with an optional port
var ErrNotAnAddress = errors.New("enter a host name or IP address, optionally followed by a colon and a port")

// ParseAddress reads an address someone typed or pasted (a https://host[:port] link too, whose port is 443 when it has
// none). It refuses an empty one, and anything but a host with an optional port: a path, a space, a user name, another
// scheme, a name that isn't ASCII (enter its xn-- form), an IPv6 zone or a port out of range.
func ParseAddress(text string) (Address, error) {
	rest := strings.TrimSpace(text)
	isLink := false
	if i := strings.Index(rest, "://"); i >= 0 {
		if !strings.EqualFold(rest[:i], "https") {
			return Address{}, ErrNotAnAddress
		}
		isLink = true
		rest = strings.TrimSuffix(rest[i+len("://"):], "/")
	}
	var host, portText string
	hasPort := false
	switch {
	case strings.HasPrefix(rest, "["):
		end := strings.IndexByte(rest, ']')
		if end < 0 {
			return Address{}, ErrNotAnAddress
		}
		host = rest[1:end]
		if after := rest[end+1:]; after != "" {
			if !strings.HasPrefix(after, ":") {
				return Address{}, ErrNotAnAddress
			}
			portText, hasPort = after[1:], true
		}
		if !isIPv6(host) {
			return Address{}, ErrNotAnAddress
		}
	case strings.Count(rest, ":") > 1:
		// An IPv6 address without brackets has no port
		host = rest
	case strings.Contains(rest, ":"):
		i := strings.IndexByte(rest, ':')
		host, portText, hasPort = rest[:i], rest[i+1:], true
	default:
		host = rest
	}
	if !isHostName(host) && !isIPv6(host) {
		return Address{}, ErrNotAnAddress
	}
	a := Address{Host: host}
	if hasPort {
		if portText == "" || strings.Trim(portText, "0123456789") != "" {
			return Address{}, ErrNotAnAddress
		}
		port, err := strconv.Atoi(portText)
		if err != nil || port < 1 || port > 65535 {
			return Address{}, ErrNotAnAddress
		}
		a.Port = port
	} else if isLink {
		a.Port = 443
	}
	return a, nil
}

// FormatAddress gives a host and port as pairing codes and settings show them (host:port, [fd00::1]:port)
func FormatAddress(host string, port int) string {
	return net.JoinHostPort(host, strconv.Itoa(port))
}

// isHostName reports whether host is a DNS name or IPv4 address in ASCII: labels of letters, digits, hyphens and
// underscores, none empty (but for a final dot) and none starting or ending with a hyphen. Other names would be
// converted when connecting, and then no longer match what is stored.
func isHostName(host string) bool {
	name := strings.TrimSuffix(host, ".")
	if name == "" {
		return false
	}
	for _, label := range strings.Split(name, ".") {
		if label == "" || strings.HasPrefix(label, "-") || strings.HasSuffix(label, "-") {
			return false
		}
		for _, c := range label {
			if !('a' <= c && c <= 'z' || 'A' <= c && c <= 'Z' || '0' <= c && c <= '9' || c == '-' || c == '_') {
				return false
			}
		}
	}
	return true
}

// isIPv6 reports whether host is an IPv6 address without a zone (%en0), which URLs can't carry as typed
func isIPv6(host string) bool {
	return strings.Contains(host, ":") && !strings.Contains(host, "%") && net.ParseIP(host) != nil
}
