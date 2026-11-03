// Copyright (C) 2026 Gordon
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this file,
// You can obtain one at https://mozilla.org/MPL/2.0/.

package files

import (
	"context"
	"database/sql"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"t-shaped.nl/sushitrain/v2/remoteapi"
	"t-shaped.nl/sushitrain/v2/remoteclient"
)

const (
	// Downloads running at once, as in the Kit, and of those for files kept on this device (which nobody waits for):
	// fewer, so that a file someone opens doesn't wait behind them
	maxTransfers     = 4
	maxKeepTransfers = 2
	// Bytes asked for at once in the background, and while a read waits for particular blocks
	rangeBytes       = 8 << 20
	urgentRangeBytes = 1 << 20
	// How long a read waits for its blocks: longer than the gateway waits for a block itself (60 s)
	readWait = 90 * time.Second
	// How often kept items are checked for content that isn't here, and the cache for its budget
	keepInterval = time.Minute
)

var (
	// ErrVersionChanged means the file changed on the gateway: the open file can't be read further, and opening it
	// again reads the new version
	ErrVersionChanged = errors.New("the file changed on the gateway")
	// ErrContentUnavailable means the gateway can't be reached, or can't provide the content, now
	ErrContentUnavailable = errors.New("the file's content can't be fetched from its gateway now")
)

// cache keeps verified content, named after its content ID, and the downloads that fill it
type cache struct {
	s            *Service
	contentDir   string
	transfersDir string

	mutex     sync.Mutex
	pinned    map[string]int
	transfers map[string]*transfer
	slots     chan struct{}
	keepSlots chan struct{}
	// When content was last recorded as used, so that opening it often doesn't write each time
	touched map[string]int64
	wake    chan struct{}
	stopped chan struct{}
}

func openCache(s *Service) (*cache, error) {
	c := &cache{s: s, contentDir: filepath.Join(s.cfg.CacheDir, "Content"),
		transfersDir: filepath.Join(s.cfg.CacheDir, "Transfers"), pinned: map[string]int{},
		transfers: map[string]*transfer{}, slots: make(chan struct{}, maxTransfers),
		keepSlots: make(chan struct{}, maxKeepTransfers), touched: map[string]int64{}, wake: make(chan struct{}, 1),
		stopped: make(chan struct{})}
	for _, dir := range []string{c.contentDir, c.transfersDir} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return nil, err
		}
	}
	// Partial files of downloads that never got as far as their manifest, or whose folder was removed, can't resume
	resumable, err := c.contentSet(`SELECT content_id FROM transfers`)
	if err != nil {
		return nil, err
	}
	entries, _ := os.ReadDir(c.transfersDir)
	partial := map[string]bool{}
	for _, entry := range entries {
		if contentID, ok := strings.CutSuffix(entry.Name(), ".part"); !ok || !resumable[contentID] {
			_ = os.Remove(filepath.Join(c.transfersDir, entry.Name()))
		} else {
			partial[contentID] = true
		}
	}
	// Records of downloads whose partial file is gone (the cache was emptied) say nothing of what is on disk
	for contentID := range resumable {
		if !partial[contentID] {
			if err := c.forgetTransfer(contentID); err != nil {
				return nil, err
			}
		}
	}
	// Content that no record refers to (a download that was published as the daemon stopped) is never evicted
	known, err := c.contentSet(`SELECT content_id FROM content`)
	if err != nil {
		return nil, err
	}
	entries, _ = os.ReadDir(c.contentDir)
	for _, entry := range entries {
		if !known[entry.Name()] {
			_ = os.Remove(filepath.Join(c.contentDir, entry.Name()))
		}
	}
	return c, nil
}

// forgetTransfer drops the record of a download's verified blocks
func (c *cache) forgetTransfer(contentID string) error {
	return c.s.store.tx(context.Background(), func(tx *sql.Tx) error {
		_, err := tx.Exec(`DELETE FROM transfers WHERE content_id = ?`, contentID)
		return err
	})
}

func (c *cache) contentPath(contentID string) string {
	return filepath.Join(c.contentDir, contentID)
}

func (c *cache) partialPath(contentID string) string {
	return filepath.Join(c.transfersDir, contentID+".part")
}

// run keeps kept items downloaded and the cache within its budget
func (c *cache) run(ctx context.Context) {
	defer close(c.stopped)
	timer := time.NewTimer(0)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-c.wake:
		case <-timer.C:
		}
		if err := c.downloadKept(); err != nil && ctx.Err() == nil {
			c.s.log.Warn("Could not check kept items", "error", err)
		}
		if err := c.evict(); err != nil && ctx.Err() == nil {
			c.s.log.Warn("Could not trim the cache", "error", err)
		}
		timer.Reset(keepInterval)
	}
}

func (c *cache) poke() {
	select {
	case c.wake <- struct{}{}:
	default:
	}
}

