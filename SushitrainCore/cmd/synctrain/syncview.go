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
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/diamondburned/gotk4-adwaita/pkg/adw"
	coreglib "github.com/diamondburned/gotk4/pkg/core/glib"
	"github.com/diamondburned/gotk4/pkg/gdk/v4"
	"github.com/diamondburned/gotk4/pkg/glib/v2"
	"github.com/diamondburned/gotk4/pkg/gtk/v4"
	"github.com/vitrun/qart/qr"
	"t-shaped.nl/sushitrain/v2/adminsock"
	"t-shaped.nl/sushitrain/v2/files"
	"t-shaped.nl/sushitrain/v2/gatewayadmin"
)

// syncView is Synchronize mode: Syncthing's folders and devices, and the remote file service through which phones
// and machines in Files mode reach them. Syncthing's own settings stay in its web GUI.
type syncView struct {
	w      *mainWindow
	home   gatewayadmin.Home
	banner *adw.Banner

	device  *adw.ActionRow
	webGUI  *adw.ActionRow
	linger  *adw.SwitchRow
	folders *listGroup
	devices *listGroup

	service      *adw.SwitchRow
	serviceState *adw.ActionRow
	port         *adw.SpinRow
	host         *adw.EntryRow
	pairButton   *gtk.Button
	exports      *listGroup
	clients      *listGroup
	operations   *adw.ActionRow
	// The rows of clients, whose subtitles change without the rows being made again (which would close them)
	clientRows map[string]*adw.ExpanderRow

	// Set while settings are shown, so that showing them doesn't change them
	showing bool
	busy    bool
}

// syncState is what the view shows, fetched from the gateway
type syncState struct {
	service    gatewayadmin.ServiceStatus
	folders    []gatewayadmin.Folder
	devices    []gatewayadmin.Device
	status     gatewayadmin.Status
	clients    []gatewayadmin.Client
	operations []gatewayadmin.PendingOperation
	linger     bool
}

// Access to a folder, as the combo rows offer it: for sharing it, and for a client
var (
	exportNames = []string{"Not shared", "Read only", "Read and write"}
	grantNames  = []string{"No access", "Read only", "Read and write"}
)

