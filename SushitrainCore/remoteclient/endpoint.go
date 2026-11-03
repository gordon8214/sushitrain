// Copyright (C) 2026 Gordon
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this file,
// You can obtain one at https://mozilla.org/MPL/2.0/.

// Package remoteclient is a client of version 1 of the remote file service's API (Docs/remote-api), the Go
// counterpart of the Swift RemoteFileClient in SynctrainFileProviderKit. Credentials go in the Authorization header
// only; a gateway's certificate is trusted normally or by the public key its pairing code pinned, never otherwise;
// redirects are never followed.
package remoteclient

import (
	"errors"
	"net"
	"net/url"
	"strconv"
	"strings"
)

// Endpoint is where a gateway is and how its certificate is trusted
type Endpoint struct {
	ServiceID string `json:"serviceID"`
	Host      string `json:"host"`
	Port      int    `json:"port"`
	// Public key hashes (remoteapi.SPKIHash) of which the gateway's certificate must have one: the pin from the pairing
	// code, and the next one the gateway announced. Without any, the certificate must be trusted by the system for Host.
	Pins []string `json:"pins,omitempty"`
}

// BaseURL is the gateway's origin, such as https://nas.local:20920
func (e Endpoint) BaseURL() string {
	return "https://" + net.JoinHostPort(e.Host, strconv.Itoa(e.Port))
}

// Invitation is what a pairing code carries:
// synctrain+pair://<host>:<port>/?service=<service ID>&secret=<secret>[&spki=<hash>]
type Invitation struct {
	Host      string
	Port      int
	ServiceID string
	Secret    string
	// The pinned public key hash, for a gateway with a self-signed certificate
	SPKI string
}

// InvitationScheme is the scheme of pairing links
const InvitationScheme = "synctrain+pair"

var errNotAnInvitation = errors.New("not a Synctrain pairing link")

// ParseInvitation reads a pairing link, as the Swift PairingInvitation does
func ParseInvitation(link string) (Invitation, error) {
	u, err := url.Parse(strings.TrimSpace(link))
	if err != nil || !strings.EqualFold(u.Scheme, InvitationScheme) {
		return Invitation{}, errNotAnInvitation
	}
	host := u.Hostname()
	port, err := strconv.Atoi(u.Port())
	if host == "" || err != nil || port < 1 || port > 65535 {
		return Invitation{}, errors.New("the pairing link has no gateway address")
	}
	query := u.Query()
	inv := Invitation{Host: host, Port: port, ServiceID: query.Get("service"), Secret: query.Get("secret"),
		SPKI: query.Get("spki")}
	if inv.ServiceID == "" || inv.Secret == "" {
		return Invitation{}, errors.New("the pairing link has no service or secret")
	}
	return inv, nil
}

// Endpoint is the gateway the invitation is from
func (i Invitation) Endpoint() Endpoint {
	e := Endpoint{ServiceID: i.ServiceID, Host: i.Host, Port: i.Port}
	if i.SPKI != "" {
		e.Pins = []string{i.SPKI}
	}
	return e
}
