// Copyright (C) 2026 Gordon
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this file,
// You can obtain one at https://mozilla.org/MPL/2.0/.

// Package files is Synctrain's client of gateways for Linux: it keeps an index of the folders added from gateways,
// follows their change feeds, and fetches, verifies and caches their files on demand. It is the Go counterpart of
// the Swift SynctrainFileProviderKit, whose semantics it keeps (see Docs/remote-api/invariants.md); the FUSE mount
// that shows it is in files/fusefs.
package files

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"path/filepath"
	"strconv"

	"github.com/google/uuid"
)

const schemaVersion = 3

var errUnsupportedSchema = errors.New("the index was created by a newer version")

var schema = []string{
	`CREATE TABLE meta (
		key TEXT PRIMARY KEY,
		value TEXT NOT NULL
	)`,
	// Paired gateways. pins are the accepted public key hashes, space-separated (none: system trust).
	`CREATE TABLE gateways (
		service_id TEXT PRIMARY KEY,
		host TEXT NOT NULL,
		port INTEGER NOT NULL,
		pins TEXT NOT NULL DEFAULT '',
		name TEXT NOT NULL,
		authority_generation INTEGER,
		features TEXT NOT NULL DEFAULT '',
		state TEXT NOT NULL DEFAULT '',
		added_at INTEGER NOT NULL
	)`,
	// Folders added from gateways. dir_name is the folder's directory in the mount.
	`CREATE TABLE folders (
		id INTEGER PRIMARY KEY,
		service_id TEXT NOT NULL REFERENCES gateways(service_id),
		remote_id TEXT NOT NULL,
		label TEXT NOT NULL,
		dir_name TEXT NOT NULL UNIQUE,
		root_item INTEGER NOT NULL,
		root_gateway_id TEXT,
		can_read INTEGER NOT NULL DEFAULT 0,
		can_write INTEGER NOT NULL DEFAULT 0,
		state TEXT NOT NULL DEFAULT '',
		added_at INTEGER NOT NULL,
		UNIQUE (service_id, remote_id)
	)`,
	// What the gateway says, by its item ID: the projection (RemoteProjector) turns it into items
	`CREATE TABLE remote_items (
		gateway_id TEXT PRIMARY KEY,
		folder INTEGER NOT NULL,
		item INTEGER,
		parent_gateway_id TEXT,
		name TEXT NOT NULL,
		kind TEXT NOT NULL,
		revision TEXT NOT NULL,
		content_id TEXT,
		size INTEGER NOT NULL DEFAULT 0,
		modified_ns INTEGER NOT NULL DEFAULT 0,
		subtree_generation INTEGER,
		deleted INTEGER NOT NULL DEFAULT 0,
		placed INTEGER NOT NULL DEFAULT 0,
		seen_mark INTEGER NOT NULL DEFAULT 0
	)`,
	`CREATE UNIQUE INDEX remote_items_item ON remote_items(item) WHERE item IS NOT NULL`,
	`CREATE INDEX remote_items_waiting ON remote_items(folder) WHERE placed = 0 AND deleted = 0`,
	`CREATE INDEX remote_items_seen ON remote_items(folder, seen_mark)`,
	// What the mount shows. An item keeps its ID through renames and moves; node is its FUSE inode number, which
	// changes when its content does, so that a handle on the old content keeps its own pages.
	//
	// Changes made here are in local (local… flags) until they are settled into operations, and an item shows them
	// until the gateway has committed those. base_… is what the gateway had of the item when it was first changed
	// here: what the changes are based on. local_content is the file in the state directory with the item's content
	// while the gateway hasn't committed it.
	`CREATE TABLE items (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		folder INTEGER NOT NULL,
		parent INTEGER,
		name TEXT NOT NULL,
		kind INTEGER NOT NULL,
		size INTEGER NOT NULL DEFAULT 0,
		mod_ns INTEGER NOT NULL DEFAULT 0,
		content_id TEXT,
		node INTEGER NOT NULL,
		keep INTEGER NOT NULL DEFAULT 0,
		last_used INTEGER NOT NULL DEFAULT 0,
		deleted INTEGER NOT NULL DEFAULT 0,
		deleted_at INTEGER,
		local INTEGER NOT NULL DEFAULT 0,
		base_revision TEXT,
		base_content_id TEXT,
		base_subtree INTEGER,
		changed_at INTEGER NOT NULL DEFAULT 0,
		local_content TEXT
	)`,
	`CREATE INDEX items_local ON items(folder) WHERE local != 0`,
	// Working copies of files being written, one per node: a reflink of the content it started from, which writes
	// change. writing means writable handles were open (after a crash, its content may be torn); detached means its
	// node no longer shows an item (it was removed or replaced), so what is written to it is dropped, as on a local
	// filesystem.
	`CREATE TABLE work (
		node INTEGER PRIMARY KEY,
		item INTEGER NOT NULL,
		file TEXT NOT NULL,
		writing INTEGER NOT NULL DEFAULT 0,
		detached INTEGER NOT NULL DEFAULT 0
	)`,
	// Operations to send, in order per folder. payload is the request exactly as sent, stored before it is sent the
	// first time (one operation ID per intent, replayed unchanged). staged is the immutable copy of the content to
	// upload, in the state directory. A row whose base_revision is empty takes it from the result of the operation
	// before it on the same item. resends counts the times it was sent again as a new intent after the gateway
	// refused it; awaited_generation is the subtree generation a directory's removal the gateway found out of date
	// waits for the change feed to reach, before it is checked and sent again.
	`CREATE TABLE outbox (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		op_id TEXT NOT NULL UNIQUE,
		folder INTEGER NOT NULL,
		item INTEGER NOT NULL,
		kind TEXT NOT NULL,
		item_kind TEXT NOT NULL DEFAULT '',
		state TEXT NOT NULL,
		base_revision TEXT NOT NULL DEFAULT '',
		base_content_id TEXT NOT NULL DEFAULT '',
		subtree_generation INTEGER,
		awaited_generation INTEGER,
		resends INTEGER NOT NULL DEFAULT 0,
		target_parent INTEGER,
		target_name TEXT NOT NULL DEFAULT '',
		staged TEXT,
		digest TEXT NOT NULL DEFAULT '',
		size INTEGER NOT NULL DEFAULT 0,
		mod_ns INTEGER,
		upload_id TEXT NOT NULL DEFAULT '',
		payload BLOB,
		server_op TEXT NOT NULL DEFAULT '',
		authority_generation INTEGER,
		result_revision TEXT NOT NULL DEFAULT '',
		result_content_id TEXT NOT NULL DEFAULT '',
		attempts INTEGER NOT NULL DEFAULT 0,
		not_before INTEGER NOT NULL DEFAULT 0,
		error TEXT NOT NULL DEFAULT '',
		created_at INTEGER NOT NULL,
		updated_at INTEGER NOT NULL
	)`,
	`CREATE INDEX outbox_folder ON outbox(folder, state)`,
	`CREATE INDEX outbox_item ON outbox(item)`,
	`CREATE UNIQUE INDEX items_child ON items(parent, name) WHERE deleted = 0`,
	`CREATE UNIQUE INDEX items_node ON items(node)`,
	`CREATE INDEX items_folder ON items(folder, deleted)`,
	`CREATE INDEX items_content ON items(content_id) WHERE deleted = 0`,
	keepIndex,
	// Where each folder's change feed is, and its snapshot in progress
	`CREATE TABLE cursors (
		folder INTEGER PRIMARY KEY,
		cursor TEXT NOT NULL
	)`,
	`CREATE TABLE snapshots (
		folder INTEGER PRIMARY KEY,
		snapshot_id TEXT,
		start_cursor TEXT,
		next_page TEXT,
		catch_up_cursor TEXT,
		mark INTEGER NOT NULL
	)`,
	// Verified files in the content cache, named after their content ID
	`CREATE TABLE content (
		content_id TEXT PRIMARY KEY,
		size INTEGER NOT NULL,
		last_used INTEGER NOT NULL
	)`,
	// Downloads in progress: the verified manifest (32-byte hashes then 4-byte sizes, per block) and the blocks of the
	// partial file that are verified
	`CREATE TABLE transfers (
		content_id TEXT PRIMARY KEY,
		item INTEGER NOT NULL,
		size INTEGER NOT NULL,
		block_count INTEGER NOT NULL,
		manifest BLOB,
		verified BLOB,
		etag TEXT NOT NULL DEFAULT '',
		updated_at INTEGER NOT NULL
	)`,
}

