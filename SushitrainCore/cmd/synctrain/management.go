//go:build linux && cgo

// Copyright (C) 2026 Gordon
// SPDX-License-Identifier: MPL-2.0
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"github.com/google/uuid"
	"path/filepath"
	"strings"

	"github.com/diamondburned/gotk4-adwaita/pkg/adw"
	"github.com/diamondburned/gotk4/pkg/gio/v2"
	"github.com/diamondburned/gotk4/pkg/gtk/v4"
	"t-shaped.nl/sushitrain/v2/desktopapi"
)

func (d *desktopUI) folders() {
	d.button("Add Folder…", func() { d.addFolder("", "", false) })
	d.load("engine.folders", nil, func(b json.RawMessage) {
		var folders []desktopapi.Folder
		if err := json.Unmarshal(b, &folders); err != nil {
			d.error(err)
			return
		}
		g := d.group("Synchronized Folders")
		shown, rows := folderListKey(folders), d.folderRows(g, folders)
		widgets := folderWidgets(g, rows, len(folders))
		// Folder states and syncing progress are live. A folder added, removed or shared elsewhere makes the rows again.
		pollPage(d, func(ctx context.Context) ([]desktopapi.Folder, error) {
			var next []desktopapi.Folder
			err := d.request(ctx, "engine.folders", nil, &next)
			return next, err
		}, func(next []desktopapi.Folder, err error) {
			if err != nil {
				return
			}
			if key := folderListKey(next); key != shown {
				for _, w := range widgets {
					g.Remove(w)
				}
				shown, rows = key, d.folderRows(g, next)
				widgets = folderWidgets(g, rows, len(next))
				return
			}
			for _, f := range next {
				rows[f.ID].update(f)
			}
		})
	})
}

// folderListKey changes when the folder rows have to be made again: a folder is added or removed, or something its
// row's actions use changes (they keep the folder as it was when the row was made)
func folderListKey(folders []desktopapi.Folder) string {
	var b strings.Builder
	for _, f := range folders {
		b.WriteString(strings.Join([]string{f.ID, f.Label, f.Path, f.Type, strings.Join(f.Devices, ",")}, "\x00") + "\x01")
	}
	return b.String()
}

// folderWidgets are the rows of a folder list, with the row saying there are none
func folderWidgets(g *adw.PreferencesGroup, rows map[string]*folderRow, count int) []gtk.Widgetter {
	var widgets []gtk.Widgetter
	for _, r := range rows {
		widgets = append(widgets, r.row)
	}
	if count == 0 {
		empty := row("No folders yet", "Add a folder to start synchronizing.")
		g.Add(empty)
		widgets = append(widgets, empty)
	}
	return widgets
}

// folderRow is a synchronized folder's row, with how far a syncing folder has come
type folderRow struct {
	row       *adw.ExpanderRow
	pause     *gtk.Button
	sync      *gtk.Box
	bar       *gtk.ProgressBar
	remaining *gtk.Label
	paused    bool
}

// folderState is a folder's state as its row says it
func folderState(f desktopapi.Folder) string {
	if f.State == "syncing" {
		return "Synchronizing…"
	}
	return f.State
}

// folderRemaining is how much a syncing folder has yet to receive ("" when that isn't known)
func folderRemaining(f desktopapi.Folder) string {
	if f.Sync == nil || f.Sync.GlobalBytes <= 0 {
		return ""
	}
	return humanBytes(max(0, f.Sync.GlobalBytes-f.Sync.LocalBytes)) + " remaining"
}

func (r *folderRow) update(f desktopapi.Folder) {
	r.row.SetSubtitle(strings.Join(nonEmpty(f.Path, f.Type, folderState(f), f.Error), " · "))
	r.paused = f.Paused
	if f.Paused {
		r.pause.SetLabel("Resume")
	} else {
		r.pause.SetLabel("Pause")
	}
	remaining := folderRemaining(f)
	r.sync.SetVisible(remaining != "")
	if remaining != "" {
		setProgress(r.bar, f.Sync.Fraction)
		r.remaining.SetText(remaining)
	}
}

