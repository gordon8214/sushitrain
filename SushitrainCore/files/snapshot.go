// Copyright (C) 2026 Gordon
// SPDX-License-Identifier: MPL-2.0

package files

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"t-shaped.nl/sushitrain/v2/sharedfile"
	"time"
)

// RecordUnchangedSnapshot verifies a native recovery capture against this exact
// indexed version and records a replay receipt without scheduling an upload.
// This also permits unchanged read-only files to regain placeholder identity.
func (s *Service) RecordUnchangedSnapshot(ctx context.Context, entry Entry, source io.ReaderAt, size int64, captureID string) (bool, error) {
	if err := ctx.Err(); err != nil {
		return false, err
	}
	if size != entry.Size || captureID == "" {
		return false, nil
	}
	if previous, ok, err := s.SnapshotReceipt(captureID); err != nil {
		return false, err
	} else if ok {
		return previous.Item == entry.Item && previous.Node == entry.Node, nil
	}
	reader, err := s.OpenNode(entry)
	if err != nil {
		return false, err
	}
	defer reader.Close()
	if reader.Size() != size {
		return false, nil
	}
	local, expected := make([]byte, 1<<20), make([]byte, 1<<20)
	for offset := int64(0); offset < size; {
		if err := ctx.Err(); err != nil {
			return false, err
		}
		count := int(min(int64(len(local)), size-offset))
		n, err := source.ReadAt(local[:count], offset)
		if err != nil && !errors.Is(err, io.EOF) {
			return false, err
		}
		if n != count {
			return false, io.ErrUnexpectedEOF
		}
		n, err = reader.ReadAt(ctx, expected[:count], offset)
		if err != nil && !errors.Is(err, io.EOF) {
			return false, err
		}
		if n != count {
			return false, io.ErrUnexpectedEOF
		}
		if !bytes.Equal(local[:count], expected[:count]) {
			return false, nil
		}
		offset += int64(count)
	}
	err = s.store.tx(ctx, func(tx *sql.Tx) error {
		if data, err := metaValue(tx, "native-capture/"+captureID); err == nil {
			var receipt snapshotReceipt
			if err := json.Unmarshal([]byte(data), &receipt); err != nil {
				return err
			}
			if receipt.Entry.Item != entry.Item || receipt.Entry.Node != entry.Node {
				return ErrVersionChanged
			}
			return nil
		} else if !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		current, err := liveItem(tx, entry.Item)
		if err != nil {
			return err
		}
		if current == nil || uint64(current.node) != entry.Node {
			return ErrVersionChanged
		}
		data, err := json.Marshal(snapshotReceipt{Entry: entry, Created: time.Now().Unix()})
		if err != nil {
			return err
		}
		return setMeta(tx, "native-capture/"+captureID, string(data))
	})
	return err == nil, err
}

// ImportSnapshot installs a complete, immutable capture without exposing partial content as a saved edit.
// The entry identifies the version the editor started from. A changed node is refused, so the caller can preserve
// the capture as a conflict copy. No local state changes unless the entire capture has been copied and flushed.
func (s *Service) ImportSnapshot(ctx context.Context, entry Entry, source io.Reader, size int64, modified time.Time, captureIDs ...string) (Entry, error) {
	if !entry.Writable {
		return Entry{}, ErrReadOnly
	}
	if entry.IsDir() {
		return Entry{}, ErrIsDir
	}
	return s.importSnapshot(ctx, entry, 0, "", false, source, size, modified, firstCaptureID(captureIDs))
}

// CreateSnapshot creates a file and its complete contents in one transaction. With conflict, its name follows
// Syncthing's conflict-copy convention; a failed capture never leaves an empty file queued for upload.
func (s *Service) CreateSnapshot(ctx context.Context, parent int64, name string, conflict bool, source io.Reader, size int64, modified time.Time, captureIDs ...string) (Entry, error) {
	name = normalizedName(name)
	if !validName(name) || reservedName(name) {
		return Entry{}, ErrInvalidName
	}
	if conflict {
		id, err := s.ClientIdentity()
		if err != nil {
			return Entry{}, err
		}
		name = conflictName(name, id, time.Now())
	}
	return s.importSnapshot(ctx, Entry{}, parent, name, conflict, source, size, modified, firstCaptureID(captureIDs))
}

