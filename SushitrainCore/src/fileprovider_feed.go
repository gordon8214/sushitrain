// Copyright (C) 2026 Gordon
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this file,
// You can obtain one at https://mozilla.org/MPL/2.0/.
package sushitrain

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"path"
	"strings"

	"github.com/gobwas/glob"
	"github.com/syncthing/syncthing/lib/ignore"
	"github.com/syncthing/syncthing/lib/protocol"
)

// Receives batches of file provider records as a JSON array. Returning an error aborts the pass.
type FileProviderRecordDelegate interface {
	OnRecords(batchJSON []byte) error
}

type FileProviderPassResult struct {
	// The highest database sequence number seen for any global entry in the folder
	HighestSequence int64
	// Set when the highest sequence is below the cursor that was passed in, which means the index was rebuilt. The
	// caller should then perform a full pass.
	SequenceReset bool
	// The number of records delivered to the delegate
	Count int
}

// The JSON shape of a file provider record. Kept unexported so gomobile does not bind it.
type fileProviderRecord struct {
	Path          string `json:"path"`
	Sequence      int64  `json:"seq"`
	Type          string `json:"type"`
	Deleted       bool   `json:"deleted,omitempty"`
	Size          int64  `json:"size"`
	ModifiedNanos int64  `json:"modNs"`
	BlocksHash    string `json:"blocksHash,omitempty"`
	ModifiedBy    string `json:"modifiedBy,omitempty"`
	SymlinkTarget string `json:"symlinkTarget,omitempty"`
	Selected      bool   `json:"selected"`
	Hidden        bool   `json:"hidden,omitempty"`
	// Full records were read from the complete file information; partial records only contain index metadata
	Full bool `json:"full"`
}

const (
	fileProviderRecordTypeFile      = "file"
	fileProviderRecordTypeDirectory = "directory"
	fileProviderRecordTypeSymlink   = "symlink"
	fileProviderDefaultBatchSize    = 500
)

// A minimal view on a global index entry, so the feed can be tested without a database
type globalFileMeta struct {
	name       string
	sequence   int64
	fileType   protocol.FileInfoType
	deleted    bool
	size       int64
	modNanos   int64
	localFlags protocol.FlagLocal
}

type globalIndex interface {
	allGlobalFiles(folderID string, yield func(globalFileMeta) bool) error
	globalFileInfo(folderID string, path string) (protocol.FileInfo, bool, error)
}

type internalsGlobalIndex struct {
	client *Client
}

func (idx internalsGlobalIndex) allGlobalFiles(folderID string, yield func(globalFileMeta) bool) error {
	files, errFn := idx.client.app.Internals.AllGlobalFiles(folderID)
	for meta := range files {
		if !yield(globalFileMeta{
			name:       meta.Name,
			sequence:   meta.Sequence,
			fileType:   meta.Type,
			deleted:    meta.Deleted,
			size:       meta.Size,
			modNanos:   meta.ModNanos,
			localFlags: meta.LocalFlags,
		}) {
			break
		}
	}
	return errFn()
}

func (idx internalsGlobalIndex) globalFileInfo(folderID string, path string) (protocol.FileInfo, bool, error) {
	return idx.client.app.Internals.GlobalFileInfo(folderID, path)
}

func (fld *Folder) globalIndex() (globalIndex, error) {
	if fld.client.app == nil || fld.client.app.Internals == nil {
		return nil, ErrStillLoading
	}
	return internalsGlobalIndex{client: fld.client}, nil
}

// Whether an entry must never be exposed to the file provider: Syncthing's private files, and names containing NUL,
// which no file name on Apple platforms can contain (and which C strings, such as those SQLite takes, truncate)
func isExcludedFromFileProvider(name string, markerName string) bool {
	return strings.ContainsRune(name, 0) || isSyncthingPrivatePath(name, markerName)
}

// Whether a path refers to one of Syncthing's private files, which should never be exposed
func isSyncthingPrivatePath(name string, markerName string) bool {
	for component := range strings.SplitSeq(name, "/") {
		if component == markerName || component == ignoreFileName || component == versionsDirName {
			return true
		}
		if strings.HasPrefix(component, ".syncthing.") || strings.HasPrefix(component, "~syncthing~") {
			return true
		}
	}
	return false
}

func recordType(fileType protocol.FileInfoType) string {
	switch fileType {
	case protocol.FileInfoTypeDirectory:
		return fileProviderRecordTypeDirectory
	case protocol.FileInfoTypeSymlink, protocol.FileInfoTypeSymlinkDirectory, protocol.FileInfoTypeSymlinkFile:
		return fileProviderRecordTypeSymlink
	default:
		return fileProviderRecordTypeFile
	}
}