func newSyncView(w *mainWindow) (gtk.Widgetter, func()) {
	v := &syncView{w: w}
	toolbar := adw.NewToolbarView()
	header := adw.NewHeaderBar()
	stack := adw.NewViewStack()
	switcher := adw.NewViewSwitcher()
	switcher.SetStack(stack)
	switcher.SetPolicy(adw.ViewSwitcherPolicyWide)
	header.SetTitleWidget(switcher)
	header.PackEnd(w.menuButton("Switch to Files…"))
	toolbar.AddTopBar(header)
	v.banner = adw.NewBanner("Synctrain isn't running")
	v.banner.SetButtonLabel("Start")
	v.banner.ConnectButtonClicked(func() {
		background(func() (struct{}, error) { return struct{}{}, systemctl("start", "synctrain-gateway.service") },
			func(_ struct{}, err error) { w.failed(err) })
	})
	toolbar.AddTopBar(v.banner)

	// Syncthing
	this := adw.NewPreferencesGroup()
	this.SetTitle("This Device")
	v.device = row("Device ID", "")
	v.device.AddCSSClass("property")
	v.device.AddSuffix(iconButton("edit-copy-symbolic", "Copy", func() {
		if v.home.DeviceID != "" {
			v.w.win.Clipboard().SetText(v.home.DeviceID)
			v.w.toast("Device ID copied")
		}
	}))
	this.Add(v.device)
	v.webGUI = row("Web GUI", "Folders, devices and Syncthing's settings")
	v.webGUI.SetActivatable(true)
	v.webGUI.AddSuffix(gtk.NewImageFromIconName("adw-external-link-symbolic"))
	v.webGUI.ConnectActivated(func() {
		if v.home.GUIURL != "" {
			gtk.NewURILauncher(v.home.GUIURL).Launch(context.Background(), w.parent(), nil)
		}
	})
	this.Add(v.webGUI)
	v.linger = adw.NewSwitchRow()
	v.linger.SetTitle("Keep Running After Logging Out")
	v.linger.SetSubtitle("Syncthing and the remote file service go on while nobody is logged in")
	v.linger.NotifyProperty("active", func() {
		if v.showing {
			return
		}
		command := "disable-linger"
		if v.linger.Active() {
			command = "enable-linger"
		}
		background(func() (struct{}, error) { return struct{}{}, run("loginctl", command) },
			func(_ struct{}, err error) { w.failed(err) })
	})
	this.Add(v.linger)
	v.folders = newListGroup("Folders", "Add and share folders in the web GUI.")
	v.devices = newListGroup("Devices", "")
	syncPage := adw.NewPreferencesPage()
	syncPage.Add(this)
	syncPage.Add(v.folders.group)
	syncPage.Add(v.devices.group)
	stack.AddTitledWithIcon(syncPage, "syncthing", "Syncthing", "emblem-synchronizing-symbolic")

	// Remote files
	service := adw.NewPreferencesGroup()
	service.SetTitle("Remote File Service")
	service.SetDescription("Phones, and machines in Files mode, reach the folders shared here through it once they " +
		"are paired, fetching files as they are opened.")
	v.pairButton = textButton("Pair…", v.pair)
	service.SetHeaderSuffix(v.pairButton)
	v.service = adw.NewSwitchRow()
	v.service.SetTitle("Serve Paired Clients")
	v.service.NotifyProperty("active", func() {
		if !v.showing {
			v.setService(func(s *gatewayadmin.ServiceSettings) { s.Enabled = v.service.Active() })
		}
	})
	service.Add(v.service)
	v.serviceState = row("State", "")
	v.serviceState.AddCSSClass("property")
	service.Add(v.serviceState)
	v.port = adw.NewSpinRowWithRange(1, 65535, 1)
	v.port.SetTitle("Port")
	v.port.NotifyProperty("value", func() {
		if !v.showing {
			port := int(v.port.Value())
			v.setService(func(s *gatewayadmin.ServiceSettings) {
				// The same address, or all of them
				host := ""
				if i := strings.LastIndex(s.Listen, ":"); i >= 0 {
					host = s.Listen[:i]
				}
				s.Listen = host + ":" + strconv.Itoa(port)
			})
		}
	})
	service.Add(v.port)
	v.host = adw.NewEntryRow()
	v.host.SetTitle("Host Name for Clients (empty for this machine's name)")
	v.host.SetShowApplyButton(true)
	v.host.ConnectApply(func() {
		host := strings.TrimSpace(v.host.Text())
		v.setService(func(s *gatewayadmin.ServiceSettings) { s.PublicHost = host })
	})
	service.Add(v.host)
	v.operations = row("Operations in progress", "")
	v.operations.AddCSSClass("property")
	v.operations.SetVisible(false)
	service.Add(v.operations)
	v.exports = newListGroup("Shared Folders", "What paired clients may be given access to. Read only includes "+
		"previous versions.")
	v.clients = newListGroup("Clients", "Paired phones and machines, and the folders each may access.")
	remotePage := adw.NewPreferencesPage()
	remotePage.Add(service)
	remotePage.Add(v.exports.group)
	remotePage.Add(v.clients.group)
	stack.AddTitledWithIcon(remotePage, "remote", "Remote Files", "network-server-symbolic")
	toolbar.SetContent(stack)

	background(func() (gatewayadmin.Home, error) { return describeHome() }, func(h gatewayadmin.Home, err error) {
		if err != nil {
			w.toast("Syncthing's home can't be read: " + err.Error())
			return
		}
		v.home = h
		v.device.SetSubtitle(h.DeviceID)
		v.webGUI.SetSensitive(h.GUIURL != "")
		v.refresh()
	})
	timer := coreglib.TimeoutSecondsAdd(2, func() bool {
		v.refresh()
		return true
	})
	return toolbar, func() { coreglib.SourceRemove(timer) }
}

func (v *syncView) call(method string, params, result any) error {
	return adminsock.Call(v.home.Socket, method, params, result)
}

