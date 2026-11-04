// Copyright (C) 2026 Gordon
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this file,
// You can obtain one at https://mozilla.org/MPL/2.0/.

package files

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"t-shaped.nl/sushitrain/v2/previewcache"
	"t-shaped.nl/sushitrain/v2/remoteapi"
	"t-shaped.nl/sushitrain/v2/remoteclient"
)

// Previous versions of a file, which the folder's Syncthing versioning keeps on the gateway, as the Swift
// RemoteVersionBrowser shows them. Restoring and copying happen on the gateway, so nothing is uploaded.

var (
	// ErrPendingChange refuses to write a version over a file whose changes here the gateway hasn't committed
	ErrPendingChange = errors.New("the file has changes here that aren't on the gateway yet")
	// ErrNoVersions means the gateway can't list versions (it needs an update)
	ErrNoVersions = errors.New("the gateway can't list previous versions")
)

// Version is a previous version of a file
type Version struct {
	ID        string `json:"id"`
	EntityTag string `json:"etag,omitempty"`
	// When it was archived, and its own modification time
	Archived time.Time `json:"archived"`
	ModTime  time.Time `json:"modTime"`
	Size     int64     `json:"size"`
}

// Versions lists the versions of a file worth showing, newest first, and the folder's versioning (remoteapi
// Versioning…), which says why there are none
type Versions struct {
	Versioning string    `json:"versioning"`
	Versions   []Version `json:"versions"`
	// Whether this machine may restore or copy them (the folder isn't read-only for it)
	Writable bool `json:"writable"`
}

// versionTarget is what writing a version needs of a file
type versionTarget struct {
	folder    folderRef
	gatewayID string
	revision  string
	parentID  sql.NullString
	name      string
	// Its name here, which is checked to be one; the gateway's (name) isn't
	localName string
	modNs     int64
	size      int64
	writable  bool
	pending   bool
}

func (s *Service) versionTarget(item int64) (versionTarget, *remoteclient.Client, error) {
	var t versionTarget
	var folder int64
	err := s.store.db.QueryRow(`SELECT r.gateway_id, r.revision, r.parent_gateway_id, r.name, i.name, r.modified_ns,
		r.size, i.folder, f.can_write, i.local != 0 OR EXISTS (SELECT 1 FROM outbox WHERE item = i.id AND state IN `+
		unresolvedStates+`) FROM items AS i JOIN remote_items AS r ON r.item = i.id JOIN folders AS f ON f.id = i.folder
		WHERE i.id = ? AND i.deleted = 0 AND i.kind = ?`, item, kindFile).Scan(&t.gatewayID, &t.revision, &t.parentID,
		&t.name, &t.localName, &t.modNs, &t.size, &folder, &t.writable, &t.pending)
	if errors.Is(err, sql.ErrNoRows) {
		return t, nil, errors.New("not a file on a gateway")
	}
	if err != nil {
		return t, nil, err
	}
	refs, err := s.folderRefs()
	if err != nil {
		return t, nil, err
	}
	for _, f := range refs {
		if f.id == folder {
			t.folder = f
		}
	}
	client, err := s.client(t.folder.serviceID)
	return t, client, err
}

// Versions lists a file's previous versions. Those whose modification time (to the second) and size are the next
// newer one's are left out: Syncthing archives a version when only a file's metadata changes.
func (s *Service) Versions(ctx context.Context, item int64) (Versions, error) {
	t, client, err := s.versionTarget(item)
	if err != nil {
		return Versions{}, err
	}
	list, err := client.Versions(ctx, t.gatewayID)
	if remoteclient.KindOf(err) == remoteclient.KindNotFound && list.ItemID == "" {
		return Versions{}, ErrNoVersions
	}
	if err != nil {
		return Versions{}, err
	}
	result := Versions{Versioning: list.Versioning, Versions: []Version{}, Writable: t.writable}
	newer := [2]int64{t.modNs / 1e9, t.size}
	for _, v := range list.Versions {
		this := [2]int64{v.ModifiedNs / 1e9, v.Size}
		if this != newer {
			archived, _ := time.Parse(time.RFC3339, v.VersionTime)
			result.Versions = append(result.Versions, Version{ID: v.ID, Archived: archived,
				ModTime: time.Unix(0, v.ModifiedNs), Size: v.Size})
		}
		newer = this
	}
	return result, nil
}

// validVersionID reports whether an ID has the documented form: it names directories of previews
func validVersionID(id string) bool {
	number, ok := strings.CutPrefix(id, remoteapi.PrefixVersion)
	_, err := strconv.ParseInt(number, 10, 64)
	return ok && err == nil
}

