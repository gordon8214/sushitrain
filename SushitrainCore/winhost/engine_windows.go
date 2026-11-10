// Copyright (C) 2026 Gordon
// SPDX-License-Identifier: MPL-2.0

package winhost

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/google/uuid"
	"os"
	"path"
	"path/filepath"
	"strings"
	"t-shaped.nl/sushitrain/v2/desktopapi"
	"t-shaped.nl/sushitrain/v2/enginecontrol"
	"t-shaped.nl/sushitrain/v2/remoteclient"
	core "t-shaped.nl/sushitrain/v2/src"
)

func stringsFrom(list *core.ListOfStrings) []string {
	out := []string{}
	if list != nil {
		for i := 0; i < list.Count(); i++ {
			out = append(out, list.ItemAt(i))
		}
	}
	return out
}

type folderSummary struct {
	ID, Label, Path, State, Error string
	Paused, Trash, CanRemoveFiles bool
	Failed                        int
	Sync                          *desktopapi.FolderSync `json:",omitempty"`
}
type deviceSummary struct {
	ID, Name          string
	Connected, Paused bool
}
type browserEntry struct {
	Path, Name, MIME, URL string
	Size                  int64
	Directory, Local      bool
}
type editorField = desktopapi.Field

func (h *Host) engineCall(ctx context.Context, method string, p request) (any, error) {
	c := h.client
	switch method {
	case "engine.pending", "engine.stats", "engine.logs", "engine.search", "engine.share", "engine.versions", "engine.restoreVersion", "engine.exportVersion":
		return h.engineExtras(ctx, method, p)
	case "engine.folders":
		out := []folderSummary{}
		for _, id := range stringsFrom(c.Folders()) {
			f := c.FolderWithID(id)
			if f == nil {
				// Removed since the list was read
				continue
			}
			state := f.State()
			out = append(out, folderSummary{ID: id, Label: f.Label(), Path: f.Path(), State: state, Error: f.StateError(), Paused: f.IsPaused(), Trash: f.IsTrashEnabled(), CanRemoveFiles: f.CanRemoveFiles(), Failed: f.FailedItemCount(), Sync: enginecontrol.FolderSyncProgress(f, state)})
		}
		return out, nil
	case "engine.devices":
		out := []deviceSummary{}
		for _, id := range stringsFrom(c.Peers()) {
			d := c.PeerWithID(id)
			out = append(out, deviceSummary{id, d.Name(), d.IsConnected(), d.IsPaused()})
		}
		return out, nil
	case "engine.addFolder":
		return nil, c.AddFolder(p.ID, p.Path, p.Enabled)
	case "engine.addDevice":
		return nil, c.AddPeer(p.ID)
	case "engine.browse":
		f := c.FolderWithID(p.Folder)
		if !f.Exists() {
			return nil, errors.New("folder no longer exists")
		}
		dirs, err := f.List(p.Path, true, false)
		if err != nil {
			return nil, err
		}
		names, err := f.List(p.Path, false, false)
		if err != nil {
			return nil, err
		}
		out := []browserEntry{}
		seen := map[string]bool{}
		for _, name := range append(stringsFrom(dirs), stringsFrom(names)...) {
			if seen[name] {
				continue
			}
			seen[name] = true
			e, err := f.GetFileInformation(path.Join(p.Path, name))
			if err != nil || e == nil {
				continue
			}
			out = append(out, browserEntry{e.Path(), e.Name(), e.MIMEType(), e.OnDemandURL(), e.Size(), e.IsDirectory(), e.IsLocallyPresent()})
		}
		return out, nil
	case "engine.action":
		if p.Folder != "" {
			f := c.FolderWithID(p.Folder)
			if !f.Exists() {
				return nil, errors.New("folder no longer exists")
			}
			switch p.Action {
			case "pause":
				return nil, f.SetPaused(true)
			case "resume":
				return nil, f.SetPaused(false)
			case "rescan":
				return nil, f.Rescan()
			case "remove":
				return nil, f.Unlink()
			case "relocate":
				return f.RelocateInto(p.Path)
			case "trash":
				return nil, f.SetTrashEnabled(p.Enabled)
			case "removeFiles":
				return nil, f.Remove()
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
				lines, err := f.IgnoreLines()
				return stringsFrom(lines), err
			case "saveIgnores":
				return nil, f.SetIgnoreLines(core.List(splitLines(p.Path)))
			case "deleted":
				list, err := f.DeletedFiles()
				if err != nil {
					return nil, err
				}
				out := []*core.VersionedFile{}
				for i := 0; i < list.Count(); i++ {
					out = append(out, list.ItemAt(i))
				}
				return out, nil
			case "restoreDeleted":
				return f.RestoreLatestVersions(core.List(splitLines(p.Path)))
			}
		}
		if p.Device != "" {
			d := c.PeerWithID(p.Device)
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
			return nil, c.SetDevicesPaused(c.Peers(), true)
		case "resumeAll":
			return nil, c.SetDevicesPaused(c.Peers(), false)
		case "restart":
			return nil, c.RestartEngine()
		case "rescanAll":
			for _, id := range stringsFrom(c.Folders()) {
				if err := c.FolderWithID(id).Rescan(); err != nil {
					return nil, err
				}
			}
			return nil, nil
		case "dismiss":
			return nil, c.DismissPendingDevice(p.ID)
		case "ignore":
			return nil, c.IgnoreDevice(p.ID)
		case "unignore":
			return nil, c.UnignoreDevice(p.ID)
		}
	case "settings.close":
		delete(h.editors, p.Token)
		return nil, nil
	case "settings.index":
		data, err := c.SettingsSearchIndex()
		return json.RawMessage(data), err
	case "settings.get":
		sectionID := strings.TrimPrefix(p.Section, "reset/")
		if strings.HasPrefix(sectionID, "basic/folder/") {
			sectionID = strings.TrimPrefix(sectionID, "basic/")
		} else if strings.HasPrefix(sectionID, "versioning/") {
			sectionID = "folder/" + strings.TrimPrefix(sectionID, "versioning/")
		} else if sectionID == "defaults/versioning" {
			sectionID = "defaults/folder"
		}
		var section *core.AdvancedSection
		var err error
		if strings.HasPrefix(p.Section, "reset/") {
			section, err = c.ResetFolderSyncSection(sectionID)
		} else {
			section, err = c.AdvancedSection(sectionID)
		}
		if err != nil {
			return nil, err
		}
		if len(h.editors) >= 64 {
			for token := range h.editors {
				delete(h.editors, token)
				break
			}
		}
		token := uuid.NewString()
		h.editors[token] = section
		fields := []editorField{}
		for i := 0; i < section.FieldCount(); i++ {
			f := section.FieldAt(i)
			if !visibleFolderEditorField(p.Section, f.Path) {
				continue
			}
			label := f.Label
			if (strings.HasPrefix(sectionID, "folder/") || sectionID == "defaults/folder") && f.Path == "minDiskFree.value" {
				label = "Minimum free disk space"
			} else if (strings.HasPrefix(sectionID, "folder/") || sectionID == "defaults/folder") && f.Path == "minDiskFree.unit" {
				label = "Free space unit"
			}
			fields = append(fields, editorField{Path: f.Path, Label: label, Group: f.Group, Kind: f.Kind, Value: f.Value,
				IsSecret: f.IsSecret, IsReadOnly: f.IsReadOnly, Options: stringsFrom(f.Options()), IsAdvanced: desktopapi.FolderFieldAdvanced(p.Section, f.Path)})
		}
		return map[string]any{"token": token, "fields": fields}, nil
	case "settings.save":
		section, ok := h.editors[p.Token]
		if !ok {
			return nil, errors.New("open the settings page again before saving")
		}
		for i := 0; i < section.FieldCount(); i++ {
			f := section.FieldAt(i)
			if value, ok := p.Values[f.Path]; ok && !f.IsReadOnly {
				f.Value = value
			}
		}
		if err := c.SaveAdvancedSection(section); err != nil {
			return nil, err
		}
		delete(h.editors, p.Token)
		return map[string]bool{"restartRequired": c.IsEngineRestartRequired()}, nil
	case "gateway.enable":
		wasRunning := h.gateway != nil
		if p.Enabled && h.gateway == nil {
			if err := h.startGateway(); err != nil {
				return nil, err
			}
		} else if !p.Enabled && h.gateway != nil {
			if err := h.gateway.Stop(); err != nil {
				return nil, err
			}
			h.gateway = nil
		}
		h.optionsMu.Lock()
		defer h.optionsMu.Unlock()
		old := h.settings.Gateway
		h.settings.Gateway = p.Enabled
		if err := h.saveLocked(); err != nil {
			h.settings.Gateway = old
			if !wasRunning && h.gateway != nil {
				stopErr := h.gateway.Stop()
				if stopErr == nil {
					h.gateway = nil
				}
				return nil, errors.Join(err, stopErr)
			}
			if wasRunning && h.gateway == nil {
				return nil, errors.Join(err, h.startGateway())
			}
			return nil, err
		}
		return nil, nil
	case "gateway.address":
		// The address as text (the app), or a host and a port given apart, each kept when it is missing (MCP)
		h.optionsMu.Lock()
		oldHost, oldPort := h.settings.GatewayHost, h.settings.GatewayPort
		var host string
		var port int
		var err error
		if p.PublicHost != nil || p.PublicPort != nil {
			host, port, err = mergeGatewayAddress(oldHost, oldPort, p.PublicHost, p.PublicPort)
		} else {
			host, port, err = gatewayAddressSetting(p.Address)
		}
		if err == nil {
			h.settings.GatewayHost, h.settings.GatewayPort = host, port
			if err = h.saveLocked(); err != nil {
				h.settings.GatewayHost, h.settings.GatewayPort = oldHost, oldPort
			}
		}
		h.optionsMu.Unlock()
		if err != nil {
			return nil, err
		}
		h.revision.Add(1)
		publicHost, publicPort := h.gatewayAddress()
		// Only pairing uses it: a running service takes it at once, and the devices connected to it stay
		if h.gateway != nil {
			h.gateway.SetPublicAddress(publicHost, publicPort)
		}
		return map[string]string{"address": remoteclient.FormatAddress(publicHost, publicPort)}, nil
	}
	if h.gateway != nil {
		r := h.gateway
		switch method {
		case "gateway.status":
			return raw(r.StatusJSON())
		case "gateway.exports":
			return raw(r.ExportsJSON())
		case "gateway.export":
			return nil, r.SetExport(p.Folder, p.Enabled)
		case "gateway.unexport":
			return nil, r.RemoveExport(p.Folder)
		case "gateway.clients":
			return raw(r.ClientsJSON())
		case "gateway.revoke":
			return nil, r.RevokeClient(p.ID)
		case "gateway.permission":
			return nil, r.SetPermission(p.ID, p.Folder, p.Action)
		case "gateway.pair":
			return raw(r.StartPairingJSON())
		case "gateway.pending":
			return raw(r.PendingPairingsJSON())
		case "gateway.approve":
			return nil, r.ApprovePairing(p.ID, p.Grants)
		case "gateway.deny":
			return nil, r.DenyPairing(p.ID)
		}
	}
	return nil, fmt.Errorf("%s is not available", method)
}
func raw(data []byte, err error) (any, error) { return json.RawMessage(data), err }

