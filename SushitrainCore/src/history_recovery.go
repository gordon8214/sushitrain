// Copyright (C) 2026 Gordon
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this file,
// You can obtain one at https://mozilla.org/MPL/2.0/.
package sushitrain

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/syncthing/syncthing/lib/config"
)

const historyJournalPrefix = ".syncthing.history-journal-"

type historyJournal struct {
	Target  string
	Backup  string
	Staged  string
	OldHash string
	NewHash string
}

func syncHistoryDirectory(root *os.Root) error {
	dir, err := root.Open(".")
	if err != nil {
		return err
	}
	defer dir.Close()
	return dir.Sync()
}

// A hard link preserves the complete original, including resource forks, before the versioner removes its name.
// Filesystems without hard links refuse the restore before anything is displaced.
func beginHistoryJournal(ctx context.Context, root *os.Root, name, staged, oldHash, newHash, pendingDirectory string) (string, historyJournal, error) {
	backup := filepath.Join(filepath.Dir(name), ".syncthing.history-old-"+rand.Text()+".tmp")
	record := historyJournal{Target: name, Backup: backup, Staged: staged, OldHash: oldHash, NewHash: newHash}
	if err := root.Link(name, backup); err != nil {
		return "", record, fmt.Errorf("could not preserve the original before restoring: %w", err)
	}
	if hash, err := historyFileHashContext(ctx, root, backup); err != nil || hash != oldHash {
		_ = root.Remove(backup)
		return "", record, errors.Join(errRestoreTargetChanged, err)
	}
	parent, err := root.OpenRoot(filepath.Dir(name))
	if err != nil {
		_ = root.Remove(backup)
		return "", record, err
	}
	syncErr := syncHistoryDirectory(parent)
	parent.Close()
	if syncErr != nil {
		_ = root.Remove(backup)
		return "", record, syncErr
	}
	journal := historyJournalPrefix + rand.Text() + ".tmp"
	if err := writeHistoryJSON(root, journal, record); err != nil {
		_ = root.Remove(backup)
		return "", record, err
	}
	// This marker lives on the engine's configuration volume, so an unavailable or repointed folder cannot bypass recovery.
	if err := os.MkdirAll(pendingDirectory, 0o700); err != nil {
		return "", record, err
	}
	configRoot, err := os.OpenRoot(filepath.Dir(pendingDirectory))
	if err != nil {
		return "", record, err
	}
	err = syncHistoryDirectory(configRoot)
	configRoot.Close()
	if err != nil {
		return "", record, err
	}
	pendingRoot, err := os.OpenRoot(pendingDirectory)
	if err != nil {
		return "", record, err
	}
	defer pendingRoot.Close()
	pending := historyPending{Root: root.Name(), Journal: journal, Record: record}
	if err := writeHistoryJSON(pendingRoot, historyPendingName(root.Name(), journal), pending); err != nil {
		return "", record, err
	}
	return journal, record, nil
}

