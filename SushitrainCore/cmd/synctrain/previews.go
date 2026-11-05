//go:build linux && cgo

// Copyright (C) 2026 Gordon
// SPDX-License-Identifier: MPL-2.0
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"t-shaped.nl/sushitrain/v2/desktopsettings"
	"t-shaped.nl/sushitrain/v2/linuxdesktop"
	"t-shaped.nl/sushitrain/v2/previewcache"

	"github.com/diamondburned/gotk4-adwaita/pkg/adw"
	"github.com/diamondburned/gotk4/pkg/gio/v2"
	"github.com/diamondburned/gotk4/pkg/gtk/v4"
)

type Preferences struct {
	desktopsettings.Preferences
	baseline desktopsettings.Preferences
}

func preferencesPath() string { return desktopsettings.Path() }
func readPreferences() Preferences {
	p, err := desktopsettings.Read(preferencesPath())
	if err != nil {
		p, _ = desktopsettings.Read("")
	}
	return Preferences{Preferences: p, baseline: p}
}
func writePreferences(p *Preferences) error {
	before, after := desktopsettings.Values(p.baseline), desktopsettings.Values(p.Preferences)
	changes := map[string]string{}
	for key, value := range after {
		if before[key] != value {
			changes[key] = value
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	updated, err := desktopsettings.Update(ctx, preferencesPath(), p.baseline, changes)
	if err == nil {
		p.Preferences = updated
		p.baseline = updated
	}
	return err
}
func (d *desktopUI) previewSettings() { d.subpage(func() { d.previewSettingsPage() }) }
func (d *desktopUI) previewSettingsPage() {
	d.reset("Files & Previews")
	g := d.group("Browsing and Previews")
	for _, s := range []struct {
		title string
		value *bool
	}{{"Hide dotfiles", &d.preferences.HideDotfiles}, {"Preview images", &d.preferences.PreviewImages}, {"Preview audio and video", &d.preferences.PreviewMedia}, {"Use grid view", &d.preferences.Grid}} {
		s := s
		r := adw.NewSwitchRow()
		r.SetTitle(s.title)
		r.SetActive(*s.value)
		r.NotifyProperty("active", func() {
			old := *s.value
			*s.value = r.Active()
			if err := writePreferences(&d.preferences); err != nil {
				*s.value = old
				r.SetActive(old)
				d.error(err)
			}
		})
		g.Add(r)
	}
	limit := adw.NewSpinRowWithRange(1, 100, 1)
	limit.SetTitle("Maximum image preview size (MiB)")
	limit.SetValue(float64(d.preferences.ImageLimitMiB))
	limit.NotifyProperty("value", func() {
		old := d.preferences.ImageLimitMiB
		d.preferences.ImageLimitMiB = int(limit.Value())
		if err := writePreferences(&d.preferences); err != nil {
			d.preferences.ImageLimitMiB = old
			limit.SetValue(float64(old))
			d.error(err)
		}
	})
	g.Add(limit)
}
func (d *desktopUI) exportConfiguration() {
	d.pickSave("config.xml", func(dest string) {
		d.run("configuration.export", map[string]string{"Path": dest}, func(json.RawMessage) { d.w.toast("Exported the configuration.") })
	})
}
func (d *desktopUI) preview(location, name string, size int64) {
	ext := strings.ToLower(filepath.Ext(name))
	image := strings.Contains(" .png .jpg .jpeg .gif .webp .bmp .tiff .avif ", " "+ext+" ")
	media := strings.Contains(" .mp4 .m4v .mov .webm .mp3 .m4a .ogg .oga .wav .flac .opus ", " "+ext+" ")
	pdf := ext == ".pdf"
	text := strings.Contains(" .txt .md .go .swift .c .h .cpp .rs .py .json .xml .yaml .yml .toml .ini .log .csv .sh .css ", " "+ext+" ")
	if (image && !d.preferences.PreviewImages) || (media && !d.preferences.PreviewMedia) || (!image && !media && !pdf && !text) {
		d.launch(location, name)
		return
	}
	if image && size > int64(d.preferences.ImageLimitMiB)*1024*1024 {
		d.w.inform("Image Too Large to Preview", "Open this file in its default application to view it.")
		return
	}
	dialog := adw.NewDialog()
	dialog.SetTitle(name)
	dialog.SetContentWidth(900)
	dialog.SetContentHeight(650)
	toolbar := adw.NewToolbarView()
	header := adw.NewHeaderBar()
	header.PackEnd(textButton("Open in Default App", func() { d.launch(location, name) }))
	toolbar.AddTopBar(header)
	toolbar.SetContent(gtk.NewSpinner())
	dialog.SetChild(toolbar)
	ctx, cancel := context.WithCancel(context.Background())
	closed := false
	var video *gtk.Video
	previewDirectory := ""
	var releasePreview func()
	dialog.ConnectClosed(func() {
		closed = true
		cancel()
		if releasePreview != nil {
			releasePreview()
		}
		if previewDirectory != "" {
			os.RemoveAll(previewDirectory)
		}
		if video != nil {
			if stream := video.MediaStream(); stream != nil {
				gtk.BaseMediaStream(stream).Pause()
			}
			video.SetFile(nil)
		}
	})
	dialog.Present(d.w.win)
	if media {
		file := gio.NewFileForPath(location)
		if strings.HasPrefix(location, "http://") {
			file = gio.NewFileForURI(location)
		}
		video = gtk.NewVideoForFile(file)
		video.SetAutoplay(true)
		toolbar.SetContent(video)
		return
	}
	limit := int64(512 * 1024 * 1024)
	if text {
		limit = 1024 * 1024
	}
	if image {
		limit = int64(d.preferences.ImageLimitMiB) * 1024 * 1024
	}
	background(func() (string, error) { return previewCopy(ctx, location, name, limit) }, func(file string, err error) {
		if closed {
			if file != "" {
				os.RemoveAll(filepath.Dir(file))
			}
			return
		}
		if err != nil {
			toolbar.SetContent(gtk.NewLabel("Could not preview: " + err.Error()))
			return
		}
		previewDirectory = filepath.Dir(file)
		releasePreview, err = previewcache.Pin(previewDirectory)
		if err != nil {
			toolbar.SetContent(gtk.NewLabel(err.Error()))
			return
		}
		if image {
			picture := gtk.NewPictureForFilename(file)
			picture.SetCanShrink(true)
			toolbar.SetContent(picture)
		} else if text {
			b, err := os.ReadFile(file)
			if err != nil {
				toolbar.SetContent(gtk.NewLabel(err.Error()))
				return
			}
			if !utf8.Valid(b) {
				toolbar.SetContent(gtk.NewLabel("This file is not UTF-8 text. Open it in its default application."))
				return
			}
			buffer := gtk.NewTextBuffer(nil)
			buffer.SetText(string(b))
			view := gtk.NewTextViewWithBuffer(buffer)
			view.SetEditable(false)
			view.SetMonospace(true)
			scroll := gtk.NewScrolledWindow()
			scroll.SetChild(view)
			toolbar.SetContent(scroll)
		} else {
			d.pdfPreview(ctx, toolbar, file)
		}
	})
}
func (d *desktopUI) launch(location, name string) {
	if strings.HasPrefix(location, "http://") || strings.HasPrefix(location, "https://") {
		ctx, generation := d.ctx, d.generation
		background(func() (string, error) {
			return previewCopy(ctx, location, name, previewcache.MaxFile)
		}, func(file string, err error) {
			if d.closed || generation != d.generation || ctx.Err() != nil {
				if file != "" {
					os.RemoveAll(filepath.Dir(file))
				}
				return
			}
			if err == nil {
				err = previewcache.RetainExternal(filepath.Dir(file))
			}
			if err != nil {
				d.error(err)
			} else {
				d.w.openPath(file)
			}
		})
	} else {
		d.w.openPath(location)
	}
}
func previewCopy(ctx context.Context, location, name string, limit int64) (string, error) {
	return linuxdesktop.PreviewCopy(ctx, location, name, limit)
}

func (d *desktopUI) pdfPreview(ctx context.Context, toolbar *adw.ToolbarView, file string) {
	page := 1
	pages := 1
	box := gtk.NewBox(gtk.OrientationVertical, 8)
	controls := gtk.NewBox(gtk.OrientationHorizontal, 8)
	picture := gtk.NewPicture()
	picture.SetCanShrink(true)
	picture.SetVExpand(true)
	status := gtk.NewLabel("")
	box.Append(controls)
	box.Append(picture)
	box.Append(status)
	toolbar.SetContent(box)
	var renderCancel context.CancelFunc
	var renderGeneration uint64
	render := func() {
		if renderCancel != nil {
			renderCancel()
		}
		renderCtx, stop := context.WithCancel(ctx)
		renderCancel = stop
		renderGeneration++
		generation := renderGeneration
		n := page
		status.SetText("Rendering page…")
		background(func() (string, error) {
			ctx, cancel := context.WithTimeout(renderCtx, 20*time.Second)
			defer cancel()
			output := filepath.Join(filepath.Dir(file), fmt.Sprintf("page-%d-%d", n, generation))
			b, err := exec.CommandContext(ctx, "pdftoppm", "-f", strconv.Itoa(n), "-l", strconv.Itoa(n), "-singlefile", "-scale-to", "1600", "-png", file, output).CombinedOutput()
			if err != nil {
				return "", fmt.Errorf("PDF rendering: %s", strings.TrimSpace(string(b)))
			}
			return output + ".png", nil
		}, func(p string, err error) {
			if renderCtx.Err() != nil || renderGeneration != generation || page != n {
				if p != "" {
					os.Remove(p)
				}
				return
			}
			if err != nil {
				status.SetText(err.Error())
				return
			}
			if entries, e := os.ReadDir(filepath.Dir(file)); e == nil {
				for _, entry := range entries {
					if strings.HasPrefix(entry.Name(), "page-") && strings.HasSuffix(entry.Name(), ".png") && entry.Name() != filepath.Base(p) {
						os.Remove(filepath.Join(filepath.Dir(file), entry.Name()))
					}
				}
			}
			picture.SetFilename(p)
			status.SetText(fmt.Sprintf("Page %d of %d", n, pages))
		})
	}
	controls.Append(textButton("Previous Page", func() {
		if page > 1 {
			page--
			render()
		}
	}))
	controls.Append(textButton("Next Page", func() {
		if page < pages {
			page++
			render()
		}
	}))
	background(func() (int, error) {
		ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
		defer cancel()
		command := exec.CommandContext(ctx, "pdfinfo", file)
		command.Env = append(os.Environ(), "LC_ALL=C")
		b, err := command.Output()
		if err != nil {
			return 0, err
		}
		for _, line := range strings.Split(string(b), "\n") {
			if strings.HasPrefix(line, "Pages:") {
				return strconv.Atoi(strings.TrimSpace(strings.TrimPrefix(line, "Pages:")))
			}
		}
		return 1, nil
	}, func(n int, err error) {
		if ctx.Err() != nil {
			return
		}
		if err != nil {
			status.SetText("Could not read PDF: " + err.Error())
			return
		}
		pages = n
		render()
	})
}

func isImagePreview(name string) bool {
	ext := strings.ToLower(filepath.Ext(name))
	return strings.Contains(" .png .jpg .jpeg .gif .webp .bmp .tiff .avif ", " "+ext+" ")
}
func isTextPreview(name string) bool {
	ext := strings.ToLower(filepath.Ext(name))
	return strings.Contains(" .txt .md .go .swift .c .h .cpp .rs .py .json .xml .yaml .yml .toml .ini .log .csv .sh .css ", " "+ext+" ")
}
func needsBoundedPreview(name string, p Preferences) bool {
	return strings.EqualFold(filepath.Ext(name), ".pdf") || isTextPreview(name) || p.PreviewImages && isImagePreview(name)
}