// refresh shows what the gateway has now
func (v *syncView) refresh() {
	if v.busy || v.home.Socket == "" {
		return
	}
	v.busy = true
	background(func() (syncState, error) {
		var st syncState
		st.linger = lingering()
		if err := v.call("service.status", nil, &st.service); err != nil {
			return st, err
		}
		if err := v.call("folders.list", nil, &st.folders); err != nil {
			return st, err
		}
		if err := v.call("devices.list", nil, &st.devices); err != nil {
			return st, err
		}
		if !st.service.Running {
			return st, nil
		}
		if err := v.call("status", nil, &st.status); err != nil {
			return st, err
		}
		if err := v.call("clients.list", nil, &st.clients); err != nil {
			return st, err
		}
		return st, v.call("operations.pending", nil, &st.operations)
	}, func(st syncState, err error) {
		v.busy = false
		v.banner.SetRevealed(err != nil)
		if err != nil {
			return
		}
		v.show(st)
	})
}

func (v *syncView) show(st syncState) {
	v.showing = true
	defer func() { v.showing = false }()
	v.linger.SetActive(st.linger)
	sort.Slice(st.folders, func(i, j int) bool { return st.folders[i].Label < st.folders[j].Label })
	v.folders.set(key(st.folders), func() []gtk.Widgetter {
		var rows []gtk.Widgetter
		for _, f := range st.folders {
			label := f.Label
			if label == "" {
				label = f.ID
			}
			state := f.State
			if f.Paused {
				state = "paused"
			}
			shared := "not shared"
			if len(f.Devices) > 0 {
				shared = "shared with " + plural(len(f.Devices), "device", "devices")
			}
			r := row(label, strings.Join(nonEmpty(f.Path, state, shared), " · "))
			path := f.Path
			r.AddSuffix(iconButton("folder-open-symbolic", "Open", func() { v.w.openPath(path) }))
			rows = append(rows, r)
		}
		if len(rows) == 0 {
			rows = append(rows, row("No folders yet", "Add one in the web GUI."))
		}
		return rows
	})
	v.devices.set(key(st.devices), func() []gtk.Widgetter {
		var rows []gtk.Widgetter
		for _, d := range st.devices {
			state := "not connected"
			switch {
			case d.Paused:
				state = "paused"
			case d.Connected:
				state = "connected"
			}
			name := d.Name
			if name == "" {
				name = d.ID[:7]
			}
			rows = append(rows, row(name, state))
		}
		if len(rows) == 0 {
			rows = append(rows, row("No devices yet", "Add one in the web GUI with its device ID."))
		}
		return rows
	})

	s := st.service
	v.service.SetActive(s.Enabled)
	v.service.SetSensitive(!fixed(s, "enabled"))
	switch {
	case s.Running && strings.HasPrefix(s.Listen, ":"):
		v.serviceState.SetSubtitle("Listening on port " + s.Listen[1:])
	case s.Running:
		v.serviceState.SetSubtitle("Listening on " + s.Listen)
	case s.Enabled && s.Error != "":
		v.serviceState.SetSubtitle("Not running: " + s.Error)
	case s.Enabled:
		v.serviceState.SetSubtitle("Starting…")
	default:
		v.serviceState.SetSubtitle("Off")
	}
	if i := strings.LastIndex(s.Listen, ":"); i >= 0 {
		if port, err := strconv.Atoi(s.Listen[i+1:]); err == nil {
			v.port.SetValue(float64(port))
		}
	}
	v.port.SetSensitive(!fixed(s, "listen"))
	if !v.host.HasFocus() && v.host.Text() != s.PublicHost {
		v.host.SetText(s.PublicHost)
	}
	v.host.SetSensitive(!fixed(s, "publicHost"))
	v.pairButton.SetSensitive(s.Running)
	v.operations.SetVisible(len(st.operations) > 0)
	v.operations.SetSubtitle(strconv.Itoa(len(st.operations)))

	exported := map[string]gatewayadmin.ExportStatus{}
	// What the rows show, without what changes all the time (states, item counts, when clients were last seen)
	type shownExport struct {
		ID, Label string
		Write     bool
	}
	var exports []shownExport
	for _, e := range st.status.Exports {
		exported[e.FolderID] = e
		exports = append(exports, shownExport{e.FolderID, e.Label, e.AllowWrite})
	}
	var folders []shownExport
	for _, f := range st.folders {
		folders = append(folders, shownExport{ID: f.ID, Label: f.Label})
	}
	v.exports.set(key([]any{s.Running, folders, exports}), func() []gtk.Widgetter {
		if !s.Running {
			return []gtk.Widgetter{row("The remote file service is off", "Turn it on to share folders.")}
		}
		var rows []gtk.Widgetter
		for _, f := range st.folders {
			label := f.Label
			if label == "" {
				label = f.ID
			}
			access := 0
			if e, ok := exported[f.ID]; ok {
				access = 1
				if e.AllowWrite {
					access = 2
				}
			}
			folderID := f.ID
			rows = append(rows, accessRow(label, f.ID, exportNames, access, func(access int) {
				v.setExport(folderID, access)
			}))
		}
		if len(rows) == 0 {
			rows = append(rows, row("No folders yet", "Add one in the web GUI."))
		}
		return rows
	})
	type shownClient struct {
		ID, Name    string
		Revoked     bool
		Permissions map[string]string
	}
	var clients []shownClient
	for _, c := range st.clients {
		clients = append(clients, shownClient{c.ClientID, c.Name, c.Revoked, c.Permissions})
	}
	clientSubtitle := func(c gatewayadmin.Client) string {
		seen := "never seen"
		if !c.LastSeen.IsZero() {
			seen = "last seen " + c.LastSeen.Local().Format("2 Jan 2006 15:04")
		}
		return strings.Join(nonEmpty(c.Platform, seen, plural(len(c.Permissions), "folder", "folders")), " · ")
	}
	v.clients.set(key([]any{s.Running, clients, exports}), func() []gtk.Widgetter {
		v.clientRows = map[string]*adw.ExpanderRow{}
		if !s.Running {
			return []gtk.Widgetter{row("The remote file service is off", "")}
		}
		var rows []gtk.Widgetter
		for _, c := range st.clients {
			client := c
			if c.Revoked {
				rows = append(rows, row(c.Name, strings.Join(nonEmpty(c.Platform, "revoked"), " · ")))
				continue
			}
			r := adw.NewExpanderRow()
			r.SetTitle(c.Name)
			v.clientRows[c.ClientID] = r
			for _, e := range st.status.Exports {
				label := e.Label
				if label == "" {
					label = e.FolderID
				}
				access := map[string]int{"read": 1, "write": 2}[c.Permissions[e.FolderID]]
				folderID := e.FolderID
				r.AddRow(accessRow(label, "", grantNames, access, func(access int) {
					v.setPermission(client.ClientID, folderID, access)
				}))
			}
			revoke := textButton("Revoke…", func() { v.revoke(client) })
			revoke.AddCSSClass("destructive-action")
			r.AddSuffix(revoke)
			rows = append(rows, r)
		}
		if len(rows) == 0 {
			rows = append(rows, row("No clients yet", "Pair a phone or a machine in Files mode with Pair…"))
		}
		return rows
	})
	for _, c := range st.clients {
		if r, ok := v.clientRows[c.ClientID]; ok {
			r.SetSubtitle(clientSubtitle(c))
		}
	}
}

