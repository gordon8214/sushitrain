//go:build linux && cgo

// Copyright (C) 2026 Gordon
// SPDX-License-Identifier: MPL-2.0
package main

import (
	"encoding/json"
	"fmt"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/diamondburned/gotk4/pkg/core/glib"
	gdk "github.com/diamondburned/gotk4/pkg/gdk/v4"
	glibv2 "github.com/diamondburned/gotk4/pkg/glib/v2"
	"github.com/diamondburned/gotk4/pkg/gtk/v4"
	"t-shaped.nl/sushitrain/v2/desktopapi"
	"t-shaped.nl/sushitrain/v2/files"
)

type browserRow struct {
	name, detail, path, url, native string
	identity                        string
	directory                       bool
	item                            int64
	node                            uint64
	keep, writable                  bool
	size                            int64
}

func (d *desktopUI) browse() {
	d.browserRows = nil
	d.browserSelected = ""
	d.browserSearch = false
	d.browsePage(0)
}
func (d *desktopUI) browsePage(offset int) {
	d.generation++
	if d.mode == modeSynchronize && d.folder == "" {
		d.reset("Files")
		d.text("Choose a synchronized folder to browse.")
		d.folders()
		return
	}
	method := "engine.browsePage"
	var params any = desktopapi.Request{Folder: d.folder, Path: d.directory, Offset: int64(offset)}
	if d.mode == modeFiles {
		method = "files.childrenPage"
		params = map[string]any{"Parent": d.parent, "Offset": offset}
	}
	d.load(method, params, func(b json.RawMessage) {
		rows := []browserRow{}
		var page struct {
			Entries json.RawMessage
			Next    int
		}
		if err := json.Unmarshal(b, &page); err != nil {
			d.error(err)
			return
		}
		b = page.Entries
		d.browserNext = page.Next
		if d.mode == modeFiles {
			var entries []files.Entry
			if err := json.Unmarshal(b, &entries); err != nil {
				d.error(err)
				return
			}
			for _, e := range entries {
				rows = append(rows, browserRow{name: e.Name, detail: humanBytes(e.Size), directory: e.IsDir(), item: e.Item, node: e.Node, keep: e.Keep, writable: e.Writable, size: e.Size})
			}
		} else {
			var entries []desktopapi.Entry
			if err := json.Unmarshal(b, &entries); err != nil {
				d.error(err)
				return
			}
			for _, e := range entries {
				rows = append(rows, browserRow{name: e.Name, detail: humanBytes(e.Size), path: e.Path, url: e.URL, native: e.NativePath, identity: e.Identity, directory: e.Directory, writable: e.Local, size: e.Size})
			}
		}
		d.browserRows = append(d.browserRows, rows...)
		d.showBrowser(d.browserRows)
	})
}
func (r browserRow) key() string {
	if r.item != 0 {
		return fmt.Sprint(r.item)
	}
	return r.path
}