func (c *cache) close() {
	c.mutex.Lock()
	transfers := make([]*transfer, 0, len(c.transfers))
	for _, t := range c.transfers {
		transfers = append(transfers, t)
	}
	c.mutex.Unlock()
	for _, t := range transfers {
		t.stopAndWait()
	}
}

func (c *cache) pin(contentID string) {
	c.mutex.Lock()
	defer c.mutex.Unlock()
	c.pinned[contentID]++
}

func (c *cache) unpin(contentID string) {
	c.mutex.Lock()
	defer c.mutex.Unlock()
	if c.pinned[contentID]--; c.pinned[contentID] <= 0 {
		delete(c.pinned, contentID)
	}
}

// cached reports whether the content is complete in the cache
func (c *cache) cached(contentID string) (bool, error) {
	var exists bool
	err := c.s.store.db.QueryRow(`SELECT EXISTS (SELECT 1 FROM content WHERE content_id = ?)`, contentID).Scan(&exists)
	if err != nil || !exists {
		return false, err
	}
	if _, err := os.Stat(c.contentPath(contentID)); err != nil {
		// Removed from under the cache: it is fetched again
		_ = c.s.store.tx(context.Background(), func(tx *sql.Tx) error {
			_, err := tx.Exec(`DELETE FROM content WHERE content_id = ?`, contentID)
			return err
		})
		return false, nil
	}
	return true, nil
}

// touch records that content was used, at most once a minute: eviction needs no finer order, and each record is a
// write to the disk
func (c *cache) touch(contentID string) {
	now := nowUnix()
	c.mutex.Lock()
	if now-c.touched[contentID] < 60 {
		c.mutex.Unlock()
		return
	}
	c.touched[contentID] = now
	c.mutex.Unlock()
	_ = c.s.store.tx(context.Background(), func(tx *sql.Tx) error {
		_, err := tx.Exec(`UPDATE content SET last_used = ? WHERE content_id = ?`, now, contentID)
		return err
	})
}

// Reader reads a file's content as it was when opened, fetching blocks from the gateway as they are read. A reader
// of a node reads its working copy while it has one, as every handle of a file on a local filesystem reads what is
// written through the others.
type Reader struct {
	c         *cache
	node      uint64
	contentID string
	size      int64
	file      *os.File
	t         *transfer
	pinned    bool
	closeOnce sync.Once
}

// OpenFile opens an item's content as it is now. Content that isn't here is fetched in the background, and reads
// wait only for the blocks they cover.
func (s *Service) OpenFile(item int64) (*Reader, error) {
	e, ok, err := s.Item(item)
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, fs.ErrNotExist
	}
	return s.open(e, 0)
}

// OpenNode opens what a node shows. A node that no longer shows its item (it was given a new one when its content
// changed on the gateway) can't be opened again, unless it is being written.
func (s *Service) OpenNode(e Entry) (*Reader, error) {
	current, ok, err := s.Item(e.Item)
	if err != nil {
		return nil, err
	}
	if !ok || current.Node != e.Node {
		if s.workingCopy(e.Node) == nil {
			return nil, ErrVersionChanged
		}
		current = e
	}
	return s.open(current, e.Node)
}

func (s *Service) open(e Entry, node uint64) (*Reader, error) {
	if e.Kind != kindFile {
		return nil, ErrIsDir
	}
	r := &Reader{c: s.cache, node: node, contentID: e.ContentID, size: e.Size}
	// Content the gateway hasn't committed yet is read from here
	if path, err := s.localContentPath(e.Item); err != nil {
		return nil, err
	} else if path != "" {
		f, err := os.Open(path)
		if err != nil {
			return nil, err
		}
		info, err := f.Stat()
		if err != nil {
			f.Close()
			return nil, err
		}
		r.file, r.size, r.contentID = f, info.Size(), ""
		return r, nil
	}
	if e.Size == 0 || e.ContentID == "" {
		r.size = 0
		return r, nil
	}
	r.pinned = true
	s.cache.pin(e.ContentID)
	cached, err := s.cache.cached(e.ContentID)
	if err != nil {
		s.cache.unpin(e.ContentID)
		return nil, err
	}
	if cached {
		if r.file, err = os.Open(s.cache.contentPath(e.ContentID)); err == nil {
			s.cache.touch(e.ContentID)
			return r, nil
		}
	}
	if r.t, err = s.cache.transferFor(e.ContentID, e.Item, e.Size, false); err != nil {
		s.cache.unpin(e.ContentID)
		return nil, err
	}
	return r, nil
}

// ContentID is the content the reader reads
func (r *Reader) ContentID() string {
	return r.contentID
}

// Size is the content's length
func (r *Reader) Size() int64 {
	return r.size
}

// Local reports whether the whole content is here
func (r *Reader) Local() bool {
	return r.size == 0 || r.file != nil || (r.t != nil && r.t.complete())
}

