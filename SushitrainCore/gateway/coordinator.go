// Copyright (C) 2026 Gordon
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this file,
// You can obtain one at https://mozilla.org/MPL/2.0/.

package gateway

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path"
	"slices"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/syncthing/syncthing/lib/fs"
	"github.com/syncthing/syncthing/lib/osutil"
	"github.com/syncthing/syncthing/lib/protocol"
	"github.com/syncthing/syncthing/lib/scanner"
	"github.com/syncthing/syncthing/lib/syncthing"
	"golang.org/x/sys/unix"
)

// The mutation coordinator publishes operations in a folder's serve loop (RunInFolderLoop), where Syncthing's
// scanner and puller cannot run, so precondition checks, the change on disk and indexing it happen without them
// interfering. It writes through directory file descriptors without following symbolic links, keeps displaced
// content (versioner, Trash or a recovery copy) and journals each step so that a crash at any point is recovered
// without applying an operation twice.

var (
	// The operation waits for something, such as this device having the base version; it stays pending
	errWaiting = errors.New("waiting")
	// The staged file is missing (for instance removed as an old temporary file); it is staged again
	errRestage = errors.New("staged file missing")
)

const maxOperationAttempts = 20

func (s *Service) runOperation(ctx context.Context, id string) {
	op, ok, err := s.loadOperation(s.store.db, id)
	if err != nil || !ok || op.state != stateResultPending {
		return
	}
	var result Operation
	// Until it starts changing files, the client must still be allowed to make the change: it may have been revoked
	// or lost write access since it sent the operation
	if op.journal == journalIntent || op.journal == journalStaged {
		err = s.authorizeOperation(&op)
	}
	if err == nil {
		result, err = s.execute(ctx, &op)
	}
	switch {
	case err == nil:
		if err := s.finish(&op, result, 0); err != nil {
			s.log.Warn("Could not store an operation's result", "operation", op.id, "kind", op.kind, "error", err)
		}
	case errors.Is(err, errWaiting) || ctx.Err() != nil:
		// Retried after index changes and periodically
	default:
		ae := asAPIError(err)
		// Once the change is on disk, Syncthing indexes it in any case: the operation keeps trying to finish
		published := op.journal == journalPublished || op.journal == journalIndexed
		if ae.status >= 500 || published {
			op.data.Attempts++
			attempts := op.data.Attempts
			_ = s.saveJournal(&op, op.journal)
			if published || attempts < maxOperationAttempts {
				s.log.Warn("Operation failed; will retry", "operation", op.id, "kind", op.kind, "error", err)
				return
			}
		}
		s.removeStaged(&op)
		detail := ae.detail("")
		failed := Operation{ID: op.clientOp, Operation: op.id, Kind: op.kind, State: stateResultFailed, Error: &detail,
			AuthorityGeneration: op.generation}
		if err := s.finish(&op, failed, ae.status); err != nil {
			s.log.Warn("Could not store an operation's result", "operation", op.id, "error", err)
		}
	}
}

// authorizeOperation checks that the client is still paired and may still write the operation's folders
func (s *Service) authorizeOperation(op *operationRecord) error {
	var revoked bool
	err := s.store.db.QueryRow(`SELECT revoked_at IS NOT NULL FROM clients WHERE client_id = ?`, op.clientID).Scan(&revoked)
	if errors.Is(err, sql.ErrNoRows) || err == nil && revoked {
		return errForbidden
	}
	if err != nil {
		return err
	}
	folders := []string{op.folder}
	if op.req.DestinationFolderID != "" && op.req.DestinationFolderID != op.folder {
		folders = append(folders, op.req.DestinationFolderID)
	}
	for _, folder := range folders {
		access, err := s.access(op.clientID, folder)
		if errors.Is(err, errNotFound) {
			// Paused or otherwise unavailable for now
			return errWaiting
		}
		if err != nil {
			return err
		}
		if err := access.requireWrite(); err != nil {
			return err
		}
	}
	return nil
}

// removeStaged removes the staged file of an operation that failed before publishing it
func (s *Service) removeStaged(op *operationRecord) {
	if op.data.Temp == "" || op.journal == journalPublished || op.journal == journalIndexed {
		return
	}
	_, root, err := s.folderRoot(op.folder)
	if err != nil {
		return
	}
	defer root.close()
	if pdir, err := root.openDir(parentPath(op.data.Temp)); err == nil {
		if st, err := pdir.lstat(baseName(op.data.Temp)); err == nil && st.exists && st.id.dev == op.data.Device &&
			st.id.ino == op.data.Inode {
			_ = pdir.remove(baseName(op.data.Temp))
		}
		pdir.close()
	}
}

// beginPublish starts a folder loop callback that changes files. It refuses once the service is stopping, so that
// nothing changes on disk that the journal could no longer record; Close waits for the callbacks that started.
func (s *Service) beginPublish() (func(), error) {
	s.publishing.RLock()
	if err := s.ctx.Err(); err != nil {
		s.publishing.RUnlock()
		return nil, err
	}
	return s.publishing.RUnlock, nil
}

// openDirError maps the error of opening a directory on the way to an item: a missing directory is a client-facing
// condition, anything else (such as too many open files) a server error that is retried
func openDirError(err error, missing error) error {
	if errors.Is(err, errSymlink) {
		return newAPIError(http.StatusConflict, codeNameConflict, "the path goes through a symbolic link")
	}
	if errors.Is(err, unix.ENOENT) || errors.Is(err, unix.ENOTDIR) || errors.Is(err, os.ErrNotExist) {
		return missing
	}
	return err
}

func (s *Service) execute(ctx context.Context, op *operationRecord) (Operation, error) {
	switch op.kind {
	case opCreate:
		if op.req.ItemKind == kindDirectory {
			return s.executeCreateDirectory(ctx, op)
		}
		return s.executeWriteFile(ctx, op)
	case opReplace:
		return s.executeWriteFile(ctx, op)
	case opDelete:
		return s.executeDelete(ctx, op)
	case opMove:
		return s.executeMove(ctx, op)
	}
	return Operation{}, newAPIError(http.StatusBadRequest, codeInvalidRequest, "unknown operation kind")
}

// liveRow returns the live item with the ID in the folder
func (s *Service) liveRow(folder, id string) (itemRow, error) {
	row, ok, err := itemByID(s.store.db, id)
	if err != nil {
		return itemRow{}, err
	}
	if !ok || row.deleted || row.folder != folder {
		return itemRow{}, newAPIError(http.StatusNotFound, codeNotFound, "no such item")
	}
	return row, nil
}

