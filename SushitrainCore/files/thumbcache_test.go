// Copyright (C) 2026 Gordon
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this file,
// You can obtain one at https://mozilla.org/MPL/2.0/.

package files

import (
	"bytes"
	"image"
	"image/color"
	"image/png"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"t-shaped.nl/sushitrain/v2/gateway/gatewaytest"
)

// The URIs the thumbnail cache is keyed by, as GLib's g_filename_to_uri writes them
func TestFileURI(t *testing.T) {
	for path, want := range map[string]string{
		"/home/gordon/Synctrain/Photos/a b.jpg": "file:///home/gordon/Synctrain/Photos/a%20b.jpg",
		"/home/gordon/Synctrain/Photos/été.jpg": "file:///home/gordon/Synctrain/Photos/%C3%A9t%C3%A9.jpg",
		"/x/!$&'()*+,-.:=@_~":                   "file:///x/!$&'()*+,-.:=@_~",
		"/x/#?%;<>[]^`{|}\"\\":                  "file:///x/%23%3F%25%3B%3C%3E%5B%5D%5E%60%7B%7C%7D%22%5C",
	} {
		if got := fileURI(path); got != want {
			t.Errorf("fileURI(%q) = %q, want %q", path, got, want)
		}
	}
}

func TestPNGText(t *testing.T) {
	var buf bytes.Buffer
	png.Encode(&buf, image.NewRGBA(image.Rect(0, 0, 4, 4)))
	data, err := withPNGText(buf.Bytes(), map[string]string{"Thumb::URI": "file:///a", "Thumb::MTime": "42"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := png.Decode(bytes.NewReader(data)); err != nil {
		t.Fatalf("the PNG with text doesn't decode: %v", err)
	}
	path := filepath.Join(t.TempDir(), "t.png")
	os.WriteFile(path, data, 0o600)
	if v, _ := pngTextValue(path, "Thumb::MTime"); v != "42" {
		t.Fatalf("Thumb::MTime is %q", v)
	}
	if v, _ := pngTextValue(path, "Thumb::URI"); v != "file:///a" {
		t.Fatalf("Thumb::URI is %q", v)
	}
	big := image.NewRGBA(image.Rect(0, 0, 2000, 1000))
	if b := downscale(big, 256).Bounds(); b.Dx() != 256 || b.Dy() != 128 {
		t.Fatalf("downscaled to %v", b)
	}
}

func TestThumbnails(t *testing.T) {
	img := image.NewRGBA(image.Rect(0, 0, 800, 600))
	for y := range 600 {
		for x := range 800 {
			img.Set(x, y, color.RGBA{uint8(x), uint8(y), 128, 255})
		}
	}
	var buf bytes.Buffer
	png.Encode(&buf, img)
	e := gatewaytest.NewEngine(t, "docs", nil)
	e.Write("photo.png", buf.String())
	e.Write("odd.webp", "not rendered by the gateway")
	g := gatewaytest.NewGateway(t, e, "")
	g.Export(false)
	thumbs := t.TempDir()
	ts := newTestServiceWith(t, g, func(c *Config) { c.ThumbnailDir = thumbs })
	children, err := ts.Children(ts.dir("docs"))
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(t.TempDir(), "Synctrain", "docs")
	ts.FillThumbnails(dir, children)
	photo := ts.mustLookup("docs/photo.png")
	key := thumbnailKey(fileURI(filepath.Join(dir, "photo.png")))
	for _, size := range thumbnailSizes {
		path := filepath.Join(thumbs, size.dir, key+".png")
		gatewaytest.WaitFor(t, 30*time.Second, "the "+size.dir+" thumbnail", func() bool {
			_, err := os.Stat(path)
			return err == nil
		})
		f, _ := os.Open(path)
		decoded, err := png.Decode(f)
		f.Close()
		if err != nil {
			t.Fatal(err)
		}
		if b := decoded.Bounds(); max(b.Dx(), b.Dy()) > size.size {
			t.Fatalf("the %s thumbnail is %v", size.dir, b)
		}
		if uri, _ := pngTextValue(path, "Thumb::URI"); uri != fileURI(filepath.Join(dir, "photo.png")) {
			t.Fatalf("Thumb::URI %q", uri)
		}
		if mtime, _ := pngTextValue(path, "Thumb::MTime"); mtime != strconv.FormatInt(photo.ModTime.Unix(), 10) {
			t.Fatalf("Thumb::MTime %q", mtime)
		}
	}
	// A kind the gateway can't render gets a failure, which GNOME doesn't try again
	fail := filepath.Join(thumbs, "fail", "gnome-thumbnail-factory", thumbnailKey(fileURI(filepath.Join(dir, "odd.webp")))+".png")
	gatewaytest.WaitFor(t, 30*time.Second, "the failure entry", func() bool {
		_, err := os.Stat(fail)
		return err == nil
	})
	// Listed again, nothing is fetched again
	before := len(ts.thumbnails.queue)
	ts.FillThumbnails(dir, children)
	if len(ts.thumbnails.queue) != before {
		t.Fatal("thumbnails that are in the cache were queued again")
	}
	if st, err := ts.Storage(); err != nil || st.DownloadedBytes != 0 {
		t.Fatalf("thumbnails downloaded file content: %+v, %v", st, err)
	}

}
