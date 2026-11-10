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
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"t-shaped.nl/sushitrain/v2/linuxdesktop"
	"t-shaped.nl/sushitrain/v2/remoteclient"

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
// and machines in Files mode reach them. Native management lives in the main sidebar.
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
	portChange   debouncer
	host         *adw.EntryRow
	pairButton   *gtk.Button
	exports      *listGroup
	clients      *listGroup
	operations   *adw.ActionRow
	// The rows of clients and their folder access, which change without the rows being made again (which would
	// close them)
	clientRows map[string]*adw.ExpanderRow
	grantRows  map[string]map[string]*adw.ComboRow
	// The folders last shown, for their labels
	shownFolders []gatewayadmin.Folder

	// Set while settings are shown, so that showing them doesn't change them
	showing           bool
	busy              bool
	closed, hostDirty bool
	savedHost         string
}

// syncState is what the view shows, fetched from the gateway
type syncState struct {
	service    gatewayadmin.ServiceStatus
	folders    []gatewayadmin.Folder
	devices    []gatewayadmin.Device
	exports    []gatewayadmin.Export
	clients    []gatewayadmin.Client
	operations []gatewayadmin.PendingOperation
}

// Access to a folder, as the combo rows offer it: for sharing it, and for a client
var (
	exportNames = []string{"Not shared", "Read only", "Read and write"}
	grantNames  = []string{"No access", "Read only", "Read and write"}
)

