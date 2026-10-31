// Copyright (C) 2026 Gordon
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this file,
// You can obtain one at https://mozilla.org/MPL/2.0/.

package gateway

import (
	"context"
	"errors"
	"net/http"
	"os"
	"sort"
	"strings"
	"unicode"

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
	return s.backend.KeepSelected(info.ID, paths)
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