// ReadAt reads len(p) bytes at off, or fewer at the end of the content, waiting for blocks that aren't here
func (r *Reader) ReadAt(ctx context.Context, p []byte, off int64) (int, error) {
	if r.node != 0 {
		if n, ok, err := r.c.s.readWorkingCopy(r.node, p, off); ok {
			return n, err
		}
	}
	if off >= r.size {
		return 0, io.EOF
	}
	n := int(min(int64(len(p)), r.size-off))
	if r.file != nil {
		return r.file.ReadAt(p[:n], off)
	}
	if err := r.t.wait(ctx, off, n); err != nil {
		return 0, err
	}
	return r.t.file.ReadAt(p[:n], off)
}

// Close releases the reader
func (r *Reader) Close() error {
	r.closeOnce.Do(func() {
		if r.file != nil {
			r.file.Close()
		}
		if r.t != nil {
			r.c.release(r.t)
		}
		if r.pinned {
			r.c.unpin(r.contentID)
		}
	})
	return nil
}

// transfer fetches one content ID into a partial file, block by block, each verified against the manifest before it
// is written. It is published to the cache by an atomic rename when complete and checked again.
type transfer struct {
	c         *cache
	contentID string
	item      int64
	size      int64

	mutex    sync.Mutex
	file     *os.File
	blocks   []remoteclient.Block
	verified []bool
	count    int
	wanted   []int
	refs     int
	keep     bool
	// Whether something reads it, rather than only keeping it here
	read     bool
	running  bool
	closed   bool
	done     bool
	err      error
	changed  chan struct{}
	stop     context.CancelFunc
	finished chan struct{}
}

// transferFor returns the content's transfer, creating it (or resuming one from before a restart) and starting it.
// The caller releases it.
func (c *cache) transferFor(contentID string, item, size int64, keep bool) (*transfer, error) {
	c.mutex.Lock()
	defer c.mutex.Unlock()
	t, ok := c.transfers[contentID]
	if ok && t.failed() {
		// A failed one that still runs (as it stops) gives way when it ends; a new one starts over
		t.mutex.Lock()
		running := t.running
		t.mutex.Unlock()
		if running {
			return nil, ErrContentUnavailable
		}
		ok = false
	}
	if !ok {
		path := c.partialPath(contentID)
		_, statErr := os.Stat(path)
		file, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o600)
		if err != nil {
			return nil, err
		}
		if err := file.Truncate(size); err != nil {
			file.Close()
			return nil, err
		}
		t = &transfer{c: c, contentID: contentID, item: item, size: size, file: file, changed: make(chan struct{})}
		if statErr != nil {
			// A new partial file has none of the blocks a record of an earlier one says were verified
			if err := c.forgetTransfer(contentID); err != nil {
				file.Close()
				return nil, err
			}
		} else if err := t.resume(); err != nil {
			c.s.log.Warn("Could not resume a download; starting over", "error", err)
		}
		c.transfers[contentID] = t
	}
	t.mutex.Lock()
	defer t.mutex.Unlock()
	t.refs++
	t.keep = t.keep || keep
	t.read = t.read || !keep
	if !t.running && !t.done {
		t.start()
	}
	return t, nil
}

func (c *cache) release(t *transfer) {
	c.mutex.Lock()
	defer c.mutex.Unlock()
	t.mutex.Lock()
	defer t.mutex.Unlock()
	if t.refs--; t.refs > 0 {
		return
	}
	// Nothing reads it any more: the download stops, and resumes from its verified blocks when needed again. One that
	// still runs (as it stops, or publishes) leaves when it ends, so that a new one doesn't use the same partial file.
	if t.stop != nil {
		t.stop()
	}
	if !t.running {
		t.closeFile()
		if c.transfers[t.contentID] == t {
			delete(c.transfers, t.contentID)
		}
	}
}

// closeFile closes the partial file once nothing uses it. Callers hold the mutex.
func (t *transfer) closeFile() {
	if !t.closed {
		t.closed = true
		t.file.Close()
	}
}

// resume loads the manifest and verified blocks of a download from before a restart
func (t *transfer) resume() error {
	var manifest, verified []byte
	var size int64
	var count int
	err := t.c.s.store.db.QueryRow(`SELECT size, block_count, manifest, verified FROM transfers WHERE content_id = ?`,
		t.contentID).Scan(&size, &count, &manifest, &verified)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	blocks, err := decodeManifest(manifest, count)
	if err != nil || size != t.size {
		return fmt.Errorf("the stored manifest is unusable: %w", err)
	}
	t.blocks = blocks
	t.verified = make([]bool, len(blocks))
	for i := range blocks {
		if i/8 < len(verified) && verified[i/8]&(1<<(i%8)) != 0 {
			t.verified[i] = true
			t.count++
		}
	}
	return nil
}

func encodeManifest(blocks []remoteclient.Block) []byte {
	out := make([]byte, 0, len(blocks)*36)
	for _, b := range blocks {
		out = append(out, b.Hash[:]...)
		out = binary.BigEndian.AppendUint32(out, uint32(b.Size))
	}
	return out
}