func (s *Service) currentItem(folder, p string) *Item {
	row, ok, err := liveItemByPath(s.store.db, folder, p)
	if err != nil || !ok {
		return nil
	}
	item := row.toItem(true)
	return &item
}

func (s *Service) folderRoot(folder string) (FolderInfo, *dir, error) {
	info, ok := s.backend.Folder(folder)
	if !ok || info.NativePath == "" {
		return FolderInfo{}, nil, newAPIError(http.StatusNotFound, codeNotFound, "no such folder")
	}
	root, err := openRoot(info.NativePath)
	if err != nil {
		return FolderInfo{}, nil, err
	}
	return info, root, nil
}

func joinPath(parent, name string) string {
	if parent == "" {
		return name
	}
	return parent + "/" + name
}

// tempNameFor is the staged file of an operation, named as Syncthing names temporary files, so that scans skip it
func tempNameFor(op *operationRecord) string {
	return fs.UnixTempPrefix + strings.TrimPrefix(op.id, prefixOperation) + ".tmp"
}

func reservedNameError() error {
	return newAPIError(http.StatusForbidden, codeReservedName, "the name is reserved by Syncthing")
}

func nameConflict(current *Item) error {
	return newAPIError(http.StatusConflict, codeNameConflict, "an item with that name exists").withItem(current)
}

func ignoredPath() error {
	return newAPIError(http.StatusConflict, codeIgnoredPath, "the folder's ignore patterns exclude the result")
}

func revisionMismatch(current *Item) error {
	return newAPIError(http.StatusPreconditionFailed, codeRevisionMismatch, "the item changed since the base revision").withItem(current)
}

// exists reports whether the index has a live (not deleted, not invalid) entry for name
func liveEntry(h syncthing.FolderHandle, name string) (protocol.FileInfo, bool, error) {
	fi, ok, err := h.GlobalFile(name)
	if err != nil || !ok || fi.Deleted || fi.IsInvalid() {
		return fi, false, err
	}
	return fi, true, nil
}

// matchesIndex reports whether a file or directory on disk is what the local index says, as the puller checks
// before replacing or deleting it
func matchesIndex(st entryStat, local protocol.FileInfo, window time.Duration) bool {
	if !st.exists {
		return false
	}
	switch local.Type {
	case protocol.FileInfoTypeDirectory:
		return st.kind == unix.S_IFDIR
	case protocol.FileInfoTypeFile:
		return st.kind == unix.S_IFREG && st.size == local.Size && st.modTime.Sub(local.ModTime()).Abs() <= window
	}
	return false
}

// Writing files (create and replace)

func (s *Service) executeWriteFile(ctx context.Context, op *operationRecord) (Operation, error) {
	info, root, err := s.folderRoot(op.folder)
	if err != nil {
		return Operation{}, err
	}
	defer root.close()

	lock := s.folderLock(op.folder)
	lock.Lock()
	var target, base string
	if op.kind == opCreate {
		parent, err := s.liveRow(op.folder, op.req.ParentID)
		if err == nil && parent.kind != protocol.FileInfoTypeDirectory {
			err = newAPIError(http.StatusConflict, codeNameConflict, "the parent is not a directory")
		}
		if err != nil {
			lock.Unlock()
			return Operation{}, err
		}
		target = joinPath(parent.path, op.req.Name)
	} else {
		row, err := s.liveRow(op.folder, op.req.ItemID)
		if err == nil && row.kind != protocol.FileInfoTypeFile {
			err = newAPIError(http.StatusConflict, codeInvalidRequest, "only files can be replaced")
		}
		if err != nil {
			lock.Unlock()
			return Operation{}, err
		}
		target, base = row.path, op.req.BaseRevision
	}
	lock.Unlock()
	if op.data.Target == "" {
		op.data.Target = target
	}
	if IsSyncthingPrivatePath(target, info.MarkerName) {
		return Operation{}, reservedNameError()
	}
	// A run that stopped while putting the file in place may have done so: the target is then the staged file
	if op.journal == journalPublishing && isEntry(root, op.data.Target, op.data.Device, op.data.Inode) {
		op.journal = journalPublished
	}
	// Nothing to stage while this device doesn't have the base version yet (checked again in the folder loop)
	if (op.journal == journalIntent || op.journal == journalStaged) && op.kind == opReplace && !op.data.Conflict &&
		s.awaitingBase(op.folder, target, base) {
		return Operation{}, errWaiting
	}

	// Staging copies the upload next to the target: it can take long, so it happens outside the folder lock
	var blocks []protocol.BlockInfo
	if op.journal == journalIntent || op.journal == journalStaged || op.journal == journalPublishing {
		blocks, err = s.stage(ctx, op, root, parentPath(target))
		if err != nil {
			return Operation{}, err
		}
	}

	lock.Lock()
	defer lock.Unlock()
	var displaced string
	err = s.backend.RunInFolderLoop(ctx, op.folder, func(ctx context.Context, h syncthing.FolderHandle) error {
		done, err := s.beginPublish()
		if err != nil {
			return err
		}
		defer done()
		displaced, err = s.publishFile(ctx, op, h, info, root, base, blocks)
		return err
	})
	if errors.Is(err, errRestage) {
		op.journal = journalIntent
		return Operation{}, errWaiting
	}
	if err != nil {
		return Operation{}, err
	}
	if displaced != "" {
		if parent, err := root.openDir(parentPath(displaced)); err == nil {
			_ = parent.remove(baseName(displaced))
			parent.close()
		}
	}
	if err := s.reconcileLocked(ctx, op.folder); err != nil {
		return Operation{}, err
	}
	return s.fileResult(op, target)
}

// isEntry reports whether the entry at p (folder-relative) has the given identity
func isEntry(root *dir, p string, device, inode uint64) bool {
	pdir, err := root.openDir(parentPath(p))
	if err != nil {
		return false
	}
	defer pdir.close()
	st, err := pdir.lstat(baseName(p))
	return err == nil && st.exists && st.id.dev == device && st.id.ino == inode
}

// awaitingBase reports whether a replacement waits for this device to have its base version
func (s *Service) awaitingBase(folder, target, base string) bool {
	global, ok, err := s.backend.GlobalFile(folder, target)
	if err != nil || !ok || global.Deleted || global.IsInvalid() || revisionOf(global) != base {
		return false
	}
	local, ok, err := s.backend.LocalFile(folder, target)
	return err == nil && (!ok || !local.Version.Equal(global.Version))
}