// keepIndex finds what is kept on this device without reading every item
const keepIndex = `CREATE INDEX items_keep ON items(id) WHERE keep = 1 AND deleted = 0`

// migrations bring an index made with an earlier schema up to date: migrations[v-1] goes from version v to v+1
var migrations = [][]string{
	{keepIndex},
	{`ALTER TABLE outbox ADD COLUMN awaited_generation INTEGER`,
		`ALTER TABLE outbox ADD COLUMN resends INTEGER NOT NULL DEFAULT 0`},
}

const (
	metaClientID = "client_id"
	metaNextNode = "next_node"
	metaNextMark = "next_mark"
)

// rootNode is the FUSE inode number of the mount's root; items' nodes are allocated after it
const rootNode = 1

// Store is the index in SQLite, owned by one daemon
type Store struct {
	db *sql.DB
}

// OpenStore opens or creates the index at path
func OpenStore(path string) (*Store, error) {
	// A relative path would be read as the URI's authority
	path, err := filepath.Abs(path)
	if err != nil {
		return nil, err
	}
	u := url.URL{Scheme: "file", Path: path, RawQuery: driverOptions}
	db, err := sql.Open(driverName, u.String())
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(8)
	s := &Store{db: db}
	if err := s.migrate(); err != nil {
		db.Close()
		return nil, err
	}
	return s, nil
}

