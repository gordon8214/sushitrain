// Copyright (C) 2026 Gordon
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this file,
// You can obtain one at https://mozilla.org/MPL/2.0/.

package gateway

import (
	"context"
	"database/sql"
	"errors"
	"maps"
	"net/http"
	"os"
	"slices"
	"sort"
	"strings"
	"time"
	"unicode"

	"github.com/syncthing/syncthing/lib/fs"
	"github.com/syncthing/syncthing/lib/osutil"
	"github.com/syncthing/syncthing/lib/protocol"
	"github.com/syncthing/syncthing/lib/scanner"
	"github.com/syncthing/syncthing/lib/syncthing"
	"golang.org/x/sys/unix"
)

// An on-demand folder is a selective folder clients may write to, although this device holds few of its files: it has
// the global index, and streams the rest from other devices. Every other folder clients write to is a complete replica,
// which the rest of the coordinator relies on, so operations go through what is here wherever a path may be missing.
//
// Three things differ from a complete replica, and getting them wrong shows on other devices:
//
//   - A new version must build on the global one. This device usually has no entry for a file it doesn't hold, or an
//     ignored one the puller wrote. A scan would start a new version vector from those, which is concurrent with what
//     others have: a conflict on every device. See mergedFile.
//   - What was written must be selected by the time the folder loop runs on, and stay so until others have it. A scan
//     marks an entry ignored if its path is, deletions included, and others then keep their old copy. Publishing,
//     committing and selecting therefore happen in one callback, in that order: selecting before the loop would let a
//     scan and a pull run on the path first (and download a whole directory only to delete it), and selecting before
//     committing would let a scan index the file with the wrong version, in a restart.
//   - h.IsIgnored can't tell: the selection's last pattern matches every path that isn't selected, and it shows the
//     patterns as they were when the callback started. See isExcluded.
//
// What was written doesn't stay selected for good, as .stignore and the folder would go on growing, and a line for a
// path takes in what other devices do there. The service records what it selected, and deselects it once it no longer
// has to announce it. See deselectWritten.

// How often the service looks for what it can deselect in on-demand folders
var deselectInterval = time.Minute

// How many of a directory's paths have to be selected on their own before the directory can take over from them
const minGroupedPaths = 2

// How much a move of a directory may have to download. Moving contents takes having them: an entry at the new path
// without a file behind it would be announced as something this device can't provide, and turn into a deletion.
var onDemandMoveLimit int64 = 4 << 30

func tooLargeToMove() error {
	return newAPIError(http.StatusConflict, codeMoveTooLarge,
		"moving this directory would download too much: keep it on this device first, or copy it elsewhere")
}

// A path as this device has it, in an on-demand folder
type pathState int

const (
	// This device has the global version, as in a complete replica
	pathHeld pathState = iota
	// This device has an older version, which the puller replaces
	pathStale
	// Nothing here: the normal state of a file that isn't selected
	pathAbsent
	// On disk, but not in the index: it may be the only copy of something
	pathUnindexed
)

// isIndexed reports whether a local entry says this device has the item
func isIndexed(local protocol.FileInfo, ok bool) bool {
	return ok && !local.Deleted && !local.IsInvalid()
}

// entryAt returns what is at a folder-relative path, whose parent may not exist
func entryAt(root *dir, p string) (entryStat, error) {
	pdir, err := root.openDir(parentPath(p))
	if err != nil {
		if errors.Is(err, unix.ENOENT) || errors.Is(err, unix.ENOTDIR) || errors.Is(err, os.ErrNotExist) {
			return entryStat{}, nil
		}
		return entryStat{}, err
	}
	defer pdir.close()
	return pdir.lstat(baseName(p))
}

// classify tells how this device has a live file. It returns the local entry when it holds it.
func classify(h syncthing.FolderHandle, root *dir, name string, global protocol.FileInfo) (pathState, protocol.FileInfo, error) {
	local, ok, err := h.CurrentFile(name)
	if err != nil {
		return 0, protocol.FileInfo{}, err
	}
	if isIndexed(local, ok) {
		if local.Version.Equal(global.Version) {
			return pathHeld, local, nil
		}
		return pathStale, local, nil
	}
	st, err := entryAt(root, name)
	if err != nil {
		return 0, protocol.FileInfo{}, err
	}
	if st.exists {
		return pathUnindexed, local, nil
	}
	return pathAbsent, local, nil
}

