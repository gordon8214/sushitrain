// Copyright (C) 2026 Gordon
// SPDX-License-Identifier: MPL-2.0

// Package enginecontrol exposes validated native desktop operations on an engine
// whose lifecycle belongs to its host. It never starts another Syncthing instance.
package enginecontrol

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"t-shaped.nl/sushitrain/v2/adminsock"
	"t-shaped.nl/sushitrain/v2/desktopapi"
	"t-shaped.nl/sushitrain/v2/safecopy"
	core "t-shaped.nl/sushitrain/v2/src"
)

type Request = desktopapi.Request
type Folder = desktopapi.Folder
type Device = desktopapi.Device
type Entry = desktopapi.Entry
type Version = desktopapi.Version

type Control struct {
	previewMu  sync.Mutex
	thumbMu    sync.Mutex
	thumbSlots chan struct{}
	thumbnails map[string][]byte
	ctx        context.Context
	cancel     context.CancelFunc
	Client     *core.Client
	Restart    func() error
	Pause      func(context.Context, []string, bool) error
	mu         sync.RWMutex
	editors    map[string]*draft
	closed     bool
}

func New(c *core.Client, restart func() error) *Control {
	ctx, cancel := context.WithCancel(context.Background())
	return &Control{Client: c, Restart: restart, editors: map[string]*draft{}, ctx: ctx, cancel: cancel, thumbSlots: make(chan struct{}, 1), thumbnails: map[string][]byte{}}
}
func (c *Control) Close() {
	c.cancel()
	c.mu.Lock()
	defer c.mu.Unlock()
	c.closed = true
	clear(c.editors)
}
func Strings(list *core.ListOfStrings) []string {
	out := []string{}
	if list != nil {
		for i := 0; i < list.Count(); i++ {
			out = append(out, list.ItemAt(i))
		}
	}
	return out
}
func (c *Control) Register(a *adminsock.Server) {
	desktopapi.RegisterItems(a, c.itemStatus)
	for _, name := range []string{"engine.diagnostics", "engine.differences", "engine.snapshots", "engine.exportSnapshot", "engine.inspect", "engine.read", "engine.replace", "engine.mkdir", "engine.import", "engine.rename", "engine.move", "engine.status", "engine.folders", "engine.devices", "engine.addFolder", "engine.addDevice", "engine.action", "engine.browse", "engine.browsePage", "engine.thumbnail", "engine.search", "engine.pending", "engine.pendingFolders", "engine.ignored", "engine.stats", "engine.logs", "engine.share", "engine.versions", "engine.restoreVersion", "engine.exportVersion", "engine.previewVersion", "engine.exportCurrent", "engine.resolvePath", "engine.captureRestore", "engine.restoreDownloaded", "settings.get", "settings.save", "settings.close", "configuration.export"} {
		a.Register(name, func(ctx context.Context, params json.RawMessage) (any, error) {
			var p Request
			if len(params) > 0 {
				if err := json.Unmarshal(params, &p); err != nil {
					return nil, err
				}
			}
			return c.Call(ctx, name, p)
		})
	}
}
func (c *Control) Call(ctx context.Context, method string, p Request) (any, error) {
	if strings.HasPrefix(method, "settings.") {
		c.mu.Lock()
		defer c.mu.Unlock()
	} else {
		c.mu.RLock()
		defer c.mu.RUnlock()
	}
	if c.ctx != nil {
		var cancel context.CancelFunc
		ctx, cancel = context.WithCancel(ctx)
		stop := context.AfterFunc(c.ctx, cancel)
		defer stop()
		defer cancel()
	}
	if c.closed || c.ctx != nil && c.ctx.Err() != nil {
		return nil, errors.New("the engine restarted; refresh this page")
	}
	cl := c.Client
	if cl == nil {
		return nil, errors.New("the engine is not running")
	}
	if cl.EngineState() != core.EngineStateRunning {
		if method == "engine.status" {
			return map[string]any{"state": cl.EngineState(), "error": cl.EngineError(), "engineVersion": core.Version()}, nil
		}
		return nil, errors.New("the engine is not running; wait for startup or restart it in Synctrain")
	}
	if p.Folder != "" {
		if _, err := c.folder(p.Folder); err != nil {
			return nil, err
		}
	}
	if p.Device != "" {
		peer := cl.PeerWithID(p.Device)
		if peer == nil {
			return nil, errors.New("invalid device ID")
		}
	}
	if method == "engine.pendingFolders" && p.Device == "" {
		return nil, errors.New("choose a device")
	}
	switch method {
	case "engine.diagnostics":
		b, err := cl.DiagnosticsJSON()
		return json.RawMessage(b), err
	case "engine.differences":
		f, err := c.folder(p.Folder)
		if err != nil {
			return nil, err
		}
		stats, err := f.Statistics()
		if err != nil {
			return nil, err
		}
		needed, err := f.FilesNeeded()
		if err != nil {
			return nil, err
		}
		return map[string]any{"statistics": stats, "needed": Strings(needed), "localChanges": f.LocalChangedItems(), "outOfSync": f.OutOfSyncItems()}, nil
	case "engine.snapshots":
		f, err := c.folder(p.Folder)
		if err != nil {
			return nil, err
		}
		text, err := f.VersionSnapshots(p.Path)
		return json.RawMessage(text), err
	case "engine.exportSnapshot":
		f, err := c.folder(p.Folder)
		if err != nil {
			return nil, err
		}
		return nil, f.ExportVersionSnapshotContext(ctx, p.Path, p.ID, p.Token, p.Name)
	case "engine.inspect", "engine.read", "engine.replace", "engine.mkdir", "engine.import", "engine.rename", "engine.move":
		return c.fileOperation(ctx, method, p)
	case "engine.status":
		name, _ := cl.GetName()
		return map[string]any{"engineVersion": core.Version(), "error": cl.EngineError(), "deviceID": cl.DeviceID(), "name": name, "state": cl.EngineState(), "restartRequired": cl.IsEngineRestartRequired(), "guiURL": cl.GUIURL(), "guiEnabled": cl.IsGUIEnabled()}, nil
	case "engine.folders":
		out := []Folder{}
		for _, id := range Strings(cl.Folders()) {
			f := cl.FolderWithID(id)
			if f == nil {
				continue
			}
			out = append(out, Folder{id, f.Label(), f.Path(), f.FolderType(), f.State(), f.StateError(), f.IsPaused(), f.CanRemoveFiles(), f.FailedItemCount(), Strings(f.SharedWithDeviceIDs())})
		}
		return out, nil
	case "engine.devices":
		out := []Device{}
		for _, id := range c.peers() {
			d := cl.PeerWithID(id)
			if d == nil {
				continue
			}
			out = append(out, Device{id, d.Name(), cl.GetLastPeerAddress(id), d.IsConnected(), d.IsPaused()})
		}
		return out, nil
	case "engine.addFolder":
		if strings.TrimSpace(p.ID) == "" {
			return nil, errors.New("enter a folder ID")
		}
		if cl.FolderWithID(p.ID) != nil {
			return nil, errors.New("this folder ID is already configured")
		}
		if p.Action != "" && p.Action != "sendreceive" && p.Action != "sendonly" && p.Action != "receiveonly" && p.Action != "receiveencrypted" {
			return nil, errors.New("choose a supported folder type")
		}
		if err := cl.AddFolder(p.ID, p.Path, p.Enabled || p.Action == "receiveencrypted"); err != nil {
			return nil, err
		}
		f, err := c.folder(p.ID)
		if err != nil {
			return nil, err
		}
		if p.Action != "" && p.Action != "receiveencrypted" {
			kind := p.Action
			if kind == "sendreceive" {
				kind = core.FolderTypeSendReceive
			}
			if err := f.SetFolderType(kind); err != nil {
				return nil, err
			}
		}
		if p.Name != "" {
			if err := f.SetLabel(p.Name); err != nil {
				return nil, err
			}
		}
		if p.Device != "" {
			return nil, f.ShareWithDevice(p.Device, true, p.Password)
		}
		return nil, nil
	case "engine.addDevice":
		if p.ID == cl.DeviceID() {
			return nil, errors.New("this is this machine’s device ID")
		}
		return nil, cl.AddPeer(p.ID)
	case "engine.share":
		f, err := c.folder(p.Folder)
		if err != nil {
			return nil, err
		}
		return nil, f.ShareWithDevice(p.Device, p.Enabled, p.Password)
	case "engine.pendingFolders":
		peer := cl.PeerWithID(p.Device)
		list, err := peer.PendingFolderIDs()
		if err != nil {
			return nil, err
		}
		out := []map[string]any{}
		for _, id := range Strings(list) {
			encrypted, err := cl.IsPendingFolderOfferedReceiveEncrypted(id)
			if err != nil {
				return nil, err
			}
			out = append(out, map[string]any{"ID": id, "Encrypted": encrypted})
		}
		return out, nil
	case "engine.pending", "engine.ignored":
		list, err := cl.PendingDevices()
		if method == "engine.ignored" {
			list = cl.IgnoredDevices()
			err = nil
		}
		if err != nil {
			return nil, err
		}
		out := []*core.ObservedDevice{}
		for i := 0; i < list.Count(); i++ {
			out = append(out, list.ItemAt(i))
		}
		return out, nil
	case "engine.stats":
		stats, err := cl.Statistics()
		if err != nil {
			return nil, err
		}
		return map[string]any{"totals": stats, "download": cl.GetTotalDownloadProgress(), "upload": cl.GetTotalUploadProgress()}, nil
	case "engine.logs":
		b, e := cl.LogLinesJSON(p.Offset)
		return json.RawMessage(b), e
	case "engine.thumbnail":
		return c.thumbnail(ctx, p)
	case "engine.browsePage":
		p.Action = "page"
		return c.browse(ctx, p, false)
	case "engine.browse", "engine.search":
		return c.browse(ctx, p, method == "engine.search")
	case "engine.resolvePath":
		if !filepath.IsAbs(p.Path) {
			return nil, errors.New("choose a full local file path")
		}
		for _, id := range Strings(cl.Folders()) {
			f := cl.FolderWithID(id)
			if f == nil {
				continue
			}
			rel, err := filepath.Rel(f.Path(), p.Path)
			if err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
				return map[string]string{"Folder": id, "Path": filepath.ToSlash(rel)}, nil
			}
		}
		return nil, errors.New("choose a file in a synchronized folder")
	case "engine.exportCurrent":
		return nil, c.exportCurrent(ctx, p)
	case "engine.versions", "engine.restoreVersion", "engine.exportVersion", "engine.previewVersion":
		f, err := c.folder(p.Folder)
		if err != nil {
			return nil, err
		}
		list, err := f.FileVersions(p.Path)
		if err != nil {
			return nil, err
		}
		out := []Version{}
		for i := 0; i < list.Count(); i++ {
			v := list.ItemAt(i)
			id := fmt.Sprint(v.VersionTime().UnixMilliseconds())
			revision, err := f.FileVersionRevision(p.Path, v)
			if err != nil {
				return nil, err
			}
			if id == p.ID {
				if p.Token != "" && revision != p.Token {
					return nil, errors.New("the selected archive changed; refresh before restoring or exporting it")
				}
				if method == "engine.restoreVersion" {
					if p.Token != "" {
						return nil, f.RestoreFileVersionChecked(ctx, p.Path, v, p.Token)
					}
					return nil, f.RestoreFileVersion(p.Path, v)
				}
				if method == "engine.exportVersion" || method == "engine.previewVersion" {
					return c.exportVersion(ctx, p, v, method == "engine.previewVersion")
				}
			}
			out = append(out, Version{ID: id, Revision: revision, Size: v.Size, Archived: v.VersionTime().UnixMilliseconds(), Modified: v.ModTime().UnixMilliseconds()})
		}
		if method != "engine.versions" {
			return nil, errors.New("the selected version is no longer kept")
		}
		return out, nil
	case "engine.captureRestore":
		f, err := c.folder(p.Folder)
		if err != nil {
			return nil, err
		}
		return f.CaptureRestoreTarget(p.Path)
	case "engine.restoreDownloaded":
		f, err := c.folder(p.Folder)
		if err != nil {
			return nil, err
		}
		return nil, f.RestoreDownloadedVersionContext(ctx, p.Path, p.Name, p.Token, p.Offset)
	case "configuration.export":
		data, err := os.ReadFile(cl.ConfigurationFilePath())
		if err != nil {
			return nil, err
		}
		dest, err := safecopy.Create(p.Path)
		if err != nil {
			return nil, err
		}
		defer dest.Close()
		if _, err = dest.File.Write(data); err != nil {
			return nil, err
		}
		return nil, dest.Commit()

	case "settings.get":
		return c.getSettings(p.Section)
	case "settings.save":
		return c.saveSettings(p)
	case "settings.close":
		delete(c.editors, p.Token)
		return nil, nil
	case "engine.action":
		if p.Action == "mkdir" || p.Action == "import" || p.Action == "rename" || p.Action == "move" || p.Action == "trashFile" {
			f, err := c.folder(p.Folder)
			if err != nil {
				return nil, err
			}
			if mutator, ok := any(f).(interface {
				DesktopFileAction(context.Context, string, string, string, string, string) error
			}); ok {
				return nil, mutator.DesktopFileAction(ctx, p.Action, p.Path, p.Name, p.ID, p.Token)
			}
			return nil, errors.New("native file actions are not available on this host")
		}
		return c.action(ctx, p)
	}
	return nil, fmt.Errorf("unknown engine operation %q", method)
}
func (c *Control) action(ctx context.Context, p Request) (any, error) {
	cl := c.Client
	if c.Pause != nil && (p.Action == "pauseAll" || p.Action == "resumeAll" || (p.Device != "" && (p.Action == "pause" || p.Action == "resume"))) {
		ids := c.peers()
		if p.Device != "" {
			ids = []string{p.Device}
		}
		return nil, c.Pause(ctx, ids, p.Action == "pause" || p.Action == "pauseAll")
	}
	if p.Folder != "" {
		f, err := c.folder(p.Folder)
		if err != nil {
			return nil, err
		}
		switch p.Action {
		case "pause":
			return nil, f.SetPaused(true)
		case "resume":
			return nil, f.SetPaused(false)
		case "rescan":
			if p.Path != "" {
				return nil, f.RescanSubdirectory(p.Path)
			}
			return nil, f.Rescan()
		case "remove":
			return nil, f.Unlink()
		case "relocate":
			return f.RelocateInto(p.Path)
		case "revert":
			return nil, f.Revert()
		case "override":
			return nil, f.Override()
		case "failed":
			list := f.FailedItems()
			out := []*core.FailedItem{}
			for i := 0; i < list.Count(); i++ {
				out = append(out, list.ItemAt(i))
			}
			return out, nil
		case "ignores":
			list, e := f.IgnoreLines()
			return Strings(list), e
		case "saveIgnores":
			return nil, f.SetIgnoreLines(core.List(strings.Split(strings.ReplaceAll(p.Path, "\r\n", "\n"), "\n")))
		case "deleted":
			list, e := f.DeletedFiles()
			if e != nil {
				return nil, e
			}
			out := []*core.VersionedFile{}
			for i := 0; i < list.Count(); i++ {
				out = append(out, list.ItemAt(i))
			}
			return out, nil
		case "restoreDeleted":
			return f.RestoreLatestVersions(core.List(strings.Split(p.Path, "\n")))
		case "deleteFile":
			e, err := f.GetFileInformation(p.Path)
			if err != nil {
				return nil, err
			}
			if e == nil {
				return nil, errors.New("file no longer exists")
			}
			return nil, e.Remove()
		}
	}
	if p.Device != "" {
		d := cl.PeerWithID(p.Device)
		switch p.Action {
		case "pause":
			return nil, d.SetPaused(true)
		case "resume":
			return nil, d.SetPaused(false)
		case "remove":
			return nil, d.Remove()
		}
	}
	switch p.Action {
	case "pauseAll":
		return nil, cl.SetDevicesPaused(cl.Peers(), true)
	case "resumeAll":
		return nil, cl.SetDevicesPaused(cl.Peers(), false)
	case "restart":
		return nil, c.Restart()
	case "rescanAll":
		for _, id := range Strings(cl.Folders()) {
			f := cl.FolderWithID(id)
			if f == nil {
				continue
			}
			if err := f.Rescan(); err != nil {
				return nil, err
			}
		}
		return nil, nil
	case "dismiss":
		return nil, cl.DismissPendingDevice(p.ID)
	case "ignore":
		return nil, cl.IgnoreDevice(p.ID)
	case "unignore":
		return nil, cl.UnignoreDevice(p.ID)
	case "gui":
		return nil, cl.SetGUIEnabled(p.Enabled)
	case "name":
		return nil, cl.SetName(p.Name)
	case "maintenance":
		return nil, cl.PerformMaintenanceBlocking()
	case "resetLogLevels":
		cl.ResetLogLevels()
		return nil, nil
	case "logFacilities":
		b, e := cl.LogFacilitiesJSON()
		return json.RawMessage(b), e
	case "logLevel":
		levels := map[string]int64{"debug": -4, "info": 0, "warn": 4, "error": 8}
		if p.ID != "" {
			level, ok := levels[p.ID]
			if !ok {
				return nil, errors.New("choose debug, info, warn, or error")
			}
			p.Offset = level
		}
		cl.SetLogLevel(p.Name, int(p.Offset))
		return nil, nil
	}
	return nil, errors.New("this action is not available")
}
func entry(e *core.Entry) Entry {
	native := ""
	if e.IsLocallyPresent() {
		native, _ = e.LocalNativePath()
	}
	result := Entry{Path: e.Path(), Name: e.Name(), MIME: e.MIMEType(), URL: e.OnDemandURL(), NativePath: native, Size: e.Size(), Directory: e.IsDirectory(), Local: e.IsLocallyPresent()}
	if identified, ok := any(e).(interface{ DesktopIdentity() string }); ok {
		result.Identity = identified.DesktopIdentity()
	}
	return result
}