func (s *Store) Close() error {
	return s.db.Close()
}

func (s *Store) migrate() error {
	return s.tx(context.Background(), func(tx *sql.Tx) error {
		var version int
		if err := tx.QueryRow(`PRAGMA user_version`).Scan(&version); err != nil {
			return err
		}
		switch {
		case version == schemaVersion:
			return nil
		case version > schemaVersion:
			return fmt.Errorf("%w (schema %d)", errUnsupportedSchema, version)
		case version != 0:
			for _, step := range migrations[version-1:] {
				for _, statement := range step {
					if _, err := tx.Exec(statement); err != nil {
						return fmt.Errorf("updating the index from schema %d: %w", version, err)
					}
				}
			}
			_, err := tx.Exec(fmt.Sprintf(`PRAGMA user_version = %d`, schemaVersion))
			return err
		}
		for _, statement := range schema {
			if _, err := tx.Exec(statement); err != nil {
				return fmt.Errorf("creating the index: %w", err)
			}
		}
		for key, value := range map[string]string{
			// The stable client identity, kept across re-pairing and rebuilds
			metaClientID: uuid.NewString(),
			metaNextNode: strconv.Itoa(rootNode + 1),
			metaNextMark: "1",
		} {
			if err := setMeta(tx, key, value); err != nil {
				return err
			}
		}
		_, err := tx.Exec(fmt.Sprintf(`PRAGMA user_version = %d`, schemaVersion))
		return err
	})
}

// tx runs fn in a write transaction (BEGIN IMMEDIATE). Transactions must stay short: never wait for the network in
// one.
func (s *Store) tx(ctx context.Context, fn func(*sql.Tx) error) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	// Also releases the write lock if fn panics; a no-op after Commit
	defer func() { _ = tx.Rollback() }()
	if err := fn(tx); err != nil {
		return err
	}
	return tx.Commit()
}

type queryer interface {
	QueryRow(query string, args ...any) *sql.Row
}

func metaValue(q queryer, key string) (string, error) {
	var value string
	err := q.QueryRow(`SELECT value FROM meta WHERE key = ?`, key).Scan(&value)
	return value, err
}

func setMeta(tx *sql.Tx, key, value string) error {
	_, err := tx.Exec(`INSERT INTO meta (key, value) VALUES (?, ?) ON CONFLICT (key) DO UPDATE SET value = excluded.value`,
		key, value)
	return err
}

// allocate returns the next number of a counter in meta and advances it
func allocate(tx *sql.Tx, key string) (int64, error) {
	value, err := metaValue(tx, key)
	if err != nil {
		return 0, err
	}
	next, err := strconv.ParseInt(value, 10, 64)
	if err != nil {
		return 0, err
	}
	return next, setMeta(tx, key, strconv.FormatInt(next+1, 10))
}

// ClientID is this client's stable identity, sent when pairing
func (s *Store) ClientID() (string, error) {
	return metaValue(s.db, metaClientID)
}

// sqlTx is a transaction, for tests that change the index directly
type sqlTx = sql.Tx