// gatewayAddressSetting reads the address for clients the person entered: host or host:port, giving the host to keep
// (empty for this PC's name) and the port (0 for the one the service listens on). Empty text is this PC's name and port.
func gatewayAddressSetting(text string) (string, int, error) {
	if strings.TrimSpace(text) == "" {
		return "", 0, nil
	}
	a, err := remoteclient.ParseAddress(text)
	if err != nil {
		return "", 0, err
	}
	if name, _ := os.Hostname(); strings.EqualFold(a.Host, name) {
		a.Host = ""
	}
	if a.Port == gatewayListenPort {
		a.Port = 0
	}
	return a.Host, a.Port, nil
}

// mergeGatewayAddress applies a host and a port given apart (nil: keep it) to the address for clients: a host without a
// port (empty, or this PC's name, for this PC's name), and a port (0, or the listening port, for the listening port)
func mergeGatewayAddress(host string, port int, newHost *string, newPort *int) (string, int, error) {
	if newHost != nil {
		host = ""
		if text := strings.TrimSpace(*newHost); text != "" {
			a, err := remoteclient.ParseAddress(text)
			if err != nil {
				return "", 0, err
			}
			if a.Port != 0 {
				return "", 0, errors.New("give the public host without a port, and the port as publicPort")
			}
			if name, _ := os.Hostname(); !strings.EqualFold(a.Host, name) {
				host = a.Host
			}
		}
	}
	if newPort != nil {
		if *newPort < 0 || *newPort > 65535 {
			return "", 0, errors.New("choose a public port between 1 and 65535, or 0 for the listening port")
		}
		port = *newPort
		if port == gatewayListenPort {
			port = 0
		}
	}
	return host, port, nil
}
func splitLines(text string) []string {
	var out []string
	for _, line := range strings.Split(strings.ReplaceAll(text, "\r\n", "\n"), "\n") {
		out = append(out, line)
	}
	return out
}