// mergedFile returns the entry a change of name builds on: the global one, without what this device's own entry says
// about its state here, and with a version that has seen everything this device and others announced. A deleted global
// entry counts, so that a file created over it isn't older than the deletion.
func mergedFile(h syncthing.FolderHandle, name string) (protocol.FileInfo, bool, error) {
	global, hasGlobal, err := h.GlobalFile(name)
	if err != nil {
		return protocol.FileInfo{}, false, err
	}
	return mergedWith(h, name, global, hasGlobal)
}

// mergedWith is mergedFile for a global entry that was checked. Other devices' changes reach the index without going
// through the folder loop: reading it again could build on a version nobody looked at, and replace it without a
// conflict.
func mergedWith(h syncthing.FolderHandle, name string, global protocol.FileInfo, hasGlobal bool) (protocol.FileInfo, bool, error) {
	local, hasLocal, err := h.CurrentFile(name)
	if err != nil {
		return protocol.FileInfo{}, false, err
	}
	if !hasGlobal && !hasLocal {
		return protocol.FileInfo{}, false, nil
	}
	merged := global
	if !hasGlobal {
		merged = local
	}
	// Merge and Update change a vector in place, and the database's copy may be shared
	merged.Version = merged.Version.Copy()
	if hasGlobal && hasLocal {
		merged.Version = merged.Version.Merge(local.Version)
	}
	merged.LocalFlags = 0
	return merged, true, nil
}

// writeBase is what a write to an on-demand folder builds on. It is kept from when the operation's preconditions held
// until the file is indexed, which can be after a restart, so that a version that arrives in between stays concurrent
// with the write, and devices keep both.
type writeBase struct {
	Version    []protocol.Counter `json:"version,omitempty"`
	BlocksHash []byte             `json:"blocksHash,omitempty"`
}

func newWriteBase(file protocol.FileInfo, ok bool) *writeBase {
	if !ok {
		return &writeBase{}
	}
	return &writeBase{Version: file.Version.Counters, BlocksHash: file.BlocksHash}
}

// knownFile is a scanner.CurrentFiler for one file, whose current entry is known
type knownFile struct {
	file protocol.FileInfo
	ok   bool
}

func (k knownFile) CurrentFile(string) (protocol.FileInfo, bool) {
	return k.file, k.ok
}

// fileInfoFor builds the index entry of a published file or directory. In an on-demand folder it builds on base, if
// the operation has one.
func fileInfoFor(ctx context.Context, h syncthing.FolderHandle, info FolderInfo, name string,
	blocks []protocol.BlockInfo, base *writeBase,
) (protocol.FileInfo, error) {
	cfg := h.ScanConfig()
	if info.OnDemand {
		file, ok, err := mergedFile(h, name)
		if err != nil {
			return protocol.FileInfo{}, err
		}
		if base != nil {
			file.Version, file.BlocksHash, ok = protocol.Vector{Counters: base.Version}, base.BlocksHash, true
		}
		cfg.CurrentFiler = knownFile{file: file, ok: ok}
	}
	return scanner.FileInfoFor(ctx, cfg, name, blocks)
}

// ancestorEntries returns entries for the directories above name that this device has on disk but not in its index, at
// the version others know, parents first. A scan would index them too, but as new versions of what they are, from an
// ignored entry that dropped what others announced.
func ancestorEntries(h syncthing.FolderHandle, info FolderInfo, name string) ([]protocol.FileInfo, error) {
	if !info.OnDemand {
		return nil, nil
	}
	var entries []protocol.FileInfo
	for parent := parentPath(name); parent != ""; parent = parentPath(parent) {
		local, ok, err := h.CurrentFile(parent)
		if err != nil {
			return nil, err
		}
		if isIndexed(local, ok) {
			continue
		}
		global, live, err := liveEntry(h, parent)
		if err != nil {
			return nil, err
		}
		if live && global.IsDirectory() {
			entries = append([]protocol.FileInfo{global}, entries...)
		}
	}
	return entries, nil
}

// keepSelected makes an on-demand folder go on offering what was written at paths. Call it after committing them, in
// the same folder loop callback, and before recording that they are indexed.
func (s *Service) keepSelected(info FolderInfo, paths ...string) error {
	if !info.OnDemand {
		return nil
	}
	return s.backend.KeepSelected(info.ID, paths, func(added []string) error { return s.recordWritten(info.ID, added) })
}

