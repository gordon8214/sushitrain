// Copyright (C) 2026 Gordon
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this file,
// You can obtain one at https://mozilla.org/MPL/2.0/.

//go:build linux && cgo

package main

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/diamondburned/gotk4-adwaita/pkg/adw"
	coreglib "github.com/diamondburned/gotk4/pkg/core/glib"
	"github.com/diamondburned/gotk4/pkg/gio/v2"
	"github.com/diamondburned/gotk4/pkg/gtk/v4"
	"t-shaped.nl/sushitrain/v2/files"
	"t-shaped.nl/sushitrain/v2/remoteclient"
)

// filesView is Files mode: folders, gateways, changes that aren't on the gateway yet, and storage
type filesView struct {
	w        *mainWindow
	banner   *adw.Banner
	folders  *listGroup
	gateways *listGroup
	edits    *listGroup
	used     *adw.ActionRow
	kept     *adw.ActionRow
	budget   *adw.SpinRow
	// Each change of the budget trims the cache: only the last of a quick series is sent
	budgetChange debouncer
	// Set while the budget is shown, so that showing it doesn't set it
	showing bool
	mount   string
	busy    bool
	// A gateway's address is being changed (and checked, which can take a while): one change at a time
	changingAddress bool
	// Downloads, live while the Changes page is in view
	transfersGroup   *adw.PreferencesGroup
	transfers        *progressRows
	transfersProblem *gtk.Label
	stack            *adw.ViewStack
}

// filesState is what the view shows, fetched from the daemon
type filesState struct {
	status  files.Status
	edits   []files.Edit
	storage files.Storage
	// Fetched only while the Changes page is in view
	transfers       []files.TransferProgress
	transfersErr    error
	transfersLoaded bool
}

func newFilesView(w *mainWindow) (gtk.Widgetter, func()) { return newFilesPage(w, "") }

func newFilesPage(w *mainWindow, selected string) (gtk.Widgetter, func()) {
	v := &filesView{w: w}
	toolbar := adw.NewToolbarView()
	stack := adw.NewViewStack()
	switcher := adw.NewViewSwitcher()
	switcher.SetStack(stack)
	switcher.SetPolicy(adw.ViewSwitcherPolicyNarrow)
	switcher.SetHAlign(gtk.AlignCenter)
	switcher.SetMarginTop(4)
	switcher.SetMarginBottom(4)
	if selected == "" {
		toolbar.AddTopBar(switcher)
	}
	v.banner = adw.NewBanner("Synctrain Files isn't running")
	v.banner.SetButtonLabel("Start")
	v.banner.ConnectButtonClicked(func() {
		background(func() (struct{}, error) { return struct{}{}, systemctl("start", "synctrain-files.service") },
			func(_ struct{}, err error) { w.failed(err) })
	})
	toolbar.AddTopBar(v.banner)

	// Folders
	v.folders = newListGroup("Folders", "Folders added from gateways, in "+mountName+". They stay on their "+
		"gateway when they are removed here.")
	v.folders.group.SetHeaderSuffix(textButton("Add…", v.addFolder))
	stack.AddTitledWithIcon(page(v.folders.group), "folders", "Folders", "folder-symbolic")

	// Gateways
	v.gateways = newListGroup("Gateways", "Systems that have the folders' files: a Mac in Synchronize mode, or a "+
		"server running synctrain-gateway.")
	v.gateways.group.SetHeaderSuffix(textButton("Pair…", v.pair))
	stack.AddTitledWithIcon(page(v.gateways.group), "gateways", "Gateways", "network-server-symbolic")

	// Changes
	v.edits = newListGroup("Changes", "Changes made here that aren't on their gateway yet. Those it can't take "+
		"wait here: export a copy, or discard them to show the gateway's version again.")
	// Transfers come first, as on the Mac. Uploads are the changes below.
	transfers := adw.NewPreferencesGroup()
	transfers.SetTitle("Transfers")
	// Shown once the daemon has said what they are, and never by a daemon older than the window that can't
	transfers.SetVisible(false)
	v.transfersGroup = transfers
	v.transfers = newProgressRows("No recent transfers")
	transfers.Add(v.transfers.list)
	v.transfersProblem = gtk.NewLabel("")
	v.transfersProblem.SetXAlign(0)
	v.transfersProblem.SetWrap(true)
	v.transfersProblem.AddCSSClass("error")
	v.transfersProblem.SetVisible(false)
	transfers.Add(v.transfersProblem)
	changes := page(transfers)
	changes.Add(v.edits.group)
	stack.AddTitledWithIcon(changes, "changes", "Changes", "document-send-symbolic")

	// Storage
	storage := adw.NewPreferencesGroup()
	storage.SetTitle("Storage")
	storage.SetDescription("Files fetched from gateways are kept until the cache needs room, least recently opened " +
		"first. Files kept on this device don't count.")
	// The daemon's minimum is 100 MiB, a little over 0.1 GB
	v.budget = adw.NewSpinRowWithRange(0.2, 1000, 1)
	v.budget.SetDigits(1)
	v.budget.SetTitle("Cache size (GB)")
	v.budget.NotifyProperty("value", func() {
		if v.showing {
			return
		}
		v.budgetChange.call(func() {
			bytes := int64(v.budget.Value() * 1e9)
			background(func() (struct{}, error) {
				return struct{}{}, callFiles("storage.budget", map[string]int64{"bytes": bytes}, nil)
			}, func(_ struct{}, err error) { v.mutationFinished(err); w.failed(err) })
		})
	})
	storage.Add(v.budget)
	v.used = row("Fetched files", "")
	v.kept = row("Kept on this device", "")
	storage.Add(v.used)
	storage.Add(v.kept)
	open := row("Open the folders", "")
	open.SetActivatable(true)
	open.AddSuffix(gtk.NewImageFromIconName("folder-open-symbolic"))
	open.ConnectActivated(func() {
		if v.mount != "" {
			w.openPath(v.mount)
		}
	})
	storage.Add(open)
	stack.AddTitledWithIcon(page(storage), "storage", "Storage", "drive-harddisk-symbolic")
	if w.desk != nil {
		switch w.desk.route {
		case "Remote Access":
			stack.SetVisibleChildName("gateways")
		case "Activity":
			stack.SetVisibleChildName("changes")
		case "settings:filesAndStorage":
			stack.SetVisibleChildName("storage")
		}
	}
	if selected != "" {
		stack.SetVisibleChildName(selected)
	}
	v.stack = stack
	toolbar.SetContent(stack)

	v.refresh()
	timer := coreglib.TimeoutSecondsAdd(2, func() bool {
		v.refresh()
		return true
	})
	return toolbar, func() { coreglib.SourceRemove(timer) }
}