// Decides whether entries are selected (kept on this device) or hidden (can never be kept on this device)
type selectionClassifier struct {
	matcher     *ignore.Matcher
	selective   bool
	globalGlobs []glob.Glob
}

// Fails when the ignore patterns cannot be loaded: without them, selection cannot be told, and a pass should be tried
// again later rather than report everything as not selected
func (fld *Folder) selectionClassifier() (*selectionClassifier, error) {
	matcher, err := fld.loadIgnores()
	if err != nil {
		return nil, fmt.Errorf("could not load ignore patterns: %w", err)
	}

	classifier := &selectionClassifier{matcher: matcher}
	sel := newSelection(matcher.Lines())
	if sel.isSelectiveIgnore() {
		classifier.selective = true
		if globs, err := sel.globalIgnoreGlobs(); err == nil {
			classifier.globalGlobs = globs
		}
	}
	return classifier, nil
}

func (sc *selectionClassifier) classify(name string) (selected bool, hidden bool) {
	if sc.matcher == nil {
		return false, false
	}

	ignored := sc.matcher.Match(name).IsIgnored()
	if !sc.selective {
		// In regular folders, ignored files can never become local
		return !ignored, ignored
	}

	// In selective folders, everything not selected is ignored. Only the global ignore patterns hide files.
	for prefix := name; prefix != "." && prefix != "/" && prefix != ""; prefix = path.Dir(prefix) {
		for _, gl := range sc.globalGlobs {
			if gl.Match(prefix) {
				return false, true
			}
		}
	}
	return !ignored, false
}

func fullRecord(info protocol.FileInfo, sequence int64, sc *selectionClassifier) fileProviderRecord {
	selected, hidden := sc.classify(info.Name)
	rec := fileProviderRecord{
		Path:          info.Name,
		Sequence:      sequence,
		Type:          recordType(info.Type),
		Deleted:       info.IsDeleted(),
		Size:          info.Size,
		ModifiedNanos: info.ModTime().UnixNano(),
		ModifiedBy:    info.FileModifiedBy().String(),
		Selected:      selected,
		Hidden:        hidden || info.IsInvalid(),
		Full:          true,
	}
	if len(info.BlocksHash) > 0 {
		rec.BlocksHash = base64.StdEncoding.EncodeToString(info.BlocksHash)
	}
	if info.IsSymlink() {
		rec.SymlinkTarget = string(info.SymlinkTarget)
	}
	if rec.Deleted {
		rec.Size = 0
	}
	return rec
}

func partialRecord(meta globalFileMeta, sc *selectionClassifier) fileProviderRecord {
	selected, hidden := sc.classify(meta.name)
	rec := fileProviderRecord{
		Path:          meta.name,
		Sequence:      meta.sequence,
		Type:          recordType(meta.fileType),
		Deleted:       meta.deleted,
		Size:          meta.size,
		ModifiedNanos: meta.modNanos,
		Selected:      selected,
		Hidden:        hidden || meta.localFlags&protocol.LocalInvalidFlags != 0,
	}
	if rec.Deleted {
		rec.Size = 0
	}
	return rec
}

type recordBatcher struct {
	delegate FileProviderRecordDelegate
	batch    []fileProviderRecord
	size     int
	count    int
}

func (rb *recordBatcher) add(rec fileProviderRecord) error {
	rb.batch = append(rb.batch, rec)
	if len(rb.batch) >= rb.size {
		return rb.flush()
	}
	return nil
}

func (rb *recordBatcher) flush() error {
	if len(rb.batch) == 0 {
		return nil
	}
	js, err := json.Marshal(rb.batch)
	if err != nil {
		return err
	}
	if err := rb.delegate.OnRecords(js); err != nil {
		return err
	}
	rb.count += len(rb.batch)
	rb.batch = rb.batch[:0]
	return nil
}

// Delivers full records for every global entry whose database sequence is higher than the cursor. Pass a cursor of
// zero to receive all entries. Records are delivered in path order, so the caller must only advance its cursor to the
// returned highest sequence after the pass has completed successfully.
func (fld *Folder) GlobalChanges(cursor int64, delegate FileProviderRecordDelegate) (*FileProviderPassResult, error) {
	idx, err := fld.globalIndex()
	if err != nil {
		return nil, err
	}
	return fld.globalChanges(idx, cursor, delegate)
}