// Export a configuration home to an explicitly chosen destination without moving the user's synchronized folders.
func (h *Host) importHome(source string) error {
	h.transition.Lock()
	defer h.transition.Unlock()
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.client != nil || h.settings.Mode != "" {
		return errors.New("stop the engine before importing a configuration")
	}
	_, err := core.ImportSyncthingHome(source, filepath.Join(h.State, "Engine"))
	if err != nil {
		return err
	}
	old := h.settings.ImportedHome
	h.settings.ImportedHome = source
	if err := h.save(); err != nil {
		h.settings.ImportedHome = old
		return err
	}
	return nil
}

// General and versioning reuse the raw folder draft without dropping any fields
// from the full synchronization editor.
func visibleFolderEditorField(section, field string) bool {
	if strings.HasPrefix(section, "reset/") {
		return core.IsFolderSyncResetField(field)
	}
	if strings.HasPrefix(section, "basic/folder/") {
		switch field {
		case "id", "label", "path", "type", "rescanIntervalS", "fsWatcherEnabled", "fsWatcherDelayS", "maxConflicts", "blockIndexing":
			return true
		default:
			return false
		}
	}
	if strings.HasPrefix(section, "versioning/") || section == "defaults/versioning" {
		return strings.HasPrefix(field, "versioning.")
	}
	return true
}