func decodeManifest(data []byte, count int) ([]remoteclient.Block, error) {
	if len(data) != count*36 {
		return nil, errors.New("the manifest has the wrong length")
	}
	blocks := make([]remoteclient.Block, count)
	var offset int64
	for i := range blocks {
		record := data[i*36 : (i+1)*36]
		copy(blocks[i].Hash[:], record[:32])
		blocks[i].Index, blocks[i].Offset, blocks[i].Size = i, offset, int(binary.BigEndian.Uint32(record[32:]))
		offset += int64(blocks[i].Size)
	}
	return blocks, nil
}

func encodeBitmap(verified []bool) []byte {
	out := make([]byte, (len(verified)+7)/8)
	for i, v := range verified {
		if v {
			out[i/8] |= 1 << (i % 8)
		}
	}
	return out
}

func (t *transfer) failed() bool {
	t.mutex.Lock()
	defer t.mutex.Unlock()
	return t.err != nil
}

func (t *transfer) complete() bool {
	t.mutex.Lock()
	defer t.mutex.Unlock()
	return t.done
}

// notify wakes everything waiting for the transfer. Callers hold the mutex.
func (t *transfer) notify() {
	close(t.changed)
	t.changed = make(chan struct{})
}

// start runs the transfer. Callers hold the mutex.
func (t *transfer) start() {
	ctx, cancel := context.WithCancel(t.c.s.ctx)
	t.running, t.stop, t.finished = true, cancel, make(chan struct{})
	go t.run(ctx)
}

func (t *transfer) stopAndWait() {
	t.mutex.Lock()
	stop, finished := t.stop, t.finished
	t.mutex.Unlock()
	if stop != nil {
		stop()
		<-finished
	}
}

func (t *transfer) run(ctx context.Context) {
	defer func() {
		t.c.mutex.Lock()
		t.mutex.Lock()
		t.running = false
		finished := t.finished
		t.notify()
		if t.refs <= 0 {
			t.closeFile()
			if t.c.transfers[t.contentID] == t {
				delete(t.c.transfers, t.contentID)
			}
		}
		t.mutex.Unlock()
		t.c.mutex.Unlock()
		close(finished)
	}()
	backoff := time.Second
	for ctx.Err() == nil {
		// A slot for each attempt, not while waiting to try again, so that downloads from a gateway out of reach
		// don't hold up the others
		t.mutex.Lock()
		slots := t.c.slots
		if !t.read {
			slots = t.c.keepSlots
		}
		t.mutex.Unlock()
		select {
		case slots <- struct{}{}:
		case <-ctx.Done():
			return
		}
		err := t.fetch(ctx)
		if err == nil {
			err = t.publish()
		}
		<-slots
		if err == nil {
			return
		}
		if ctx.Err() != nil {
			return
		}
		if errors.Is(err, ErrVersionChanged) || remoteclient.KindOf(err) == remoteclient.KindUnauthenticated ||
			remoteclient.KindOf(err) == remoteclient.KindForbidden {
			t.fail(err)
			return
		}
		delay := backoff
		var e *remoteclient.Error
		if errors.As(err, &e) && e.RetryAfter > 0 {
			delay = max(delay, e.RetryAfter)
		}
		t.c.s.log.Debug("Download interrupted", "content", t.contentID, "error", err, "retry", delay)
		backoff = min(backoff*2, time.Minute)
		select {
		case <-ctx.Done():
			return
		case <-time.After(delay):
		}
	}
}

func (t *transfer) fail(err error) {
	t.mutex.Lock()
	defer t.mutex.Unlock()
	t.err = err
	t.notify()
}