// accessRow chooses access to a folder: 0 none, 1 read, 2 read and write
func accessRow(title, subtitle string, names []string, access int, changed func(int)) *adw.ComboRow {
	r := adw.NewComboRow()
	r.SetTitle(title)
	if subtitle != "" && subtitle != title {
		r.SetSubtitle(subtitle)
	}
	r.SetModel(gtk.NewStringList(names))
	r.SetSelected(uint(access))
	r.NotifyProperty("selected", func() { changed(int(r.Selected())) })
	return r
}

func fixed(s gatewayadmin.ServiceStatus, setting string) bool {
	for _, f := range s.Fixed {
		if f == setting {
			return true
		}
	}
	return false
}

// setService changes the remote file service's settings
func (v *syncView) setService(change func(*gatewayadmin.ServiceSettings)) {
	background(func() (gatewayadmin.ServiceStatus, error) {
		var st gatewayadmin.ServiceStatus
		if err := v.call("service.status", nil, &st); err != nil {
			return st, err
		}
		settings := st.ServiceSettings
		change(&settings)
		err := v.call("service.set", settings, &st)
		return st, err
	}, func(_ gatewayadmin.ServiceStatus, err error) {
		v.w.failed(err)
		v.refresh()
	})
}

func (v *syncView) setExport(folderID string, access int) {
	background(func() (struct{}, error) {
		if access == 0 {
			return struct{}{}, v.call("exports.remove", gatewayadmin.Export{FolderID: folderID}, nil)
		}
		return struct{}{}, v.call("exports.set", gatewayadmin.Export{FolderID: folderID, AllowWrite: access == 2}, nil)
	}, func(_ struct{}, err error) {
		v.w.failed(err)
		v.refresh()
	})
}