// stage copies what the operation writes (its upload, or the version it names) into a temporary file in the target's
// directory, hashing it into blocks and checking it against the upload's digest or the version's size, and returns the
// blocks
func (s *Service) stage(ctx context.Context, op *operationRecord, root *dir, parent string) ([]protocol.BlockInfo, error) {
	pdir, err := root.openDir(parent)
	if err != nil {
		return nil, openDirError(err, newAPIError(http.StatusNotFound, codeNotFound, "the parent directory no longer exists"))
	}
	defer pdir.close()
	temp := tempNameFor(op)

	if op.journal == journalStaged || op.journal == journalPublishing {
		// Staged before a restart or while waiting: use it if it is still there and unchanged
		if st, err := pdir.lstat(temp); err == nil && st.exists && st.id.dev == op.data.Device && st.id.ino == op.data.Inode {
			f, err := pdir.openFile(temp)
			if err == nil {
				defer f.Close()
				blocks, _, err := hashBlocks(ctx, f, s.blockSizeFor(op, st.size))
				if err == nil && bytes.Equal(protocol.BlocksHash(blocks), op.data.BlocksHash) {
					return blocks, nil
				}
			}
		}
		_ = pdir.remove(temp)
	}

	src, size, err := s.stagingSource(op)
	if err != nil {
		return nil, err
	}
	defer src.Close()
	_ = pdir.remove(temp)
	dst, err := pdir.createExclusive(temp, 0o644)
	if err != nil {
		return nil, err
	}
	ok := false
	defer func() {
		if !ok {
			dst.Close()
			_ = pdir.remove(temp)
		}
	}()
	blocks, digest, err := hashBlocks(ctx, io.TeeReader(src, dst), s.blockSizeFor(op, size))
	if err != nil {
		return nil, err
	}
	if op.req.SourceVersion != "" {
		var copied int64
		for _, b := range blocks {
			copied += int64(b.Size)
		}
		if copied != size {
			return nil, errVersionNotFound
		}
	} else if want, _ := parseDigest(op.req.UploadDigest); !bytes.Equal(digest, want) {
		return nil, newAPIError(http.StatusConflict, codeUploadMismatch, "the upload does not match uploadDigest")
	}
	if err := dst.Sync(); err != nil {
		return nil, err
	}
	if err := dst.Close(); err != nil {
		return nil, err
	}
	ok = true
	st, err := pdir.lstat(temp)
	if err != nil || !st.exists {
		return nil, errRestage
	}
	op.data.Temp = joinPath(parent, temp)
	op.data.BlocksHash = protocol.BlocksHash(blocks)
	op.data.Device, op.data.Inode = st.id.dev, st.id.ino
	if err := s.saveJournal(op, journalStaged); err != nil {
		return nil, err
	}
	return blocks, nil
}

// stagingSource opens what an operation writes, the version it names or its upload, and returns its size
func (s *Service) stagingSource(op *operationRecord) (io.ReadCloser, int64, error) {
	if op.req.SourceVersion != "" {
		return s.openVersionSource(op)
	}
	src, err := os.Open(s.uploads.path(op.req.Upload))
	if err != nil {
		return nil, 0, newAPIError(http.StatusConflict, codeUploadMismatch, "the upload is gone")
	}
	stat, err := src.Stat()
	if err != nil {
		src.Close()
		return nil, 0, err
	}
	return src, stat.Size(), nil
}

// blockSizeFor picks the block size a scan would use for the new content of the target
func (s *Service) blockSizeFor(op *operationRecord, size int64) int {
	cur, ok, err := s.backend.LocalFile(op.folder, op.data.Target)
	if err != nil || !ok || cur.Deleted {
		return scanner.BlockSizeFor(size, protocol.FileInfo{}, false)
	}
	return scanner.BlockSizeFor(size, cur, true)
}

// hashBlocks reads r into Syncthing blocks of the given size, and returns them with the SHA-256 of all of it
func hashBlocks(ctx context.Context, r io.Reader, blockSize int) ([]protocol.BlockInfo, []byte, error) {
	whole := sha256.New()
	blocks, err := scanner.Blocks(ctx, io.TeeReader(r, whole), blockSize, -1, nil)
	if err != nil {
		return nil, nil, err
	}
	return blocks, whole.Sum(nil), nil
}