func newSyncView(w *mainWindow) (gtk.Widgetter, func()) {
	v := &syncView{w: w}
	toolbar := adw.NewToolbarView()
	stack := adw.NewViewStack()
	switcher := adw.NewViewSwitcher()
	switcher.SetStack(stack)
	switcher.SetPolicy(adw.ViewSwitcherPolicyNarrow)
	switcher.SetHAlign(gtk.AlignCenter)
	switcher.SetMarginTop(4)
	switcher.SetMarginBottom(4)
	toolbar.AddTopBar(switcher)
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
	v.webGUI = row("Web GUI", "Optional browser access to the engine")
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
			func(_ struct{}, err error) {
				w.failed(err)
				v.showLinger()
			})
	})
	this.Add(v.linger)
	v.folders = newListGroup("Folders", "Add and share folders on the Folders page.")
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
			enabled := v.service.Active()
			v.setService(func(s *gatewayadmin.ServiceSettings) { s.Enabled = enabled })
		}
	})
	service.Add(v.service)
	v.serviceState = row("State", "")
	v.serviceState.AddCSSClass("property")
	service.Add(v.serviceState)
	v.port = adw.NewSpinRowWithRange(1, 65535, 1)
	v.port.SetTitle("Port")
	v.port.NotifyProperty("value", func() {
		if v.showing {
			return
		}
		// Each change restarts the service: only the last of a quick series is sent
		v.portChange.call(func() {
			port := int(v.port.Value())
			v.setService(func(s *gatewayadmin.ServiceSettings) {
				// The same address, or all of them
				host := ""
				if i := strings.LastIndex(s.Listen, ":"); i >= 0 {
					host = s.Listen[:i]
				}
				s.Listen = host + ":" + strconv.Itoa(port)
			})
		})
	})
	service.Add(v.port)
	v.host = adw.NewEntryRow()
	v.host.SetTitle("Address for Clients: host or host:port (empty for this machine's name and the listening port)")
	v.host.SetShowApplyButton(true)
	v.host.ConnectChanged(func() {
		if v.showing || v.closed {
			return
		}
		v.hostDirty = v.host.Text() != v.savedHost
		if w.desk != nil {
			w.desk.dirty = v.hostDirty
		}
	})
	v.host.ConnectApply(func() {
		text := strings.TrimSpace(v.host.Text())
		if w.desk == nil || w.desk.saving {
			return
		}
		if _, err := remoteclient.ParseAddress(text); text != "" && err != nil {
			w.toast(sentence(err))
			return
		}
		d := w.desk
		d.saving = true
		d.detail.SetSensitive(false)
		background(func() (string, error) {
			var st gatewayadmin.ServiceStatus
			if err := v.call("service.status", nil, &st); err != nil {
				return "", err
			}
			settings := st.ServiceSettings
			host, port, err := publicAddress(text, settings.Listen)
			if err != nil {
				return "", err
			}
			// What a flag of synctrain-gateway serve sets stays as it is
			if fixed(st, "publicPort") && port == 0 {
				port = settings.PublicPort
			}
			switch {
			case fixed(st, "publicHost") && host != settings.PublicHost:
				return "", errors.New("the host is set by a flag of synctrain-gateway serve: change only the port")
			case fixed(st, "publicPort") && port != settings.PublicPort:
				return "", errors.New("the port is set by a flag of synctrain-gateway serve: change only the host")
			}
			settings.PublicHost, settings.PublicPort = host, port
			return displayedAddress(host, port, settings.Listen), v.call("service.set", settings, nil)
		}, func(shown string, err error) {
			d.saving = false
			d.detail.SetSensitive(true)
			if v.closed || d.closed {
				return
			}
			if err == nil {
				v.hostDirty, d.dirty = false, false
				v.savedHost = shown
				v.host.SetText(shown)
			}
			v.changed(struct{}{}, err)
		})
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
	stack.SetVisibleChildName("remote")
	toolbar.SetContent(stack)
	if w.desk != nil {
		w.desk.revealSetting("gateway.enabled", v.service)
		w.desk.revealSetting("gateway.port", v.port)
		w.desk.revealSetting("gateway.host", v.host)
	}

	background(describeHome, func(h gatewayadmin.Home, err error) {
		if err != nil {
			w.toast("Syncthing's home can't be read: " + err.Error())
			return
		}
		v.home = h
		v.device.SetSubtitle(h.DeviceID)
		v.webGUI.SetSensitive(h.GUIURL != "")
		v.refresh()
	})
	v.showLinger()
	timer := coreglib.TimeoutSecondsAdd(2, func() bool {
		v.refresh()
		return true
	})
	return toolbar, func() { v.closed = true; coreglib.SourceRemove(timer) }
}

func (v *syncView) call(method string, params, result any) error {
	return adminsock.Call(v.home.Socket, method, params, result)
}

// showLinger shows whether this user's services run while they aren't logged in (asked once, not at every refresh)
func (v *syncView) showLinger() {
	background(func() (bool, error) { return lingering(), nil }, func(on bool, _ error) {
		v.showing = true
		v.linger.SetActive(on)
		v.showing = false
	})
}

// refresh shows what the gateway has now
func (v *syncView) refresh() {
	if v.busy || v.home.Socket == "" {
		return
	}
	v.busy = true
	socket := v.home.Socket
	background(func() (syncState, error) {
		var st syncState
		call := func(method string, result any) error { return callBounded(socket, method, nil, result) }
		if err := call("service.status", &st.service); err != nil {
			return st, err
		}
		if err := call("folders.list", &st.folders); err != nil {
			return st, err
		}
		if err := call("devices.list", &st.devices); err != nil {
			return st, err
		}
		if !st.service.Running {
			return st, nil
		}
		if err := call("exports.list", &st.exports); err != nil {
			return st, err
		}
		if err := call("clients.list", &st.clients); err != nil {
			return st, err
		}
		return st, call("operations.pending", &st.operations)
	}, func(st syncState, err error) {
		v.busy = false
		v.banner.SetRevealed(err != nil)
		if err != nil {
			return
		}
		v.show(st)
	})
}

// label is a folder's label, or its ID without one
func label(f gatewayadmin.Folder) string {
	if f.Label != "" {
		return f.Label
	}
	return f.ID
}

func (v *syncView) folderLabel(id string) string {
	for _, f := range v.shownFolders {
		if f.ID == id {
			return label(f)
		}
	}
	return id
}

func (v *syncView) show(st syncState) {
	v.showing = true
	defer func() { v.showing = false }()
	sort.Slice(st.folders, func(i, j int) bool { return label(st.folders[i]) < label(st.folders[j]) })
	v.shownFolders = st.folders
	v.folders.set(key(st.folders), func() []gtk.Widgetter {
		var rows []gtk.Widgetter
		for _, f := range st.folders {
			state := f.State
			if f.Paused {
				state = "paused"
			}
			shared := "not shared"
			if len(f.Devices) > 0 {
				shared = "shared with " + plural(len(f.Devices), "device", "devices")
			}
			r := row(label(f), strings.Join(nonEmpty(f.Path, state, shared), " · "))
			path := f.Path
			r.AddSuffix(iconButton("folder-open-symbolic", "Open", func() { v.w.openPath(path) }))
			rows = append(rows, r)
		}
		if len(rows) == 0 {
			rows = append(rows, row("No folders yet", "Add one on the Folders page."))
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
			rows = append(rows, row("No devices yet", "Add one on the Devices page with its device ID."))
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
	if i := strings.LastIndex(s.Listen, ":"); i >= 0 && !editing(v.port) {
		if port, err := strconv.Atoi(s.Listen[i+1:]); err == nil && float64(port) != v.port.Value() {
			v.port.SetValue(float64(port))
		}
	}
	v.port.SetSensitive(!fixed(s, "listen"))
	address := displayedAddress(s.PublicHost, s.PublicPort, s.Listen)
	if !v.hostDirty && !editing(v.host) && v.host.Text() != address {
		v.host.SetText(address)
	}
	if !v.hostDirty {
		v.savedHost = address
	}
	v.host.SetSensitive(!fixed(s, "publicHost") || !fixed(s, "publicPort"))
	v.pairButton.SetSensitive(s.Running)
	v.operations.SetVisible(len(st.operations) > 0)
	v.operations.SetSubtitle(strconv.Itoa(len(st.operations)))

	exported := map[string]gatewayadmin.Export{}
	for _, e := range st.exports {
		exported[e.FolderID] = e
	}
	// What the rows show, without what changes all the time (states, when clients were last seen, their access,
	// which is shown in place below)
	type shownFolder struct {
		ID, Label string
		Access    int
	}
	var shared, all []shownFolder
	for _, f := range st.folders {
		access := 0
		if e, ok := exported[f.ID]; ok {
			access = 1
			if e.AllowWrite {
				access = 2
			}
			shared = append(shared, shownFolder{ID: f.ID, Label: label(f)})
		}
		all = append(all, shownFolder{f.ID, label(f), access})
	}
	v.exports.set(key([]any{s.Running, all}), func() []gtk.Widgetter {
		if !s.Running {
			return []gtk.Widgetter{row("The remote file service is off", "Turn it on to share folders.")}
		}
		var rows []gtk.Widgetter
		for _, f := range all {
			folderID := f.ID
			rows = append(rows, accessRow(f.Label, f.ID, exportNames, f.Access, func(access int) {
				if !v.showing {
					v.setExport(folderID, access)
				}
			}))
		}
		if len(rows) == 0 {
			rows = append(rows, row("No folders yet", "Add one on the Folders page."))
		}
		return rows
	})
	type shownClient struct {
		ID, Name string
		Revoked  bool
	}
	var clients []shownClient
	for _, c := range st.clients {
		clients = append(clients, shownClient{c.ClientID, c.Name, c.Revoked})
	}
	access := func(c gatewayadmin.Client, folderID string) int {
		return map[string]int{"read": 1, "write": 2}[c.Permissions[folderID]]
	}
	v.clients.set(key([]any{s.Running, clients, shared}), func() []gtk.Widgetter {
		v.clientRows = map[string]*adw.ExpanderRow{}
		v.grantRows = map[string]map[string]*adw.ComboRow{}
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
			r.SetUseMarkup(false)
			r.SetTitle(c.Name)
			v.clientRows[c.ClientID] = r
			v.grantRows[c.ClientID] = map[string]*adw.ComboRow{}
			for _, f := range shared {
				folderID := f.ID
				grant := accessRow(f.Label, "", grantNames, access(c, f.ID), func(access int) {
					if !v.showing {
						v.setPermission(client.ClientID, folderID, access)
					}
				})
				v.grantRows[c.ClientID][f.ID] = grant
				r.AddRow(grant)
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
			seen := "never seen"
			if !c.LastSeen.IsZero() {
				seen = "last seen " + c.LastSeen.Local().Format("2 Jan 2006 15:04")
			}
			r.SetSubtitle(strings.Join(nonEmpty(c.Platform, seen, plural(len(c.Permissions), "folder", "folders")),
				" · "))
		}
		for folderID, grant := range v.grantRows[c.ClientID] {
			if a := uint(access(c, folderID)); grant.Selected() != a {
				grant.SetSelected(a)
			}
		}
	}
}

// accessRow chooses access to a folder: 0 none, 1 read, 2 read and write
func accessRow(title, subtitle string, names []string, access int, changed func(int)) *adw.ComboRow {
	r := adw.NewComboRow()
	r.SetUseMarkup(false)
	r.SetTitle(title)
	if subtitle != "" && subtitle != title {
		r.SetSubtitle(subtitle)
	}
	r.SetModel(gtk.NewStringList(names))
	r.SetSelected(uint(access))
	r.NotifyProperty("selected", func() { changed(int(r.Selected())) })
	return r
}

// defaultPublicHost is the host in pairing codes when none is set, as synctrain-gateway works it out: the host of its
// listening address when it has a specific one, else this machine's name
func defaultPublicHost(listen string) string {
	if host, _, err := net.SplitHostPort(listen); err == nil && host != "" && host != "0.0.0.0" && host != "::" {
		return host
	}
	name, _ := os.Hostname()
	return name
}

// publicAddress reads the address for clients the person entered: host or host:port (a port left out is 0, the
// listening port). Empty text, and the default host (defaultPublicHost), are an empty host that follows the default.
func publicAddress(text, listen string) (host string, port int, err error) {
	if strings.TrimSpace(text) == "" {
		return "", 0, nil
	}
	a, err := remoteclient.ParseAddress(text)
	if err != nil {
		return "", 0, err
	}
	if strings.EqualFold(a.Host, defaultPublicHost(listen)) {
		a.Host = ""
	}
	return a.Host, a.Port, nil
}

// displayedAddress is the address for clients as the field shows it: the host, with the port when it has one
func displayedAddress(host string, port int, listen string) string {
	switch {
	case port == 0:
		return host
	case host == "":
		host = defaultPublicHost(listen)
	}
	return remoteclient.FormatAddress(host, port)
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
	background(func() (struct{}, error) {
		var st gatewayadmin.ServiceStatus
		if err := v.call("service.status", nil, &st); err != nil {
			return struct{}{}, err
		}
		settings := st.ServiceSettings
		change(&settings)
		return struct{}{}, v.call("service.set", settings, nil)
	}, v.changed)
}

// changed shows what the gateway has after a change. A refused one is undone on the page: the rows showing the
// choice are made again from what the gateway has.
func (v *syncView) changed(_ struct{}, err error) {
	if err != nil {
		v.w.toast(err.Error())
		v.exports.key, v.clients.key = "", ""
	}
	v.refresh()
}

func (v *syncView) setExport(folderID string, access int) {
	background(func() (struct{}, error) {
		if access == 0 {
			return struct{}{}, v.call("exports.remove", gatewayadmin.Export{FolderID: folderID}, nil)
		}
		return struct{}{}, v.call("exports.set", gatewayadmin.Export{FolderID: folderID, AllowWrite: access == 2}, nil)
	}, v.changed)
}

func (v *syncView) setPermission(clientID, folderID string, access int) {
	background(func() (struct{}, error) {
		return struct{}{}, v.call("clients.permission", map[string]string{"clientID": clientID, "folderID": folderID,
			"access": []string{"", "read", "write"}[access]}, nil)
	}, v.changed)
}

func (v *syncView) revoke(c gatewayadmin.Client) {
	v.w.confirm("Revoke “"+c.Name+"”?", "It can no longer reach any folder here, and has to pair again to.",
		"Revoke", true, func() {
			background(func() (struct{}, error) {
				return struct{}{}, v.call("clients.revoke", map[string]string{"clientID": c.ClientID}, nil)
			}, v.changed)
		})
}

// pairing is a pairing dialog, which may be closed while its calls run
type pairing struct {
	v       *syncView
	d       *adw.Dialog
	content *gtk.Box
	closed  bool
}

func (p *pairing) close() {
	if !p.closed {
		p.d.Close()
	}
}

// pair shows a pairing code, waits for a client to submit it, and asks which folders it gets
func (v *syncView) pair() {
	p := &pairing{v: v, d: adw.NewDialog(), content: gtk.NewBox(gtk.OrientationVertical, 12)}
	p.d.SetTitle("Pair a Phone or Machine")
	p.d.SetContentWidth(480)
	toolbar := adw.NewToolbarView()
	toolbar.AddTopBar(adw.NewHeaderBar())
	p.content.SetMarginStart(24)
	p.content.SetMarginEnd(24)
	p.content.SetMarginBottom(24)
	toolbar.SetContent(p.content)
	p.d.SetChild(toolbar)
	p.d.ConnectClosed(func() { p.closed = true })
	p.d.Present(v.w.win)

	background(func() (gatewayadmin.PairingSession, error) {
		var session gatewayadmin.PairingSession
		err := v.call("pairing.start", nil, &session)
		return session, err
	}, func(session gatewayadmin.PairingSession, err error) {
		if p.closed {
			return
		}
		if err != nil {
			p.close()
			v.w.toast("Pairing can't start: " + err.Error())
			return
		}
		explanation := gtk.NewLabel("Scan this code in Synctrain on an iPhone, iPad or Mac (Settings › Gateways), " +
			"or paste the link in Synctrain on a machine in Files mode. It works once, until " +
			session.Expires.Local().Format("15:04") + ".")
		explanation.SetWrap(true)
		p.content.Append(explanation)
		if picture := qrPicture(session.URL); picture != nil {
			p.content.Append(picture)
		}
		copyLink := textButton("Copy Link", func() {
			v.w.win.Clipboard().SetText(session.URL)
			v.w.toast("Pairing link copied")
		})
		copyLink.SetHAlign(gtk.AlignCenter)
		copyLink.AddCSSClass("pill")
		p.content.Append(copyLink)
		waiting := gtk.NewLabel("Waiting for a client…")
		waiting.AddCSSClass("dim-label")
		p.content.Append(waiting)
		p.wait(session)
	})
}

// wait asks the gateway for a request with the session's code until one comes, the code expires or the dialog is
// closed, and then shows the request for approval
func (p *pairing) wait(session gatewayadmin.PairingSession) {
	background(func() (gatewayadmin.PendingPairing, error) {
		var request gatewayadmin.PendingPairing
		err := p.v.call("pairing.wait", map[string]string{"sessionID": session.SessionID}, &request)
		return request, err
	}, func(request gatewayadmin.PendingPairing, err error) {
		switch {
		case p.closed:
			return
		case err != nil && strings.Contains(err.Error(), gatewayadmin.NoPairingRequestYet):
			if time.Now().After(session.Expires) {
				p.close()
				p.v.w.toast("The pairing code expired")
				return
			}
			p.wait(session)
		case err != nil:
			p.close()
			p.v.w.toast("Pairing failed: " + err.Error())
		default:
			p.approve(request)
		}
	})
}

// approve shows a client that submitted the code, with the folders it would get
func (p *pairing) approve(request gatewayadmin.PendingPairing) {
	v := p.v
	for child := p.content.FirstChild(); child != nil; child = p.content.FirstChild() {
		p.content.Remove(child)
	}
	title := gtk.NewLabel(fmt.Sprintf("“%s” (%s) wants to pair.", request.ClientName, request.Platform))
	title.SetWrap(true)
	title.AddCSSClass("title-4")
	p.content.Append(title)
	if request.Replaces != "" {
		note := gtk.NewLabel("It uses the identity of the paired client “" + request.Replaces + "”, whose access " +
			"approving it replaces.")
		note.SetWrap(true)
		p.content.Append(note)
	}
	group := adw.NewPreferencesGroup()
	group.SetTitle("Folders It Gets")
	p.content.Append(group)
	buttons := gtk.NewBox(gtk.OrientationHorizontal, 12)
	buttons.SetHAlign(gtk.AlignCenter)
	buttons.SetMarginTop(12)
	grants := map[string]int{}
	var deny, approve *gtk.Button
	answer := func(method string, params any, done string) {
		deny.SetSensitive(false)
		approve.SetSensitive(false)
		background(func() (struct{}, error) { return struct{}{}, v.call(method, params, nil) },
			func(_ struct{}, err error) {
				p.close()
				if err != nil {
					v.w.toast(err.Error())
					return
				}
				v.w.toast(done)
				v.refresh()
			})
	}
	deny = textButton("Deny", func() {
		answer("pairing.deny", map[string]string{"requestID": request.RequestID}, "Denied")
	})
	deny.AddCSSClass("pill")
	approve = textButton("Approve", func() {
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
	// Until the folders it would get are known
	approve.SetSensitive(false)
	buttons.Append(deny)
	buttons.Append(approve)
	p.content.Append(buttons)
	background(func() ([]gatewayadmin.Export, error) {
		var exports []gatewayadmin.Export
		err := v.call("exports.list", nil, &exports)
		return exports, err
	}, func(exports []gatewayadmin.Export, err error) {
		if p.closed {
			return
		}
		if err != nil {
			group.SetDescription("The shared folders can't be listed: " + err.Error())
			return
		}
		for _, e := range exports {
			access := 1
			if e.AllowWrite {
				access = 2
			}
			folderID := e.FolderID
			grants[folderID] = access
			group.Add(accessRow(v.folderLabel(folderID), "", grantNames, access, func(access int) {
				grants[folderID] = access
			}))
		}
		if len(exports) == 0 {
			group.SetDescription("No folders are shared yet: it gets none until you share some and give it access.")
		}
		approve.SetSensitive(true)
	})
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

// synchronizeCheck is what setting Synchronize mode up needs to know first, found away from the main loop
type synchronizeCheck struct {
	home gatewayadmin.Home
	// Why it can't be set up, when it can't
	refusal string
	tray    bool
}

func checkSynchronize() (synchronizeCheck, error) {
	c, err := linuxdesktop.Inspect()
	return synchronizeCheck{home: c.Home, refusal: c.Refusal, tray: c.Tray}, err
}

// askSynchronize sets Synchronize mode up after asking: a new Syncthing, the one already here taken over in place, or
// the one Synctrain ran before. From Files mode, the folders of ~/Synctrain are taken off this machine first.
func (w *mainWindow) askSynchronize(fromFiles bool) {
	background(checkSynchronize, func(c synchronizeCheck, err error) {
		switch {
		case err != nil:
			w.inform("Synchronize Mode Can't Be Set Up", sentence(err))
			return
		case c.refusal != "":
			w.inform("Synchronize Mode Can't Be Set Up", c.refusal)
			return
		}
		h := c.home
		id := h.DeviceID
		if len(id) > 7 {
			id = id[:7]
		}
		var heading, body, action string
		switch {
		case !h.Exists:
			heading, action = "Run Syncthing on This Machine?", "Start"
			body = "Synctrain starts Syncthing as a new device, now and at every login. Add folders and devices " +
				"in Synctrain."
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
		}
		if c.tray {
			body += "\n\nSyncthing Tray is set to start Syncthing itself: turn that off in its settings " +
				"(Launcher), or it competes with Synctrain at the next login."
		}
		if fromFiles {
			heading, action = "Switch to Synchronize?", "Switch"
			body = "The folders in " + mountName + " are taken off this machine. They stay on their gateways: " +
				"changes made here are sent first, and files that are only here are moved out of them. The " +
				"pairings with gateways stay, for switching back.\n\n" + body
		}
		w.confirm(heading, body, action, false, func() { w.startSynchronize(h, fromFiles) })
	})
}

// synchronizeResult is what a setup of Synchronize mode has to tell, whether it worked or not
type synchronizeResult struct {
	backup   string
	removals []files.Removal
	// Set once the gateway may have opened Syncthing's database, which the stock Syncthing then mustn't be
	// restarted on: the engine's may be newer
	gatewayStarted bool
}

func (r synchronizeResult) notes() string {
	var notes []string
	if r.backup != "" {
		notes = append(notes, "The previous configuration and database are in "+r.backup+".")
	}
	for _, removal := range r.removals {
		if removal.Moved > 0 {
			notes = append(notes, "Files that were only on this machine are in "+removal.Leftovers+".")
		}
	}
	return strings.Join(notes, "\n\n")
}

// startSynchronize takes over the Syncthing here (when there is one that Synctrain didn't run before), leaves Files
// mode (when switching from it) and starts the gateway's unit. Files mode is left last of what can be undone, as its
// folders can't be put back. The mode changes last; a failure puts back what ran, except the stock Syncthing once
// the gateway may have changed its database.
func (w *mainWindow) startSynchronize(h gatewayadmin.Home, fromFiles bool) {
	if !w.beginChange("Starting Syncthing") {
		return
	}
	background(func() (synchronizeResult, error) {
		result, err := linuxdesktop.Synchronize(h, fromFiles)
		return synchronizeResult{backup: result.Backup, removals: result.Removals, gatewayStarted: result.GatewayStarted}, err
	}, func(r synchronizeResult, err error) {
		notes := r.notes()
		switch {
		case err != nil && r.gatewayStarted && r.backup != "":
			w.report("Synctrain Couldn't Start Syncthing", sentence(err)+"\n\nsyncthing.service stays masked, as "+
				"Synctrain's engine may have changed the database. "+notes+" To go back, restore it there, then "+
				"unmask and start syncthing.service.")
		case err != nil:
			w.report("Synctrain Couldn't Start Syncthing", strings.TrimSpace(sentence(err)+"\n\n"+notes))
		case notes != "":
			w.report("Synctrain Runs Syncthing", notes)
		}
		w.endChange()
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
		if !w.beginChange("Starting Synctrain Files") {
			return
		}
		background(func() (struct{}, error) {
			return struct{}{}, linuxdesktop.Files(true)
		}, func(_ struct{}, err error) {
			if err != nil {
				w.report("Synctrain Files Couldn't Start", sentence(err))
			}
			w.endChange()
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
