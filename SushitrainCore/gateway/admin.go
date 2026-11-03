// Copyright (C) 2026 Gordon
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this file,
// You can obtain one at https://mozilla.org/MPL/2.0/.

package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"t-shaped.nl/sushitrain/v2/adminsock"
)

// The admin socket is how the command-line interface (and other local tools) manage a running service: one engine
// owns Syncthing's database, so management goes through the process that runs it rather than opening the database
// a second time. Access is controlled by the socket's file permissions. The socket itself is in adminsock.

type (
	AdminRequest  = adminsock.Request
	AdminResponse = adminsock.Response
	AdminMethod   = adminsock.Method
	AdminServer   = adminsock.Server
)

func NewAdminServer() *AdminServer {
	return adminsock.NewServer()
}

// AdminCall sends one request to an admin socket and decodes the result into result (if not nil)
func AdminCall(path, method string, params any, result any) error {
	return adminsock.Call(path, method, params, result)
}

// Status summarizes the service for the host's status displays
type Status struct {
	ServiceID           string         `json:"serviceID"`
	ServiceName         string         `json:"serviceName"`
	DeviceID            string         `json:"deviceID"`
	Address             string         `json:"address"`
	SPKI                string         `json:"spki,omitempty"`
	AuthorityGeneration int64          `json:"authorityGeneration"`
	Exports             []ExportStatus `json:"exports"`
	Clients             int            `json:"clients"`
	PendingOperations   int            `json:"pendingOperations"`
	PendingPairings     int            `json:"pendingPairings"`
	ContentCacheBytes   int64          `json:"contentCacheBytes"`
	ContentCacheQuota   int64          `json:"contentCacheQuota"`
}

type ExportStatus struct {
	Export
	Label string `json:"label"`
	State string `json:"state"`
	Items int64  `json:"items"`
}

func (s *Service) Status() (Status, error) {
	st := Status{ServiceName: s.cfg.ServiceName, DeviceID: s.backend.DeviceID().String(), SPKI: s.SPKI(),
		ContentCacheQuota: s.cfg.SpoolQuota}
	var err error
	if st.ServiceID, err = s.store.serviceID(); err != nil {
		return st, err
	}
	if st.AuthorityGeneration, err = s.store.generation(); err != nil {
		return st, err
	}
	if addr := s.Addr(); addr != nil {
		st.Address = addr.String()
	}
	exports, err := s.Exports()
	if err != nil {
		return st, err
	}
	for _, e := range exports {
		es := ExportStatus{Export: e, State: s.backend.FolderState(e.FolderID)}
		if info, ok := s.backend.Folder(e.FolderID); ok {
			es.Label = info.Label
		}
		_ = s.store.db.QueryRow(`SELECT COUNT(*) FROM items WHERE folder_id = ? AND deleted = 0`, e.FolderID).Scan(&es.Items)
		st.Exports = append(st.Exports, es)
	}
	_ = s.store.db.QueryRow(`SELECT COUNT(*) FROM clients WHERE revoked_at IS NULL`).Scan(&st.Clients)
	_ = s.store.db.QueryRow(`SELECT COUNT(*) FROM operations WHERE state = 'pending'`).Scan(&st.PendingOperations)
	pending, _ := s.PendingPairings()
	st.PendingPairings = len(pending)
	s.content.mutex.Lock()
	st.ContentCacheBytes = s.content.reserved
	s.content.mutex.Unlock()
	return st, nil
}

// PendingOperation describes an operation that has not finished, for the host's display. File names are left out.
type PendingOperation struct {
	Operation string    `json:"operation"`
	ClientID  string    `json:"clientID"`
	Kind      string    `json:"kind"`
	FolderID  string    `json:"folderID"`
	Journal   string    `json:"journal"`
	Created   time.Time `json:"created"`
}

