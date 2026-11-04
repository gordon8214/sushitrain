// Copyright (C) 2026 Gordon
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this file,
// You can obtain one at https://mozilla.org/MPL/2.0/.

package files

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/godbus/dbus/v5"
)

// SecretService keeps credentials in the desktop's keyring (GNOME Keyring, KWallet) through the Secret Service API,
// as items of the default collection labeled after the gateway. A locked keyring is an error worth trying again
// later: the daemon never prompts.
type SecretService struct {
	// Application tells the keyring's items apart from those of other programs, and from tests
	Application string

	mutex   sync.Mutex
	conn    *dbus.Conn
	session dbus.ObjectPath
}

const (
	secretsName       = "org.freedesktop.secrets"
	secretsPath       = dbus.ObjectPath("/org/freedesktop/secrets")
	secretsService    = "org.freedesktop.Secret.Service"
	secretsCollection = "org.freedesktop.Secret.Collection"
	secretsItem       = "org.freedesktop.Secret.Item"
	defaultCollection = dbus.ObjectPath("/org/freedesktop/secrets/aliases/default")
	noPrompt          = dbus.ObjectPath("/")
)

// errKeyringLocked means the keyring is locked (before the first unlock after a login without a password, say)
var errKeyringLocked = errors.New("the keyring is locked")

type secret struct {
	Session     dbus.ObjectPath
	Parameters  []byte
	Value       []byte
	ContentType string
}

// NewSecretService connects to the session's Secret Service, or fails if there is none
func NewSecretService(application string) (*SecretService, error) {
	s := &SecretService{Application: application}
	if err := s.connect(); err != nil {
		return nil, err
	}
	return s, nil
}

// connect opens a connection and a session. Callers hold the mutex, but for a new one.
func (s *SecretService) connect() error {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return s.connectContext(ctx)
}

func (s *SecretService) connectContext(ctx context.Context) error {
	// Dial first so cancellation can close the transport during authentication
	// and Hello as well as OpenSession. The successful session outlives ctx.
	conn, err := dbus.SessionBusPrivateNoAutoStartup()
	if err != nil {
		return err
	}
	stopHandshake := context.AfterFunc(ctx, func() { conn.Close() })
	defer stopHandshake()
	if err = conn.Auth(nil); err == nil {
		err = conn.Hello()
	}
	if err != nil {
		conn.Close()
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return err
	}
	var output dbus.Variant
	var session dbus.ObjectPath
	if err := conn.Object(secretsName, secretsPath).CallWithContext(ctx, secretsService+".OpenSession", 0, "plain",
		dbus.MakeVariant("")).Store(&output, &session); err != nil {
		conn.Close()
		return fmt.Errorf("opening a Secret Service session: %w", err)
	}
	if !stopHandshake() || ctx.Err() != nil {
		conn.Close()
		return ctx.Err()
	}
	if s.conn != nil {
		s.conn.Close()
	}
	s.conn, s.session = conn, session
	return nil
}

// A disappeared or unresponsive keyring must not hold daemon startup, shutdown,
// or another credential operation indefinitely. Never prompt to unlock it.
func (s *SecretService) call(path dbus.ObjectPath, method string, args ...any) *dbus.Call {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return s.conn.Object(secretsName, path).CallWithContext(ctx, method, 0, args...)
}

// retry runs fn, and once more on a new session if the keyring lost the one it had (gnome-keyring restarted).
// Callers hold the mutex.
func (s *SecretService) retry(fn func() error) error {
	err := fn()
	var e dbus.Error
	lost := errors.Is(err, dbus.ErrClosed) || (errors.As(err, &e) &&
		(e.Name == "org.freedesktop.Secret.Error.NoSession" || e.Name == "org.freedesktop.DBus.Error.UnknownObject" ||
			e.Name == "org.freedesktop.DBus.Error.ServiceUnknown" || e.Name == "org.freedesktop.DBus.Error.NoReply"))
	if !lost {
		return err
	}
	if err := s.connect(); err != nil {
		return err
	}
	return fn()
}

func (s *SecretService) Close() error {
	return s.conn.Close()
}

func (s *SecretService) attributes(serviceID string) map[string]string {
	return map[string]string{"application": s.Application, "service": serviceID}
}

// find returns the keyring's item for a gateway, if there is one
func (s *SecretService) find(serviceID string) (dbus.ObjectPath, error) {
	var unlocked, locked []dbus.ObjectPath
	if err := s.call(secretsPath, secretsService+".SearchItems",
		s.attributes(serviceID)).Store(&unlocked, &locked); err != nil {
		return "", err
	}
	switch {
	case len(unlocked) > 0:
		return unlocked[0], nil
	case len(locked) > 0:
		return "", errKeyringLocked
	}
	return "", nil
}

func (s *SecretService) Credential(serviceID string) (string, error) {
	s.mutex.Lock()
	defer s.mutex.Unlock()
	var credential string
	err := s.retry(func() error {
		item, err := s.find(serviceID)
		if err != nil || item == "" {
			return err
		}
		var value secret
		if err := s.call(item, secretsItem+".GetSecret", s.session).Store(&value); err != nil {
			return err
		}
		credential = string(value.Value)
		return nil
	})
	return credential, err
}

func (s *SecretService) SetCredential(serviceID, credential string) error {
	s.mutex.Lock()
	defer s.mutex.Unlock()
	properties := map[string]dbus.Variant{
		"org.freedesktop.Secret.Item.Label":      dbus.MakeVariant("Synctrain: gateway " + serviceID),
		"org.freedesktop.Secret.Item.Attributes": dbus.MakeVariant(s.attributes(serviceID)),
	}
	return s.retry(func() error {
		value := secret{Session: s.session, Value: []byte(credential), ContentType: "text/plain"}
		var item, prompt dbus.ObjectPath
		if err := s.call(defaultCollection, secretsCollection+".CreateItem", properties,
			value, true).Store(&item, &prompt); err != nil {
			return err
		}
		if prompt != noPrompt {
			return errKeyringLocked
		}
		return nil
	})
}

func (s *SecretService) DeleteCredential(serviceID string) error {
	s.mutex.Lock()
	defer s.mutex.Unlock()
	return s.retry(func() error {
		item, err := s.find(serviceID)
		if err != nil || item == "" {
			return err
		}
		var prompt dbus.ObjectPath
		if err := s.call(item, secretsItem+".Delete").Store(&prompt); err != nil {
			return err
		}
		if prompt != noPrompt {
			return errKeyringLocked
		}
		return nil
	})
}

// MoveCredentials moves credentials from one store to another (from the file kept without a keyring, once there is
// one), keeping those it can't move
func MoveCredentials(from *FileCredentials, to CredentialStore) (int, error) {
	from.mutex.Lock()
	credentials, err := from.load()
	from.mutex.Unlock()
	if err != nil || len(credentials) == 0 {
		return 0, err
	}
	moved := 0
	for serviceID, credential := range credentials {
		if err := to.SetCredential(serviceID, credential); err != nil {
			return moved, err
		}
		if err := from.DeleteCredential(serviceID); err != nil {
			return moved, err
		}
		moved++
	}
	return moved, nil
}
