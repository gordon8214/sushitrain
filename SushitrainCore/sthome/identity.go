// Copyright (C) 2026 Gordon
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this file,
// You can obtain one at https://mozilla.org/MPL/2.0/.

package sthome

import (
	"crypto/tls"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"

	"github.com/syncthing/syncthing/lib/syncthing"
)

// ErrIdentity means the device certificate or key is missing while the other exists, or can't be parsed. The device
// identity can't be used, and only resetting it (a new device ID) gets past this.
var ErrIdentity = errors.New("The device identity is damaged or incomplete")

// LoadIdentity loads the device certificate, or creates a new device identity when neither the certificate nor the key
// exists. A certificate or key that exists but can't be loaded is an error, and the files are left alone: a new
// identity would make this a different device to all others. The error wraps ErrIdentity when the identity itself is
// unusable, rather than unreadable at the moment.
func LoadIdentity(certFile, keyFile string) (cert tls.Certificate, created bool, err error) {
	certMissing, err := isMissing(certFile)
	if err != nil {
		return tls.Certificate{}, false, err
	}
	keyMissing, err := isMissing(keyFile)
	if err != nil {
		return tls.Certificate{}, false, err
	}
	switch {
	case certMissing && keyMissing:
		cert, err = generateIdentity(certFile, keyFile)
		return cert, err == nil, err
	case certMissing:
		return tls.Certificate{}, false, fmt.Errorf("%w: %s is missing", ErrIdentity, filepath.Base(certFile))
	case keyMissing:
		return tls.Certificate{}, false, fmt.Errorf("%w: %s is missing", ErrIdentity, filepath.Base(keyFile))
	}

	cert, err = tls.LoadX509KeyPair(certFile, keyFile)
	var pathErr *fs.PathError
	if errors.As(err, &pathErr) {
		return tls.Certificate{}, false, fmt.Errorf("The device identity can't be read: %w", err)
	} else if err != nil {
		return tls.Certificate{}, false, fmt.Errorf("%w: %w", ErrIdentity, err)
	}
	return cert, false, nil
}

// generateIdentity writes a new certificate and key under temporary names, then moves the key into place before the
// certificate, so that a certificate is never left without its key.
func generateIdentity(certFile, keyFile string) (tls.Certificate, error) {
	newCert, newKey := certFile+".new", keyFile+".new"
	cert, err := syncthing.GenerateCertificate(newCert, newKey)
	if err == nil {
		err = os.Rename(newKey, keyFile)
	}
	if err == nil {
		err = os.Rename(newCert, certFile)
	}
	if err != nil {
		_ = os.Remove(newCert)
		_ = os.Remove(newKey)
		return tls.Certificate{}, fmt.Errorf("creating a device identity: %w", err)
	}
	return cert, nil
}

// isMissing reports whether nothing exists at path. A symbolic link counts as existing even if its target doesn't, so
// that a broken link is an error rather than something to replace.
func isMissing(path string) (bool, error) {
	_, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return true, nil
	}
	return false, err
}