// VersionToFile downloads a version for previewing into dir, under the file's name (so that a viewer recognizes its
// type) in a directory of the version. Its content isn't verified, and is never kept as the file's.
func (s *Service) VersionToFile(ctx context.Context, item int64, versionID, dir string) (string, error) {
	s.previewMutex.Lock()
	defer s.previewMutex.Unlock()
	if !validVersionID(versionID) {
		return "", errors.New("invalid version")
	}
	t, client, err := s.versionTarget(item)
	if err != nil {
		return "", err
	}
	// Named as the file is here: one name, which the gateway's needn't be
	if !validName(t.localName) {
		return "", ErrInvalidName
	}
	if err := os.MkdirAll(dir, 0700); err != nil {
		return "", err
	}
	dest := filepath.Join(dir, fmt.Sprintf("%d-%s", item, versionID), t.localName)
	if _, err := os.Stat(dest); err == nil {
		return dest, nil
	}
	versions, err := s.Versions(ctx, item)
	if err != nil {
		return "", err
	}
	size := int64(-1)
	for _, v := range versions.Versions {
		if v.ID == versionID {
			size = v.Size
		}
	}
	if size < 0 {
		return "", errors.New("no such version")
	}
	releaseQuota, err := previewcache.Reserve(ctx, dir, size)
	if err != nil {
		return "", err
	}
	defer releaseQuota()
	if err := os.MkdirAll(filepath.Dir(dest), 0o700); err != nil {
		return "", err
	}
	body, err := client.VersionContent(ctx, t.gatewayID, versionID)
	if err != nil {
		return "", err
	}
	defer body.Close()
	tmp, err := os.CreateTemp(filepath.Dir(dest), ".version-*")
	if err != nil {
		return "", err
	}
	defer os.Remove(tmp.Name())
	// No more than the version is: previews may be in memory (the runtime directory)
	if n, err := io.Copy(tmp, io.LimitReader(body, size+1)); err != nil || n != size {
		tmp.Close()
		if err == nil {
			err = fmt.Errorf("the gateway sent %d bytes of a %d-byte version", n, size)
		}
		return "", err
	}
	if err := tmp.Chmod(0o400); err != nil {
		tmp.Close()
		return "", err
	}
	if err := tmp.Close(); err != nil {
		return "", err
	}
	return dest, os.Rename(tmp.Name(), dest)
}

// RestoreVersion replaces a file with a version, on the gateway, which archives the current content (unless it is the
// version's). A file with changes here isn't restored until they are on the gateway; one that changed on the gateway
// since this machine saw it is not overwritten.
func (s *Service) RestoreVersion(ctx context.Context, item int64, versionID string) error {
	if !validVersionID(versionID) {
		return errors.New("invalid version")
	}
	t, client, err := s.versionTarget(item)
	if err != nil {
		return err
	}
	if !t.writable {
		return ErrReadOnly
	}
	if t.pending {
		return ErrPendingChange
	}
	_, err = s.perform(ctx, client, remoteapi.OperationRequest{ID: uuid.NewString(), Kind: remoteapi.OpReplace,
		FolderID: t.folder.remoteID, ItemID: t.gatewayID, BaseRevision: t.revision, SourceVersion: versionID,
		OnConflict: remoteapi.OnConflictReject})
	if err == nil {
		err = s.refreshFolder(ctx, t.folder, client, 0)
	}
	return err
}

// CopyVersion saves a copy of a version next to the file, on the gateway, named "Name (2026-10-20 15.56).ext" (or
// numbered when that is taken), and returns its name
func (s *Service) CopyVersion(ctx context.Context, item int64, versionID string) (string, error) {
	if !validVersionID(versionID) {
		return "", errors.New("invalid version")
	}
	t, client, err := s.versionTarget(item)
	if err != nil {
		return "", err
	}
	if !t.writable {
		return "", ErrReadOnly
	}
	if !t.parentID.Valid {
		return "", errors.New("the file has no directory on the gateway")
	}
	versions, err := s.Versions(ctx, item)
	if err != nil {
		return "", err
	}
	var modTime time.Time
	for _, v := range versions.Versions {
		if v.ID == versionID {
			modTime = v.ModTime
		}
	}
	taken := map[string]bool{}
	rows, err := s.store.db.Query(`SELECT name FROM remote_items WHERE parent_gateway_id = ? AND deleted = 0`,
		t.parentID.String)
	if err != nil {
		return "", err
	}
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err == nil {
			taken[name] = true
		}
	}
	rows.Close()
	for range 10 {
		name := versionCopyName(t.name, modTime, taken)
		_, err := s.perform(ctx, client, remoteapi.OperationRequest{ID: uuid.NewString(), Kind: remoteapi.OpCreate,
			FolderID: t.folder.remoteID, ParentID: t.parentID.String, Name: name, ItemKind: remoteapi.KindFile,
			SourceItemID: t.gatewayID, SourceVersion: versionID})
		if remoteclient.KindOf(err) == remoteclient.KindNameConflict {
			// A name this machine doesn't know of yet
			taken[name] = true
			continue
		}
		if err == nil {
			err = s.refreshFolder(ctx, t.folder, client, 0)
		}
		return name, err
	}
	return "", errors.New("no free name for the copy")
}