type collector struct {
	ctx     context.Context
	entries []Entry
}

func (s *collector) Result(e *core.Entry) {
	if e != nil {
		s.entries = append(s.entries, entry(e))
	}
}
func (s *collector) IsCancelled() bool { return s.ctx.Err() != nil }
func (c *Control) browse(ctx context.Context, p Request, search bool) (any, error) {
	if search {
		s := &collector{ctx: ctx, entries: []Entry{}}
		prefix := strings.TrimSuffix(p.Path, "/")
		if prefix != "" {
			prefix += "/"
		}
		err := c.Client.Search(p.Name, s, 10000, p.Folder, prefix)
		return s.entries, err
	}
	if p.Action == "page" {
		return c.browsePage(ctx, p)
	}
	f, err := c.folder(p.Folder)
	if err != nil {
		return nil, err
	}
	out := []Entry{}
	for _, dirs := range []bool{true, false} {
		names, e := f.List(p.Path, dirs, false)
		if e != nil {
			return nil, e
		}
		for _, name := range Strings(names) {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
			e, err := f.GetFileInformation(path.Join(p.Path, name))
			if err != nil {
				return nil, err
			}
			if e != nil && !e.IsDeleted() {
				out = append(out, entry(e))
			}
		}
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Directory != out[j].Directory {
			return out[i].Directory
		}
		return strings.ToLower(out[i].Name) < strings.ToLower(out[j].Name)
	})

	return out, nil
}