func page(group *adw.PreferencesGroup) *adw.PreferencesPage {
	p := adw.NewPreferencesPage()
	p.Add(group)
	return p
}

// A management action can change metadata shown by a retained Files page.
func (v *filesView) mutationFinished(err error) {
	if err == nil && v.w.desk != nil {
		v.w.desk.filesCacheInvalid = true
		v.w.desk.populateFolderShortcuts()
	}
}

// refresh shows what the daemon has now
func (v *filesView) refresh() {
	if v.busy {
		return
	}
	v.busy = true
	withTransfers := v.stack.VisibleChildName() == "changes"
	background(func() (filesState, error) {
		var st filesState
		socket := filesSocket()
		if err := callBounded(socket, "status", nil, &st.status); err != nil {
			return st, err
		}
		if err := callBounded(socket, "pending.list", nil, &st.edits); err != nil {
			return st, err
		}
		if err := callBounded(socket, "storage", nil, &st.storage); err != nil {
			return st, err
		}
		if withTransfers {
			// Not mcp.files.transfers, which brings bookmarks and places up to date at each call
			st.transfersErr = callBounded(socket, "transfers", nil, &st.transfers)
			st.transfersLoaded = true
		}
		return st, nil
	}, func(st filesState, err error) {
		v.busy = false
		v.banner.SetRevealed(err != nil)
		if err != nil {
			return
		}
		v.show(st)
	})
}

func key(v any) string {
	data, _ := json.Marshal(v)
	return string(data)
}