// recordWritten notes paths the service selected. Only these are its to deselect: the others in .stignore are what the
// user selected, and what no other device had when the folder became on demand. Which version has to reach other
// devices is not part of it: that is what the local index says at the time, which a scan or a pull may have moved on.
func (s *Service) recordWritten(folder string, paths []string) error {
	return s.store.tx(context.Background(), func(tx *sql.Tx) error {
		for _, path := range paths {
			if _, err := tx.Exec(`INSERT OR IGNORE INTO written_paths (folder_id, path) VALUES (?, ?)`, folder, path); err != nil {
				return err
			}
		}
		return nil
	})
}

func (s *Service) forgetWritten(folder string, paths []string) error {
	return s.store.tx(context.Background(), func(tx *sql.Tx) error {
		for _, path := range paths {
			if _, err := tx.Exec(`DELETE FROM written_paths WHERE folder_id = ? AND path = ?`, folder, path); err != nil {
				return err
			}
		}
		return nil
	})
}

func (s *Service) writtenPaths(folder string) ([]string, error) {
	return queryStrings(s.store.db, `SELECT path FROM written_paths WHERE folder_id = ? ORDER BY path`, folder)
}

func queryStrings(q queryer, query string, args ...any) ([]string, error) {
	rows, err := q.Query(query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var values []string
	for rows.Next() {
		var value string
		if err := rows.Scan(&value); err != nil {
			return nil, err
		}
		values = append(values, value)
	}
	return values, rows.Err()
}

// runDeselection deselects what the service wrote to on-demand folders, once it can
func (s *Service) runDeselection(ctx context.Context) {
	ticker := time.NewTicker(deselectInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
		folders, err := queryStrings(s.store.db, `SELECT DISTINCT folder_id FROM written_paths`)
		if err != nil && ctx.Err() == nil {
			s.log.Warn("Could not list what was written to on-demand folders", "error", err)
		}
		for _, folder := range folders {
			if err := s.deselectWritten(ctx, folder); err != nil && ctx.Err() == nil {
				s.log.Warn("Could not deselect what other devices have", "folder", folder, "error", err)
			}
		}
	}
}

// deselectWritten deselects the paths the service selected in a folder that this device no longer has to announce, and
// removes their local copies: they open through Finder from then on, as what was never written here does. A deselected
// path stops being announced at the next scan, and this device passes on only its own entries, so it isn't enough for
// one device to have a change: a device that is behind may never meet that one. Every device that shares the folder
// has to have got there first (caughtUp), which means that one that is away for good holds this up until it is removed
// from the folder.
//
// It runs in the folder's loop, where neither a scan nor a pull can move what it checks, and not next to operations, some
// of which wait for what they selected. A first look outside the loop leaves the folder alone while there is nothing to
// do, which is most of the time when a device is away.
func (s *Service) deselectWritten(ctx context.Context, folder string) error {
	paths, err := s.writtenPaths(folder)
	if err != nil || len(paths) == 0 {
		return err
	}
	info, ok := s.backend.Folder(folder)
	if !ok {
		return s.forgetWritten(folder, paths)
	}
	if !info.OnDemand || info.Paused {
		return nil
	}
	settled, err := s.settledPaths(folder, paths, func(name string) (protocol.FileInfo, bool, error) {
		return s.backend.LocalFile(folder, name)
	})
	if err != nil {
		return err
	}
	directories, err := s.groupableDirectories(folder, paths)
	if err != nil || len(settled) == 0 && len(directories) == 0 {
		return err
	}
	lock := s.folderLock(folder)
	if !lock.TryLock() {
		// An operation is at work: there is always a next time
		return nil
	}
	defer lock.Unlock()
	pending, err := s.pendingOperationIDs(folder)
	if err != nil || len(pending) > 0 {
		return err
	}
	root, err := openRoot(info.NativePath)
	if err != nil {
		return err
	}
	defer root.close()
	return s.backend.RunInFolderLoop(ctx, folder, func(ctx context.Context, h syncthing.FolderHandle) error {
		done, err := s.beginPublish()
		if err != nil {
			return err
		}
		defer done()
		paths, err := s.writtenPaths(folder)
		if err != nil {
			return err
		}
		deselected, err := s.deselectSettled(ctx, h, root, info, paths)
		if err != nil {
			return err
		}
		gone := make(map[string]struct{}, len(deselected))
		for _, path := range deselected {
			gone[path] = struct{}{}
		}
		return s.groupWritten(h, info, slices.DeleteFunc(paths, func(path string) bool {
			_, ok := gone[path]
			return ok
		}))
	})
}

// caughtUp reports whether this device can stop announcing its entry for name: every device that shares the folder has
// got at least as far, whether it keeps the file or ignores it, and one of them has the file, unless it is a deletion. A
// device whose entry conflicts with this one hasn't got there yet. An entry that isn't announced has nothing to wait for.
func (s *Service) caughtUp(folder, name string, local protocol.FileInfo, ok bool, peers []protocol.DeviceID) (bool, error) {
	if !ok || local.IsInvalid() {
		return true, nil
	}
	held := local.Deleted
	for _, peer := range peers {
		file, ok, err := s.backend.PeerFile(folder, peer, name)
		switch {
		case err != nil:
			return false, err
		case !ok:
			// A device keeps no entry for the deletion of what it never had, as with a temporary file that was gone before
			// it got to it. One that is away may have the file from a third device, without this one knowing yet.
			if !local.Deleted || !s.backend.IsConnected(peer) {
				return false, nil
			}
		case !file.Version.GreaterEqual(local.Version):
			return false, nil
		case !file.IsInvalid():
			held = true
		}
	}
	return held, nil
}

// settledPaths returns those of paths whose entries and those of all below them are caught up, going by what current
// says this device has
func (s *Service) settledPaths(folder string, paths []string,
	current func(name string) (protocol.FileInfo, bool, error),
) ([]string, error) {
	peers := s.backend.Peers(folder)
	settled := func(path string) (bool, error) {
		local, ok, err := current(path)
		if err != nil {
			return false, err
		}
		return s.caughtUp(folder, path, local, ok, peers)
	}
	var candidates []string
	below := make(map[string][]string)
	for _, path := range paths {
		ok, err := settled(path)
		if err != nil {
			return nil, err
		}
		if ok {
			candidates = append(candidates, path)
			below[path] = nil
		}
	}
	if len(candidates) == 0 {
		return nil, nil
	}
	// A selected directory selects all that is in it, and deleted entries are no longer on disk to find
	err := s.backend.GlobalEntries(folder, func(entry GlobalEntry) error {
		for parent := parentPath(entry.Name); parent != ""; parent = parentPath(parent) {
			if names, ok := below[parent]; ok {
				below[parent] = append(names, entry.Name)
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	result := candidates[:0]
next:
	for _, path := range candidates {
		for _, name := range below[path] {
			ok, err := settled(name)
			if err != nil {
				return nil, err
			}
			if !ok {
				continue next
			}
		}
		result = append(result, path)
	}
	return result, nil
}

// A file or directory that goes with a deselected path
type localCopy struct {
	name      string
	directory bool
}

// localCopiesOf lists what is on disk at and below path, directories before what is in them, and reports whether
// removing it would lose nothing: what the index doesn't have, or has differently, is all there is of it until a scan
// has looked at it. What the folder's patterns ignore is left out, and stays.
func (s *Service) localCopiesOf(h syncthing.FolderHandle, root *dir, info FolderInfo, path string,
	ignored func(name string) bool,
) ([]localCopy, bool, error) {
	var copies []localCopy
	complete := true
	err := h.Filesystem().Walk(path, func(walked string, fi fs.FileInfo, err error) error {
		if err != nil {
			complete = complete && fs.IsNotExist(err)
			return nil
		}
		name := osutil.NormalizedFilename(walked)
		switch {
		case IsSyncthingPrivatePath(name, info.MarkerName) || ignored(name):
			if fi.IsDir() {
				return fs.SkipDir
			}
			return nil
		case fi.IsDir():
			copies = append(copies, localCopy{name: name, directory: true})
			return nil
		}
		local, ok, err := h.CurrentFile(name)
		if err != nil {
			return err
		}
		st, err := entryAt(root, name)
		if err != nil {
			return err
		}
		symlink := st.exists && st.kind == unix.S_IFLNK && local.IsSymlink()
		if !isIndexed(local, ok) || !symlink && !matchesIndex(st, local, h.ModTimeWindow()) {
			complete = false
			return nil
		}
		copies = append(copies, localCopy{name: name})
		return nil
	})
	return copies, complete, err
}

// deselectPaths takes paths out of the selection, and returns what the patterns ignore then. Without paths it says what
// they ignore now.
func (s *Service) deselectPaths(folder string, paths []string) (func(name string) bool, error) {
	ignores, err := s.backend.DeselectKept(folder, paths)
	if err != nil {
		return nil, err
	}
	return func(name string) bool {
		normalized, err := scanner.NameFor(name)
		return err == nil && ignores(normalized)
	}, nil
}

// deselectSettled deselects those of paths that are settled and whose local copies can go, removes the copies, and
// returns the paths. The lines go first: a scan that finds a file gone that is still selected announces a deletion.
// Stopping in between leaves a file that nothing refers to, as switching to on demand again would clear up. The scan
// the new patterns schedule would mark the entries ignored anyway, but an operation may get to the folder first, and
// find an entry that says this device has what it doesn't. Copies are unlinked rather than deleted through the folder's
// filesystem, which for a folder that uses the Trash would put a file there that is safe on other devices.
func (s *Service) deselectSettled(ctx context.Context, h syncthing.FolderHandle, root *dir, info FolderInfo, paths []string,
) ([]string, error) {
	settled, err := s.settledPaths(info.ID, paths, h.CurrentFile)
	if err != nil || len(settled) == 0 {
		return nil, err
	}
	ignored, err := s.deselectPaths(info.ID, nil)
	if err != nil {
		return nil, err
	}
	var deselected []string
	var copies []localCopy
	for _, path := range settled {
		found, complete, err := s.localCopiesOf(h, root, info, path, ignored)
		if err != nil {
			return nil, err
		}
		if complete {
			deselected = append(deselected, path)
			copies = append(copies, found...)
		}
	}
	if len(deselected) == 0 {
		return nil, nil
	}
	if ignored, err = s.deselectPaths(info.ID, deselected); err != nil {
		return nil, err
	}
	_ = h.Scan(ctx, deselected)
	for _, c := range copies {
		if !c.directory && ignored(c.name) {
			if err := removeCopy(root, c.name, (*dir).remove); err != nil {
				s.log.Warn("Could not remove the local copy of what other devices have", "path", c.name, "error", err)
			}
		}
	}
	// Directories that are left empty, deepest first, up to what is still selected
	for _, c := range slices.Backward(copies) {
		if c.directory && ignored(c.name) {
			_ = removeCopy(root, c.name, (*dir).rmdir)
		}
	}
	for _, path := range deselected {
		for parent := parentPath(path); parent != "" && ignored(parent); parent = parentPath(parent) {
			if err := removeCopy(root, parent, (*dir).rmdir); err != nil {
				break
			}
		}
	}
	return deselected, s.forgetWritten(info.ID, deselected)
}

func removeCopy(root *dir, name string, remove func(d *dir, name string) error) error {
	parent, err := root.openDir(parentPath(name))
	if err != nil {
		return err
	}
	defer parent.close()
	return remove(parent, baseName(name))
}

// groupableDirectories returns the directories that could be selected in place of paths below them without selecting
// anything more, parents first. Selecting a directory takes in all of it, so that in most directories every path needs
// a line of its own, and a directory that has nothing else has to wait for a device that is away with as many lines as
// it had paths.
func (s *Service) groupableDirectories(folder string, paths []string) ([]string, error) {
	written := make(map[string]struct{}, len(paths))
	for _, path := range paths {
		written[path] = struct{}{}
	}
	children := make(map[string]int)
	for _, path := range paths {
		parent := parentPath(path)
		if _, ok := written[parent]; !ok && parent != "" && SelectablePath(parent) {
			children[parent]++
		}
	}
	maps.DeleteFunc(children, func(_ string, count int) bool { return count < minGroupedPaths })
	if len(children) == 0 {
		return nil, nil
	}
	err := s.backend.GlobalEntries(folder, func(entry GlobalEntry) error {
		if entry.Deleted || entry.Invalid {
			return nil
		}
		for path := entry.Name; path != ""; path = parentPath(path) {
			if _, ok := written[path]; ok {
				return nil
			}
		}
		for parent := parentPath(entry.Name); parent != ""; parent = parentPath(parent) {
			delete(children, parent)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return slices.Sorted(maps.Keys(children)), nil
}

// groupWritten selects directories in place of the paths written below them, where that selects nothing more. Their
// lines go with it, and the directory's record takes over from theirs.
func (s *Service) groupWritten(h syncthing.FolderHandle, info FolderInfo, paths []string) error {
	directories, err := s.groupableDirectories(info.ID, paths)
	if err != nil {
		return err
	}
	for _, directory := range directories {
		global, live, err := liveEntry(h, directory)
		if err != nil {
			return err
		}
		if !live || !global.IsDirectory() {
			continue
		}
		if err := s.keepSelected(info, directory); err != nil {
			return err
		}
		var below []string
		for _, path := range paths {
			if isDescendantPath(path, directory) {
				below = append(below, path)
			}
		}
		if err := s.forgetWritten(info.ID, below); err != nil {
			return err
		}
	}
	return nil
}

// exclusions returns what keeps a path from being written, by the folder's patterns. In an on-demand folder, its
// selection doesn't count: what is written gets selected.
func (s *Service) exclusions(h syncthing.FolderHandle, info FolderInfo) (func(name string) bool, error) {
	if !info.OnDemand {
		return h.IsIgnored, nil
	}
	ignored, err := s.globalIgnores(info.ID)
	if err != nil {
		return nil, err
	}
	return func(name string) bool { return !SelectablePath(name) || ignored(name) }, nil
}

// globalIgnores returns what an on-demand folder's patterns exclude, in the form a scan matches them with
func (s *Service) globalIgnores(folder string) (func(name string) bool, error) {
	ignored, err := s.backend.GlobalIgnores(folder)
	if err != nil {
		return nil, err
	}
	return func(name string) bool {
		normalized, err := scanner.NameFor(name)
		return err != nil || ignored(normalized)
	}, nil
}

// SelectablePath reports whether a line in .stignore can select p: such lines can't contain "*", and they are read one
// by one and trimmed, so a name with a line break or at the edge of a space would select something else
func SelectablePath(p string) bool {
	return !strings.ContainsFunc(p, func(r rune) bool { return r == '*' || unicode.IsControl(r) }) &&
		p == strings.TrimSpace(p)
}

// ensureDir creates the directories of rel that don't exist, with the permissions the index says they have. Staging
// and publishing happen next to the target, and the directories of what isn't selected aren't on disk.
func (s *Service) ensureDir(root *dir, info FolderInfo, rel string) error {
	if rel == "" {
		return nil
	}
	current := ""
	for component := range strings.SplitSeq(rel, "/") {
		parent, err := root.openDir(current)
		if err != nil {
			return err
		}
		current = joinPath(current, component)
		mode := uint32(0o755)
		if global, ok, err := s.backend.GlobalFile(info.ID, current); err == nil && ok && !global.Deleted &&
			!info.IgnorePerms && !global.NoPermissions {
			mode = global.Permissions & 0o777
		}
		err = parent.mkdir(component, mode)
		parent.close()
		if err != nil && !isExists(err) {
			return err
		}
	}
	return nil
}

// collectOnDemand is collectSubtree for an on-demand folder: it lists an item and everything below it, deepest first,
// with what is on this device. It goes by the engine's global index, as the change log lags behind it and leaves out
// symbolic links, which this device doesn't hold and can't vouch for. For anything this device doesn't have, the entry
// is one to build on (mergedWith) and absent is set; directories are all listed like that, as whether they are on disk
// says nothing about what they hold. What the folder's patterns exclude is left out: this device can't announce a change
// of it, as they outrank the selection, and it is no more this device's to change than an ignored file elsewhere.
func (s *Service) collectOnDemand(ctx context.Context, h syncthing.FolderHandle, root *dir, row itemRow, current *Item,
) ([]subtreeEntry, error) {
	names := []string{row.path}
	if row.kind == protocol.FileInfoTypeDirectory {
		ignored, err := s.globalIgnores(row.folder)
		if err != nil {
			return nil, err
		}
		err = s.backend.GlobalEntries(row.folder, func(entry GlobalEntry) error {
			switch {
			case !isDescendantPath(entry.Name, row.path) || entry.Deleted || entry.Invalid || ignored(entry.Name):
				return nil
			case entry.Type == protocol.FileInfoTypeSymlink:
				return newAPIError(http.StatusConflict, codeNotEmpty, "the directory holds symbolic links, which can't be seen here")
			}
			names = append(names, entry.Name)
			return nil
		})
		if err != nil {
			return nil, err
		}
	}
	lower, upper := descendantRange(row.path)
	rows, err := queryItems(s.store.db, `SELECT `+itemColumns+` FROM items WHERE folder_id = ? AND deleted = 0
		AND path > ? AND path < ?`, row.folder, lower, upper)
	if err != nil {
		return nil, err
	}
	revisions := make(map[string]string, len(rows)+1)
	revisions[row.path] = row.revision
	for _, r := range rows {
		revisions[r.path] = r.revision
	}

	known := make(map[string]struct{}, len(names))
	entries := make([]subtreeEntry, 0, len(names))
	for _, name := range names {
		global, live, err := liveEntry(h, name)
		if err != nil {
			return nil, err
		}
		if !live {
			continue
		}
		// The change log is behind the index: the client hasn't seen this, which the subtree generation it sent cannot
		// account for
		if revision, ok := revisions[name]; !ok || revisionOf(global) != revision {
			return nil, revisionMismatch(current)
		}
		known[name] = struct{}{}
		if global.IsDirectory() {
			st, err := entryAt(root, name)
			if err != nil {
				return nil, err
			}
			if st.exists && st.kind != unix.S_IFDIR {
				return nil, newAPIError(http.StatusConflict, codeNotEmpty, "a file here is not synchronized")
			}
			merged, _, err := mergedWith(h, name, global, true)
			if err != nil {
				return nil, err
			}
			entries = append(entries, subtreeEntry{path: name, local: merged, absent: true})
			continue
		}
		state, local, err := classify(h, root, name, global)
		if err != nil {
			return nil, err
		}
		switch state {
		case pathStale:
			return nil, errWaiting
		case pathUnindexed:
			return nil, newAPIError(http.StatusConflict, codeNotEmpty, "a file here is not synchronized")
		case pathAbsent:
			merged, _, err := mergedWith(h, name, global, true)
			if err != nil {
				return nil, err
			}
			entries = append(entries, subtreeEntry{path: name, local: merged, absent: true})
		case pathHeld:
			st, err := entryAt(root, name)
			if err != nil {
				return nil, err
			}
			if !matchesIndex(st, local, h.ModTimeWindow()) {
				_ = h.Scan(ctx, []string{row.path})
				return nil, revisionMismatch(current)
			}
			entries = append(entries, subtreeEntry{path: name, local: local})
		}
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

// checkDestination refuses a move for what would otherwise only show once all of it was downloaded
func (s *Service) checkDestination(destInfo FolderInfo, dest string) error {
	if destInfo.OnDemand {
		ignored, err := s.globalIgnores(destInfo.ID)
		if err != nil {
			return err
		}
		if !SelectablePath(dest) || ignored(dest) {
			return ignoredPath()
		}
	}
	global, ok, err := s.backend.GlobalFile(destInfo.ID, dest)
	if err != nil {
		return err
	}
	if ok && !global.Deleted && !global.IsInvalid() {
		return nameConflict(s.currentItem(destInfo.ID, dest))
	}
	return nil
}

// collectForMove lists what moves with an item of an on-demand folder, once this device has all of it. Until then it
// selects the item, so that the engine fetches it, and waits.
func (s *Service) collectForMove(ctx context.Context, h syncthing.FolderHandle, root *dir, info FolderInfo, row itemRow,
	current *Item,
) ([]subtreeEntry, error) {
	entries, err := s.collectOnDemand(ctx, h, root, row, current)
	if err != nil {
		return nil, err
	}
	var missing int64
	complete := true
	for i, e := range entries {
		local, ok, err := h.CurrentFile(e.path)
		if err != nil {
			return nil, err
		}
		if isIndexed(local, ok) {
			if e.local.IsDirectory() {
				entries[i].local, entries[i].absent = local, false
			}
			continue
		}
		complete = false
		if !e.local.IsDirectory() {
			missing += e.local.Size
		}
	}
	if complete {
		return entries, nil
	}
	if row.kind == protocol.FileInfoTypeDirectory && missing > onDemandMoveLimit {
		return nil, tooLargeToMove()
	}
	if err := s.keepSelected(info, row.path); err != nil {
		return nil, err
	}
	return nil, errWaiting
}