// lease gets a read lease for the item's current revision, whose content must still be the transfer's
func (t *transfer) lease(ctx context.Context) (remoteapi.ReadLease, *remoteclient.Client, error) {
	var gatewayID, revision, serviceID string
	var contentID sql.NullString
	t.mutex.Lock()
	item := t.item
	t.mutex.Unlock()
	err := t.c.s.store.db.QueryRow(`SELECT r.gateway_id, r.revision, r.content_id, f.service_id FROM remote_items AS r
		JOIN folders AS f ON f.id = r.folder WHERE r.item = ? AND r.deleted = 0`, item).
		Scan(&gatewayID, &revision, &contentID, &serviceID)
	if errors.Is(err, sql.ErrNoRows) || (err == nil && contentID.String != t.contentID) {
		// The item it was started for changed; another with the same content serves as well
		var other int64
		err = t.c.s.store.db.QueryRow(`SELECT r.item, r.gateway_id, r.revision, r.content_id, f.service_id
			FROM remote_items AS r JOIN folders AS f ON f.id = r.folder WHERE r.content_id = ? AND r.deleted = 0
			AND r.item IS NOT NULL LIMIT 1`, t.contentID).Scan(&other, &gatewayID, &revision, &contentID, &serviceID)
		if errors.Is(err, sql.ErrNoRows) {
			return remoteapi.ReadLease{}, nil, ErrVersionChanged
		}
		if err == nil {
			t.mutex.Lock()
			t.item = other
			t.mutex.Unlock()
		}
	}
	if err != nil {
		return remoteapi.ReadLease{}, nil, err
	}
	client, err := t.c.s.client(serviceID)
	if err != nil {
		return remoteapi.ReadLease{}, nil, err
	}
	lease, err := client.CreateReadLease(ctx, gatewayID, revision)
	var e *remoteclient.Error
	if errors.As(err, &e) && e.Kind == remoteclient.KindRevisionMismatch && e.Item != nil &&
		e.Item.ContentID == t.contentID {
		// Only its metadata changed (a touch, a move): the same content under the current revision
		lease, err = client.CreateReadLease(ctx, gatewayID, e.Item.Revision)
	}
	if errors.As(err, &e) && e.Kind == remoteclient.KindRevisionMismatch {
		return remoteapi.ReadLease{}, nil, ErrVersionChanged
	}
	if err != nil {
		return remoteapi.ReadLease{}, nil, err
	}
	if lease.ContentID != t.contentID || lease.Size != t.size {
		return remoteapi.ReadLease{}, nil, ErrVersionChanged
	}
	if lease.Preparation.State == remoteapi.PreparationFailed {
		return remoteapi.ReadLease{}, nil, &remoteclient.Error{Kind: remoteclient.KindUnavailable,
			RetryAfter: time.Duration(lease.Preparation.RetryAfter) * time.Second, Err: ErrContentUnavailable}
	}
	return lease, client, nil
}

// fetch fetches the blocks that aren't verified yet, those reads wait for first
func (t *transfer) fetch(ctx context.Context) error {
	t.mutex.Lock()
	complete := t.blocks != nil && t.count == len(t.blocks)
	t.mutex.Unlock()
	if complete {
		return nil
	}
	lease, client, err := t.lease(ctx)
	if err != nil {
		return err
	}
	t.mutex.Lock()
	haveManifest := t.blocks != nil
	t.mutex.Unlock()
	if !haveManifest {
		blocks, err := client.Manifest(ctx, lease)
		if err != nil {
			return err
		}
		err = t.c.s.store.tx(context.Background(), func(tx *sql.Tx) error {
			_, err := tx.Exec(`INSERT INTO transfers (content_id, item, size, block_count, manifest, verified, updated_at)
				VALUES (?, ?, ?, ?, ?, ?, ?) ON CONFLICT (content_id) DO UPDATE SET item = excluded.item,
				manifest = excluded.manifest, verified = excluded.verified, block_count = excluded.block_count`,
				t.contentID, t.item, t.size, len(blocks), encodeManifest(blocks), encodeBitmap(make([]bool, len(blocks))),
				nowUnix())
			return err
		})
		if err != nil {
			return err
		}
		t.mutex.Lock()
		t.blocks, t.verified, t.count = blocks, make([]bool, len(blocks)), 0
		t.notify()
		t.mutex.Unlock()
	}
	etag := `"` + t.contentID + `"`
	buffer := make([]byte, 0, remoteclient.MaxBlockSize)
	leases := 0
	for {
		first, last, ok := t.nextRange()
		if !ok {
			return nil
		}
		start := t.blocks[first].Offset
		end := t.blocks[last].Offset + int64(t.blocks[last].Size) - 1
		r, err := client.ReadRange(ctx, lease.Lease, "", start, end, etag)
		if errors.Is(err, remoteclient.ErrLeaseExpired) && leases < 3 {
			// Expired, or the gateway no longer has the leased content prepared: lease again (the content must be the
			// same) and go on with the blocks verified so far. Leases that keep expiring at once wait, as other
			// errors do.
			leases++
			if lease, client, err = t.lease(ctx); err != nil {
				return err
			}
			continue
		}
		if err != nil {
			return err
		}
		leases = 0
		if r.ETag != etag {
			r.Body.Close()
			return remoteclient.ErrProtocolViolation
		}
		for i := first; i <= last; i++ {
			b := t.blocks[i]
			data := buffer[:b.Size]
			if _, err := io.ReadFull(r.Body, data); err != nil {
				r.Body.Close()
				return &remoteclient.Error{Kind: remoteclient.KindUnreachable, Message: "the content stopped", Err: err}
			}
			if err := remoteclient.VerifyBlock(b, data); err != nil {
				r.Body.Close()
				return err
			}
			if _, err := t.file.WriteAt(data, b.Offset); err != nil {
				r.Body.Close()
				return err
			}
			// Readers may have it at once; the record of it waits until it is on disk
			t.mutex.Lock()
			if !t.verified[i] {
				t.verified[i] = true
				t.count++
			}
			t.notify()
			t.mutex.Unlock()
		}
		r.Body.Close()
		if err := t.file.Sync(); err != nil {
			return err
		}
		t.mutex.Lock()
		bitmap := encodeBitmap(t.verified)
		t.mutex.Unlock()
		// Recorded even when the transfer is stopping: the blocks are on disk
		if err := t.c.s.store.tx(context.Background(), func(tx *sql.Tx) error {
			_, err := tx.Exec(`UPDATE transfers SET verified = ?, updated_at = ? WHERE content_id = ?`, bitmap,
				nowUnix(), t.contentID)
			return err
		}); err != nil {
			return err
		}
	}
}