func (s *Service) importSnapshot(ctx context.Context, entry Entry, parent int64, targetName string, conflict bool, source io.Reader, size int64, modified time.Time, captureID string) (Entry, error) {
	if captureID != "" {
		if previous, ok, err := s.SnapshotReceipt(captureID); err != nil {
			return Entry{}, err
		} else if ok {
			return previous, nil
		}
	}
	if size < 0 {
		return Entry{}, os.ErrInvalid
	}
	if err := os.MkdirAll(s.workDir(), 0700); err != nil {
		return Entry{}, err
	}
	name := "snapshot-" + randomName()
	path := filepath.Join(s.workDir(), name)
	file, err := sharedfile.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_RDWR, 0600)
	if err != nil {
		return Entry{}, err
	}
	committed := false
	defer func() {
		file.Close()
		if !committed {
			_ = os.Remove(path)
		}
	}()
	buffer := make([]byte, 1<<20)
	var copied int64
	for {
		if err := ctx.Err(); err != nil {
			return Entry{}, err
		}
		n, readErr := source.Read(buffer)
		if n > 0 {
			copied += int64(n)
			if copied > size {
				return Entry{}, errors.New("snapshot is larger than its recorded size")
			}
			if _, err := file.Write(buffer[:n]); err != nil {
				return Entry{}, err
			}
		}
		if errors.Is(readErr, io.EOF) {
			break
		}
		if readErr != nil {
			return Entry{}, readErr
		}
		if n == 0 {
			return Entry{}, io.ErrNoProgress
		}
	}
	if copied != size {
		return Entry{}, io.ErrUnexpectedEOF
	}
	if err := file.Sync(); err != nil {
		return Entry{}, err
	}
	if err := syncDir(s.workDir()); err != nil {
		return Entry{}, err
	}
	s.settleMutex.Lock()
	wc := s.workingCopy(entry.Node)
	if wc != nil {
		wc.mutex.Lock()
		defer wc.mutex.Unlock()
		if wc.writers > 0 && !wc.detached {
			s.settleMutex.Unlock()
			return Entry{}, errors.New("the file is still being edited")
		}
	}
	var changes Changes
	var oldWork sql.NullString
	replayed := false
	err = s.store.tx(ctx, func(tx *sql.Tx) error {
		if captureID != "" {
			data, err := metaValue(tx, "native-capture/"+captureID)
			if err == nil {
				var receipt snapshotReceipt
				if err := json.Unmarshal([]byte(data), &receipt); err != nil {
					return err
				}
				entry = receipt.Entry
				replayed = true
				return nil
			} else if !errors.Is(err, sql.ErrNoRows) {
				return err
			}
		}
		var r *localRow
		var d *localRow
		var err error
		node, err := allocate(tx, metaNextNode)
		if err != nil {
			return err
		}
		if entry.Item == 0 {
			d, err = directory(tx, parent)
			if err != nil {
				return err
			}
			base := targetName
			for i := 0; ; i++ {
				existing, err := liveChild(tx, parent, targetName)
				if err != nil {
					return err
				}
				if existing == nil {
					break
				}
				if !conflict {
					return os.ErrExist
				}
				ext := filepath.Ext(base)
				targetName = fmt.Sprintf("%s (%d)%s", strings.TrimSuffix(base, ext), i+2, ext)
			}
			local := localCreated
			if localOnlyName(targetName) || d.local&localOnly != 0 {
				local |= localOnly
			}
			result, err := tx.Exec(`INSERT INTO items (folder,parent,name,kind,mod_ns,node,local,changed_at) VALUES (?,?,?,?,?,?,?,?)`, d.folder, parent, targetName, kindFile, modified.UnixNano(), node, local, nowMillis())
			if err != nil {
				return err
			}
			entry.Item, err = result.LastInsertId()
			if err != nil {
				return err
			}
			r = &localRow{name: targetName, local: local}
		} else {
			r, err = liveItem(tx, entry.Item)
			if err != nil {
				return err
			}
			if r == nil {
				return os.ErrNotExist
			}
			if uint64(r.node) != entry.Node {
				return ErrVersionChanged
			}
			d, err = directory(tx, r.parent.Int64)
			if err != nil {
				return err
			}
			if err := captureBase(tx, entry.Item); err != nil {
				return err
			}
		}
		entry.Folder = d.folder
		if err := tx.QueryRow(`SELECT file FROM work WHERE node = ?`, entry.Node).Scan(&oldWork); err != nil && !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		if _, err := tx.Exec(`DELETE FROM work WHERE node = ?`, entry.Node); err != nil {
			return err
		}
		if _, err := tx.Exec(`INSERT INTO work (node,item,file,writing) VALUES (?,?,?,0)`, node, entry.Item, name); err != nil {
			return err
		}
		local := localContent
		if r.local&localCreated != 0 {
			local = 0
		}
		if _, err := tx.Exec(`UPDATE items SET node=?,local=local|?,local_content=?,size=?,mod_ns=?,changed_at=? WHERE id=?`, node, local, filepath.Join("Work", name), size, modified.UnixNano(), nowMillis(), entry.Item); err != nil {
			return err
		}
		changes.entry(uint64(d.node), r.name, entry.Node)
		if captureID != "" {
			saved, err := scanEntry(tx.QueryRow(entrySelect+` WHERE i.id = ?`, entry.Item))
			if err != nil {
				return err
			}
			data, err := json.Marshal(snapshotReceipt{Entry: saved, Created: time.Now().Unix()})
			if err != nil {
				return err
			}
			if err := setMeta(tx, "native-capture/"+captureID, string(data)); err != nil {
				return err
			}
		}
		return nil
	})
	if err == nil && !replayed && wc != nil {
		wc.detached = true
		wc.retired = true
		if wc.file != nil {
			_ = wc.file.Close()
			wc.file = nil
		}
	}
	s.settleMutex.Unlock()
	if err != nil {
		return Entry{}, fmt.Errorf("installing snapshot: %w", err)
	}
	if replayed {
		return entry, nil
	}
	committed = true
	if oldWork.Valid {
		_ = os.Remove(filepath.Join(s.workDir(), oldWork.String))
	}
	s.changed(changes)
	s.settleLater(entry.Folder)
	result, ok, err := s.Item(entry.Item)
	if err == nil && !ok {
		err = os.ErrNotExist
	}
	return result, err
}

// ClientIdentity is the stable identity that binds native placeholders to this index.
func (s *Service) ClientIdentity() (string, error) { return s.store.ClientID() }

type snapshotReceipt struct {
	Entry   Entry
	Created int64
}

func firstCaptureID(ids []string) string {
	if len(ids) > 0 {
		return ids[0]
	}
	return ""
}

// SnapshotReceipt makes a lost local capture response safe to replay after a process restart.
func (s *Service) SnapshotReceipt(id string) (Entry, bool, error) {
	data, err := metaValue(s.store.db, "native-capture/"+id)
	if errors.Is(err, sql.ErrNoRows) {
		return Entry{}, false, nil
	}
	if err != nil {
		return Entry{}, false, err
	}
	var receipt snapshotReceipt
	if err := json.Unmarshal([]byte(data), &receipt); err != nil {
		return Entry{}, false, err
	}

	return receipt.Entry, true, nil
}

// AcknowledgeSnapshot releases the receipt only after the native provider durably records the imported identity.
func (s *Service) AcknowledgeSnapshot(id string) error {
	return s.store.tx(context.Background(), func(tx *sql.Tx) error {
		_, err := tx.Exec(`DELETE FROM meta WHERE key = ?`, "native-capture/"+id)
		return err
	})
}