// publishFile runs in the folder loop: it checks the preconditions, puts the staged file in place and indexes it.
// It returns the path of displaced content to remove once the operation is done.
func (s *Service) publishFile(ctx context.Context, op *operationRecord, h syncthing.FolderHandle, info FolderInfo,
	root *dir, base string, blocks []protocol.BlockInfo,
) (string, error) {
	target := op.data.Target
	pdir, err := root.openDir(parentPath(op.data.Temp))
	if err != nil {
		return "", openDirError(err, newAPIError(http.StatusNotFound, codeNotFound, "the parent directory no longer exists"))
	}
	defer pdir.close()
	temp := baseName(op.data.Temp)

	if op.journal == journalPublished || op.journal == journalIndexed {
		return op.data.Displaced, s.indexPublished(ctx, op, h, root)
	}

	st, err := pdir.lstat(temp)
	if err != nil || !st.exists || st.id.dev != op.data.Device || st.id.ino != op.data.Inode {
		return "", errRestage
	}
	if op.journal == journalPublishing && op.data.Method == methodArchive {
		// A run that stopped after moving the replaced file to the versions or the Trash finishes putting the edit in
		// its place, whatever the index says about the target meanwhile
		if tst, err := pdir.lstat(baseName(target)); err == nil && !tst.exists {
			return "", s.putInPlace(ctx, op, h, pdir, temp, blocks)
		}
	}
	if h.IsIgnored(target) {
		_ = pdir.remove(temp)
		return "", ignoredPath()
	}

	replace := op.kind == opReplace && !op.data.Conflict
	// Whether the edit writes the contents the file has (a phone saving a file it only opened, for instance)
	var unchanged bool
	if replace {
		global, live, err := liveEntry(h, target)
		if err != nil {
			return "", err
		}
		conflict := !live || revisionOf(global) != base
		var local protocol.FileInfo
		if !conflict {
			var ok bool
			local, ok, err = h.CurrentFile(target)
			if err != nil {
				return "", err
			}
			if !ok || !local.Version.Equal(global.Version) {
				// This device doesn't have the base version yet
				return "", errWaiting
			}
			tst, err := pdir.lstat(baseName(target))
			if err != nil {
				return "", err
			}
			if !matchesIndex(tst, local, h.ModTimeWindow()) {
				// Changed on disk since the last scan: index that change first, which makes this edit a conflict
				if err := h.Scan(ctx, []string{target}); err != nil {
					return "", err
				}
				global, live, err = liveEntry(h, target)
				if err != nil {
					return "", err
				}
				conflict = true
			} else {
				// Swapping the edit in would lose the replaced file's extended attributes, which archiving keeps
				unchanged = bytes.Equal(blocksHashOf(local), op.data.BlocksHash) && !pdir.hasXattrs(baseName(target))
			}
		}
		if conflict {
			current := s.currentItem(op.folder, target)
			if live && current != nil {
				fresh := itemRow{id: current.ID, folder: op.folder, path: target}
				fresh.setFromFileInfo(global, info.IgnorePerms)
				item := fresh.toItem(true)
				current = &item
			}
			if op.req.OnConflict == onConflictReject {
				_ = pdir.remove(temp)
				return "", revisionMismatch(current)
			}
			op.data.Conflict = true
			replace = false
		}
	}
	if op.data.Conflict {
		if op.data.Original == "" {
			op.data.Original = target
		}
		name, err := freeConflictName(h, pdir, op.data.Original)
		if err != nil {
			return "", err
		}
		if h.IsIgnored(name) {
			_ = pdir.remove(temp)
			return "", ignoredPath()
		}
		target = name
		op.data.Target = name
	} else if !replace {
		if tst, err := pdir.lstat(baseName(target)); err != nil || tst.exists {
			_ = pdir.remove(temp)
			return "", nameConflict(s.currentItem(op.folder, target))
		}
		if _, live, err := liveEntry(h, target); err != nil || live {
			_ = pdir.remove(temp)
			return "", nameConflict(s.currentItem(op.folder, target))
		}
	}

	// The modification time is set once the file is in place: Syncthing removes temporary files that look old
	modTime := s.store.now()
	if op.req.ModifiedNs != nil {
		modTime = time.Unix(0, *op.req.ModifiedNs)
	} else if op.req.SourceVersion != "" {
		// A version keeps its modification time, as when Syncthing restores it
		modTime = time.Unix(0, op.data.SourceModNs)
	}
	op.data.ModNs = modTime.UnixNano()
	if replace && !info.IgnorePerms {
		// Keep the replaced file's permissions, which Syncthing synchronizes
		if local, ok, err := h.CurrentFile(target); err == nil && ok && !local.Deleted {
			if err := pdir.chmod(temp, local.Permissions&0o777); err != nil {
				return "", err
			}
		}
	}

	op.data.Method, op.data.Displaced = methodRename, ""
	if replace {
		// Unchanged contents aren't archived: the versions would fill up with copies of the file as it is
		if (h.HasVersioner() || info.FilesystemType == "basic-trash") && !unchanged {
			op.data.Method = methodArchive
		} else {
			// Swapped, so the replaced content stays until the edit is indexed
			op.data.Method, op.data.Displaced = methodSwap, op.data.Temp
		}
	}
	if err := s.saveJournal(op, journalPublishing); err != nil {
		return "", err
	}
	return op.data.Displaced, s.putInPlace(ctx, op, h, pdir, temp, blocks)
}

// putInPlace moves the staged file to the operation's target by its method, then indexes it
func (s *Service) putInPlace(ctx context.Context, op *operationRecord, h syncthing.FolderHandle, pdir *dir, temp string,
	blocks []protocol.BlockInfo,
) error {
	target := op.data.Target
	name := baseName(target)
	var err error
	switch op.data.Method {
	case methodArchive:
		if tst, err := pdir.lstat(name); err != nil {
			return err
		} else if tst.exists {
			if h.HasVersioner() {
				err = h.Archive(target)
			} else {
				err = h.Filesystem().Remove(target)
			}
			if err != nil && !fs.IsNotExist(err) {
				return err
			}
		}
		err = renameNoReplace(pdir, temp, pdir, name)
	case methodSwap:
		err = renameExchange(pdir, temp, pdir, name)
		if errors.Is(err, errExchangeUnsupported) {
			err = unix.Renameat(pdir.fd, temp, pdir.fd, name)
		}
	default:
		err = renameNoReplace(pdir, temp, pdir, name)
	}
	if err != nil {
		if isExists(err) {
			_ = pdir.remove(temp)
			return nameConflict(s.currentItem(op.folder, target))
		}
		return err
	}
	if err := s.setPublishedTime(op, pdir, name); err != nil {
		return err
	}
	if err := pdir.sync(); err != nil {
		return err
	}
	if err := s.saveJournal(op, journalPublished); err != nil {
		return err
	}
	return s.indexFile(ctx, op, h, pdir, target, blocks)
}

// setPublishedTime gives the published file its modification time. Operations journaled before it was recorded set it
// before publishing.
func (s *Service) setPublishedTime(op *operationRecord, pdir *dir, name string) error {
	if op.data.ModNs == 0 {
		return nil
	}
	return pdir.chtimes(name, time.Unix(0, op.data.ModNs))
}

// freeConflictName returns a conflict copy name for target, named as Syncthing names them, that is free
func freeConflictName(h syncthing.FolderHandle, pdir *dir, target string) (string, error) {
	ext := path.Ext(target)
	stem := target[:len(target)-len(ext)]
	stamp := time.Now().Format(".sync-conflict-20060102-150405-") + h.ShortID().String()
	// Shorten the stem (on a character boundary) when the name would be too long for the filesystem
	for len(baseName(stem))+len(stamp)+len(ext)+4 > maxNameBytes && len(baseName(stem)) > 0 {
		_, size := utf8.DecodeLastRuneInString(stem)
		stem = stem[:len(stem)-size]
	}
	for i := 0; i < 100; i++ {
		name := stem + stamp + ext
		if i > 0 {
			name = fmt.Sprintf("%s%s-%d%s", stem, stamp, i, ext)
		}
		st, err := pdir.lstat(baseName(name))
		if err != nil {
			return "", err
		}
		if _, live, err := liveEntry(h, name); err == nil && !live && !st.exists {
			return name, nil
		}
	}
	return "", newAPIError(http.StatusConflict, codeNameConflict, "no free conflict copy name")
}