func (s *Service) PendingOperations() ([]PendingOperation, error) {
	rows, err := s.store.db.Query(`SELECT op_id, client_id, kind, folder_id, journal, created_at FROM operations
		WHERE state = 'pending' ORDER BY created_at`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var result []PendingOperation
	for rows.Next() {
		var p PendingOperation
		var created int64
		if err := rows.Scan(&p.Operation, &p.ClientID, &p.Kind, &p.FolderID, &p.Journal, &created); err != nil {
			return nil, err
		}
		p.Created = time.Unix(created, 0)
		result = append(result, p)
	}
	return result, rows.Err()
}

// RegisterAdmin adds the service's management methods to an admin server. pairHost and pairPort are where clients
// reach the service, for pairing codes.
func (s *Service) RegisterAdmin(a *AdminServer, pairHost string, pairPort int) {
	decode := func(params json.RawMessage, v any) error {
		if len(params) == 0 {
			return errors.New("missing parameters")
		}
		return json.Unmarshal(params, v)
	}
	a.Register("status", func(context.Context, json.RawMessage) (any, error) { return s.Status() })
	a.Register("exports.list", func(context.Context, json.RawMessage) (any, error) { return s.Exports() })
	a.Register("exports.set", func(_ context.Context, params json.RawMessage) (any, error) {
		var e Export
		if err := decode(params, &e); err != nil {
			return nil, err
		}
		return nil, s.SetExport(e.FolderID, e.AllowWrite)
	})
	a.Register("exports.remove", func(_ context.Context, params json.RawMessage) (any, error) {
		var e Export
		if err := decode(params, &e); err != nil {
			return nil, err
		}
		return nil, s.RemoveExport(e.FolderID)
	})
	a.Register("clients.list", func(context.Context, json.RawMessage) (any, error) { return s.Clients() })
	a.Register("clients.revoke", func(_ context.Context, params json.RawMessage) (any, error) {
		var p struct {
			ClientID string `json:"clientID"`
		}
		if err := decode(params, &p); err != nil {
			return nil, err
		}
		return nil, s.RevokeClient(p.ClientID)
	})
	a.Register("clients.permission", func(_ context.Context, params json.RawMessage) (any, error) {
		var p struct {
			ClientID string `json:"clientID"`
			FolderID string `json:"folderID"`
			Access   string `json:"access"`
		}
		if err := decode(params, &p); err != nil {
			return nil, err
		}
		return nil, s.SetPermission(p.ClientID, p.FolderID, p.Access)
	})
	a.Register("pairing.start", func(context.Context, json.RawMessage) (any, error) {
		spki := ""
		if s.UsesSelfSignedCertificate() {
			spki = s.SPKI()
		}
		return s.StartPairing(pairHost, pairPort, spki)
	})
	a.Register("pairing.pending", func(context.Context, json.RawMessage) (any, error) { return s.PendingPairings() })
	a.Register("pairing.wait", func(ctx context.Context, params json.RawMessage) (any, error) {
		// Waits up to a minute for a request of the given session
		var p struct {
			SessionID string `json:"sessionID"`
		}
		_ = json.Unmarshal(params, &p)
		deadline := time.Now().Add(time.Minute)
		for {
			changed := s.pairingChanged.wait("")
			pending, err := s.PendingPairings()
			if err != nil {
				return nil, err
			}
			for _, req := range pending {
				if p.SessionID == "" || req.SessionID == p.SessionID {
					return req, nil
				}
			}
			select {
			case <-changed:
			case <-time.After(time.Until(deadline)):
				return nil, errors.New("no pairing request yet")
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		}
	})
	a.Register("pairing.approve", func(_ context.Context, params json.RawMessage) (any, error) {
		var p struct {
			RequestID string            `json:"requestID"`
			Grants    map[string]string `json:"grants"`
		}
		if err := decode(params, &p); err != nil {
			return nil, err
		}
		return nil, s.ApprovePairing(p.RequestID, p.Grants)
	})
	a.Register("pairing.deny", func(_ context.Context, params json.RawMessage) (any, error) {
		var p struct {
			RequestID string `json:"requestID"`
		}
		if err := decode(params, &p); err != nil {
			return nil, err
		}
		return nil, s.DenyPairing(p.RequestID)
	})
	a.Register("operations.pending", func(context.Context, json.RawMessage) (any, error) { return s.PendingOperations() })
}