// nextRange picks the blocks to ask for next: the first block a read waits for, or else the first missing one, and
// the missing blocks after it, up to a range's size
func (t *transfer) nextRange() (first, last int, ok bool) {
	t.mutex.Lock()
	defer t.mutex.Unlock()
	first = -1
	limit := int64(rangeBytes)
	for len(t.wanted) > 0 {
		if w := t.wanted[0]; !t.verified[w] {
			first, limit = w, urgentRangeBytes
			break
		}
		t.wanted = t.wanted[1:]
	}
	if first < 0 {
		for i, v := range t.verified {
			if !v {
				first = i
				break
			}
		}
	}
	if first < 0 {
		return 0, 0, false
	}
	last = first
	total := int64(t.blocks[first].Size)
	for last+1 < len(t.blocks) && !t.verified[last+1] && total+int64(t.blocks[last+1].Size) <= limit {
		last++
		total += int64(t.blocks[last].Size)
	}
	return first, last, true
}

// publish checks the complete file against the manifest once more and moves it into the cache
func (t *transfer) publish() error {
	if err := t.file.Sync(); err != nil {
		return err
	}
	buffer := make([]byte, remoteclient.MaxBlockSize)
	bad := 0
	for i, b := range t.blocks {
		data := buffer[:b.Size]
		if _, err := t.file.ReadAt(data, b.Offset); err != nil {
			return err
		}
		if remoteclient.VerifyBlock(b, data) != nil {
			// Not what was verified (a power loss, a disk error): fetched again, with every other one like it
			t.mutex.Lock()
			t.verified[i] = false
			t.count--
			t.mutex.Unlock()
			bad++
		}
	}
	if bad > 0 {
		t.mutex.Lock()
		bitmap := encodeBitmap(t.verified)
		t.mutex.Unlock()
		_ = t.c.s.store.tx(context.Background(), func(tx *sql.Tx) error {
			_, err := tx.Exec(`UPDATE transfers SET verified = ? WHERE content_id = ?`, bitmap, t.contentID)
			return err
		})
		return fmt.Errorf("%d blocks of %s changed on disk", bad, t.contentID)
	}
	err := os.Rename(t.c.partialPath(t.contentID), t.c.contentPath(t.contentID))
	if errors.Is(err, os.ErrNotExist) {
		// Published already, by an attempt whose record of it failed: the record is what is left to do
		_, err = os.Stat(t.c.contentPath(t.contentID))
	}
	if err != nil {
		return err
	}
	if dir, err := os.Open(t.c.contentDir); err == nil {
		_ = dir.Sync()
		dir.Close()
	}
	err = t.c.s.store.tx(context.Background(), func(tx *sql.Tx) error {
		if _, err := tx.Exec(`INSERT INTO content (content_id, size, last_used) VALUES (?, ?, ?)
			ON CONFLICT (content_id) DO UPDATE SET last_used = excluded.last_used`, t.contentID, t.size,
			nowUnix()); err != nil {
			return err
		}
		_, err := tx.Exec(`DELETE FROM transfers WHERE content_id = ?`, t.contentID)
		return err
	})
	if err != nil {
		return err
	}
	t.mutex.Lock()
	t.done = true
	t.notify()
	t.mutex.Unlock()
	t.c.poke()
	return nil
}

// wait waits until the blocks covering n bytes at off are verified
func (t *transfer) wait(ctx context.Context, off int64, n int) error {
	deadline := time.NewTimer(readWait)
	defer deadline.Stop()
	for {
		t.mutex.Lock()
		if t.done {
			t.mutex.Unlock()
			return nil
		}
		if t.err != nil {
			err := t.err
			t.mutex.Unlock()
			return err
		}
		if t.blocks != nil {
			first := sort.Search(len(t.blocks), func(i int) bool {
				return t.blocks[i].Offset+int64(t.blocks[i].Size) > off
			})
			ready := true
			for i := first; i < len(t.blocks) && t.blocks[i].Offset < off+int64(n); i++ {
				if !t.verified[i] {
					if ready {
						// The first missing block goes to the front of the queue
						t.wanted = append([]int{i}, t.wanted...)
					}
					ready = false
					break
				}
			}
			if ready {
				t.mutex.Unlock()
				return nil
			}
		}
		if !t.running {
			if t.c.s.ctx.Err() != nil {
				t.mutex.Unlock()
				return ErrContentUnavailable
			}
			t.start()
		}
		changed := t.changed
		t.mutex.Unlock()
		select {
		case <-changed:
		case <-ctx.Done():
			return ctx.Err()
		case <-deadline.C:
			t.c.s.notify(Notice{Kind: "unreachable", Message: "A file can't be opened: its gateway can't be reached, " +
				"or can't provide it now."})
			return ErrContentUnavailable
		}
	}
}