// indexFile commits the published file's index entry (with blocks, when known, else hashing it), checking that the
// file on disk is still the staged one
func (s *Service) indexFile(ctx context.Context, op *operationRecord, h syncthing.FolderHandle, pdir *dir, target string,
	blocks []protocol.BlockInfo,
) error {
	st, err := pdir.lstat(baseName(target))
	if err != nil {
		return err
	}
	if !st.exists || st.id.dev != op.data.Device || st.id.ino != op.data.Inode {
		return s.keepAsConflict(ctx, op, h, target)
	}
	fi, err := scanner.FileInfoFor(ctx, h.ScanConfig(), target, blocks)
	if err != nil {
		return err
	}
	if !bytes.Equal(fi.BlocksHash, op.data.BlocksHash) {
		return s.keepAsConflict(ctx, op, h, target)
	}
	if err := h.CommitLocal([]protocol.FileInfo{fi}); err != nil {
		return err
	}
	return s.saveJournal(op, journalIndexed)
}

// keepAsConflict handles a published edit that something replaced or changed before it was indexed: what is there
// is indexed, and the edit is put in place again from its upload, as a conflict copy
func (s *Service) keepAsConflict(ctx context.Context, op *operationRecord, h syncthing.FolderHandle, target string) error {
	_ = h.Scan(ctx, []string{target})
	if op.data.Original == "" {
		op.data.Original = target
	}
	op.data.Conflict = true
	op.data.Method, op.data.Displaced = "", ""
	if err := s.saveJournal(op, journalIntent); err != nil {
		return err
	}
	return errWaiting
}

// indexPublished finishes an operation whose file was published before a restart: Syncthing's index decides
// whether it was indexed
func (s *Service) indexPublished(ctx context.Context, op *operationRecord, h syncthing.FolderHandle, root *dir) error {
	target := op.data.Target
	local, ok, err := h.CurrentFile(target)
	if err != nil {
		return err
	}
	if ok && !local.Deleted && bytes.Equal(blocksHashOf(local), op.data.BlocksHash) {
		return s.saveJournal(op, journalIndexed)
	}
	pdir, err := root.openDir(parentPath(target))
	if err != nil {
		return err
	}
	defer pdir.close()
	st, err := pdir.lstat(baseName(target))
	if err != nil {
		return err
	}
	if !st.exists || st.id.dev != op.data.Device || st.id.ino != op.data.Inode {
		// The published file was replaced or removed before it was indexed
		return s.keepAsConflict(ctx, op, h, target)
	}
	// A run that stopped right after publishing may not have set it
	if err := s.setPublishedTime(op, pdir, baseName(target)); err != nil {
		return err
	}
	return s.indexFile(ctx, op, h, pdir, target, nil)
}

func (s *Service) fileResult(op *operationRecord, target string) (Operation, error) {
	row, ok, err := liveItemByPath(s.store.db, op.folder, op.data.Target)
	if err != nil {
		return Operation{}, err
	}
	if !ok {
		return Operation{}, fmt.Errorf("published item %q is not in the change log", op.data.Target)
	}
	item := row.toItem(true)
	result := Operation{ID: op.clientOp, Operation: op.id, Kind: op.kind, State: stateResultCommitted, Item: &item,
		ContentID: row.contentID, AuthorityGeneration: op.generation}
	if op.data.Conflict {
		result.State = stateResultConflict
		result.ConflictItem = &item
		result.Item = s.currentItem(op.folder, target)
	}
	return result, nil
}

// Directories

func (s *Service) executeCreateDirectory(ctx context.Context, op *operationRecord) (Operation, error) {
	info, root, err := s.folderRoot(op.folder)
	if err != nil {
		return Operation{}, err
	}
	defer root.close()
	lock := s.folderLock(op.folder)
	lock.Lock()
	defer lock.Unlock()
	parent, err := s.liveRow(op.folder, op.req.ParentID)
	if err != nil {
		return Operation{}, err
	}
	if parent.kind != protocol.FileInfoTypeDirectory {
		return Operation{}, newAPIError(http.StatusConflict, codeNameConflict, "the parent is not a directory")
	}
	target := joinPath(parent.path, op.req.Name)
	if IsSyncthingPrivatePath(target, info.MarkerName) {
		return Operation{}, reservedNameError()
	}
	op.data.Target = target
	err = s.backend.RunInFolderLoop(ctx, op.folder, func(ctx context.Context, h syncthing.FolderHandle) error {
		done, err := s.beginPublish()
		if err != nil {
			return err
		}
		defer done()
		pdir, err := root.openDir(parent.path)
		if err != nil {
			return openDirError(err, newAPIError(http.StatusNotFound, codeNotFound, "the parent directory no longer exists"))
		}
		defer pdir.close()
		if op.journal == journalPublishing {
			// A run that stopped after creating the directory finishes indexing it
			if st, err := pdir.lstat(op.req.Name); err == nil && st.exists && st.kind == unix.S_IFDIR {
				op.journal = journalPublished
			}
		}
		if op.journal != journalPublished && op.journal != journalIndexed {
			if h.IsIgnored(target) {
				return ignoredPath()
			}
			if _, live, err := liveEntry(h, target); err != nil || live {
				return nameConflict(s.currentItem(op.folder, target))
			}
			op.data.Method = methodMkdir
			if err := s.saveJournal(op, journalPublishing); err != nil {
				return err
			}
			if err := pdir.mkdir(op.req.Name, 0o755); err != nil {
				if isExists(err) {
					return nameConflict(s.currentItem(op.folder, target))
				}
				return err
			}
			if err := pdir.sync(); err != nil {
				return err
			}
			if err := s.saveJournal(op, journalPublished); err != nil {
				return err
			}
		}
		if local, ok, err := h.CurrentFile(target); err == nil && ok && !local.Deleted && local.IsDirectory() {
			return s.saveJournal(op, journalIndexed)
		}
		fi, err := scanner.FileInfoFor(ctx, h.ScanConfig(), target, nil)
		if err != nil {
			return err
		}
		if !fi.IsDirectory() {
			return nameConflict(s.currentItem(op.folder, target))
		}
		if err := h.CommitLocal([]protocol.FileInfo{fi}); err != nil {
			return err
		}
		return s.saveJournal(op, journalIndexed)
	})
	if err != nil {
		return Operation{}, err
	}
	if err := s.reconcileLocked(ctx, op.folder); err != nil {
		return Operation{}, err
	}
	return s.fileResult(op, target)
}

// subtreeEntry is an item below (or at) the root of a directory operation, with its local index entry
type subtreeEntry struct {
	path  string
	local protocol.FileInfo
}

