// Copyright (C) 2026 Gordon
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this file,
// You can obtain one at https://mozilla.org/MPL/2.0/.

package gateway

import (
	"bytes"
	"context"
	"image"
	"image/color"
	_ "image/gif" // decoders for thumbnails
	"image/jpeg"
	_ "image/png"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/syncthing/syncthing/lib/osutil"
	"github.com/syncthing/syncthing/lib/protocol"
)

// ThumbnailRenderer renders thumbnails of complete local files
type ThumbnailRenderer interface {
	// Features returns the thumbnails.* capability features the renderer provides
	Features() []string
	// Supports reports whether the renderer can render a file with this name, and whether it needs only part of it
	// (video renderers read the beginning); a file it can't read locally is then not prepared just for a thumbnail
	Supports(name string) (supported bool, localOnly bool)
	// Render returns a JPEG whose longer side is at most size pixels
	Render(ctx context.Context, path string, size int) ([]byte, error)
}

const (
	thumbnailDefaultSize = 256
	thumbnailMaxSize     = 1024
	// Larger files are only rendered when this device has them, never fetched from peers for a thumbnail
	thumbnailMaxFetched = 64 << 20
	thumbnailTimeout    = 30 * time.Second
	// Cached thumbnails not used for this long are removed
	thumbnailRetention = 30 * 24 * time.Hour
)

// Sizes thumbnails are rendered and cached at; a request gets the next larger one
var thumbnailSizes = []int{64, 128, 256, 512, thumbnailMaxSize}

func (s *Service) handleThumbnail(w http.ResponseWriter, r *http.Request, p principal) error {
	if err := requireCredential(p); err != nil {
		return err
	}
	renderer := s.cfg.Thumbnails
	row, access, err := s.itemForClient(p.clientID, r.PathValue("itemID"))
	if err != nil {
		return err
	}
	if renderer == nil || row.deleted || row.kind != protocol.FileInfoTypeFile {
		return errNotFound
	}
	supported, localOnly := renderer.Supports(row.path)
	if !supported {
		return errNotFound
	}
	if r.URL.Query().Get("revision") != row.revision {
		item := row.toItem(access.write)
		return revisionMismatch(&item)
	}
	size := thumbnailDefaultSize
	if v := r.URL.Query().Get("size"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 16 {
			return newAPIError(http.StatusBadRequest, codeInvalidRequest, "invalid size")
		}
		size = thumbnailMaxSize
		for _, bucket := range thumbnailSizes {
			if bucket >= n {
				size = bucket
				break
			}
		}
	}

	cached := filepath.Join(s.thumbnailDir(), b64.EncodeToString(secretHash(row.contentID))[:32]+"-"+strconv.Itoa(size)+".jpg")
	if data, err := os.ReadFile(cached); err == nil {
		now := time.Now()
		_ = os.Chtimes(cached, now, now)
		return writeThumbnail(w, data)
	}

	file, ok, err := s.backend.GlobalFile(row.folder, row.path)
	if err != nil {
		return err
	}
	if !ok || contentIDOf(file) != row.contentID {
		item := row.toItem(access.write)
		return revisionMismatch(&item)
	}
	source := ""
	rep := &representation{m: s.content, folder: row.folder, file: file,
		localPath: filepath.Join(access.info.NativePath, osutil.NativeFilename(file.Name))}
	if rep.localMatches() {
		source = rep.localPath
	} else if localOnly || file.Size > thumbnailMaxFetched {
		return errContentUnavailable
	} else {
		prepared, err := s.content.get(access.info, file)
		if err != nil {
			return err
		}
		for i := range file.Blocks {
			if err := prepared.waitFor(r.Context(), i); err != nil {
				return err
			}
		}
		// Renderers recognize files by their extension, which spool files (shared by content) don't have
		link := filepath.Join(filepath.Dir(prepared.spoolPath), newID("thm_")+filepath.Ext(file.Name))
		if err := os.Link(prepared.spoolPath, link); err != nil {
			return err
		}
		defer os.Remove(link)
		source = link
	}

	// One thumbnail at a time: they must never delay reading or saving files
	select {
	case s.thumbnailSlot <- struct{}{}:
		defer func() { <-s.thumbnailSlot }()
	case <-r.Context().Done():
		return nil
	}
	ctx, cancel := context.WithTimeout(r.Context(), thumbnailTimeout)
	defer cancel()
	data, err := renderer.Render(ctx, source, size)
	if err != nil {
		s.log.Debug("Could not render a thumbnail", "error", err)
		return errNotFound
	}
	if err := os.MkdirAll(filepath.Dir(cached), 0o700); err == nil {
		_ = writeFileAtomic(cached, data, 0o600)
	}
	return writeThumbnail(w, data)
}

func (s *Service) thumbnailDir() string {
	return filepath.Join(s.cfg.Home, "thumbnails")
}