// State is how an item is here: "remote" (its content isn't here), "partial", "local", "kept" (local, and kept on
// this device on purpose), "pending" (changed here, not committed by the gateway yet), "failed" or "blocked" (a
// change that can't be sent, waiting to be exported or discarded) or "local-only" (never sent)
func (s *Service) State(e Entry) (string, error) {
	var local int
	var review, pending bool
	err := s.store.db.QueryRow(`SELECT local,
		EXISTS (SELECT 1 FROM outbox WHERE item = items.id AND state IN ('failed', 'unknown')),
		EXISTS (SELECT 1 FROM outbox WHERE item = items.id AND state IN ('queued', 'uploaded', 'submitted', 'pending')
			) OR EXISTS (SELECT 1 FROM work WHERE item = items.id AND detached = 0)
		FROM items WHERE id = ?`, e.Item).Scan(&local, &review, &pending)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return "", err
	}
	var blocked bool
	if err := s.store.db.QueryRow(`SELECT EXISTS (SELECT 1 FROM outbox WHERE item = ? AND state = 'blocked')`,
		e.Item).Scan(&blocked); err != nil {
		return "", err
	}
	switch {
	case local&localOnly != 0:
		return "local-only", nil
	case blocked:
		return "blocked", nil
	case review:
		return "failed", nil
	case pending || local != 0:
		return "pending", nil
	}
	if e.IsDir() {
		if e.Keep {
			return "kept", nil
		}
		return "", nil
	}
	here := e.Size == 0
	if !here {
		if here, err = s.cache.cached(e.ContentID); err != nil {
			return "", err
		}
	}
	switch {
	case here && e.Keep:
		return "kept", nil
	case here:
		return "local", nil
	}
	var partial bool
	err = s.store.db.QueryRow(`SELECT EXISTS (SELECT 1 FROM transfers WHERE content_id = ?)`, e.ContentID).Scan(&partial)
	if partial {
		return "partial", err
	}
	return "remote", err
}

// SetKeep keeps an item on this device (a directory with everything below it), downloading it, or stops keeping it,
// removing its downloaded content unless something reads it
func (s *Service) SetKeep(item int64, keep bool) error {
	err := s.store.tx(context.Background(), func(tx *sql.Tx) error {
		res, err := tx.Exec(`UPDATE items SET keep = ? WHERE id = ? AND deleted = 0`, keep, item)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return fs.ErrNotExist
		}
		if !keep {
			// Kept because a directory above it is: only that directory can stop it
			var above sql.NullString
			if err := tx.QueryRow(`WITH RECURSIVE up(id) AS (SELECT parent FROM items WHERE id = ?
					UNION ALL SELECT i.parent FROM items AS i JOIN up ON i.id = up.id WHERE i.parent IS NOT NULL)
				SELECT i.name FROM items AS i JOIN up ON i.id = up.id WHERE i.keep = 1 AND i.deleted = 0 LIMIT 1`,
				item).Scan(&above); err != nil && !errors.Is(err, sql.ErrNoRows) {
				return err
			}
			if above.Valid {
				return fmt.Errorf("%w: it is kept with %q, which contains it", ErrKeptAbove, above.String)
			}
			// Stop keeping it all: nothing below keeps its own mark
			_, err = tx.Exec(`WITH RECURSIVE sub(id) AS (SELECT id FROM items WHERE parent = ? AND deleted = 0
				UNION ALL SELECT i.id FROM items AS i JOIN sub ON i.parent = sub.id WHERE i.deleted = 0)
				UPDATE items SET keep = 0 WHERE id IN (SELECT id FROM sub)`, item)
		}
		return err
	})
	if err != nil {
		return err
	}
	if keep {
		s.cache.poke()
		return nil
	}
	return s.cache.evictBelow(item)
}

// ErrKeptAbove refuses to stop keeping an item that is kept because a directory containing it is
var ErrKeptAbove = errors.New("it is kept with a directory that contains it")

// keptContent is the content of everything kept on this device
const keptContent = `WITH RECURSIVE kept(id) AS (
		SELECT id FROM items WHERE keep = 1 AND deleted = 0
		UNION SELECT i.id FROM items AS i JOIN kept ON i.parent = kept.id WHERE i.deleted = 0)
	SELECT DISTINCT i.content_id FROM items AS i JOIN kept ON kept.id = i.id
	WHERE i.kind = 0 AND i.content_id IS NOT NULL AND i.size > 0`