// collectSubtree returns the local index entries of an item and everything below it, deepest first, after checking
// that disk matches them and that nothing untracked is in the way. A mismatch is scanned and reported as a changed
// item.
func (s *Service) collectSubtree(ctx context.Context, h syncthing.FolderHandle, root *dir, row itemRow, current *Item) ([]subtreeEntry, error) {
	lower, upper := descendantRange(row.path)
	rows, err := queryItems(s.store.db, `SELECT `+itemColumns+` FROM items WHERE folder_id = ? AND deleted = 0
		AND path > ? AND path < ?`, row.folder, lower, upper)
	if err != nil {
		return nil, err
	}
	rows = append(rows, row)
	known := make(map[string]struct{}, len(rows))
	entries := make([]subtreeEntry, 0, len(rows))
	for _, r := range rows {
		local, ok, err := h.CurrentFile(r.path)
		if err != nil {
			return nil, err
		}
		global, live, err := liveEntry(h, r.path)
		if err != nil {
			return nil, err
		}
		if live && revisionOf(global) != r.revision {
			// The change log is behind the index: the client hasn't seen this version, which the subtree generation
			// it sent cannot account for
			return nil, revisionMismatch(current)
		}
		if !ok || local.Deleted || !live || !local.Version.Equal(global.Version) {
			if live && (!ok || !local.Version.Equal(global.Version)) {
				return nil, errWaiting
			}
			continue
		}
		pdir, err := root.openDir(parentPath(r.path))
		if err != nil {
			return nil, revisionMismatch(current)
		}
		st, err := pdir.lstat(baseName(r.path))
		pdir.close()
		if err != nil {
			return nil, err
		}
		if !matchesIndex(st, local, h.ModTimeWindow()) {
			_ = h.Scan(ctx, []string{row.path})
			return nil, revisionMismatch(current)
		}
		known[r.path] = struct{}{}
		entries = append(entries, subtreeEntry{path: r.path, local: local})
	}
	// Anything on disk the index doesn't know (other than temporary files) would be lost or block the operation
	if row.kind == protocol.FileInfoTypeDirectory {
		if err := checkUntracked(h, row.path, known); err != nil {
			return nil, err
		}
	}
	sort.Slice(entries, func(i, j int) bool {
		return strings.Count(entries[i].path, "/") > strings.Count(entries[j].path, "/") ||
			strings.Count(entries[i].path, "/") == strings.Count(entries[j].path, "/") && entries[i].path > entries[j].path
	})
	return entries, nil
}

func checkUntracked(h syncthing.FolderHandle, p string, known map[string]struct{}) error {
	names, err := h.Filesystem().DirNames(p)
	if err != nil {
		return err
	}
	for _, name := range names {
		child := joinPath(p, osutil.NormalizedFilename(name))
		if fs.IsTemporary(child) {
			continue
		}
		if _, ok := known[child]; !ok {
			return newAPIError(http.StatusConflict, codeNotEmpty, "the directory holds items the operation would affect that are not synchronized")
		}
		if info, err := h.Filesystem().Lstat(child); err == nil && info.IsDir() {
			if err := checkUntracked(h, child, known); err != nil {
				return err
			}
		}
	}
	return nil
}

func (s *Service) executeDelete(ctx context.Context, op *operationRecord) (Operation, error) {
	info, root, err := s.folderRoot(op.folder)
	if err != nil {
		return Operation{}, err
	}
	defer root.close()
	lock := s.folderLock(op.folder)
	lock.Lock()
	defer lock.Unlock()

	row, ok, err := itemByID(s.store.db, op.req.ItemID)
	if err != nil {
		return Operation{}, err
	}
	if !ok || row.folder != op.folder {
		return Operation{}, newAPIError(http.StatusNotFound, codeNotFound, "no such item")
	}
	if row.deleted {
		// Already gone: the intended result holds
		return s.deleteResult(op, row)
	}
	if row.path == "" {
		return Operation{}, newAPIError(http.StatusForbidden, codeForbidden, "a folder's root cannot be deleted")
	}
	current := row.toItem(true)
	// A run that stopped while removing items finishes removing them; the preconditions held when it started
	resuming := op.journal == journalPublishing
	if !resuming && row.kind == protocol.FileInfoTypeDirectory &&
		(op.req.SubtreeGeneration == nil || *op.req.SubtreeGeneration != row.subtreeGen) {
		return Operation{}, revisionMismatch(&current)
	}

	err = s.backend.RunInFolderLoop(ctx, op.folder, func(ctx context.Context, h syncthing.FolderHandle) error {
		done, err := s.beginPublish()
		if err != nil {
			return err
		}
		defer done()
		global, live, err := liveEntry(h, row.path)
		if err != nil {
			return err
		}
		if !live {
			return nil
		}
		if !resuming && revisionOf(global) != op.req.BaseRevision {
			fresh := row
			fresh.setFromFileInfo(global, info.IgnorePerms)
			item := fresh.toItem(true)
			return revisionMismatch(&item)
		}
		entries, err := s.collectSubtree(ctx, h, root, row, &current)
		if err != nil {
			return err
		}
		op.data.Method = methodDelete
		if err := s.saveJournal(op, journalPublishing); err != nil {
			return err
		}
		deletions := make([]protocol.FileInfo, 0, len(entries))
		for _, e := range entries {
			if e.local.IsDirectory() {
				if err := h.Filesystem().Remove(e.path); err != nil && !fs.IsNotExist(err) {
					break
				}
			} else if h.HasVersioner() {
				if err := h.Archive(e.path); err != nil {
					break
				}
			} else if err := h.Filesystem().Remove(e.path); err != nil && !fs.IsNotExist(err) {
				break
			}
			deletions = append(deletions, h.DeletionOf(e.local))
		}
		if err := h.CommitLocal(deletions); err != nil {
			return err
		}
		if len(deletions) < len(entries) {
			// Index what was removed; what remains is reported
			return newAPIError(http.StatusConflict, codeNotEmpty, "some items could not be removed")
		}
		return s.saveJournal(op, journalIndexed)
	})
	if err != nil {
		return Operation{}, err
	}
	if err := s.reconcileLocked(ctx, op.folder); err != nil {
		return Operation{}, err
	}
	row, _, err = itemByID(s.store.db, op.req.ItemID)
	if err != nil {
		return Operation{}, err
	}
	return s.deleteResult(op, row)
}