// pruneThumbnails removes cached thumbnails nobody asked for in a while (their content may be long gone)
func (s *Service) pruneThumbnails() {
	entries, err := os.ReadDir(s.thumbnailDir())
	if err != nil {
		return
	}
	for _, entry := range entries {
		if info, err := entry.Info(); err == nil && time.Since(info.ModTime()) > thumbnailRetention {
			_ = os.Remove(filepath.Join(s.thumbnailDir(), entry.Name()))
		}
	}
}

func writeThumbnail(w http.ResponseWriter, data []byte) error {
	w.Header().Set("Content-Type", "image/jpeg")
	w.Header().Set("Content-Length", strconv.Itoa(len(data)))
	w.WriteHeader(http.StatusOK)
	_, err := w.Write(data)
	return err
}

// CommandRenderer renders images with Go's decoders, and videos and PDFs with ffmpeg and pdftoppm where they are
// installed
type CommandRenderer struct {
	ffmpeg   string
	pdftoppm string
}

func NewCommandRenderer() *CommandRenderer {
	r := &CommandRenderer{}
	r.ffmpeg, _ = exec.LookPath("ffmpeg")
	r.pdftoppm, _ = exec.LookPath("pdftoppm")
	return r
}

var (
	imageExtensions = []string{".jpg", ".jpeg", ".png", ".gif"}
	videoExtensions = []string{".mp4", ".m4v", ".mov", ".mkv", ".avi", ".webm", ".3gp"}
)

func (r *CommandRenderer) Features() []string {
	features := []string{featureThumbnailsImage}
	if r.ffmpeg != "" {
		features = append(features, featureThumbnailsVideo)
	}
	if r.pdftoppm != "" {
		features = append(features, featureThumbnailsPDF)
	}
	return features
}

func (r *CommandRenderer) Supports(name string) (bool, bool) {
	ext := strings.ToLower(filepath.Ext(name))
	switch {
	case containsString(imageExtensions, ext):
		return true, false
	case containsString(videoExtensions, ext):
		return r.ffmpeg != "", true
	case ext == ".pdf":
		return r.pdftoppm != "", false
	}
	return false, false
}

func (r *CommandRenderer) Render(ctx context.Context, path string, size int) ([]byte, error) {
	ext := strings.ToLower(filepath.Ext(path))
	switch {
	case containsString(videoExtensions, ext):
		scale := "scale='if(gt(iw,ih)," + strconv.Itoa(size) + ",-2)':'if(gt(iw,ih),-2," + strconv.Itoa(size) + ")'"
		// ffmpeg recognizes formats by content: only plain video containers, reading only the file itself, so that a
		// playlist or manifest disguised as a video can't make it read other files or fetch URLs
		cmd := exec.CommandContext(ctx, r.ffmpeg, "-nostdin", "-loglevel", "error",
			"-protocol_whitelist", "file", "-format_whitelist", "mov,mp4,m4a,3gp,3g2,mj2,matroska,webm,avi",
			"-ss", "1", "-i", "file:"+path,
			"-frames:v", "1", "-vf", scale, "-f", "image2", "-c:v", "mjpeg", "-")
		return cmd.Output()
	case ext == ".pdf":
		cmd := exec.CommandContext(ctx, r.pdftoppm, "-jpeg", "-singlefile", "-f", "1", "-l", "1", "-scale-to",
			strconv.Itoa(size), path)
		return cmd.Output()
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	cfg, _, err := image.DecodeConfig(f)
	if err != nil {
		return nil, err
	}
	// Bounded decoding: refuse images whose pixels would need too much memory
	if cfg.Width*cfg.Height > 64_000_000 {
		return nil, os.ErrInvalid
	}
	if _, err := f.Seek(0, 0); err != nil {
		return nil, err
	}
	img, _, err := image.Decode(f)
	if err != nil {
		return nil, err
	}
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, downscale(img, size), &jpeg.Options{Quality: 80}); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// downscale scales an image so its longer side is at most size, averaging the pixels each output pixel covers
func downscale(src image.Image, size int) image.Image {
	b := src.Bounds()
	w, h := b.Dx(), b.Dy()
	if w <= size && h <= size {
		return src
	}
	var nw, nh int
	if w >= h {
		nw, nh = size, max(1, h*size/w)
	} else {
		nw, nh = max(1, w*size/h), size
	}
	dst := image.NewRGBA(image.Rect(0, 0, nw, nh))
	for y := 0; y < nh; y++ {
		y0, y1 := b.Min.Y+y*h/nh, b.Min.Y+max((y+1)*h/nh, y*h/nh+1)
		for x := 0; x < nw; x++ {
			x0, x1 := b.Min.X+x*w/nw, b.Min.X+max((x+1)*w/nw, x*w/nw+1)
			var r, g, bl, a, n uint64
			for sy := y0; sy < y1; sy++ {
				for sx := x0; sx < x1; sx++ {
					cr, cg, cb, ca := src.At(sx, sy).RGBA()
					r, g, bl, a, n = r+uint64(cr), g+uint64(cg), bl+uint64(cb), a+uint64(ca), n+1
				}
			}
			dst.Set(x, y, color.RGBA64{R: uint16(r / n), G: uint16(g / n), B: uint16(bl / n), A: uint16(a / n)})
		}
	}
	return dst
}

func containsString(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}