// downloadKept starts downloads of kept files whose content isn't here
func (c *cache) downloadKept() error {
	rows, err := c.s.store.db.Query(`WITH RECURSIVE kept(id) AS (
			SELECT id FROM items WHERE keep = 1 AND deleted = 0
			UNION SELECT i.id FROM items AS i JOIN kept ON i.parent = kept.id WHERE i.deleted = 0)
		SELECT i.id, i.content_id, i.size FROM items AS i JOIN kept ON kept.id = i.id
		WHERE i.kind = 0 AND i.size > 0 AND i.content_id IS NOT NULL
			AND NOT EXISTS (SELECT 1 FROM content AS c WHERE c.content_id = i.content_id)
		GROUP BY i.content_id LIMIT 64`)
	if err != nil {
		return err
	}
	type missing struct {
		item      int64
		contentID string
		size      int64
	}
	var list []missing
	for rows.Next() {
		var m missing
		if err := rows.Scan(&m.item, &m.contentID, &m.size); err != nil {
			rows.Close()
			return err
		}
		list = append(list, m)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	for _, m := range list {
		c.mutex.Lock()
		t, running := c.transfers[m.contentID]
		c.mutex.Unlock()
		if running && !t.failed() {
			continue
		}
		t, err := c.transferFor(m.contentID, m.item, m.size, true)
		if errors.Is(err, ErrContentUnavailable) {
			continue
		}
		if err != nil {
			return err
		}
		// The transfer holds itself until it is done; then it is released
		t.mutex.Lock()
		finished := t.finished
		t.mutex.Unlock()
		if finished == nil {
			// Complete already
			c.release(t)
			continue
		}
		go func() {
			<-finished
			c.release(t)
		}()
	}
	return nil
}

// evict trims the cache to its budget, least recently used first. Content that nothing shows any more goes first;
// content that is open, or kept on this device, never.
func (c *cache) evict() error {
	kept, err := c.contentSet(keptContent)
	if err != nil {
		return err
	}
	rows, err := c.s.store.db.Query(`SELECT c.content_id, c.size,
		EXISTS (SELECT 1 FROM items AS i WHERE i.content_id = c.content_id AND i.deleted = 0)
		FROM content AS c ORDER BY c.last_used`)
	if err != nil {
		return err
	}
	type entry struct {
		contentID string
		size      int64
		shown     bool
	}
	var entries []entry
	var total int64
	for rows.Next() {
		var e entry
		if err := rows.Scan(&e.contentID, &e.size, &e.shown); err != nil {
			rows.Close()
			return err
		}
		if !kept[e.contentID] {
			entries = append(entries, e)
			total += e.size
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	sort.SliceStable(entries, func(i, j int) bool { return !entries[i].shown && entries[j].shown })
	for _, e := range entries {
		if total <= c.s.cacheBudget() && e.shown {
			break
		}
		removed, err := c.remove(e.contentID)
		if err != nil {
			return err
		}
		if removed {
			total -= e.size
		}
	}
	return nil
}

// evictBelow removes the cached content of an item and everything below it, unless it is open or kept otherwise
func (c *cache) evictBelow(item int64) error {
	below, err := c.contentSet(`WITH RECURSIVE sub(id) AS (SELECT ?1
			UNION ALL SELECT i.id FROM items AS i JOIN sub ON i.parent = sub.id WHERE i.deleted = 0)
		SELECT DISTINCT i.content_id FROM items AS i JOIN sub ON sub.id = i.id WHERE i.content_id IS NOT NULL`, item)
	if err != nil {
		return err
	}
	kept, err := c.contentSet(keptContent)
	if err != nil {
		return err
	}
	for contentID := range below {
		if !kept[contentID] {
			if _, err := c.remove(contentID); err != nil {
				return err
			}
		}
	}
	return nil
}

func (c *cache) contentSet(query string, args ...any) (map[string]bool, error) {
	rows, err := c.s.store.db.Query(query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	set := map[string]bool{}
	for rows.Next() {
		var contentID string
		if err := rows.Scan(&contentID); err != nil {
			return nil, err
		}
		set[contentID] = true
	}
	return set, rows.Err()
}

// remove removes content from the cache unless it is open, and reports whether it did
func (c *cache) remove(contentID string) (bool, error) {
	c.mutex.Lock()
	defer c.mutex.Unlock()
	if c.pinned[contentID] > 0 {
		return false, nil
	}
	err := c.s.store.tx(context.Background(), func(tx *sql.Tx) error {
		_, err := tx.Exec(`DELETE FROM content WHERE content_id = ?`, contentID)
		return err
	})
	if err != nil {
		return false, err
	}
	if err := os.Remove(c.contentPath(contentID)); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return false, err
	}
	return true, nil
}

// FromGateway reports whether the reader reads content as the gateway committed it (rather than content written
// here), which stays the same as long as its node does
func (r *Reader) FromGateway() bool {
	return r.contentID != ""
}