func (s *Service) deleteResult(op *operationRecord, row itemRow) (Operation, error) {
	item := row.toItem(true)
	return Operation{ID: op.clientOp, Operation: op.id, Kind: op.kind, State: stateResultCommitted, Item: &item,
		AuthorityGeneration: op.generation}, nil
}

// Moves

func (s *Service) executeMove(ctx context.Context, op *operationRecord) (Operation, error) {
	destFolder := op.folder
	if op.req.DestinationFolderID != "" {
		destFolder = op.req.DestinationFolderID
	}
	info, root, err := s.folderRoot(op.folder)
	if err != nil {
		return Operation{}, err
	}
	defer root.close()
	destInfo, destRoot := info, root
	if destFolder != op.folder {
		if destInfo, destRoot, err = s.folderRoot(destFolder); err != nil {
			return Operation{}, err
		}
		defer destRoot.close()
	}

	// Lock both folders in a fixed order
	folders := []string{op.folder}
	if destFolder != op.folder {
		folders = append(folders, destFolder)
		slices.Sort(folders)
	}
	for _, f := range folders {
		lock := s.folderLock(f)
		lock.Lock()
		defer lock.Unlock()
	}

	// A run that stopped part way may have moved the item already, and the reconciler may have marked it deleted at
	// its old path meanwhile
	recovering := op.journal == journalPublishing || op.journal == journalPublished || op.journal == journalIndexed
	row, ok, err := itemByID(s.store.db, op.req.ItemID)
	if err != nil {
		return Operation{}, err
	}
	if !ok || row.deleted && !recovering || row.folder != op.folder && !(recovering && row.folder == destFolder) {
		return Operation{}, newAPIError(http.StatusNotFound, codeNotFound, "no such item")
	}
	if row.path == "" {
		return Operation{}, newAPIError(http.StatusForbidden, codeForbidden, "a folder's root cannot be moved")
	}
	parent, err := s.liveRow(destFolder, op.req.ParentID)
	if err != nil {
		return Operation{}, err
	}
	if parent.kind != protocol.FileInfoTypeDirectory {
		return Operation{}, newAPIError(http.StatusConflict, codeNameConflict, "the destination is not a directory")
	}
	dest := joinPath(parent.path, op.req.Name)
	if IsSyncthingPrivatePath(dest, destInfo.MarkerName) {
		return Operation{}, reservedNameError()
	}
	if recovering && row.folder == destFolder && row.path == dest && !row.deleted {
		// Moved and recorded; only the result is missing
		return s.moveResult(op, row)
	}
	if op.journal == journalPublishing {
		switch {
		case isEntry(destRoot, dest, op.data.Device, op.data.Inode):
			op.journal = journalPublished
		case isEntry(root, row.path, op.data.Device, op.data.Inode):
			// The rename didn't happen: start over, checking the preconditions again
			op.journal = journalIntent
		default:
			current := row.toItem(true)
			return Operation{}, revisionMismatch(&current)
		}
	}
	if destFolder == op.folder && (dest == row.path || isDescendantPath(dest, row.path)) {
		if dest == row.path {
			return s.moveResult(op, row)
		}
		return Operation{}, newAPIError(http.StatusConflict, codeInvalidRequest, "a directory cannot move into itself")
	}
	current := row.toItem(true)
	if op.journal == journalIntent && row.kind == protocol.FileInfoTypeDirectory &&
		(op.req.SubtreeGeneration != nil && *op.req.SubtreeGeneration != row.subtreeGen) {
		return Operation{}, revisionMismatch(&current)
	}

	var entries []subtreeEntry
	commitDest := func(ctx context.Context, h syncthing.FolderHandle) error {
		if op.journal != journalPublished {
			// The folder's ignore patterns apply to everything that moves, not only to the item itself
			if h.IsIgnored(dest) {
				return ignoredPath()
			}
			for _, e := range entries {
				if h.IsIgnored(dest + strings.TrimPrefix(e.path, row.path)) {
					return ignoredPath()
				}
			}
			if _, live, err := liveEntry(h, dest); err != nil || live {
				return nameConflict(s.currentItem(destFolder, dest))
			}
			srcDir, err := root.openDir(parentPath(row.path))
			if err != nil {
				return openDirError(err, revisionMismatch(&current))
			}
			defer srcDir.close()
			dstDir, err := destRoot.openDir(parent.path)
			if err != nil {
				return openDirError(err, newAPIError(http.StatusNotFound, codeNotFound, "the destination no longer exists"))
			}
			defer dstDir.close()
			// Recorded with the moved entry's identity, so that a stop at any point is recognized afterwards
			st, err := srcDir.lstat(baseName(row.path))
			if err != nil {
				return err
			}
			if !st.exists {
				return revisionMismatch(&current)
			}
			op.data.Method, op.data.Device, op.data.Inode = methodMove, st.id.dev, st.id.ino
			if err := s.saveJournal(op, journalPublishing); err != nil {
				return err
			}
			if err := renameNoReplace(srcDir, baseName(row.path), dstDir, op.req.Name); err != nil {
				switch {
				case isExists(err):
					return nameConflict(s.currentItem(destFolder, dest))
				case errors.Is(err, unix.EXDEV):
					return newAPIError(http.StatusConflict, codeCrossFolder,
						"the folders are on different filesystems; copy the item and delete the original instead")
				}
				return err
			}
			if err := dstDir.sync(); err != nil {
				return err
			}
			if destFolder != op.folder || parentPath(row.path) != parent.path {
				if err := srcDir.sync(); err != nil {
					return err
				}
			}
			if err := s.saveJournal(op, journalPublished); err != nil {
				return err
			}
		}
		// Index the new paths, keeping each file's block list
		added := make([]protocol.FileInfo, 0, len(entries))
		cfg := h.ScanConfig()
		for _, e := range entries {
			newPath := dest + strings.TrimPrefix(e.path, row.path)
			if local, ok, err := h.CurrentFile(newPath); err == nil && ok && !local.Deleted &&
				(local.IsDirectory() || bytes.Equal(blocksHashOf(local), blocksHashOf(e.local))) {
				continue
			}
			var blocks []protocol.BlockInfo
			if !e.local.IsDirectory() {
				blocks = e.local.Blocks
			}
			fi, err := scanner.FileInfoFor(ctx, cfg, newPath, blocks)
			if err != nil {
				return err
			}
			added = append(added, fi)
		}
		return h.CommitLocal(added)
	}
	removeSource := func(_ context.Context, h syncthing.FolderHandle) error {
		deletions := make([]protocol.FileInfo, 0, len(entries))
		for _, e := range entries {
			if local, ok, err := h.CurrentFile(e.path); err == nil && ok && !local.Deleted {
				deletions = append(deletions, h.DeletionOf(local))
			}
		}
		return h.CommitLocal(deletions)
	}

	// Once indexed, only the item rows are left to update
	moveFiles := func(ctx context.Context, h syncthing.FolderHandle) error {
		done, err := s.beginPublish()
		if err != nil {
			return err
		}
		defer done()
		if op.journal != journalPublished {
			global, live, err := liveEntry(h, row.path)
			if err != nil {
				return err
			}
			if !live || revisionOf(global) != op.req.BaseRevision {
				if live {
					fresh := row
					fresh.setFromFileInfo(global, info.IgnorePerms)
					item := fresh.toItem(true)
					return revisionMismatch(&item)
				}
				return revisionMismatch(&current)
			}
		}
		if op.journal == journalPublished {
			entries, err = s.publishedSubtree(h, row)
		} else {
			entries, err = s.collectSubtree(ctx, h, root, row, &current)
		}
		if err != nil {
			return err
		}
		// Parents before children when indexing new paths
		slices.SortFunc(entries, func(a, b subtreeEntry) int { return strings.Compare(a.path, b.path) })
		if destFolder == op.folder {
			if err := commitDest(ctx, h); err != nil {
				return err
			}
		} else if err := s.backend.RunInFolderLoop(ctx, destFolder, commitDest); err != nil {
			return err
		}
		if err := removeSource(ctx, h); err != nil {
			return err
		}
		return s.saveJournal(op, journalIndexed)
	}
	if op.journal != journalIndexed {
		if err := s.backend.RunInFolderLoop(ctx, op.folder, moveFiles); err != nil {
			return Operation{}, err
		}
	}

	// The item keeps its identity at its new path
	err = s.store.tx(ctx, func(tx *sql.Tx) error {
		if stale, ok, err := liveItemByPath(tx, destFolder, dest); err != nil {
			return err
		} else if ok && stale.id != row.id {
			if err := s.tombstone(tx, stale); err != nil {
				return err
			}
		}
		lower, upper := descendantRange(row.path)
		descendants, err := queryItems(tx, `SELECT `+itemColumns+` FROM items WHERE folder_id = ? AND deleted = 0
			AND path > ? AND path < ?`, op.folder, lower, upper)
		if err != nil {
			return err
		}
		moved := row
		moved.folder, moved.path = destFolder, dest
		moved.parent = sql.NullString{String: parent.id, Valid: true}
		if err := s.moveRow(tx, row, moved); err != nil {
			return err
		}
		for _, d := range descendants {
			to := d
			to.folder, to.path = destFolder, dest+strings.TrimPrefix(d.path, row.path)
			if err := s.moveRow(tx, d, to); err != nil {
				return err
			}
		}
		ancestors := map[string]struct{}{parent.id: {}}
		addAncestors(ancestors, row)
		return s.touchAncestors(tx, ancestors)
	})
	if err != nil {
		return Operation{}, err
	}
	for _, f := range folders {
		if err := s.reconcileLocked(ctx, f); err != nil {
			return Operation{}, err
		}
	}
	moved, _, err := itemByID(s.store.db, row.id)
	if err != nil {
		return Operation{}, err
	}
	return s.moveResult(op, moved)
}