func (fld *Folder) globalChanges(idx globalIndex, cursor int64, delegate FileProviderRecordDelegate) (*FileProviderPassResult, error) {
	fc := fld.folderConfiguration()
	if fc == nil {
		return nil, errFolderConfigNotFound
	}

	// Collect candidates first, so that the iterator (which holds a database cursor) is closed before full file
	// information is requested for each of them.
	result := &FileProviderPassResult{}
	var candidates []globalFileMeta
	err := idx.allGlobalFiles(fld.FolderID, func(meta globalFileMeta) bool {
		result.HighestSequence = max(result.HighestSequence, meta.sequence)
		if meta.sequence > cursor && !isExcludedFromFileProvider(meta.name, fc.MarkerName) {
			candidates = append(candidates, meta)
		}
		return true
	})
	if err != nil {
		return nil, err
	}

	if cursor > 0 && result.HighestSequence < cursor {
		result.SequenceReset = true
		return result, nil
	}

	sc, err := fld.selectionClassifier()
	if err != nil {
		return nil, err
	}
	batcher := &recordBatcher{delegate: delegate, size: fileProviderDefaultBatchSize}
	for _, meta := range candidates {
		rec := partialRecord(meta, sc)
		if !meta.deleted {
			// Take every field from the full information, which may be newer than the metadata we collected
			info, ok, err := idx.globalFileInfo(fld.FolderID, meta.name)
			if err != nil {
				return nil, err
			}
			if ok {
				rec = fullRecord(info, meta.sequence, sc)
			} else {
				rec.Deleted = true
				rec.Size = 0
			}
		}
		if err := batcher.add(rec); err != nil {
			return nil, err
		}
	}
	if err := batcher.flush(); err != nil {
		return nil, err
	}
	result.Count = batcher.count
	return result, nil
}

// Delivers partial records (index metadata only, no blocks hash) for all global entries, including deletions. This
// is cheap compared to GlobalChanges with a zero cursor, and is intended for periodic mark-and-sweep passes. Records
// whose sequence differs from what the caller has stored should be refreshed using GlobalRecordsJSON.
func (fld *Folder) GlobalSnapshot(delegate FileProviderRecordDelegate) (*FileProviderPassResult, error) {
	idx, err := fld.globalIndex()
	if err != nil {
		return nil, err
	}
	return fld.globalSnapshot(idx, delegate)
}

func (fld *Folder) globalSnapshot(idx globalIndex, delegate FileProviderRecordDelegate) (*FileProviderPassResult, error) {
	fc := fld.folderConfiguration()
	if fc == nil {
		return nil, errFolderConfigNotFound
	}

	sc, err := fld.selectionClassifier()
	if err != nil {
		return nil, err
	}

	// Collect entries first, so that the iterator (which holds a database cursor) is closed before records are
	// delivered to the delegate, which may take a while
	result := &FileProviderPassResult{}
	var metas []globalFileMeta
	err = idx.allGlobalFiles(fld.FolderID, func(meta globalFileMeta) bool {
		result.HighestSequence = max(result.HighestSequence, meta.sequence)
		if !isExcludedFromFileProvider(meta.name, fc.MarkerName) {
			metas = append(metas, meta)
		}
		return true
	})
	if err != nil {
		return nil, err
	}

	batcher := &recordBatcher{delegate: delegate, size: fileProviderDefaultBatchSize}
	for _, meta := range metas {
		if err := batcher.add(partialRecord(meta, sc)); err != nil {
			return nil, err
		}
	}
	if err := batcher.flush(); err != nil {
		return nil, err
	}
	result.Count = batcher.count
	return result, nil
}

// Returns full records (as a JSON array) for the paths in the given JSON array. Paths that are not in the global index,
// or are never exposed, are returned as deleted records.
func (fld *Folder) GlobalRecordsJSON(pathsJSON []byte) ([]byte, error) {
	idx, err := fld.globalIndex()
	if err != nil {
		return nil, err
	}
	return fld.globalRecordsJSON(idx, pathsJSON)
}

func (fld *Folder) globalRecordsJSON(idx globalIndex, pathsJSON []byte) ([]byte, error) {
	var paths []string
	if err := json.Unmarshal(pathsJSON, &paths); err != nil {
		return nil, err
	}

	fc := fld.folderConfiguration()
	if fc == nil {
		return nil, errFolderConfigNotFound
	}
	sc, err := fld.selectionClassifier()
	if err != nil {
		return nil, err
	}
	records := make([]fileProviderRecord, 0, len(paths))
	for _, name := range paths {
		if name == "" {
			return nil, errors.New("empty path")
		}
		if isExcludedFromFileProvider(name, fc.MarkerName) {
			records = append(records, fileProviderRecord{Path: name, Deleted: true, Type: fileProviderRecordTypeFile, Full: true})
			continue
		}
		info, ok, err := idx.globalFileInfo(fld.FolderID, name)
		if err != nil {
			return nil, err
		}
		if !ok {
			records = append(records, fileProviderRecord{Path: name, Deleted: true, Type: fileProviderRecordTypeFile, Full: true})
			continue
		}
		// The database sequence is not part of the file information; callers keep the one from the snapshot
		records = append(records, fullRecord(info, 0, sc))
	}
	return json.Marshal(records)
}