// Only complete, synced records enter the recovery namespace. A crash while writing leaves an ignored draft.
func writeHistoryJSON(root *os.Root, name string, value any) error {
	draft := ".syncthing.history-draft-" + rand.Text() + ".tmp"
	file, err := root.OpenFile(draft, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	defer root.Remove(draft)
	encodeErr := json.NewEncoder(file).Encode(value)
	syncErr := file.Sync()
	closeErr := file.Close()
	if err := errors.Join(encodeErr, syncErr, closeErr); err != nil {
		return err
	}
	if err := root.Rename(draft, name); err != nil {
		return err
	}
	return syncHistoryDirectory(root)
}

type historyPending struct {
	Root    string
	Journal string
	Record  historyJournal
}

func historyPendingName(root, journal string) string {
	hash := sha256.Sum256([]byte(root + "\x00" + journal))
	return hex.EncodeToString(hash[:]) + ".json"
}

func removeHistoryPending(pendingDirectory, root, journal string) error {
	pending, err := os.OpenRoot(pendingDirectory)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	defer pending.Close()
	if err := removeHistoryArtifact(pending, historyPendingName(root, journal)); err != nil {
		return err
	}
	return syncHistoryDirectory(pending)
}

func removeHistoryArtifact(root *os.Root, name string) error {
	if err := root.Remove(name); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	return nil
}

func historyFileHash(root *os.Root, name string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	return historyFileHashContext(ctx, root, name)
}

func historyFileHashContext(ctx context.Context, root *os.Root, name string) (string, error) {
	info, err := root.Lstat(name)
	if err != nil {
		return "", err
	}
	if !info.Mode().IsRegular() {
		return "", errRestoreTargetChanged
	}
	file, err := root.Open(name)
	if err != nil {
		return "", err
	}
	defer file.Close()
	hash := sha256.New()
	if _, err := io.Copy(hash, historyContextReader{ctx, file}); err != nil {
		return "", err
	}
	return hex.EncodeToString(hash.Sum(nil)), nil
}

func recoverHistoryJournal(root *os.Root, journal string, record historyJournal, pendingDirectory string) error {
	if filepath.Base(journal) != journal || !strings.HasPrefix(journal, historyJournalPrefix) || !strings.HasSuffix(journal, ".tmp") || !finderPathAllowed(record.Target) || !finderSafePath(root.Name(), record.Target) ||
		filepath.Dir(record.Backup) != filepath.Dir(record.Target) || filepath.Dir(record.Staged) != filepath.Dir(record.Target) ||
		!strings.HasPrefix(filepath.Base(record.Backup), ".syncthing.history-old-") ||
		!strings.HasPrefix(filepath.Base(record.Staged), ".syncthing.history-") ||
		len(record.OldHash) != 64 || len(record.NewHash) != 64 {
		return errors.New("invalid interrupted history restore journal")
	}
	hash, err := historyFileHash(root, record.Target)
	if errors.Is(err, fs.ErrNotExist) {
		backupHash, err := historyFileHash(root, record.Backup)
		if err != nil || backupHash != record.OldHash {
			return errors.New("the original of an interrupted history restore needs recovery")
		}
		if err := root.Rename(record.Backup, record.Target); err != nil {
			return err
		}
	} else if err != nil {
		return err
	} else if hash != record.OldHash && hash != record.NewHash {
		return errors.New("a file changed after an interrupted history restore; its recovery copies were kept")
	}
	parent, err := root.OpenRoot(filepath.Dir(record.Target))
	if err != nil {
		return err
	}
	defer parent.Close()
	if err := removeHistoryArtifact(root, record.Backup); err != nil {
		return err
	}
	if err := removeHistoryArtifact(root, record.Staged); err != nil {
		return err
	}
	if err := syncHistoryDirectory(parent); err != nil {
		return err
	}
	if err := removeHistoryArtifact(root, journal); err != nil {
		return err
	}
	if err := syncHistoryDirectory(root); err != nil {
		return err
	}
	return removeHistoryPending(pendingDirectory, root.Name(), journal)
}

// Called before starting scanners/pullers, so a crash in archive/publication cannot become a synchronized deletion.
func recoverHistoryRestores(folders map[string]config.FolderConfiguration, pendingDirectory string) error {
	entries, err := os.ReadDir(pendingDirectory)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	for _, entry := range entries {
		if !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		file, err := os.Open(filepath.Join(pendingDirectory, entry.Name()))
		if err != nil {
			return err
		}
		var pending historyPending
		err = json.NewDecoder(io.LimitReader(file, 64<<10)).Decode(&pending)
		file.Close()
		if err != nil {
			return err
		}
		if entry.Name() != historyPendingName(pending.Root, pending.Journal) {
			return errors.New("invalid pending history restore")
		}
		root, err := os.OpenRoot(pending.Root)
		if err != nil {
			return fmt.Errorf("the folder with an interrupted history restore must be available before starting: %w", err)
		}
		err = recoverHistoryJournal(root, pending.Journal, pending.Record, pendingDirectory)
		root.Close()
		if err != nil {
			return err
		}
	}

	for _, folder := range folders {
		if !isNativeFilesystem(folder.FilesystemType) {
			continue
		}
		entries, err := os.ReadDir(folder.Path)
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			continue
		} // An unavailable folder is reported by the engine as usual.
		for _, entry := range entries {
			if !strings.HasPrefix(entry.Name(), historyJournalPrefix) || !strings.HasSuffix(entry.Name(), ".tmp") {
				continue
			}
			root, err := os.OpenRoot(folder.Path)
			if err != nil {
				return err
			}
			file, err := root.Open(entry.Name())
			if err != nil {
				root.Close()
				return err
			}
			var record historyJournal
			err = json.NewDecoder(io.LimitReader(file, 64<<10)).Decode(&record)
			file.Close()
			if err == nil {
				err = recoverHistoryJournal(root, entry.Name(), record, pendingDirectory)
			}
			root.Close()
			if err != nil {
				return fmt.Errorf("recover interrupted history restore in folder %s: %w", folder.ID, err)
			}
		}
	}
	return nil
}

func publishHistoryFile(root *os.Root, staged, name, journal string, record historyJournal, pendingDirectory string) error {
	if err := root.Rename(staged, name); err != nil {
		if journal != "" {
			return errors.Join(err, recoverHistoryJournal(root, journal, record, pendingDirectory))
		}
		return err
	}
	if journal != "" {
		return recoverHistoryJournal(root, journal, record, pendingDirectory)
	}
	return nil
}
