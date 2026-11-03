// Copyright (C) 2026 Gordon
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this file,
// You can obtain one at https://mozilla.org/MPL/2.0/.

package files

import (
	"bytes"
	"context"
	"crypto/md5"
	"database/sql"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"hash/crc32"
	"image"
	"image/draw"
	_ "image/jpeg" // the gateway's thumbnails are JPEGs
	"image/png"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"t-shaped.nl/sushitrain/v2/remoteclient"
)

// GNOME (Nautilus, GTK's file chooser) treats the mount as a local disk, and makes thumbnails by reading files, which
// would fetch every file it shows. The thumbnails the gateway renders go into the freedesktop thumbnail cache instead,
// where GNOME finds them first; thumbnailers are refused content that isn't here (fusefs).

// thumbnailSizes are the cache's directories and the longer side of the thumbnails in them
var thumbnailSizes = []struct {
	dir  string
	size int
}{{"xx-large", 1024}, {"x-large", 512}, {"large", 256}, {"normal", 128}}

// thumbnailExtensions are the kinds of files a gateway may render (images always; video and PDF with ffmpeg and
// pdftoppm on the gateway). Others are left alone.
var thumbnailExtensions = map[string]bool{
	".jpg": true, ".jpeg": true, ".png": true, ".gif": true, ".webp": true, ".heic": true, ".heif": true,
	".tif": true, ".tiff": true, ".bmp": true, ".avif": true, ".mp4": true, ".mov": true, ".m4v": true, ".mkv": true,
	".webm": true, ".avi": true, ".pdf": true,
}

type thumbnailJob struct {
	item  int64
	path  string
	mtime int64
}

type thumbnailer struct {
	s     *Service
	dir   string
	queue chan thumbnailJob
	mutex sync.Mutex
	// What was written or is queued, by the URI's hash, with the modification time it was for
	done map[string]int64
}

func newThumbnailer(s *Service) *thumbnailer {
	dir := s.cfg.ThumbnailDir
	if dir == "" {
		cache, err := os.UserCacheDir()
		if err != nil {
			return nil
		}
		dir = filepath.Join(cache, "thumbnails")
	}
	return &thumbnailer{s: s, dir: dir, queue: make(chan thumbnailJob, 4096), done: map[string]int64{}}
}

// FillThumbnails puts the gateway's thumbnails of the files of a listed directory, at dirPath on disk, into the
// thumbnail cache, in the background
func (s *Service) FillThumbnails(dirPath string, entries []Entry) {
	t := s.thumbnails
	if t == nil {
		return
	}
	for _, e := range entries {
		if e.Kind != kindFile || e.Size == 0 || !thumbnailExtensions[strings.ToLower(filepath.Ext(e.Name))] {
			continue
		}
		path := filepath.Join(dirPath, e.Name)
		key := thumbnailKey(fileURI(path))
		mtime := e.ModTime.Unix()
		t.mutex.Lock()
		known, ok := t.done[key]
		if !ok || known != mtime {
			t.done[key] = mtime
		}
		t.mutex.Unlock()
		if ok && known == mtime {
			continue
		}
		if have, _ := pngTextValue(filepath.Join(t.dir, "large", key+".png"), "Thumb::MTime"); have ==
			strconv.FormatInt(mtime, 10) {
			continue
		}
		select {
		case t.queue <- thumbnailJob{item: e.Item, path: path, mtime: mtime}:
		default:
			// Too many at once: what didn't fit is tried when it is listed again
			t.mutex.Lock()
			delete(t.done, key)
			t.mutex.Unlock()
		}
	}
}

func (t *thumbnailer) run(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case job := <-t.queue:
			if err := t.fill(ctx, job); err != nil && ctx.Err() == nil {
				t.s.log.Debug("No thumbnail from the gateway", "path", job.path, "error", err)
				t.mutex.Lock()
				delete(t.done, thumbnailKey(fileURI(job.path)))
				t.mutex.Unlock()
			}
		}
	}
}

// fill writes a file's thumbnails, or a failure GNOME doesn't try again when the gateway can't render it
func (t *thumbnailer) fill(ctx context.Context, job thumbnailJob) error {
	var gatewayID, revision, serviceID string
	err := t.s.store.db.QueryRow(`SELECT r.gateway_id, r.revision, f.service_id FROM remote_items AS r
		JOIN items AS i ON i.id = r.item JOIN folders AS f ON f.id = i.folder
		WHERE r.item = ? AND i.local = 0 AND i.local_content IS NULL`, job.item).Scan(&gatewayID, &revision, &serviceID)
	if errors.Is(err, sql.ErrNoRows) {
		// Changed here: GNOME makes its thumbnail from the content, which is here
		return nil
	}
	if err != nil {
		return err
	}
	client, err := t.s.client(serviceID)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, time.Minute)
	defer cancel()
	data, err := client.Thumbnail(ctx, gatewayID, revision, thumbnailSizes[0].size)
	uri := fileURI(job.path)
	key := thumbnailKey(uri)
	if remoteclient.KindOf(err) == remoteclient.KindNotFound {
		// The gateway can't render this kind of file
		return t.write(filepath.Join(t.dir, "fail", "gnome-thumbnail-factory"), key, image.NewRGBA(image.Rect(0, 0,
			1, 1)), uri, job.mtime)
	}
	if err != nil {
		return err
	}
	img, _, err := image.Decode(bytes.NewReader(data))
	if err != nil {
		return err
	}
	for _, size := range thumbnailSizes {
		if err := t.write(filepath.Join(t.dir, size.dir), key, downscale(img, size.size), uri, job.mtime); err != nil {
			return err
		}
	}
	return nil
}