// versionCopyName names a copy of a version: "Name (2026-10-20 15.56).ext", numbered when taken, shortened to fit 255
// bytes
func versionCopyName(name string, modTime time.Time, taken map[string]bool) string {
	stamp := modTime.Local().Format("2006-01-02 15.04")
	ext := filepath.Ext(name)
	stem := strings.TrimSuffix(name, ext)
	if stem == "" {
		stem, ext = name, ""
	}
	for len(fmt.Sprintf("%s (%s) 11%s", stem, stamp, ext)) > 255 && stem != "" {
		_, size := lastRune(stem)
		stem = stem[:len(stem)-size]
	}
	candidate := fmt.Sprintf("%s (%s)%s", stem, stamp, ext)
	for n := 2; taken[candidate]; n++ {
		candidate = fmt.Sprintf("%s (%s) %d%s", stem, stamp, n, ext)
	}
	return candidate
}

func lastRune(s string) (rune, int) {
	for i := len(s) - 1; i >= 0; i-- {
		if s[i]&0xC0 != 0x80 {
			return rune(s[i]), len(s) - i
		}
	}
	return 0, len(s)
}

// perform submits an operation that isn't kept in the outbox (a restore or a copy, started by someone waiting for it)
// and waits until the gateway has carried it out. A request that fails on the way is repeated with the same body,
// which gets the gateway's answer to the first.
func (s *Service) perform(ctx context.Context, client *remoteclient.Client, req remoteapi.OperationRequest) (
	remoteapi.Operation, error) {
	body, err := remoteclient.EncodeOperation(req)
	if err != nil {
		return remoteapi.Operation{}, err
	}
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	op, err := client.SubmitOperation(ctx, body)
	for attempt := 0; err != nil && remoteclient.IsTransient(err) && ctx.Err() == nil; attempt++ {
		time.Sleep(time.Duration(attempt+1) * time.Second)
		op, err = client.SubmitOperation(ctx, body)
	}
	for err == nil && op.State == remoteapi.OperationPending {
		select {
		case <-ctx.Done():
			return op, errors.New("the gateway hasn't carried it out yet")
		case <-time.After(time.Second):
		}
		op, err = client.Operation(ctx, op.Operation)
	}
	if err != nil {
		return op, readOnly(err)
	}
	if op.State == remoteapi.OperationFailed {
		e := &remoteclient.Error{Kind: remoteclient.KindOther, Message: "the gateway couldn't do it"}
		if op.Error != nil {
			e.Code, e.Message = op.Error.Code, op.Error.Message
			if op.Error.Code == remoteapi.CodeNameConflict {
				e.Kind = remoteclient.KindNameConflict
			}
		}
		return op, readOnly(e)
	}
	return op, nil
}

// readOnly is ErrReadOnly for the gateway's refusal to change a folder this machine may only read (its access
// changed since the folder was last listed), and err otherwise
func readOnly(err error) error {
	var e *remoteclient.Error
	if errors.As(err, &e) && e.Code == remoteapi.CodeFolderReadOnly {
		return ErrReadOnly
	}
	return err
}

// Storage is how much the cache holds
type Storage struct {
	// Content fetched and kept within the budget
	Used   int64 `json:"used"`
	Budget int64 `json:"budget"`
	// Content kept on this device on purpose, beyond the budget
	Kept int64 `json:"kept"`
}

const metaCacheBudget = "cache_budget"

// Storage reports what the cache holds
func (s *Service) Storage() (Storage, error) {
	kept, err := s.cache.contentSet(keptContent)
	if err != nil {
		return Storage{}, err
	}
	rows, err := s.store.db.Query(`SELECT content_id, size FROM content`)
	if err != nil {
		return Storage{}, err
	}
	defer rows.Close()
	st := Storage{Budget: s.cacheBudget()}
	for rows.Next() {
		var contentID string
		var size int64
		if err := rows.Scan(&contentID, &size); err != nil {
			return Storage{}, err
		}
		if kept[contentID] {
			st.Kept += size
		} else {
			st.Used += size
		}
	}
	return st, rows.Err()
}

// cacheBudget is the budget set in the window, or the configured one
func (s *Service) cacheBudget() int64 {
	if value, err := metaValue(s.store.db, metaCacheBudget); err == nil {
		if budget, err := strconv.ParseInt(value, 10, 64); err == nil && budget > 0 {
			return budget
		}
	}
	return s.cfg.CacheBudget
}

// SetCacheBudget sets how much fetched content the cache keeps, and trims it to that
func (s *Service) SetCacheBudget(bytes int64) error {
	if bytes < 100<<20 {
		return errors.New("the cache needs at least 100 MB")
	}
	if err := s.store.tx(context.Background(), func(tx *sql.Tx) error {
		return setMeta(tx, metaCacheBudget, strconv.FormatInt(bytes, 10))
	}); err != nil {
		return err
	}
	return s.cache.evict()
}