func (v *syncView) setPermission(clientID, folderID string, access int) {
	background(func() (struct{}, error) {
		return struct{}{}, v.call("clients.permission", map[string]string{"clientID": clientID, "folderID": folderID,
			"access": []string{"", "read", "write"}[access]}, nil)
	}, func(_ struct{}, err error) {
		v.w.failed(err)
		v.refresh()
	})
}

func (v *syncView) revoke(c gatewayadmin.Client) {
	v.w.confirm("Revoke “"+c.Name+"”?", "It can no longer reach any folder here, and has to pair again to.",
		"Revoke", true, func() {
			background(func() (struct{}, error) {
				return struct{}{}, v.call("clients.revoke", map[string]string{"clientID": c.ClientID}, nil)
			}, func(_ struct{}, err error) {
				v.w.failed(err)
				v.refresh()
			})
		})
}

// pair shows a pairing code, waits for a client to submit it, and asks which folders it gets
func (v *syncView) pair() {
	d := adw.NewDialog()
	d.SetTitle("Pair a Phone or Machine")
	d.SetContentWidth(480)
	toolbar := adw.NewToolbarView()
	toolbar.AddTopBar(adw.NewHeaderBar())
	content := gtk.NewBox(gtk.OrientationVertical, 12)
	content.SetMarginStart(24)
	content.SetMarginEnd(24)
	content.SetMarginBottom(24)
	toolbar.SetContent(content)
	d.SetChild(toolbar)
	closed := false
	d.ConnectClosed(func() { closed = true })
	d.Present(v.w.win)

	background(func() (gatewayadmin.PairingSession, error) {
		var session gatewayadmin.PairingSession
		err := v.call("pairing.start", nil, &session)
		return session, err
	}, func(session gatewayadmin.PairingSession, err error) {
		if err != nil {
			d.Close()
			v.w.toast("Pairing can't start: " + err.Error())
			return
		}
		explanation := gtk.NewLabel("Scan this code in Synctrain on an iPhone, iPad or Mac (Settings › Gateways), " +
			"or paste the link in Synctrain on a machine in Files mode. It works once, until " +
			session.Expires.Local().Format("15:04") + ".")
		explanation.SetWrap(true)
		content.Append(explanation)
		if picture := qrPicture(session.URL); picture != nil {
			content.Append(picture)
		}
		copyLink := textButton("Copy Link", func() {
			v.w.win.Clipboard().SetText(session.URL)
			v.w.toast("Pairing link copied")
		})
		copyLink.SetHAlign(gtk.AlignCenter)
		copyLink.AddCSSClass("pill")
		content.Append(copyLink)
		waiting := gtk.NewLabel("Waiting for a client…")
		waiting.AddCSSClass("dim-label")
		content.Append(waiting)
		v.waitForPairing(d, content, session, &closed)
	})
}

// waitForPairing asks the gateway for a request with the session's code until one comes, the code expires or the
// dialog is closed, and then shows the request for approval
func (v *syncView) waitForPairing(d *adw.Dialog, content *gtk.Box, session gatewayadmin.PairingSession,
	closed *bool) {
	background(func() (gatewayadmin.PendingPairing, error) {
		var request gatewayadmin.PendingPairing
		err := v.call("pairing.wait", map[string]string{"sessionID": session.SessionID}, &request)
		return request, err
	}, func(request gatewayadmin.PendingPairing, err error) {
		switch {
		case *closed:
			return
		case err != nil && strings.Contains(err.Error(), gatewayadmin.NoPairingRequestYet):
			if time.Now().After(session.Expires) {
				d.Close()
				v.w.toast("The pairing code expired")
				return
			}
			v.waitForPairing(d, content, session, closed)
		case err != nil:
			d.Close()
			v.w.toast("Pairing failed: " + err.Error())
		default:
			v.approve(d, content, request)
		}
	})
}