func (d *desktopUI) showBrowser(rows []browserRow) {
	gridMode := d.preferences.Grid
	d.reset("Files")
	d.button("Up", func() {
		if d.mode == modeFiles {
			if len(d.parents) > 0 {
				d.parent = d.parents[len(d.parents)-1]
				d.parents = d.parents[:len(d.parents)-1]
			}
		} else {
			if d.directory == "" {
				d.folder = ""
			} else {
				d.directory = path.Dir(d.directory)
				if d.directory == "." {
					d.directory = ""
				}
			}
		}
		d.browse()
	})
	d.button("Refresh", d.browse)
	d.button("Search…", func() { d.input("Search Files", "File name contains", "", func(query string) { d.searchFiles(query) }) })
	if d.mode == modeFiles && d.parent != 0 {
		d.button("Import File…", func() {
			d.pickFile("Choose a file to import", func(source string) {
				d.run("files.import", map[string]any{"Parent": d.parent, "Path": source}, func(json.RawMessage) { d.browse() })
			})
		})
		d.button("New Folder…", func() {
			d.input("New Folder", "Name", "", func(name string) {
				d.run("files.create", map[string]any{"Parent": d.parent, "Name": name, "Directory": true}, func(json.RawMessage) { d.browse() })
			})
		})
	}
	if d.mode == modeSynchronize {
		d.button("New Folder…", func() {
			d.input("New Folder", "Folder name", "", func(name string) {
				if name == "" || path.Base(name) != name {
					d.w.inform("Invalid Name", "Enter a single folder name.")
					return
				}
				d.run("engine.action", desktopapi.Request{Action: "mkdir", Folder: d.folder, Path: path.Join(d.directory, name)}, func(json.RawMessage) { d.browse() })
			})
		})
		d.button("Import File…", func() {
			d.pickFile("Choose a file to import", func(source string) {
				d.run("engine.action", desktopapi.Request{Action: "import", Folder: d.folder, Path: path.Join(d.directory, filepath.Base(source)), ID: source}, func(json.RawMessage) { d.browse() })
			})
		})
	}
	filter := gtk.NewSearchEntry()
	filter.SetPlaceholderText("Filter this folder")
	d.body.Append(filter)
	model := gtk.NewStringList(nil)
	selection := gtk.NewSingleSelection(model)
	selection.SetAutoselect(true)
	selection.SetCanUnselect(false)
	factory := gtk.NewSignalListItemFactory()
	var displayed []browserRow
	factory.ConnectSetup(func(object *glib.Object) {
		item := glib.CastObject(object).(*gtk.ListItem)
		box := gtk.NewBox(gtk.OrientationVertical, 4)
		name := gtk.NewLabel("")
		name.SetXAlign(0)
		name.SetEllipsize(3)
		detail := gtk.NewLabel("")
		detail.SetXAlign(0)
		detail.AddCSSClass("dim-label")
		box.SetMarginTop(8)
		box.SetMarginBottom(8)
		if gridMode {
			visual := gtk.NewStack()
			icon := gtk.NewImageFromIconName("text-x-generic")
			icon.SetPixelSize(64)
			visual.AddNamed(icon, "icon")
			picture := gtk.NewPicture()
			picture.SetSizeRequest(128, 96)
			picture.SetCanShrink(true)
			visual.AddNamed(picture, "thumbnail")
			box.Append(visual)
		}
		box.Append(name)
		box.Append(detail)
		item.SetChild(box)
	})
	factory.ConnectBind(func(object *glib.Object) {
		item := glib.CastObject(object).(*gtk.ListItem)
		pos := int(item.Position())
		if pos >= len(displayed) {
			return
		}
		r := displayed[pos]
		box := item.Child().(*gtk.Box)
		var picture *gtk.Picture
		var visual *gtk.Stack
		first := box.FirstChild()
		if gridMode {
			visual = first.(*gtk.Stack)
			icon := visual.ChildByName("icon").(*gtk.Image)
			iconName := "text-x-generic"
			if r.directory {
				iconName = "folder"
			}
			icon.SetFromIconName(iconName)
			visual.SetVisibleChildName("icon")
			picture = visual.ChildByName("thumbnail").(*gtk.Picture)
			picture.SetPaintable(nil)
			first = visual.NextSibling()
		}
		name := first.(*gtk.Label)
		detail := name.NextSibling().(*gtk.Label)
		prefix := ""
		if r.directory {
			prefix = "▸ "
		}
		name.SetText(prefix + r.name)
		detail.SetText(r.detail)
		item.SetAccessibleLabel(r.name)
		if picture != nil && !r.directory {
			method := "engine.thumbnail"
			var p any = desktopapi.Request{Folder: d.folder, Path: r.path}
			if d.mode == modeFiles {
				method = "files.thumbnail"
				p = map[string]any{"Item": r.item}
			}
			generation := d.generation
			ctx := d.ctx
			background(func() ([]byte, error) { var data []byte; err := d.request(ctx, method, p, &data); return data, err }, func(data []byte, err error) {
				if err != nil || d.closed || generation != d.generation || item.Position() != uint(pos) || pos >= len(displayed) || displayed[pos] != r {
					return
				}
				texture, err := gdk.NewTextureFromBytes(glibv2.NewBytes(data))
				if err == nil {
					picture.SetPaintable(texture)
					visual.SetVisibleChildName("thumbnail")
				}
			})
		}
	})
	scroll := gtk.NewScrolledWindow()
	scroll.SetVExpand(true)
	scroll.SetMinContentHeight(300)
	if gridMode {
		grid := gtk.NewGridView(selection, &factory.ListItemFactory)
		grid.SetMaxColumns(6)
		grid.SetMinColumns(2)
		grid.ConnectActivate(func(position uint) {
			if int(position) < len(displayed) {
				d.openBrowserRow(displayed[position])
			}
		})
		scroll.SetChild(grid)
	} else {
		list := gtk.NewListView(selection, &factory.ListItemFactory)
		list.SetVExpand(true)
		list.SetSingleClickActivate(false)
		list.ConnectActivate(func(position uint) {
			if int(position) < len(displayed) {
				d.openBrowserRow(displayed[position])
			}
		})
		scroll.SetChild(list)
	}
	d.body.Append(scroll)
	fill := func() {
		displayed = nil
		names := []string{}
		query := strings.ToLower(filter.Text())
		for _, r := range rows {
			if !d.browserSearch && d.preferences.HideDotfiles && strings.HasPrefix(r.name, ".") {
				continue
			}
			if strings.Contains(strings.ToLower(r.name), query) {
				displayed = append(displayed, r)
				names = append(names, r.name)
			}
		}
		selected := d.browserSelected
		model.Splice(0, model.NItems(), names)
		if len(displayed) > 0 {
			chosen := 0
			for i, r := range displayed {
				if r.key() == selected {
					chosen = i
					break
				}
			}
			selection.SetSelected(uint(chosen))
			d.browserSelected = displayed[chosen].key()
		}
	}
	selection.NotifyProperty("selected", func() {
		i := int(selection.Selected())
		if i >= 0 && i < len(displayed) {
			d.browserSelected = displayed[i].key()
		}
	})
	if d.browserNext > 0 {
		d.button("Load More", func() { d.browsePage(d.browserNext) })
	}
	d.button("List / Grid", func() {
		d.preferences.Grid = !d.preferences.Grid
		if err := writePreferences(d.preferences); err != nil {
			d.error(err)
			return
		}
		d.showBrowser(rows)
	})
	filter.ConnectSearchChanged(fill)
	fill()
	d.button("Open", func() {
		i := int(selection.Selected())
		if i < len(displayed) && i >= 0 {
			d.openBrowserRow(displayed[i])
		}
	})
	d.button("Actions…", func() {
		i := int(selection.Selected())
		if i < len(displayed) && i >= 0 {
			d.fileActions(displayed[i])
		}
	})
	count := fmt.Sprintf("%d items", len(rows))
	if len(rows) == 1 {
		count = "1 item"
	}
	d.text(count)
}
func (d *desktopUI) openBrowserRow(r browserRow) {
	if r.directory {
		if d.mode == modeFiles {
			d.parents = append(d.parents, d.parent)
			d.parent = r.item
		} else {
			d.directory = r.path
		}
		d.browse()
		return
	}
	if d.mode == modeFiles {
		d.load("files.path", map[string]int64{"Item": r.item}, func(b json.RawMessage) {
			var location string
			json.Unmarshal(b, &location)
			if needsBoundedPreview(r.name, d.preferences) {
				limit := int64(512 << 20)
				if isTextPreview(r.name) {
					limit = 1 << 20
				} else if isImagePreview(r.name) {
					limit = int64(d.preferences.ImageLimitMiB) << 20
				}
				d.load("files.preview", map[string]any{"Item": r.item, "Node": r.node, "Limit": limit}, func(b json.RawMessage) {
					var preview string
					json.Unmarshal(b, &preview)
					d.preview(preview, r.name, r.size)
				})
			} else {
				d.preview(location, r.name, r.size)
			}
		})
	} else {
		location := r.url
		if r.native != "" {
			location = r.native
		}
		d.preview(location, r.name, r.size)
	}
}
func (d *desktopUI) fileActions(r browserRow) { d.subpage(func() { d.fileActionsPage(r) }) }
func (d *desktopUI) fileActionsPage(r browserRow) {
	d.generation++
	d.reset(r.name)
	d.text(r.detail)
	d.button("Back", d.browse)
	d.button("Open", func() { d.openBrowserRow(r) })
	if d.mode == modeFiles {
		keep := "Keep on This Device"
		if r.keep {
			keep = "Stop Keeping"
		}
		d.button(keep, func() {
			d.run("files.keep", map[string]any{"Item": r.item, "Node": r.node, "Keep": !r.keep}, func(json.RawMessage) { d.browse() })
		})
		d.button("Previous Versions", func() {
			d.filesVersions(r)
		})
		if !r.directory {
			d.button("Save a Copy…", func() {
				d.pickSave(r.name, func(dest string) {
					d.run("files.download", map[string]any{"Item": r.item, "Node": r.node, "Path": dest}, func(json.RawMessage) { d.w.toast("Saved a copy.") })
				})
			})
		}
		if r.writable {
			d.button("Move…", func() {
				d.input("Move File or Folder", "Destination path relative to Synctrain's mounted folders", "", func(target string) {
					d.run("files.move", map[string]any{"Item": r.item, "Node": r.node, "Path": target}, func(json.RawMessage) { d.browse() })
				})
			})
			d.button("Rename…", func() {
				d.input("Rename", "New name", r.name, func(name string) {
					d.run("files.rename", map[string]any{"Parent": d.parent, "NewParent": d.parent, "Item": r.item, "Node": r.node, "NewName": name}, func(json.RawMessage) { d.browse() })
				})
			})
			d.button("Delete…", func() {
				d.w.confirm("Delete “"+r.name+"”?", "This also removes it on its gateway.", "Delete", true, func() {
					d.run("files.delete", map[string]any{"Parent": d.parent, "Item": r.item, "Node": r.node}, func(json.RawMessage) { d.browse() })
				})
			})
		}
	} else {
		d.button("Previous Versions", func() { d.localVersions(r.path, r.name) })
		if !r.directory {
			d.button("Save a Copy…", func() {
				d.pickSave(r.name, func(dest string) {
					d.run("engine.exportCurrent", desktopapi.Request{Folder: d.folder, Path: r.path, Name: dest, Token: r.identity}, func(json.RawMessage) { d.w.toast("Saved a copy.") })
				})
			})
		}
		if r.native != "" && r.identity != "" {
			d.button("Rename…", func() {
				d.input("Rename", "New name", r.name, func(name string) {
					if name == "" || path.Base(name) != name {
						d.w.inform("Invalid Name", "Enter a single file or folder name.")
						return
					}
					d.run("engine.action", desktopapi.Request{Action: "rename", Folder: d.folder, Path: r.path, Name: path.Join(path.Dir(r.path), name), Token: r.identity}, func(json.RawMessage) { d.browse() })
				})
			})
			d.button("Move…", func() {
				d.input("Move within This Folder", "New path relative to this synchronized folder", r.path, func(target string) {
					d.run("engine.action", desktopapi.Request{Action: "move", Folder: d.folder, Path: r.path, Name: target, Token: r.identity}, func(json.RawMessage) { d.browse() })
				})
			})
			d.button("Move to Trash…", func() {
				d.w.confirm("Move “"+r.name+"” to Trash?", "This deletion also synchronizes to other devices. The local file remains recoverable from Trash.", "Move to Trash", true, func() {
					d.run("engine.action", desktopapi.Request{Action: "trashFile", Folder: d.folder, Path: r.path, Token: r.identity}, func(json.RawMessage) { d.browse() })
				})
			})
		}

		if r.native != "" {
			d.button("Show in Files", func() { d.w.openPath(filepath.Dir(r.native)) })
		}
	}
}
func (d *desktopUI) searchFiles(query string) {
	d.startView()
	d.browserNext = 0
	d.browserSearch = true
	d.browserRows = nil
	method := "engine.search"
	var p any = desktopapi.Request{Folder: d.folder, Path: d.directory, Name: query}
	if d.mode == modeFiles {
		method = "files.search"
		p = map[string]any{"Parent": d.parent, "Name": query}
	}
	d.load(method, p, func(b json.RawMessage) {
		rows := []browserRow{}
		if d.mode == modeFiles {
			var list []files.Entry
			json.Unmarshal(b, &list)
			for _, e := range list {
				rows = append(rows, browserRow{name: e.Name, directory: e.IsDir(), item: e.Item, node: e.Node, size: e.Size, detail: humanBytes(e.Size), writable: e.Writable, keep: e.Keep})
			}
		} else {
			var list []desktopapi.Entry
			json.Unmarshal(b, &list)
			for _, e := range list {
				rows = append(rows, browserRow{name: e.Name, path: e.Path, url: e.URL, native: e.NativePath, identity: e.Identity, size: e.Size, directory: e.Directory, detail: e.Path})
			}
		}
		sort.Slice(rows, func(i, j int) bool { return rows[i].name < rows[j].name })
		d.showBrowser(rows)
		limit := 10000
		if d.mode == modeFiles {
			limit = 2000
		}
		if len(rows) >= limit {
			d.text(fmt.Sprintf("Showing the first %d matches. Refine the search to find more files.", limit))
		}
	})
}
func (d *desktopUI) localVersions(file, name string) {
	d.subpage(func() { d.localVersionsPage(file, name) })
}
func (d *desktopUI) localVersionsPage(file, name string) {
	d.reset("Previous Versions — " + name)
	d.button("Back", d.browse)
	g := d.group("This Device")
	d.appendRemoteVersions(d.folder, file, name, 0, d.deviceID, "")
	d.fetch("engine.versions", desktopapi.Request{Folder: d.folder, Path: file}, func(b json.RawMessage, err error) {
		if err != nil {
			g.Add(row("No local versions", err.Error()))
			return
		}
		var list []desktopapi.Version
		if err = json.Unmarshal(b, &list); err != nil {
			g.Add(row("Could not read local history", err.Error()))
			return
		}
		if len(list) == 0 {
			g.Add(row("No local versions", "Versions from paired sources are listed separately."))
		}
		for _, v := range list {
			v := v
			r := row(time.UnixMilli(v.Modified).Local().Format("January 2, 2006 3:04:05 PM"), humanBytes(v.Size))
			r.AddSuffix(textButton("Preview", func() {
				d.run("engine.previewVersion", desktopapi.Request{Folder: d.folder, Path: file, ID: v.ID}, func(b json.RawMessage) { var local string; json.Unmarshal(b, &local); d.preview(local, name, v.Size) })
			}))
			r.AddSuffix(textButton("Save a Copy…", func() {
				d.pickSave(name, func(dest string) {
					d.run("engine.exportVersion", desktopapi.Request{Folder: d.folder, Path: file, ID: v.ID, Name: dest}, func(json.RawMessage) { d.w.toast("Saved a copy.") })
				})
			}))
			r.AddSuffix(textButton("Restore…", func() {
				d.w.confirm("Restore This Version?", "The current file is archived before it is replaced.", "Restore", true, func() {
					d.run("engine.restoreVersion", desktopapi.Request{Folder: d.folder, Path: file, ID: v.ID}, func(json.RawMessage) { d.w.toast("Restored the selected version."); d.localVersions(file, name) })
				})
			}))
			g.Add(r)
		}
	})
}