func (v *filesView) show(st filesState) {
	v.mount = st.status.Mount
	v.folders.set(key(st.status.Folders), func() []gtk.Widgetter {
		var rows []gtk.Widgetter
		for _, f := range st.status.Folders {
			state := f.State
			switch {
			case state == "unavailable":
				state = "not available on the gateway"
			case state == "unreachable":
				state = "gateway out of reach"
			case !f.Listed:
				state = "listing…"
			default:
				state = ""
			}
			access := "read only"
			if f.CanWrite {
				access = "read and write"
			}
			r := row(f.DirName, strings.Join(nonEmpty(f.Gateway, access, state), " · "))
			path := filepath.Join(st.status.Mount, f.DirName)
			r.AddSuffix(iconButton("folder-open-symbolic", "Open", func() { v.w.openPath(path) }))
			folder := f
			r.AddSuffix(iconButton("user-trash-symbolic", "Remove from This Device", func() { v.removeFolder(folder) }))
			rows = append(rows, r)
		}
		if len(rows) == 0 {
			rows = append(rows, row("No folders yet", "Pair with a gateway, then add its folders."))
		}
		return rows
	})
	v.gateways.set(key(st.status.Gateways), func() []gtk.Widgetter {
		var rows []gtk.Widgetter
		for _, g := range st.status.Gateways {
			state := ""
			switch g.State {
			case "unauthenticated":
				state = "pair again: the gateway revoked this machine"
			case "untrusted":
				state = "its certificate isn't the paired one: check its address, or pair again"
			}
			r := row(g.Name, strings.Join(nonEmpty(g.Address, plural(g.Folders, "folder", "folders")+" here", state),
				" · "))
			gateway := g
			r.AddSuffix(iconButton("document-edit-symbolic", "Change Address", func() { v.changeAddress(gateway) }))
			r.AddSuffix(iconButton("user-trash-symbolic", "Forget", func() { v.forget(gateway) }))
			rows = append(rows, r)
		}
		if len(rows) == 0 {
			rows = append(rows, row("No gateways yet", "Pair with one to add its folders here."))
		}
		return rows
	})
	v.edits.set(key(st.edits), func() []gtk.Widgetter {
		var rows []gtk.Widgetter
		for _, e := range st.edits {
			state := map[string]string{"queued": "waiting to be sent", "uploaded": "waiting to be sent",
				"submitted": "being sent", "pending": "the gateway is carrying it out", "failed": "refused",
				"unknown": "whether it reached the gateway is unknown", "blocked": "can't be sent"}[e.State]
			r := row(e.Path, strings.Join(nonEmpty(e.Kind, state, e.Error), " · "))
			if e.Review() {
				edit := e
				if e.Content {
					r.AddSuffix(textButton("Export…", func() { v.export(edit) }))
				}
				r.AddSuffix(textButton("Discard", func() { v.discard(edit) }))
			}
			rows = append(rows, r)
		}
		if len(rows) == 0 {
			rows = append(rows, row("Every change is on its gateway", ""))
		}
		return rows
	})
	if budget := float64(st.storage.Budget) / 1e9; !editing(v.budget) && budget != v.budget.Value() {
		v.showing = true
		v.budget.SetValue(budget)
		v.showing = false
	}
	v.used.SetSubtitle(humanBytes(st.storage.Used) + " of " + humanBytes(st.storage.Budget))
	v.kept.SetSubtitle(humanBytes(st.storage.Kept))
	if st.transfersLoaded {
		v.transfersGroup.SetVisible(!unknownMethod(st.transfersErr))
		v.transfersProblem.SetVisible(st.transfersErr != nil)
		if st.transfersErr != nil {
			v.transfersProblem.SetText(sentence(st.transfersErr))
		} else {
			v.transfers.set(filesTransferItems(st.transfers))
		}
	}
}

func plural(n int, one, many string) string {
	switch n {
	case 0:
		return "no " + many
	case 1:
		return "1 " + one
	}
	return fmt.Sprintf("%d %s", n, many)
}

func nonEmpty(parts ...string) []string {
	var result []string
	for _, p := range parts {
		if p != "" {
			result = append(result, p)
		}
	}
	return result
}

func (v *filesView) removeFolder(f files.Folder) {
	v.w.confirm("Remove “"+f.DirName+"” from This Device?", "It stays on "+f.Gateway+". Its files that are only "+
		"here are moved out first.", "Remove", true, func() {
		background(func() (files.Removal, error) {
			var r files.Removal
			err := callFiles("folders.remove", map[string]int64{"id": f.ID}, &r)
			return r, err
		}, func(r files.Removal, err error) {
			if err != nil {
				v.w.toast("“" + f.DirName + "” wasn't removed: " + err.Error())
				return
			}
			v.mutationFinished(nil)
			if r.Moved > 0 {
				v.w.toast(fmt.Sprintf("Removed. %d files that were only here are in %s", r.Moved, r.Leftovers))
			} else {
				v.w.toast("Removed “" + f.DirName + "”")
			}
			v.refresh()
		})
	})
}

func (v *filesView) forget(g files.Gateway) {
	v.w.confirm("Forget “"+g.Name+"”?", "This machine will need to pair with it again to add its folders.",
		"Forget", true, func() {
			background(func() (struct{}, error) {
				return struct{}{}, callFiles("gateways.forget", map[string]string{"serviceID": g.ServiceID}, nil)
			}, func(_ struct{}, err error) {
				v.mutationFinished(err)
				v.w.failed(err)
				v.refresh()
			})
		})
}