func (d *desktopUI) folderRows(g *adw.PreferencesGroup, folders []desktopapi.Folder) map[string]*folderRow {
	rows := map[string]*folderRow{}
	for _, f := range folders {
		f := f
		r := adw.NewExpanderRow()
		r.SetUseMarkup(false)
		r.SetTitle(f.Label)
		fr := &folderRow{row: r, sync: gtk.NewBox(gtk.OrientationVertical, 2),
			bar: newProgressBar(120, "Synchronization of "+f.Label), remaining: gtk.NewLabel("")}
		fr.remaining.AddCSSClass("dim-label")
		fr.remaining.AddCSSClass("caption")
		fr.sync.SetVAlign(gtk.AlignCenter)
		fr.sync.Append(fr.bar)
		fr.sync.Append(fr.remaining)
		r.AddSuffix(fr.sync)
		r.AddSuffix(textButton("Browse", func() {
			if d.route == "Files" {
				d.subpage(func() { d.folder = f.ID; d.directory = ""; d.browse() })
			} else {
				d.openFilesAt(f.ID, "", 0)
			}
		}))
		actions := []struct {
			name string
			run  func()
		}{
			{"Overview", func() { d.edit("basic/folder/"+f.ID, "Folder Settings") }},
			{"Synchronization", func() { d.edit("folder/"+f.ID, "Synchronization Settings") }},
			{"Sharing", func() { d.share(f) }},
			{"Versioning", func() { d.edit("versioning/"+f.ID, "Versioning") }},
			{"Ignore Patterns", func() { d.ignores(f.ID) }},
			{"Advanced", func() { d.edit("advanced/folder/"+f.ID, "Advanced") }},
			{"Reset Synchronization Settings…", func() { d.edit("reset/folder/"+f.ID, "Reset Synchronization Settings") }},
			{"Failed Items", func() { d.failedItems(f.ID) }},
			{"Deleted Files", func() { d.deletedFiles(f.ID) }},
			{"Rescan", func() { d.action("rescan", f.ID, "") }},
			{"Open in Files", func() { d.w.openPath(f.Path) }},
			{"Move Folder…", func() {
				d.pickFolder("Move into this directory", func(target string) {
					d.run("engine.action", desktopapi.Request{Action: "relocate", Folder: f.ID, Path: target}, nil)
				})
			}},
			{"Remove Folder…", func() {
				d.w.confirm("Remove “"+f.Label+"”?", "Local files stay on this machine. Synchronization stops.", "Remove", true, func() { d.action("remove", f.ID, "") })
			}},
		}
		// The label follows the folder's state, which can change elsewhere
		fr.pause = textButton("", func() {
			action := "pause"
			if fr.paused {
				action = "resume"
			}
			d.action(action, f.ID, "")
		})
		r.AddSuffix(fr.pause)
		if f.Type == "sendonly" {
			actions = append(actions, struct {
				name string
				run  func()
			}{"Override Remote Changes…", func() {
				d.w.confirm("Override Remote Changes?", "Other devices will be changed to match this folder, including deletions.", "Override", true, func() { d.action("override", f.ID, "") })
			}})
		}
		if f.Type == "receiveonly" || f.Type == "receiveencrypted" {
			actions = append(actions, struct {
				name string
				run  func()
			}{"Revert Local Changes…", func() {
				d.w.confirm("Revert Local Changes?", "Local changes are replaced with the synchronized version.", "Revert", true, func() { d.action("revert", f.ID, "") })
			}})
		}
		for _, a := range actions {
			a := a
			rr := choiceRow(a.name, "", a.run)
			r.AddRow(rr)
		}
		fr.update(f)
		g.Add(r)
		rows[f.ID] = fr
	}
	return rows
}
func (d *desktopUI) devices() {
	d.button("Add Device…", func() {
		d.input("Add Device", "Syncthing device ID", "", func(id string) { d.run("engine.addDevice", desktopapi.Request{ID: id}, nil) })
	})
	for _, which := range []struct{ method, title string }{{"engine.pending", "Connection Requests"}, {"engine.ignored", "Ignored Devices"}} {
		which := which
		d.load(which.method, nil, func(b json.RawMessage) {
			var list []struct{ DeviceID, Name, Address string }
			json.Unmarshal(b, &list)
			if len(list) == 0 {
				return
			}
			g := d.group(which.title)
			for _, p := range list {
				p := p
				r := row(p.Name, p.DeviceID+" · "+p.Address)
				if which.method == "engine.pending" {
					r.AddSuffix(textButton("Add", func() { d.run("engine.addDevice", desktopapi.Request{ID: p.DeviceID}, nil) }))
					r.AddSuffix(textButton("Ignore", func() { d.run("engine.action", desktopapi.Request{Action: "ignore", ID: p.DeviceID}, nil) }))
					r.AddSuffix(textButton("Dismiss", func() { d.run("engine.action", desktopapi.Request{Action: "dismiss", ID: p.DeviceID}, nil) }))
				} else {
					r.AddSuffix(textButton("Allow Requests", func() { d.run("engine.action", desktopapi.Request{Action: "unignore", ID: p.DeviceID}, nil) }))
				}
				g.Add(r)
			}
		})
	}
	d.load("engine.devices", nil, func(b json.RawMessage) {
		var list []desktopapi.Device
		json.Unmarshal(b, &list)
		g := d.group("Devices")
		for _, v := range list {
			v := v
			r := adw.NewExpanderRow()
			r.SetUseMarkup(false)
			r.SetTitle(v.Name)
			state := "Disconnected"
			if v.Connected {
				state = "Connected"
			}
			r.SetSubtitle(v.ID + " · " + state + " · " + v.Address)
			pause := "Pause"
			action := "pause"
			if v.Paused {
				pause = "Resume"
				action = "resume"
			}
			r.AddSuffix(textButton(pause, func() { d.action(action, "", v.ID) }))
			r.AddRow(choiceRow("Overview", "Name, addresses and trust", func() { d.edit("basic/device/"+v.ID, "Device Settings") }))
			r.AddRow(choiceRow("Shared Folders", "Folders shared with this device", func() { d.sharedDeviceFolders(v.ID) }))
			r.AddRow(choiceRow("Connection Settings", "Compression, limits and networks", func() { d.edit("device/"+v.ID, "Connection Settings") }))
			r.AddRow(choiceRow("Advanced", "Expert device settings", func() { d.edit("advanced/device/"+v.ID, "Advanced") }))
			r.AddRow(choiceRow("Offered Folders", "Folders this device wants to share", func() { d.offeredFolders(v.ID) }))
			r.AddRow(choiceRow("Remove Device…", "", func() {
				d.w.confirm("Remove “"+v.Name+"”?", "This device stops sharing folders with this machine.", "Remove", true, func() { d.action("remove", "", v.ID) })
			}))
			g.Add(r)
		}
	})
}
func (d *desktopUI) pickFolder(title string, done func(string)) {
	chooser := gtk.NewFileDialog()
	chooser.SetTitle(title)
	chooser.SelectFolder(d.ctx, d.w.parent(), func(result gio.AsyncResulter) {
		f, err := chooser.SelectFolderFinish(result)
		if err == nil && f != nil {
			done(f.Path())
		}
	})
}
func (d *desktopUI) pickSave(name string, done func(string)) {
	chooser := gtk.NewFileDialog()
	chooser.SetTitle("Save a Copy")
	chooser.SetInitialName(filepath.Base(name))
	chooser.Save(d.ctx, d.w.parent(), func(result gio.AsyncResulter) {
		f, err := chooser.SaveFinish(result)
		if err == nil && f != nil {
			done(f.Path())
		}
	})
}
func (d *desktopUI) share(f desktopapi.Folder) { d.subpage(func() { d.sharePage(f) }) }
func (d *desktopUI) sharePage(f desktopapi.Folder) {
	d.reset("Sharing — " + f.Label)
	d.load("engine.devices", nil, func(b json.RawMessage) {
		var devices []desktopapi.Device
		json.Unmarshal(b, &devices)
		g := d.group("Share with Devices")
		for _, v := range devices {
			v := v
			shared := false
			for _, id := range f.Devices {
				shared = shared || id == v.ID
			}
			r := row(v.Name, v.ID)
			label := "Share…"
			if shared {
				label = "Stop Sharing"
			}
			r.AddSuffix(textButton(label, func() {
				if shared {
					d.run("engine.share", desktopapi.Request{Folder: f.ID, Device: v.ID, Enabled: false}, func(json.RawMessage) { d.finishPage() })
				} else {
					d.passwordInput("Share with “"+v.Name+"”", "Encryption password for an untrusted device (leave empty for trusted devices)", func(password string) {
						d.run("engine.share", desktopapi.Request{Folder: f.ID, Device: v.ID, Enabled: true, Password: password}, func(json.RawMessage) { d.finishPage() })
					})
				}
			}))
			g.Add(r)
		}
	})
}
func (d *desktopUI) ignores(folder string) { d.subpage(func() { d.ignoresPage(folder) }) }
func (d *desktopUI) ignoresPage(folder string) {
	d.reset("Ignore Patterns")
	d.load("engine.action", desktopapi.Request{Action: "ignores", Folder: folder}, func(b json.RawMessage) {
		var lines []string
		json.Unmarshal(b, &lines)
		d.textEditor("Ignore Patterns", strings.Join(lines, "\n"), func(value string) {
			d.run("engine.action", desktopapi.Request{Action: "saveIgnores", Folder: folder, Path: value}, func(json.RawMessage) { d.setDirty(false); d.finishPage() })
		})
	})
}
func (d *desktopUI) textEditor(title, value string, save func(string)) {
	d.reset(title)
	buffer := gtk.NewTextBuffer(nil)
	buffer.SetText(value)
	view := gtk.NewTextViewWithBuffer(buffer)
	view.SetMonospace(true)
	view.SetVExpand(true)
	view.SetSizeRequest(-1, 260)
	d.body.Append(view)
	buffer.ConnectChanged(func() { d.setDirty(true) })
	view.GrabFocus()
	d.button("Save", func() { save(buffer.Text(buffer.StartIter(), buffer.EndIter(), true)) })
}
func (d *desktopUI) failedItems(folder string) { d.subpage(func() { d.failedItemsPage(folder) }) }
func (d *desktopUI) failedItemsPage(folder string) {
	d.reset("Failed Items")
	d.load("engine.action", desktopapi.Request{Action: "failed", Folder: folder}, func(b json.RawMessage) {
		d.reset("Failed Items")
		var list []struct{ Path, Error string }
		json.Unmarshal(b, &list)
		g := d.group("Needs Attention")
		for _, e := range list {
			g.Add(row(e.Path, e.Error))
		}
		if len(list) == 0 {
			d.text("There are no failed items.")
		}
	})
}
func (d *desktopUI) deletedFiles(folder string) { d.subpage(func() { d.deletedFilesPage(folder) }) }
func (d *desktopUI) deletedFilesPage(folder string) {
	d.reset("Deleted Files")
	d.load("engine.action", desktopapi.Request{Action: "deleted", Folder: folder}, func(b json.RawMessage) {
		d.reset("Deleted Files")
		var list []struct {
			Path     string
			Versions int
		}
		json.Unmarshal(b, &list)
		g := d.group("Archived Files")
		for _, e := range list {
			e := e
			r := row(e.Path, fmt.Sprintf("%d versions", e.Versions))
			r.AddSuffix(textButton("Restore…", func() {
				d.w.confirm("Restore “"+e.Path+"”?", "The latest archived version will be restored.", "Restore", true, func() {
					d.run("engine.action", desktopapi.Request{Action: "restoreDeleted", Folder: folder, Path: e.Path}, nil)
				})
			}))
			g.Add(r)
		}
		if len(list) == 0 {
			d.text("There are no archived deleted files.")
		}
	})
}