// approve shows a client that submitted the code, with the folders it would get
func (v *syncView) approve(d *adw.Dialog, content *gtk.Box, request gatewayadmin.PendingPairing) {
	for child := content.FirstChild(); child != nil; child = content.FirstChild() {
		content.Remove(child)
	}
	title := gtk.NewLabel(fmt.Sprintf("“%s” (%s) wants to pair.", request.ClientName, request.Platform))
	title.SetWrap(true)
	title.AddCSSClass("title-4")
	content.Append(title)
	if request.Replaces != "" {
		note := gtk.NewLabel("It uses the identity of the paired client “" + request.Replaces + "”, whose access " +
			"approving it replaces.")
		note.SetWrap(true)
		content.Append(note)
	}
	group := adw.NewPreferencesGroup()
	group.SetTitle("Folders It Gets")
	content.Append(group)
	grants := map[string]int{}
	background(func() (gatewayadmin.Status, error) {
		var st gatewayadmin.Status
		err := v.call("status", nil, &st)
		return st, err
	}, func(st gatewayadmin.Status, err error) {
		if err != nil {
			v.w.failed(err)
			return
		}
		for _, e := range st.Exports {
			access := 1
			if e.AllowWrite {
				access = 2
			}
			folderID := e.FolderID
			grants[folderID] = access
			label := e.Label
			if label == "" {
				label = e.FolderID
			}
			group.Add(accessRow(label, "", grantNames, access, func(access int) { grants[folderID] = access }))
		}
		if len(st.Exports) == 0 {
			group.SetDescription("No folders are shared yet: it gets none until you share some and give it access.")
		}
	})
	buttons := gtk.NewBox(gtk.OrientationHorizontal, 12)
	buttons.SetHAlign(gtk.AlignCenter)
	buttons.SetMarginTop(12)
	answer := func(method string, params any, done string) {
		background(func() (struct{}, error) { return struct{}{}, v.call(method, params, nil) },
			func(_ struct{}, err error) {
				d.Close()
				if err != nil {
					v.w.toast(err.Error())
					return
				}
				v.w.toast(done)
				v.refresh()
			})
	}
	deny := textButton("Deny", func() {
		answer("pairing.deny", map[string]string{"requestID": request.RequestID}, "Denied")
	})
	deny.AddCSSClass("pill")
	approve := textButton("Approve", func() {
		access := map[string]string{}
		for folderID, a := range grants {
			if a > 0 {
				access[folderID] = []string{"", "read", "write"}[a]
			}
		}
		answer("pairing.approve", map[string]any{"requestID": request.RequestID, "grants": access},
			"Paired with “"+request.ClientName+"”")
	})
	approve.AddCSSClass("pill")
	approve.AddCSSClass("suggested-action")
	buttons.Append(deny)
	buttons.Append(approve)
	content.Append(buttons)
}

// qrPicture shows a QR code of text, or nil when it can't be made
func qrPicture(text string) *gtk.Picture {
	code, err := qr.Encode(text, qr.M)
	if err != nil {
		return nil
	}
	texture, err := gdk.NewTextureFromBytes(glib.NewBytesWithGo(code.PNG()))
	if err != nil {
		return nil
	}
	picture := gtk.NewPictureForPaintable(texture)
	picture.SetCanShrink(true)
	picture.SetSizeRequest(260, 260)
	picture.SetHAlign(gtk.AlignCenter)
	return picture
}

// lingering reports whether this user's services run while they aren't logged in
func lingering() bool {
	out, err := exec.Command("loginctl", "show-user", strconv.Itoa(os.Getuid()), "--property=Linger",
		"--value").Output()
	return err == nil && strings.TrimSpace(string(out)) == "yes"
}

// gatewayCommand runs synctrain-gateway: the one next to this program, or on the PATH
func gatewayCommand(args ...string) ([]byte, error) {
	program := "synctrain-gateway"
	if self, err := os.Executable(); err == nil {
		if next := filepath.Join(filepath.Dir(self), program); fileExists(next) {
			program = next
		}
	}
	cmd := exec.Command(program, args...)
	var stderr strings.Builder
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		msg := strings.TrimPrefix(strings.TrimSpace(stderr.String()), "synctrain-gateway: ")
		if msg == "" {
			msg = err.Error()
		}
		return out, errors.New(msg)
	}
	return out, nil
}

