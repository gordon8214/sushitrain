// Copyright (C) 2026 Gordon
// SPDX-License-Identifier: MPL-2.0
// Package history manages read-only history pairings independently of mounted folders.
package history

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"t-shaped.nl/sushitrain/v2/adminsock"
	"t-shaped.nl/sushitrain/v2/files"
	"t-shaped.nl/sushitrain/v2/previewcache"
	"t-shaped.nl/sushitrain/v2/remoteapi"
	"t-shaped.nl/sushitrain/v2/remoteclient"
	"t-shaped.nl/sushitrain/v2/safecopy"
)

type Source struct {
	Endpoint       remoteclient.Endpoint
	Name, ClientID string
	Enabled        bool
}
type Request struct {
	Link, ID, Folder, Path, Version, ExpectedTag, Destination string
	Enabled                                                   bool
}
type Listing struct {
	Source          Source
	DeviceID, Error string
	Versions        []remoteapi.ItemVersion
}
type approval struct {
	Source     Source
	Credential string
}
type Store struct {
	mu, cacheMu   sync.Mutex
	path, cache   string
	forbidden     []string
	credentials   files.CredentialStore
	close         func() error
	ctx           context.Context
	cancel        context.CancelFunc
	operations    sync.WaitGroup
	closed        bool
	generation    map[string]uint64
	pairing       map[string]bool
	cancellations map[string]context.CancelFunc
}