// changeAddress changes where this machine reaches a gateway, as when another name reaches it from outside its
// network. The pairing stays.
func (v *filesView) changeAddress(g files.Gateway) {
	if v.changingAddress {
		v.w.toast("Another address is being checked")
		return
	}
	d := adw.NewAlertDialog("Change the Address of “"+g.Name+"”?", "Where this machine reaches the gateway: a host "+
		"name or IP address, optionally followed by a colon and a port. The pairing code gave it; another name that "+
		"reaches the gateway, such as one set up for its router or VPN, lets this machine use it away from its "+
		"network. The pairing stays as it is.")
	entry := gtk.NewEntry()
	entry.SetText(g.Address)
	entry.SetActivatesDefault(true)
	d.SetExtraChild(entry)
	d.AddResponse("cancel", "Cancel")
	d.AddResponse("save", "Change")
	d.SetResponseAppearance("save", adw.ResponseSuggested)
	d.SetDefaultResponse("save")
	d.SetCloseResponse("cancel")
	check := func() {
		_, err := remoteclient.ParseAddress(entry.Text())
		d.SetResponseEnabled("save", err == nil)
		if err == nil {
			entry.RemoveCSSClass("error")
		} else {
			entry.AddCSSClass("error")
		}
	}
	entry.ConnectChanged(check)
	check()
	d.ConnectResponse(func(response string) {
		if response != "save" {
			return
		}
		address := entry.Text()
		v.changingAddress = true
		v.w.toast("Checking whether “" + g.Name + "” answers at " + strings.TrimSpace(address) + "…")
		background(func() (files.AddressChange, error) {
			var change files.AddressChange
			err := callFiles("gateways.address", map[string]string{"serviceID": g.ServiceID, "address": address},
				&change)
			return change, err
		}, func(change files.AddressChange, err error) {
			v.changingAddress = false
			v.mutationFinished(err)
			v.refresh()
			switch {
			case err != nil:
				v.w.toast("The address wasn't changed: " + err.Error())
			case change.Problem != "":
				// A dialog, or a notification once the window is closed
				v.w.report("Check the Address of “"+change.Gateway.Name+"”", "This machine reaches it at "+
					change.Gateway.Address+" from now on, but "+change.Problem+". Change the address again if "+
					"it's wrong. If it works only away from the gateway's network, it's used once this machine "+
					"is there.")
			default:
				v.w.toast("“" + change.Gateway.Name + "” is reached at " + change.Gateway.Address + " now")
			}
		})
	})
	d.Present(v.w.win)
	// Typing goes into the address, not to the default response
	d.SetFocus(entry)
}

func (v *filesView) discard(e files.Edit) {
	v.w.confirm("Discard this change?", e.Path+" shows what the gateway has again. The change can't be "+
		"recovered unless you export it first.", "Discard", true, func() {
		background(func() (struct{}, error) {
			return struct{}{}, callFiles("pending.discard", map[string]int64{"id": e.ID}, nil)
		}, func(_ struct{}, err error) {
			v.mutationFinished(err)
			v.w.failed(err)
			v.refresh()
		})
	})
}

func (v *filesView) export(e files.Edit) {
	d := gtk.NewFileDialog()
	d.SetTitle("Export the Change")
	d.SetInitialName(filepath.Base(e.Path))
	d.Save(context.Background(), v.w.parent(), func(result gio.AsyncResulter) {
		file, err := d.SaveFinish(result)
		if err != nil || file == nil {
			return
		}
		dest := file.Path()
		background(func() (struct{}, error) {
			return struct{}{}, callFiles("pending.export", map[string]any{"id": e.ID, "dest": dest}, nil)
		}, func(_ struct{}, err error) {
			if err != nil {
				v.w.toast("Not exported: " + err.Error())
				return
			}
			v.w.toast("Exported to " + dest)
		})
	})
}