func (d *desktopUI) pickFile(title string, done func(string)) {
	chooser := gtk.NewFileDialog()
	chooser.SetTitle(title)
	chooser.Open(d.ctx, d.w.parent(), func(result gio.AsyncResulter) {
		file, err := chooser.OpenFinish(result)
		if err == nil && file != nil {
			done(file.Path())
		}
	})
}

func (d *desktopUI) addFolder(offeredID, device string, encrypted bool) {
	d.subpage(func() {
		d.reset("Add Folder")
		g := d.group("Folder Details")
		id := adw.NewEntryRow()
		id.SetTitle("Folder ID")
		if offeredID == "" {
			offeredID = uuid.NewString()[:12]
		}
		id.SetText(offeredID)
		g.Add(id)
		label := adw.NewEntryRow()
		label.SetTitle("Display name")
		g.Add(label)
		location := adw.NewEntryRow()
		location.SetTitle("Local folder")
		g.Add(location)
		location.AddSuffix(textButton("Choose…", func() { d.pickFolder("Choose the folder to synchronize", func(path string) { location.SetText(path) }) }))
		types := []string{"Send & Receive", "Send Only", "Receive Only", "Receive Encrypted"}
		values := []string{"sendreceive", "sendonly", "receiveonly", "receiveencrypted"}
		kind := adw.NewComboRow()
		kind.SetTitle("Folder type")
		kind.SetModel(gtk.NewStringList(types))
		if encrypted {
			kind.SetSelected(3)
		}
		g.Add(kind)
		id.ConnectChanged(func() { d.setDirty(true) })
		label.ConnectChanged(func() { d.setDirty(true) })
		location.ConnectChanged(func() { d.setDirty(true) })
		kind.NotifyProperty("selected", func() { d.setDirty(true) })
		d.button("Add Folder", func() {
			if location.Text() == "" {
				d.w.inform("Choose a Folder", "Select where this folder should synchronize.")
				return
			}
			index := kind.Selected()
			if int(index) >= len(values) {
				return
			}
			d.run("engine.addFolder", desktopapi.Request{ID: id.Text(), Name: label.Text(), Path: location.Text(), Action: values[index], Device: device}, func(json.RawMessage) { d.setDirty(false); d.finishPage() })
		})
		id.GrabFocus()
	})
}
func (d *desktopUI) offeredFolders(device string) {
	d.subpage(func() {
		d.reset("Offered Folders")
		d.load("engine.pendingFolders", desktopapi.Request{Device: device}, func(b json.RawMessage) {
			var list []struct {
				ID        string
				Encrypted bool
			}
			json.Unmarshal(b, &list)
			g := d.group("Folders Offered by This Device")
			for _, f := range list {
				f := f
				r := row(f.ID, "")
				r.AddSuffix(textButton("Add…", func() { d.addFolder(f.ID, device, f.Encrypted) }))
				g.Add(r)
			}
			if len(list) == 0 {
				d.text("There are no offered folders.")
			}
		})
	})
}

func (d *desktopUI) sharedDeviceFolders(device string) {
	d.subpage(func() {
		d.reset("Shared Folders")
		d.load("engine.folders", nil, func(b json.RawMessage) {
			var folders []desktopapi.Folder
			if err := json.Unmarshal(b, &folders); err != nil {
				d.error(err)
				return
			}
			g := d.group("Shared Folders")
			for _, f := range folders {
				for _, id := range f.Devices {
					if id == device {
						folder := f
						g.Add(choiceRow(f.Label, f.Path, func() { d.share(folder) }))
						break
					}
				}
			}
		})
	})
}