func Open(state, cache string, forbidden ...string) (*Store, error) {
	if err := os.MkdirAll(state, 0700); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(cache, 0700); err != nil {
		return nil, err
	}
	credentials, close, err := credentialStore(state)
	if err != nil {
		return nil, err
	}
	return openWithCredentials(state, cache, credentials, close, forbidden...)
}
func openWithCredentials(state, cache string, credentials files.CredentialStore, close func() error, forbidden ...string) (*Store, error) {
	for _, dir := range []string{state, cache} {
		if err := os.MkdirAll(dir, 0700); err != nil {
			close()
			return nil, err
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	s := &Store{path: filepath.Join(state, "sources.json"), cache: cache, forbidden: forbidden, credentials: credentials, close: close, ctx: ctx, cancel: cancel, generation: map[string]uint64{}, pairing: map[string]bool{}, cancellations: map[string]context.CancelFunc{}}
	if err := s.recoverApprovals(); err != nil {
		slog.Warn("History pairing recovery is waiting for storage or the keyring", "error", err)
	}
	return s, nil
}
func (s *Store) begin(parent context.Context) (context.Context, func(), error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil, nil, errors.New("history sources are shutting down")
	}
	s.operations.Add(1)
	ctx, cancel := context.WithCancel(parent)
	stop := context.AfterFunc(s.ctx, cancel)
	return ctx, func() { stop(); cancel(); s.operations.Done() }, nil
}
func (s *Store) Close() {
	s.mu.Lock()
	s.closed = true
	s.mu.Unlock()
	s.cancel()
	s.operations.Wait()
	s.close()
}
func (s *Store) load() ([]Source, error) {
	var list []Source
	b, err := os.ReadFile(s.path)
	if errors.Is(err, os.ErrNotExist) {
		return []Source{}, nil
	}
	if err != nil {
		return nil, err
	}
	err = json.Unmarshal(b, &list)
	return list, err
}
func atomicJSON(path string, value any) error {
	b, err := json.Marshal(value)
	if err != nil {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(path), ".history-*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if err = f.Chmod(0600); err == nil {
		_, err = f.Write(b)
	}
	err = errors.Join(err, f.Sync(), f.Close())
	if err != nil {
		return err
	}
	if err = os.Rename(f.Name(), path); err != nil {
		return err
	}
	dir, err := os.Open(filepath.Dir(path))
	if err != nil {
		return err
	}
	defer dir.Close()
	return dir.Sync()
}
func (s *Store) save(list []Source) error { return atomicJSON(s.path, list) }
func replaceSource(list []Source, source Source) []Source {
	out := []Source{}
	for _, old := range list {
		if old.Endpoint.ServiceID != source.Endpoint.ServiceID {
			out = append(out, old)
		}
	}
	return append(out, source)
}
func (s *Store) approvals() ([]approval, error) {
	var pending []approval
	b, err := os.ReadFile(filepath.Join(filepath.Dir(s.path), "approved-pairings.json"))
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	err = json.Unmarshal(b, &pending)
	return pending, err
}
func (s *Store) saveApprovals(pending []approval) error {
	return atomicJSON(filepath.Join(filepath.Dir(s.path), "approved-pairings.json"), pending)
}
func (s *Store) recoverApprovals() error { s.mu.Lock(); defer s.mu.Unlock(); return s.recoverLocked() }
func (s *Store) recoverLocked() error {
	pending, err := s.approvals()
	if err != nil {
		return err
	}
	for len(pending) > 0 {
		a := pending[0]
		if err = s.credentials.SetCredential(a.Source.Endpoint.ServiceID, a.Credential); err != nil {
			return err
		}
		list, err := s.load()
		if err != nil {
			return err
		}
		if err = s.save(replaceSource(list, a.Source)); err != nil {
			return err
		}
		pending = pending[1:]
		if err = s.saveApprovals(pending); err != nil {
			return err
		}
	}
	return nil
}
func (s *Store) Sources() ([]Source, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil, errors.New("history sources are shutting down")
	}
	if s.ctx != nil {
		if err := s.recoverLocked(); err != nil {
			return nil, fmt.Errorf("recovering an approved pairing: %w", err)
		}
	}
	return s.load()
}
func (s *Store) source(id string) (Source, error) {
	list, err := s.Sources()
	if err != nil {
		return Source{}, err
	}
	for _, source := range list {
		if source.Endpoint.ServiceID == id && source.Enabled {
			return source, nil
		}
	}
	return Source{}, errors.New("this history source was disabled or removed")
}
func (s *Store) Pair(parent context.Context, link string) (Source, error) {
	ctx, done, err := s.begin(parent)
	if err != nil {
		return Source{}, err
	}
	defer done()
	invitation, err := remoteclient.ParseInvitation(link)
	if err != nil {
		return Source{}, err
	}
	s.mu.Lock()
	if s.pairing[invitation.ServiceID] {
		s.mu.Unlock()
		return Source{}, errors.New("a pairing with this gateway is already waiting for approval")
	}
	s.pairing[invitation.ServiceID] = true
	generation := s.generation[invitation.ServiceID]
	list, err := s.load()
	s.mu.Unlock()
	defer func() { s.mu.Lock(); delete(s.pairing, invitation.ServiceID); s.mu.Unlock() }()
	if err != nil {
		return Source{}, err
	}
	id := uuid.NewString()
	for _, source := range list {
		if source.Endpoint.ServiceID == invitation.ServiceID {
			id = source.ClientID
		}
	}
	host, _ := os.Hostname()
	credential, err := remoteclient.Pair(ctx, invitation, remoteclient.Pairing{ClientID: id, ClientName: host + " (history)", Platform: "linux"})
	if err != nil {
		return Source{}, err
	}
	source := Source{Endpoint: invitation.Endpoint(), Name: invitation.Host, ClientID: id, Enabled: true}
	// Approval carries a one-time credential. Journal it before any further network
	// request, and drain this operation before a replacement store may be opened.
	s.mu.Lock()
	if generation != s.generation[invitation.ServiceID] {
		s.mu.Unlock()
		return Source{}, errors.New("the source was disabled or forgotten while pairing")
	}
	pending, err := s.approvals()
	if err == nil {
		pending = append(pending, approval{source, credential})
		err = s.saveApprovals(pending)
	}
	if err == nil {
		err = s.recoverLocked()
	}
	s.mu.Unlock()
	if err != nil {
		return source, fmt.Errorf("pairing was approved; its saved receipt will be recovered when storage is available: %w", err)
	}
	client := remoteclient.New(source.Endpoint, func() (string, error) { return credential, nil })
	defer client.Close()
	metadataCtx, metadataCancel := context.WithTimeout(ctx, 3*time.Second)
	defer metadataCancel()
	caps, err := client.Capabilities(metadataCtx)
	if err == nil && caps.ServiceID == source.Endpoint.ServiceID && caps.ServiceName != "" {
		s.mu.Lock()
		if generation == s.generation[invitation.ServiceID] && !s.closed {
			list, e := s.load()
			if e == nil {
				source.Name = caps.ServiceName
				_ = s.save(replaceSource(list, source))
			}
		}
		s.mu.Unlock()
	}
	return source, nil
}
func (s *Store) List(parent context.Context, folder, path string) ([]Listing, error) {
	ctx, done, err := s.begin(parent)
	if err != nil {
		return nil, err
	}
	defer done()
	sources, err := s.Sources()
	if err != nil {
		return nil, err
	}
	results := make([]Listing, len(sources))
	var workers sync.WaitGroup
	slots := make(chan struct{}, 4)
	for i, source := range sources {
		if !source.Enabled {
			continue
		}
		workers.Add(1)
		go func(i int, source Source) {
			defer workers.Done()
			result := Listing{Source: source, Versions: []remoteapi.ItemVersion{}}
			select {
			case slots <- struct{}{}:
				defer func() { <-slots }()
			case <-ctx.Done():
				result.Error = ctx.Err().Error()
				results[i] = result
				return
			}
			client := remoteclient.New(source.Endpoint, func() (string, error) { return s.credentials.Credential(source.Endpoint.ServiceID) })
			defer client.Close()
			caps, e := client.Capabilities(ctx)
			if e == nil && caps.ServiceID != source.Endpoint.ServiceID {
				e = errors.New("the gateway changed identity")
			}
			if e == nil {
				result.DeviceID = caps.SyncthingDeviceID
				var versions remoteapi.PathVersionList
				versions, e = client.PathVersions(ctx, folder, path)
				result.Versions = versions.Versions
			}
			if e != nil {
				result.Error = e.Error()
			}
			current, e := s.source(source.Endpoint.ServiceID)
			if e != nil || !reflect.DeepEqual(current, source) {
				result.Error = "The history source changed. Refresh history."
				result.Versions = nil
			}
			results[i] = result
		}(i, source)
	}
	workers.Wait()
	out := []Listing{}
	seen := map[string]bool{}
	for _, result := range results {
		if !result.Source.Enabled {
			continue
		}
		if result.Error == "" && result.DeviceID != "" {
			if seen[result.DeviceID] {
				result.Versions = nil
				result.Error = "Another source represents this same Syncthing device."
			}
			seen[result.DeviceID] = true
		}
		out = append(out, result)
	}
	return out, nil
}
func (s *Store) openVersion(ctx context.Context, p Request) (Source, *remoteclient.Client, remoteapi.ItemVersion, io.ReadCloser, error) {
	var zero remoteapi.ItemVersion
	if len(p.ExpectedTag) < 2 || !strings.HasPrefix(p.ExpectedTag, "\"") || !strings.HasSuffix(p.ExpectedTag, "\"") {
		return Source{}, nil, zero, nil, errors.New("refresh history before downloading a selected version")
	}
	source, err := s.source(p.ID)
	if err != nil {
		return source, nil, zero, nil, err
	}
	client := remoteclient.New(source.Endpoint, func() (string, error) { return s.credentials.Credential(p.ID) })
	list, err := client.PathVersions(ctx, p.Folder, p.Path)
	if err != nil {
		client.Close()
		return source, nil, zero, nil, err
	}
	for _, v := range list.Versions {
		if v.ID == p.Version {
			if v.EntityTag != p.ExpectedTag {
				client.Close()
				return source, nil, zero, nil, errors.New("the archived version changed; refresh history")
			}
			reader, err := client.HistoryContent(ctx, p.Folder, p.Path, v)
			if err != nil {
				client.Close()
				return source, nil, zero, nil, err
			}
			return source, client, v, reader, nil
		}
	}
	client.Close()
	return source, nil, zero, nil, errors.New("the version is no longer kept")
}
func (s *Store) unchanged(source Source) error {
	current, err := s.source(source.Endpoint.ServiceID)
	if err != nil || !reflect.DeepEqual(source, current) {
		return errors.New("the history source changed while downloading")
	}
	return nil
}
func (s *Store) Download(parent context.Context, p Request) (string, error) {
	ctx, done, err := s.begin(parent)
	if err != nil {
		return "", err
	}
	defer done()
	source, client, version, reader, err := s.openVersion(ctx, p)
	if err != nil {
		return "", err
	}
	defer client.Close()
	defer reader.Close()
	s.cacheMu.Lock()
	defer s.cacheMu.Unlock()
	releaseQuota, err := previewcache.Reserve(ctx, s.cache, version.Size)
	if err != nil {
		return "", err
	}
	defer releaseQuota()
	dir, err := os.MkdirTemp(s.cache, "history-")
	if err != nil {
		return "", err
	}
	success := false
	defer func() {
		if !success {
			os.RemoveAll(dir)
		}
	}()
	dest := filepath.Join(dir, filepath.Base(p.Path))
	f, err := os.OpenFile(dest, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return "", err
	}
	n, err := safecopy.Copy(ctx, f, io.LimitReader(reader, version.Size+1))
	err = errors.Join(err, f.Sync(), f.Close())
	if err != nil {
		return "", err
	}
	if n != version.Size {
		return "", errors.New("the downloaded version is incomplete")
	}
	if err = s.unchanged(source); err != nil {
		return "", err
	}
	success = true
	return dest, nil
}
func (s *Store) Export(parent context.Context, p Request) error {
	ctx, done, err := s.begin(parent)
	if err != nil {
		return err
	}
	defer done()
	source, client, version, reader, err := s.openVersion(ctx, p)
	if err != nil {
		return err
	}
	defer client.Close()
	defer reader.Close()
	dest, err := safecopy.Create(p.Destination, s.forbidden...)
	if err != nil {
		return err
	}
	defer dest.Close()
	n, err := safecopy.Copy(ctx, dest.File, io.LimitReader(reader, version.Size+1))
	if err != nil {
		return err
	}
	if n != version.Size {
		return errors.New("the archived version is incomplete")
	}
	if err = s.unchanged(source); err != nil {
		return err
	}
	return dest.Commit()
}
func (s *Store) update(ctx context.Context, p Request, forget bool) error {
	_, done, err := s.begin(ctx)
	if err != nil {
		return err
	}
	defer done()
	s.mu.Lock()
	defer s.mu.Unlock()
	list, err := s.load()
	if err != nil {
		return err
	}
	out := []Source{}
	for _, source := range list {
		if source.Endpoint.ServiceID == p.ID {
			if forget {
				continue
			}
			source.Enabled = p.Enabled
		}
		out = append(out, source)
	}
	// Remove obsolete one-time receipts before committing source removal. A crash
	// can leave the old source unchanged, but cannot replay a forgotten pairing.
	pending, err := s.approvals()
	if err != nil {
		return err
	}
	kept := []approval{}
	for _, a := range pending {
		if a.Source.Endpoint.ServiceID != p.ID {
			kept = append(kept, a)
		}
	}
	if err = s.saveApprovals(kept); err != nil {
		return err
	}
	if err = s.save(out); err != nil {
		return err
	}
	s.generation[p.ID]++
	if forget {
		return s.credentials.DeleteCredential(p.ID)
	}
	return nil
}
func (s *Store) Register(a *adminsock.Server) {
	for _, method := range []string{"history.sources", "history.pair", "history.cancel", "history.enable", "history.forget", "history.list", "history.download", "history.export"} {
		a.Register(method, func(ctx context.Context, b json.RawMessage) (any, error) {
			var p Request
			if len(b) > 0 {
				if err := json.Unmarshal(b, &p); err != nil {
					return nil, err
				}
			}
			switch method {
			case "history.sources":
				return s.Sources()
			case "history.pair":
				ctx, cancel := context.WithTimeout(ctx, 11*time.Minute)
				defer cancel()
				if p.ID != "" {
					s.mu.Lock()
					s.cancellations[p.ID] = cancel
					s.mu.Unlock()
					defer func() { s.mu.Lock(); delete(s.cancellations, p.ID); s.mu.Unlock() }()
				}
				return s.Pair(ctx, p.Link)
			case "history.cancel":
				s.mu.Lock()
				if cancel := s.cancellations[p.ID]; cancel != nil {
					cancel()
				}
				s.mu.Unlock()
				return nil, nil
			case "history.enable":
				return nil, s.update(ctx, p, false)
			case "history.forget":
				return nil, s.update(ctx, p, true)
			case "history.list":
				return s.List(ctx, p.Folder, p.Path)
			case "history.download":
				return s.Download(ctx, p)
			case "history.export":
				return nil, s.Export(ctx, p)
			}
			return nil, errors.New("unknown history action")
		})
	}
}