// pair pairs with a gateway from the link its pairing shows
func (v *filesView) pair() {
	d := adw.NewDialog()
	closed := false
	d.ConnectClosed(func() { closed = true })
	d.SetTitle("Pair with a Gateway")
	d.SetContentWidth(520)
	toolbar := adw.NewToolbarView()
	toolbar.AddTopBar(adw.NewHeaderBar())
	group := adw.NewPreferencesGroup()
	group.SetDescription("On the gateway, start pairing (in Synctrain's Remote Files on a machine in Synchronize " +
		"mode, or with synctrain-gateway pair on a server), and paste the link it shows. Then approve this machine " +
		"there.")
	link := adw.NewEntryRow()
	link.SetTitle("Pairing link")
	group.Add(link)
	status := gtk.NewLabel("")
	status.SetWrap(true)
	status.SetMarginTop(12)
	button := gtk.NewButtonWithLabel("Pair")
	button.AddCSSClass("suggested-action")
	button.AddCSSClass("pill")
	button.SetHAlign(gtk.AlignCenter)
	button.SetMarginTop(18)
	box := gtk.NewBox(gtk.OrientationVertical, 0)
	box.SetMarginStart(18)
	box.SetMarginEnd(18)
	box.SetMarginBottom(18)
	box.Append(group)
	box.Append(status)
	box.Append(button)
	button.ConnectClicked(func() {
		text := strings.TrimSpace(link.Text())
		if text == "" {
			return
		}
		button.SetSensitive(false)
		status.SetText("Approve this machine on the gateway…")
		background(func() (files.Gateway, error) {
			var g files.Gateway
			err := callFiles("pair", map[string]string{"link": text}, &g)
			return g, err
		}, func(g files.Gateway, err error) {
			if err == nil {
				v.w.toast("Paired with " + g.Name + ". Add its folders with Add….")
				v.refresh()
			}
			if closed {
				return
			}
			button.SetSensitive(true)
			if err != nil {
				status.SetText(err.Error())
				return
			}
			v.mutationFinished(nil)
			d.Close()
		})
	})
	toolbar.SetContent(box)
	d.SetChild(toolbar)
	d.Present(v.w.win)
}

// addFolder lists the folders of the paired gateways, to add
func (v *filesView) addFolder() {
	d := adw.NewDialog()
	d.SetTitle("Add a Folder")
	d.SetContentWidth(560)
	d.SetContentHeight(520)
	toolbar := adw.NewToolbarView()
	toolbar.AddTopBar(adw.NewHeaderBar())
	prefs := adw.NewPreferencesPage()
	toolbar.SetContent(prefs)
	d.SetChild(toolbar)
	closed := false
	d.ConnectClosed(func() { closed = true })
	d.Present(v.w.win)

	type gatewayFolders struct {
		gateway files.Gateway
		folders []files.RemoteFolder
		err     error
	}
	background(func() ([]gatewayFolders, error) {
		var st files.Status
		if err := callFiles("status", nil, &st); err != nil {
			return nil, err
		}
		var result []gatewayFolders
		for _, g := range st.Gateways {
			gf := gatewayFolders{gateway: g}
			gf.err = callFiles("folders.remote", map[string]string{"serviceID": g.ServiceID}, &gf.folders)
			result = append(result, gf)
		}
		return result, nil
	}, func(list []gatewayFolders, err error) {
		if closed {
			return
		}
		if err != nil {
			v.w.failed(err)
			d.Close()
			return
		}
		if len(list) == 0 {
			group := adw.NewPreferencesGroup()
			group.SetDescription("Pair with a gateway first.")
			prefs.Add(group)
			return
		}
		for _, gf := range list {
			group := adw.NewPreferencesGroup()
			group.SetTitle(gf.gateway.Name)
			if gf.err != nil {
				group.SetDescription(gf.err.Error())
			} else if len(gf.folders) == 0 {
				group.SetDescription("The gateway lets this machine access no folders: grant some there.")
			}
			for _, f := range gf.folders {
				access := "read only"
				if f.Write {
					access = "read and write"
				}
				r := row(f.Label, f.ID+" · "+access)
				if f.Added {
					r.AddSuffix(gtk.NewLabel("Added"))
				} else {
					serviceID, folderID := gf.gateway.ServiceID, f.ID
					var add *gtk.Button
					add = textButton("Add", func() {
						add.SetSensitive(false)
						background(func() (files.Folder, error) {
							var folder files.Folder
							err := callFiles("folders.add", map[string]string{"serviceID": serviceID,
								"folderID": folderID}, &folder)
							return folder, err
						}, func(folder files.Folder, err error) {
							if err != nil {
								add.SetSensitive(true)
								v.w.toast(err.Error())
								return
							}
							v.mutationFinished(nil)
							add.SetLabel("Added")
							v.w.toast("Added “" + folder.DirName + "” to " + mountName)
							v.refresh()
						})
					})
					add.AddCSSClass("suggested-action")
					r.AddSuffix(add)
				}
				group.Add(r)
			}
			prefs.Add(group)
		}
	})
}