// Drafts expire rather than retaining secrets indefinitely after an abandoned UI.
const draftLifetime = 30 * time.Minute

func (c *Control) browsePage(ctx context.Context, p Request) (any, error) {
	f, err := c.folder(p.Folder)
	if err != nil {
		return nil, err
	}
	type name struct {
		value     string
		directory bool
	}
	names := []name{}
	for _, dirs := range []bool{true, false} {
		list, err := f.List(p.Path, dirs, false)
		if err != nil {
			return nil, err
		}
		for _, v := range Strings(list) {
			names = append(names, name{v, dirs})
		}
	}
	sort.Slice(names, func(i, j int) bool {
		if names[i].directory != names[j].directory {
			return names[i].directory
		}
		return strings.ToLower(names[i].value) < strings.ToLower(names[j].value)
	})
	start := int(p.Offset)
	if start < 0 || start > len(names) {
		return nil, errors.New("this directory changed; refresh before continuing")
	}
	end := start + 256
	if end > len(names) {
		end = len(names)
	}
	next := 0
	if end < len(names) {
		next = end
	}
	out := []Entry{}
	for _, n := range names[start:end] {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		e, err := f.GetFileInformation(path.Join(p.Path, n.value))
		if err != nil {
			return nil, err
		}
		if e != nil && !e.IsDeleted() {
			out = append(out, entry(e))
		}
	}
	return map[string]any{"Entries": out, "Next": next}, nil
}

func (c *Control) folder(id string) (*core.Folder, error) {
	f := c.Client.FolderWithID(id)
	if f == nil || !f.Exists() {
		return nil, errors.New("the folder no longer exists")
	}
	return f, nil
}

func (c *Control) peers() []string {
	out := []string{}
	for _, id := range Strings(c.Client.Peers()) {
		if id != c.Client.DeviceID() {
			out = append(out, id)
		}
	}
	return out
}