func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

// describeHome is Syncthing's home on this machine, as the gateway would use it
func describeHome() (gatewayadmin.Home, error) {
	var h gatewayadmin.Home
	out, err := gatewayCommand("home", "--json")
	if err != nil {
		return h, err
	}
	return h, json.Unmarshal(out, &h)
}

// run runs a program, with its output as the error when it fails
func run(name string, args ...string) error {
	out, err := exec.Command(name, args...).CombinedOutput()
	if err != nil {
		if msg := strings.TrimSpace(string(out)); msg != "" {
			return errors.New(msg)
		}
	}
	return err
}

// askSynchronize sets Synchronize mode up after asking: a new Syncthing, the one already here taken over in place, or
// the one Synctrain ran before. From Files mode, the folders of ~/Synctrain are taken off this machine first.
func (w *mainWindow) askSynchronize(fromFiles bool) {
	background(describeHome, func(h gatewayadmin.Home, err error) {
		switch {
		case err != nil:
			w.toast("Synchronize mode can't be set up: " + err.Error())
			return
		case h.Problem != "":
			w.toast("The Syncthing in " + h.Dir + " can't be taken over: " + h.Problem)
			return
		case systemSyncthing():
			w.toast("Syncthing runs as a system service (syncthing@" + userName() + "): have an administrator " +
				"disable it first")
			return
		}
		id := h.DeviceID
		if len(id) > 7 {
			id = id[:7]
		}
		var heading, body, action string
		switch {
		case !h.Exists:
			heading, action = "Run Syncthing on This Machine?", "Start"
			body = "Synctrain starts Syncthing as a new device, now and at every login. Add folders and devices " +
				"in its web GUI."
		case h.Taken:
			heading, action = "Run Syncthing on This Machine?", "Start"
			body = fmt.Sprintf("Synctrain runs Syncthing again as device %s, and its folders pick up where they "+
				"were.", id)
		default:
			heading, action = "Take Over Syncthing?", "Take Over"
			body = fmt.Sprintf("Syncthing runs on this machine as device %s, with %s and %s. Synctrain runs it "+
				"from now on, with the same device, folders and database, so other devices see no change.\n\n"+
				"syncthing.service is stopped and masked, and the configuration and database are copied aside first. "+
				"Going back to the stock Syncthing needs a version at least as new as Synctrain's.", id,
				plural(len(h.Folders), "folder", "folders"), plural(h.Devices, "other device", "other devices"))
			if trayStartsSyncthing() {
				body += "\n\nSyncthing Tray is set to start Syncthing itself: turn that off in its settings " +
					"(Launcher), or it competes with Synctrain at the next login."
			}
		}
		if fromFiles {
			heading, action = "Switch to Synchronize?", "Switch"
			body = "The folders in " + mountName + " are taken off this machine first. They stay on their " +
				"gateways: changes made here are sent before that, and files that are only here are moved out of " +
				"them. The pairings with gateways stay, for switching back.\n\n" + body
		}
		w.confirm(heading, body, action, false, func() { w.startSynchronize(h, fromFiles) })
	})
}