// write writes a thumbnail with the attributes the freedesktop specification checks it by
func (t *thumbnailer) write(dir, key string, img image.Image, uri string, mtime int64) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		return err
	}
	data, err := withPNGText(buf.Bytes(), map[string]string{"Thumb::URI": uri,
		"Thumb::MTime": strconv.FormatInt(mtime, 10), "Software": "Synctrain"})
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".synctrain-*.png")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), filepath.Join(dir, key+".png"))
}

// fileURI is the URI of a local path as GLib's g_filename_to_uri writes it, which the thumbnail cache is keyed by
func fileURI(path string) string {
	const hex = "0123456789ABCDEF"
	var b strings.Builder
	b.WriteString("file://")
	for i := 0; i < len(path); i++ {
		c := path[i]
		if uriPathSafe(c) {
			b.WriteByte(c)
		} else {
			b.WriteByte('%')
			b.WriteByte(hex[c>>4])
			b.WriteByte(hex[c&15])
		}
	}
	return b.String()
}

// uriPathSafe reports whether GLib leaves a byte of a path unescaped (its UNSAFE_PATH table)
func uriPathSafe(c byte) bool {
	switch {
	case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9':
		return true
	}
	return strings.IndexByte("!$&'()*+,-./:=@_~", c) >= 0
}

func thumbnailKey(uri string) string {
	sum := md5.Sum([]byte(uri))
	return hex.EncodeToString(sum[:])
}

// withPNGText inserts tEXt chunks after a PNG's header chunk
func withPNGText(data []byte, text map[string]string) ([]byte, error) {
	const signature = 8
	if len(data) < signature+12 {
		return nil, errors.New("not a PNG")
	}
	headerEnd := signature + 8 + int(binary.BigEndian.Uint32(data[signature:])) + 4
	if headerEnd > len(data) {
		return nil, errors.New("not a PNG")
	}
	out := append([]byte{}, data[:headerEnd]...)
	for _, key := range []string{"Thumb::URI", "Thumb::MTime", "Software"} {
		value, ok := text[key]
		if !ok {
			continue
		}
		chunk := append([]byte(key), 0)
		chunk = append(chunk, value...)
		out = binary.BigEndian.AppendUint32(out, uint32(len(chunk)))
		typed := append([]byte("tEXt"), chunk...)
		out = append(out, typed...)
		out = binary.BigEndian.AppendUint32(out, crc32.ChecksumIEEE(typed))
	}
	return append(out, data[headerEnd:]...), nil
}

// pngTextValue reads a tEXt value of a PNG file ("" when it has none)
func pngTextValue(path, key string) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	for pos := 8; pos+8 <= len(data); {
		length := int(binary.BigEndian.Uint32(data[pos:]))
		kind := string(data[pos+4 : pos+8])
		if pos+8+length > len(data) || kind == "IDAT" {
			break
		}
		if kind == "tEXt" {
			chunk := data[pos+8 : pos+8+length]
			if k, v, ok := bytes.Cut(chunk, []byte{0}); ok && string(k) == key {
				return string(v), nil
			}
		}
		pos += 12 + length
	}
	return "", nil
}

// downscale shrinks an image so that its longer side is at most size, averaging the pixels it combines
func downscale(src image.Image, size int) image.Image {
	b := src.Bounds()
	w, h := b.Dx(), b.Dy()
	if w <= size && h <= size {
		return src
	}
	nw, nh := size, h*size/w
	if h > w {
		nw, nh = w*size/h, size
	}
	nw, nh = max(nw, 1), max(nh, 1)
	rgba := image.NewRGBA(image.Rect(0, 0, w, h))
	draw.Draw(rgba, rgba.Bounds(), src, b.Min, draw.Src)
	dst := image.NewRGBA(image.Rect(0, 0, nw, nh))
	for y := range nh {
		y0, y1 := y*h/nh, max((y+1)*h/nh, y*h/nh+1)
		for x := range nw {
			x0, x1 := x*w/nw, max((x+1)*w/nw, x*w/nw+1)
			var r, g, bl, a, n uint32
			for sy := y0; sy < y1; sy++ {
				row := rgba.Pix[sy*rgba.Stride:]
				for sx := x0; sx < x1; sx++ {
					p := row[sx*4 : sx*4+4]
					r, g, bl, a, n = r+uint32(p[0]), g+uint32(p[1]), bl+uint32(p[2]), a+uint32(p[3]), n+1
				}
			}
			o := dst.Pix[y*dst.Stride+x*4:]
			o[0], o[1], o[2], o[3] = uint8(r/n), uint8(g/n), uint8(bl/n), uint8(a/n)
		}
	}
	return dst
}

// FileURI is the URI of a local path as GLib writes it (g_filename_to_uri), for GTK's bookmarks and the thumbnail
// cache
func FileURI(path string) string {
	return fileURI(path)
}