// publishedSubtree lists the local entries a move interrupted after publication still has to index
func (s *Service) publishedSubtree(h syncthing.FolderHandle, row itemRow) ([]subtreeEntry, error) {
	lower, upper := descendantRange(row.path)
	rows, err := queryItems(s.store.db, `SELECT `+itemColumns+` FROM items WHERE folder_id = ? AND deleted = 0
		AND path > ? AND path < ?`, row.folder, lower, upper)
	if err != nil {
		return nil, err
	}
	rows = append(rows, row)
	var entries []subtreeEntry
	for _, r := range rows {
		if local, ok, err := h.CurrentFile(r.path); err == nil && ok && !local.Deleted {
			entries = append(entries, subtreeEntry{path: r.path, local: local})
		}
	}
	return entries, nil
}

// moveRow gives an item its new folder and path. Leaving a folder for another, it also leaves a tombstone in the
// change log of the folder it left, which names its new folder.
func (s *Service) moveRow(tx *sql.Tx, from, row itemRow) error {
	seq, err := nextChangeSeq(tx, 1)
	if err != nil {
		return err
	}
	if from.folder != row.folder {
		revision := revisionOf(protocol.FileInfo{Name: from.path, Type: from.kind, Deleted: true,
			Version: protocol.Vector{Counters: []protocol.Counter{{ID: protocol.ShortID(seq), Value: uint64(seq)}}}})
		if _, err := tx.Exec(`INSERT INTO moved_out (folder_id, item_id, path, parent_item, kind, to_folder, revision,
			change_seq, changed_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?) ON CONFLICT (folder_id, item_id) DO UPDATE SET
			path = excluded.path, parent_item = excluded.parent_item, kind = excluded.kind, to_folder = excluded.to_folder,
			revision = excluded.revision, change_seq = excluded.change_seq, changed_at = excluded.changed_at`,
			from.folder, from.id, from.path, from.parent, from.kind, row.folder, revision, seq,
			s.store.now().Unix()); err != nil {
			return err
		}
		// Back in a folder it left before: no longer gone from there
		if _, err := tx.Exec(`DELETE FROM moved_out WHERE folder_id = ? AND item_id = ?`, row.folder, row.id); err != nil {
			return err
		}
		if seq, err = nextChangeSeq(tx, 1); err != nil {
			return err
		}
	}
	// Live again if the reconciler saw the item leave its old path first; reconciling then brings its revision up to
	// date
	_, err = tx.Exec(`UPDATE items SET folder_id = ?, path = ?, parent_item = ?, deleted = 0, change_seq = ?,
		changed_at = ? WHERE item_id = ?`, row.folder, row.path, row.parent, seq, s.store.now().Unix(), row.id)
	return err
}

func (s *Service) moveResult(op *operationRecord, row itemRow) (Operation, error) {
	item := row.toItem(true)
	return Operation{ID: op.clientOp, Operation: op.id, Kind: op.kind, State: stateResultCommitted, Item: &item,
		ContentID: row.contentID, AuthorityGeneration: op.generation}, nil
}