// startSynchronize leaves Files mode (when switching from it), takes over the Syncthing here (when there is one that
// Synctrain didn't run before) and starts the gateway's unit. The mode changes last: a failure puts back what ran.
func (w *mainWindow) startSynchronize(h gatewayadmin.Home, fromFiles bool) {
	w.toast("Starting Synctrain…")
	type result struct {
		backup   string
		removals []files.Removal
	}
	background(func() (result, error) {
		var r result
		var undos []func()
		undo := func() {
			for i := len(undos) - 1; i >= 0; i-- {
				undos[i]()
			}
		}
		if fromFiles {
			if running(filesSocket()) {
				if err := callFiles("leave", map[string]time.Duration{"timeout": 2 * time.Minute},
					&r.removals); err != nil {
					return r, err
				}
			}
			if err := systemctl("disable", "--now", "synctrain-files.service"); err != nil {
				return r, err
			}
			undos = append(undos, func() { _ = systemctl("enable", "--now", "synctrain-files.service") })
		}
		if h.Exists && !h.Taken {
			wasActive := systemctl("is-active", "--quiet", "syncthing.service") == nil
			if wasActive {
				if err := systemctl("stop", "syncthing.service"); err != nil {
					undo()
					return r, err
				}
			}
			if err := systemctl("mask", "syncthing.service"); err != nil {
				undo()
				return r, err
			}
			undos = append(undos, func() {
				_ = systemctl("unmask", "syncthing.service")
				if wasActive {
					_ = systemctl("start", "syncthing.service")
				}
			})
			// One that systemd didn't start, such as from Syncthing Tray
			if _, err := gatewayCommand("home", "--stop"); err != nil {
				undo()
				return r, err
			}
			r.backup = h.Dir + ".before-synctrain-" + time.Now().Format("20060102-150405")
			if _, err := gatewayCommand("home", "--backup", r.backup); err != nil {
				undo()
				return r, err
			}
		}
		if err := systemctl("enable", "--now", "synctrain-gateway.service"); err != nil {
			undo()
			return r, err
		}
		if err := waitForSocket(h.Socket, time.Minute); err != nil {
			_ = systemctl("disable", "--now", "synctrain-gateway.service")
			undo()
			return r, err
		}
		return r, writeMode(modeSynchronize)
	}, func(r result, err error) {
		if err != nil {
			w.inform("Synctrain Couldn't Start Syncthing", sentence(err))
			w.show()
			return
		}
		var notes []string
		if r.backup != "" {
			notes = append(notes, "Its previous configuration and database are in "+r.backup+".")
		}
		for _, removal := range r.removals {
			if removal.Moved > 0 {
				notes = append(notes, "Files that were only on this machine are in "+removal.Leftovers+".")
			}
		}
		if len(notes) > 0 {
			w.inform("Synctrain Runs Syncthing", strings.Join(notes, "\n\n"))
		}
		w.show()
	})
}

// leaveSynchronize switches to Files mode: Syncthing stops, and its folders stay where they are, to pick up where they
// were when this machine switches back
func (w *mainWindow) leaveSynchronize() {
	body := "Synctrain stops running Syncthing, so this machine stops synchronizing. Its folders stay where they " +
		"are, and synchronize again when you switch back. " + mountName + " shows the folders of a gateway on " +
		"another system instead.\n\nPhones and machines paired with this one can't reach its folders until you " +
		"switch back."
	w.confirm("Switch to Files?", body, "Switch", false, func() {
		w.toast("Starting Synctrain Files…")
		background(func() (struct{}, error) {
			// syncthing.service stays masked: the engine's database may be newer than the stock Syncthing's
			if err := systemctl("disable", "--now", "synctrain-gateway.service"); err != nil {
				return struct{}{}, err
			}
			if err := systemctl("enable", "--now", "synctrain-files.service"); err != nil {
				_ = systemctl("enable", "--now", "synctrain-gateway.service")
				return struct{}{}, err
			}
			if err := waitForSocket(filesSocket(), 20*time.Second); err != nil {
				_ = systemctl("disable", "--now", "synctrain-files.service")
				_ = systemctl("enable", "--now", "synctrain-gateway.service")
				return struct{}{}, err
			}
			return struct{}{}, writeMode(modeFiles)
		}, func(_ struct{}, err error) {
			if err != nil {
				w.inform("Synctrain Files Couldn't Start", sentence(err))
			}
			w.show()
		})
	})
}

// systemSyncthing reports whether Syncthing is enabled as a system service for this user, which only an
// administrator can turn off
func systemSyncthing() bool {
	out, _ := exec.Command("systemctl", "is-enabled", "syncthing@"+userName()+".service").Output()
	return strings.TrimSpace(string(out)) == "enabled"
}

func userName() string {
	if name := os.Getenv("USER"); name != "" {
		return name
	}
	return strconv.Itoa(os.Getuid())
}

// trayStartsSyncthing reports whether Syncthing Tray is set to start Syncthing at login itself
func trayStartsSyncthing() bool {
	data, err := os.ReadFile(filepath.Join(configDir(), "syncthingtray.ini"))
	if err != nil {
		return false
	}
	section := ""
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "[") {
			section = line
			continue
		}
		if section == "[startup]" && strings.ReplaceAll(line, " ", "") == "syncthingAutostart=true" {
			return true
		}
	}
	return false
}
