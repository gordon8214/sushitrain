// Copyright (C) 2026 Gordon
// SPDX-License-Identifier: MPL-2.0

// Package winhost is the per-user Windows application's owner of its engine and remote-files state.
package winhost

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"t-shaped.nl/sushitrain/v2/adminsock"
	"t-shaped.nl/sushitrain/v2/files"
	core "t-shaped.nl/sushitrain/v2/src"
	"t-shaped.nl/sushitrain/v2/winplatform"
)

type Settings struct {
	Mode         string
	Gateway      bool
	GatewayHost  string
	CacheBudget  int64
	FilesRoot    string
	ImportedHome string
}
type Host struct {
	mu                    sync.RWMutex
	gate                  sync.Mutex
	transition            sync.Mutex
	gatewayMu             sync.Mutex
	editorMu              sync.Mutex
	optionsMu             sync.Mutex
	modeCtx               context.Context
	cancelMode            context.CancelFunc
	transitioning, closed bool
	modeSnapshot          string
	events                chan struct{}
	stopEvents            context.CancelFunc
	gatewayStale          atomic.Bool
	gatewayError          string
	State, Root, Home     string
	settings              Settings
	client                *core.Client
	files                 *files.Service
	gateway               *core.RemoteFileService
	revision              atomic.Uint64
	editors               map[string]*core.AdvancedSection
	OnModeChange          func(string, string, string) error
	BeforeModeChange      func(context.Context) error
	AfterModeChange       func()
	OnFatalError          func(error)
}

func Open(state, root, home string) (*Host, error) {
	if err := os.MkdirAll(state, 0700); err != nil {
		return nil, err
	}
	if err := winplatform.PrivateDirectory(state); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Join(state, "Captured"), 0700); err != nil {
		return nil, err
	}
	h := &Host{State: state, Root: root, Home: home, editors: map[string]*core.AdvancedSection{}, events: make(chan struct{}, 1)}
	h.modeCtx, h.cancelMode = context.WithCancel(context.Background())
	if data, err := os.ReadFile(filepath.Join(state, "settings.json")); err == nil {
		if err = json.Unmarshal(data, &h.settings); err != nil {
			return nil, fmt.Errorf("reading settings: %w", err)
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	if h.settings.FilesRoot != "" {
		h.Root = h.settings.FilesRoot
	}
	if err := h.recoverMode(); err != nil {
		return nil, err
	}
	if h.settings.Mode != "" {
		if err := h.start(h.settings.Mode); err != nil {
			h.stop()
			h.cancelMode()
			return nil, err
		}
	}
	h.modeSnapshot = h.settings.Mode
	eventCtx, stop := context.WithCancel(context.Background())
	h.stopEvents = stop
	go h.watchEngine(eventCtx)
	return h, nil
}
func (h *Host) Close() {
	h.transition.Lock()
	defer h.transition.Unlock()
	h.gate.Lock()
	h.closed = true
	h.cancelMode()
	if h.stopEvents != nil {
		h.stopEvents()
	}
	h.gate.Unlock()
	h.mu.Lock()
	defer h.mu.Unlock()
	h.stop()
}
func (h *Host) stop() error {
	if h.gateway != nil {
		h.gateway.Stop()
		h.gateway = nil
	}
	if h.client != nil {
		if err := h.client.Close(); err != nil {
			return err
		}
		h.client = nil
	}
	if h.files != nil {
		h.files.Close()
		h.files = nil
	}
	h.editors = map[string]*core.AdvancedSection{}
	return nil
}
func (h *Host) start(mode string) error {
	switch mode {
	case "synchronize":
		state := filepath.Join(h.State, "Engine")
		if err := os.MkdirAll(state, 0700); err != nil {
			return err
		}
		h.client = core.NewClient(state, h.Home, filepath.Join(h.State, "Logs"), true, false)
		h.client.Delegate = h
		if h.settings.ImportedHome != "" {
			if err := h.client.HoldSyncthingHome(h.settings.ImportedHome); err != nil {
				_ = h.client.Close()
				h.client = nil
				return err
			}
		}
		if err := h.client.Load(false); err != nil {
			_ = h.client.Close()
			h.client = nil
			return err
		}
		if err := h.client.Start(); err != nil {
			_ = h.client.Close()
			h.client = nil
			return err
		}
		if h.settings.Gateway {
			return h.startGateway()
		}
	case "files":
		service, err := files.Open(files.Config{StateDir: filepath.Join(h.State, "Files"), CacheDir: filepath.Join(h.State, "Cache"), Credentials: files.WindowsCredentials{}, CacheBudget: h.settings.CacheBudget, Platform: "Windows", Notify: func(files.Notice) { h.revision.Add(1) }})
		if err != nil {
			return err
		}
		h.files = service
		service.SetInvalidator(func(files.Changes) { h.revision.Add(1) })
		if err := service.Start(); err != nil {
			service.Close()
			h.files = nil
			return err
		}
	default:
		return errors.New("choose Synchronize or Files")
	}
	h.revision.Add(1)
	return nil
}
func (h *Host) startGateway() error {
	settings, _ := json.Marshal(map[string]any{"listenAddress": ":20920", "publicHost": h.settings.GatewayHost, "serviceName": "Synctrain on Windows"})
	r, err := h.client.StartRemoteFileService(settings)
	if err == nil {
		h.gateway = r
	}
	return err
}
func (h *Host) save() error { h.optionsMu.Lock(); defer h.optionsMu.Unlock(); return h.saveLocked() }
func (h *Host) saveLocked() error {
	data, err := json.MarshalIndent(h.settings, "", "  ")
	if err != nil {
		return err
	}
	f, err := os.CreateTemp(h.State, ".settings-*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if _, err = f.Write(data); err == nil {
		err = f.Sync()
	}
	closeErr := f.Close()
	if err != nil {
		return err
	}
	if closeErr != nil {
		return closeErr
	}
	if err := os.Rename(f.Name(), filepath.Join(h.State, "settings.json")); err != nil {
		return err
	}
	return winplatform.FlushDirectory(h.State)
}

type modeJournal struct{ From, To, Backup, Phase string }

func (h *Host) setMode(ctx context.Context, mode string) error {
	if mode != "files" && mode != "synchronize" {
		return errors.New("choose Synchronize or Files")
	}
	if mode == h.settings.Mode {
		return nil
	}
	if mode == "files" && h.client != nil && h.client.OverlappingFolderID(h.Root) != "" {
		return errors.New("the Files location overlaps a synchronized folder; choose a different location")
	}
	old := h.settings.Mode
	journal := modeJournal{From: old, To: mode}
	if h.files != nil {
		drain, cancel := context.WithTimeout(ctx, 2*time.Minute)
		defer cancel()
		if err := h.waitForFiles(drain); err != nil {
			return err
		}
		if h.OnModeChange != nil {
			if err := h.notifyMode(""); err != nil {
				return err
			}
		}
		closeErr := h.files.Close()
		h.files = nil
		if closeErr != nil {
			return h.rollbackMode(journal, closeErr)
		}
		backup, err := os.MkdirTemp(h.State, ".files-before-mode-")
		if err != nil {
			return h.rollbackMode(journal, err)
		}
		if err := copyState(filepath.Join(h.State, "Files"), backup); err != nil {
			_ = os.RemoveAll(backup)
			return h.rollbackMode(journal, err)
		}
		journal.Backup = backup
		data, _ := json.Marshal(journal)
		if err := h.writeAtomic("mode-transition.json", data); err != nil {
			return h.rollbackMode(journal, err)
		}
		if err := h.start("files"); err != nil {
			return h.rollbackMode(journal, err)
		}
		if _, err := h.files.Leave(drain); err != nil {
			return h.rollbackMode(journal, err)
		}
	} else if h.OnModeChange != nil {
		if err := h.notifyMode(""); err != nil {
			return err
		}
	}
	if journal.Backup == "" {
		data, _ := json.Marshal(journal)
		if err := h.writeAtomic("mode-transition.json", data); err != nil {
			return h.rollbackMode(journal, err)
		}
	}
	if err := h.stop(); err != nil {
		return h.rollbackMode(journal, err)
	}
	if err := h.start(mode); err != nil {
		return h.rollbackMode(journal, err)
	}
	if h.OnModeChange != nil {
		if err := h.notifyMode(mode); err != nil {
			return h.rollbackMode(journal, err)
		}
	}
	h.settings.Mode = mode
	if err := h.save(); err != nil {
		return h.rollbackMode(journal, err)
	}
	return h.finishModeJournal(journal.Backup)
}
func (h *Host) rollbackMode(j modeJournal, cause error) error {
	var failures []error
	if h.OnModeChange != nil {
		if err := h.notifyMode(""); err != nil {
			failures = append(failures, err)
		}
	}
	if err := h.stop(); err != nil {
		return errors.Join(cause, err)
	}
	j.Phase = "rollback"
	data, err := json.Marshal(j)
	if err != nil {
		return errors.Join(cause, err)
	}
	if err := h.writeAtomic("mode-transition.json", data); err != nil {
		return errors.Join(cause, err)
	}
	h.settings.Mode = j.From
	if j.Backup != "" {
		if err := h.restoreModeBackup(j.Backup); err != nil {
			failures = append(failures, err)
		}
	}
	if len(failures) == 0 && j.From != "" {
		if err := h.start(j.From); err != nil {
			failures = append(failures, err)
		} else if h.OnModeChange != nil {
			if err := h.notifyMode(j.From); err != nil {
				failures = append(failures, err)
			}
		}
	}
	if len(failures) == 0 {
		if err := h.save(); err != nil {
			failures = append(failures, err)
		}
	}
	if len(failures) == 0 {
		return errors.Join(cause, h.finishModeJournal(j.Backup))
	}
	return errors.Join(append([]error{cause, errors.New("the previous mode could not be fully restored; the transition journal and backups were retained")}, failures...)...)
}
func copyState(source, target string) error {
	return filepath.WalkDir(source, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		relative, err := filepath.Rel(source, path)
		if err != nil {
			return err
		}
		dest := filepath.Join(target, relative)
		if entry.IsDir() {
			return os.MkdirAll(dest, 0700)
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		if !info.Mode().IsRegular() {
			return errors.New("the state contains an unexpected link or special file")
		}
		in, err := os.Open(path)
		if err != nil {
			return err
		}
		defer in.Close()
		out, err := os.OpenFile(dest, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
		if err != nil {
			return err
		}
		_, err = io.Copy(out, in)
		if err == nil {
			err = out.Sync()
		}
		closeErr := out.Close()
		if err != nil {
			return err
		}
		return closeErr
	})
}
func (h *Host) recoverMode() error {
	data, err := os.ReadFile(filepath.Join(h.State, "mode-transition.json"))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	var journal modeJournal
	if err := json.Unmarshal(data, &journal); err != nil {
		return err
	}
	if journal.Backup != "" && (filepath.Clean(filepath.Dir(journal.Backup)) != filepath.Clean(h.State) || !strings.HasPrefix(filepath.Base(journal.Backup), ".files-before-mode-")) {
		return errors.New("the mode recovery backup is outside the state directory")
	}
	if h.settings.Mode == journal.To && journal.Phase != "rollback" {
		return h.finishModeJournal(journal.Backup)
	}
	if journal.Backup != "" {
		if err := h.restoreModeBackup(journal.Backup); err != nil {
			return err
		}
	}
	h.settings.Mode = journal.From
	if err := h.save(); err != nil {
		return err
	}
	return h.finishModeJournal(journal.Backup)
}

type request struct {
	Mode, ID, Path, Name, Folder, Device, Action, Section, Token, Link, ServiceID, ContentID, Captured, ClientID, Password string
	Item, Parent, NewParent, Offset, Length, Budget, LocalFolder                                                           int64
	Node                                                                                                                   uint64
	Enabled, Directory, Recovery                                                                                           bool
	Values                                                                                                                 map[string]string
	Grants, Snapshot                                                                                                       json.RawMessage
}

func (h *Host) Register(server *adminsock.Server) {
	methods := []string{"status", "mode.set", "root.set", "recovery.captures", "recovery.export", "home.import", "engine.folders", "engine.devices", "engine.addFolder", "engine.addDevice", "engine.action", "engine.browse", "engine.pending", "engine.stats", "engine.logs", "engine.search", "engine.share", "engine.versions", "engine.restoreVersion", "engine.exportVersion", "settings.get", "settings.save", "settings.close", "gateway.enable", "gateway.status", "gateway.exports", "gateway.export", "gateway.unexport", "gateway.clients", "gateway.revoke", "gateway.permission", "gateway.pair", "gateway.pending", "gateway.approve", "gateway.deny", "files.gateways", "files.pair", "files.forget", "files.available", "files.add", "files.remove", "files.folders", "files.children", "files.path", "files.entry", "files.read", "files.thumbnail", "files.keep", "files.create", "files.createAck", "files.rename", "files.delete", "files.stage", "files.stageAck", "files.resolve", "files.state", "projection.load", "projection.save", "projection.root.get", "projection.root.save", "projection.report", "files.download", "files.pending", "files.export", "files.discard", "files.versions", "files.restore", "files.copyVersion", "files.versionPreview", "files.storage", "files.budget"}
	for _, method := range methods {
		server.Register(method, func(ctx context.Context, data json.RawMessage) (any, error) {
			var p request
			if len(data) > 0 {
				if err := json.Unmarshal(data, &p); err != nil {
					return nil, err
				}
			}
			if method == "home.import" {
				return nil, h.importHome(p.Path)
			}
			if method == "root.set" {
				return nil, h.configureRoot(p.Path)
			}
			if method == "mode.set" {
				return nil, h.switchMode(ctx, p.Mode)
			}
			if method == "projection.root.get" || method == "projection.root.save" || method == "projection.load" {
				// Native registration must finish while the mode transition owns
				// the backend lease. These fixed state files need no backend.
				return h.projectionState(method, p)
			}
			operation, finish, err := h.begin(ctx)
			if err != nil {
				if method == "status" {
					h.gate.Lock()
					mode := h.modeSnapshot
					h.gate.Unlock()
					return map[string]any{"mode": mode, "state": "switching", "root": h.Root, "protocol": 1}, nil
				}
				return nil, err
			}
			defer finish()
			if strings.HasPrefix(method, "gateway.") || method == "status" {
				h.gatewayMu.Lock()
				defer h.gatewayMu.Unlock()
			}
			if strings.HasPrefix(method, "settings.") {
				h.editorMu.Lock()
				defer h.editorMu.Unlock()
			}
			return h.call(operation, method, p)
		})
	}
}
func (h *Host) call(ctx context.Context, method string, p request) (any, error) {
	if method == "status" {
		state := ""
		problem := ""
		device := ""
		gui := ""
		if h.client != nil {
			state = h.client.EngineState()
			problem = h.client.EngineError()
			device = h.client.DeviceID()
			gui = h.client.GUIURL()
		}
		clientID := ""
		if h.files != nil {
			clientID, _ = h.files.ClientIdentity()
		}
		return map[string]any{"clientID": clientID, "protocol": 1, "mode": h.settings.Mode, "state": state, "error": problem, "deviceID": device, "guiURL": gui, "root": h.Root, "captureDir": filepath.Join(h.State, "Captured"), "revision": h.revision.Load(), "gatewayEnabled": h.settings.Gateway, "gatewayRunning": h.gateway != nil, "gatewayError": h.gatewayError, "projectionIssues": h.projectionIssues()}, nil
	}
	if method == "recovery.captures" {
		return h.recoveredCaptures()
	}
	if method == "recovery.export" {
		return nil, h.exportCapture(p.Captured, p.Path)
	}
	if method == "mode.set" {
		return nil, h.setMode(ctx, p.Mode)
	}
	if strings.HasPrefix(method, "engine.") || strings.HasPrefix(method, "settings.") || strings.HasPrefix(method, "gateway.") {
		if h.client == nil {
			return nil, errors.New("this machine is not in Synchronize mode")
		}
		return h.engineCall(ctx, method, p)
	}
	if h.files == nil {
		return nil, errors.New("this machine is not in Files mode")
	}
	s := h.files
	if method == "files.read" || method == "files.thumbnail" || method == "files.stage" || (p.ClientID != "" && strings.HasPrefix(method, "files.")) {
		id, err := s.ClientIdentity()
		if err != nil {
			return nil, err
		}
		if p.ClientID != id {
			return nil, errors.New("this placeholder belongs to another local index; preserve local edits and rebuild the location")
		}
	}
	switch method {
	case "files.gateways":
		return s.Gateways()
	case "files.pair":
		return s.Pair(ctx, p.Link, nil)
	case "files.forget":
		folders, err := s.Folders()
		if err != nil {
			return nil, err
		}
		for _, folder := range folders {
			if folder.ServiceID == p.ServiceID {
				if err := h.waitForProjectionFolder(ctx, folder.ID); err != nil {
					return nil, err
				}
			}
		}
		return nil, s.Forget(p.ServiceID)
	case "files.available":
		return s.RemoteFolders(ctx, p.ServiceID)
	case "files.add":
		return s.AddFolder(ctx, p.ServiceID, p.Folder)
	case "files.remove":
		if err := h.waitForProjectionFolder(ctx, p.Item); err != nil {
			return nil, err
		}
		dest, n, err := s.RemoveFolder(p.Item)
		return map[string]any{"leftovers": dest, "moved": n}, err
	case "files.folders":
		return s.Folders()
	case "files.path":
		return s.ItemPath(p.Item)
	case "files.children":
		entries, err := s.Children(p.Parent)
		if err != nil {
			return nil, err
		}
		id, err := s.ClientIdentity()
		if err != nil {
			return nil, err
		}
		type projectedEntry struct {
			files.Entry
			ClientID      string
			ModifiedTicks int64
		}
		out := make([]projectedEntry, 0, len(entries))
		for _, entry := range entries {
			out = append(out, projectedEntry{Entry: entry, ClientID: id, ModifiedTicks: entry.ModTime.UnixNano()/100 + 116444736000000000})
		}
		return out, nil
	case "files.entry":
		e, ok, err := s.Item(p.Item)
		if err == nil && !ok {
			err = os.ErrNotExist
		}
		return e, err
	case "files.read":
		if p.Offset < 0 || p.Length < 0 || p.Length > 1<<20 {
			return nil, errors.New("invalid content range")
		}
		e, ok, err := s.Item(p.Item)
		if err != nil {
			return nil, err
		}
		if !ok {
			return nil, os.ErrNotExist
		}
		if e.Node != p.Node {
			return nil, files.ErrVersionChanged
		}
		r, err := s.OpenNode(e)
		if err != nil {
			return nil, err
		}
		defer r.Close()
		data := make([]byte, p.Length)
		readCtx, cancel := context.WithTimeout(ctx, 45*time.Second)
		defer cancel()
		n, err := r.ReadAt(readCtx, data, p.Offset)
		if err != nil && !errors.Is(err, io.EOF) {
			return nil, err
		}
		return &adminsock.Stream{Reader: io.NopCloser(bytes.NewReader(data[:n])), Length: int64(n)}, nil
	case "files.thumbnail":
		current, ok, err := s.Item(p.Item)
		if err != nil {
			return nil, err
		}
		if !ok || current.Node != p.Node {
			return nil, files.ErrVersionChanged
		}
		thumbCtx, cancel := context.WithTimeout(ctx, 35*time.Second)
		defer cancel()
		data, err := s.Thumbnail(thumbCtx, p.Item, int(p.Length))
		if err != nil {
			return nil, err
		}
		return &adminsock.Stream{Reader: io.NopCloser(bytes.NewReader(data)), Length: int64(len(data))}, nil
	case "files.keep":
		err := s.SetKeep(p.Item, p.Enabled)
		if err == nil {
			h.revision.Add(1)
		}
		return nil, err
	case "files.create":
		return s.CreateOnce(p.Parent, p.Name, p.Directory, p.ID)
	case "files.createAck":
		return nil, s.AcknowledgeCreate(p.ID)
	case "files.rename":
		return s.Rename(p.Parent, p.Name, p.NewParent, p.Path, true, expectedEntry(p)...)
	case "files.delete":
		_, err := s.Remove(p.Parent, p.Name, p.Directory, expectedEntry(p)...)
		return nil, err
	case "files.stage":
		return h.stage(ctx, p)
	case "files.stageAck":
		return nil, s.AcknowledgeSnapshot(captureKey(p))
	case "files.resolve":
		return s.Resolve(p.Path)
	case "files.state":
		entry, ok, err := s.Item(p.Item)
		if err != nil {
			return nil, err
		}
		if !ok {
			return "removed", nil
		}
		return s.State(entry)
	case "files.download":
		return nil, h.downloadFile(ctx, p.Item, p.Path)
	case "projection.report":
		if len(p.Snapshot) > 128<<10 || !json.Valid(p.Snapshot) {
			return nil, errors.New("invalid projection issues")
		}
		previous, _ := os.ReadFile(filepath.Join(h.State, "projection-issues.json"))
		if bytes.Equal(previous, p.Snapshot) {
			return nil, nil
		}
		if err := h.writeAtomic("projection-issues.json", p.Snapshot); err != nil {
			return nil, err
		}
		h.revision.Add(1)
		return nil, nil
	case "projection.root.get", "projection.root.save", "projection.load":
		return h.projectionState(method, p)
	case "projection.save":
		if len(p.Snapshot) > 128<<20 || !json.Valid(p.Snapshot) {
			return nil, errors.New("invalid native projection checkpoint")
		}
		return nil, h.writeAtomic("projection.json", p.Snapshot)

	case "files.pending":
		return s.Edits()
	case "files.export":
		return nil, s.ExportEdit(p.Item, p.Path)
	case "files.discard":
		return nil, s.DiscardEdit(p.Item)
	case "files.versions":
		return s.Versions(ctx, p.Item)
	case "files.restore":
		return nil, s.RestoreVersion(ctx, p.Item, p.ID)
	case "files.copyVersion":
		return s.CopyVersion(ctx, p.Item, p.ID)
	case "files.versionPreview":
		return s.VersionToFile(ctx, p.Item, p.ID, filepath.Join(h.State, "Previews"))
	case "files.storage":
		return s.Storage()
	case "files.budget":
		h.optionsMu.Lock()
		defer h.optionsMu.Unlock()
		old := h.settings.CacheBudget
		if err := s.SetCacheBudget(p.Budget); err != nil {
			return nil, err
		}
		h.settings.CacheBudget = p.Budget
		if err := h.saveLocked(); err != nil {
			h.settings.CacheBudget = old
			_ = s.SetCacheBudget(old)
			return nil, err
		}
		return nil, nil
	}
	return nil, errors.New("unknown request")
}

type stageResult struct {
	Entry    files.Entry
	Path     string
	Conflict bool
	Removed  bool
}

// A committed shared queue can precede its native in-sync checkpoint. Keep the
// index alive until the provider has settled those captures, so cleanup can
// prove that the Explorer files are safe to remove.
func (h *Host) waitForProjectionFolder(ctx context.Context, folder int64) error {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	tick := time.NewTicker(50 * time.Millisecond)
	defer tick.Stop()
	for {
		data, err := os.ReadFile(filepath.Join(h.State, "projection.json"))
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		if err != nil {
			return err
		}
		var records []struct {
			Entry                      files.Entry
			Capture, Create            json.RawMessage
			AwaitingSync, NeedsCapture bool
		}
		if err := json.Unmarshal(data, &records); err != nil {
			return err
		}
		pending := false
		for _, record := range records {
			owner := record.Entry.Folder
			if owner == 0 {
				for _, operation := range []json.RawMessage{record.Capture, record.Create} {
					var args struct{ Parent int64 }
					if len(operation) == 0 {
						continue
					}
					if err := json.Unmarshal(operation, &args); err != nil {
						return err
					}
					if parent, ok, err := h.files.Item(args.Parent); err != nil {
						return err
					} else if ok {
						owner = parent.Folder
					}
				}
			}
			if owner == folder && (len(record.Capture) != 0 || len(record.Create) != 0 || record.AwaitingSync || record.NeedsCapture) {
				pending = true
				break
			}
		}
		if !pending {
			return nil
		}
		select {
		case <-ctx.Done():
			return errors.New("Explorer is still synchronizing this folder; wait for its local saves to finish before removing it")
		case <-tick.C:
		}
	}
}

// The provider captures outside the Cloud Files root before calling this method. Opening a placeholder here
// would require a hydration callback into this same host and could deadlock a mode transition.
func captureKey(p request) string { return fmt.Sprintf("%d-%d-%s", p.Item, p.Node, p.Captured) }
func (h *Host) stage(ctx context.Context, p request) (stageResult, error) {
	key := captureKey(p)
	if previous, ok, err := h.files.SnapshotReceipt(key); err != nil {
		return stageResult{}, err
	} else if ok {
		path, pathErr := h.files.ItemPath(previous.Item)
		_, exists, itemErr := h.files.Item(previous.Item)
		if itemErr != nil {
			return stageResult{}, itemErr
		}
		if !exists && filepath.IsLocal(p.Path) {
			path = p.Path
			pathErr = nil
		}
		return stageResult{Entry: previous, Path: path, Conflict: p.Item != 0 && previous.Item != p.Item, Removed: !exists}, pathErr
	}

	if p.Captured == "" || filepath.Base(p.Captured) != p.Captured || strings.ContainsAny(p.Captured, `\/:`) {
		return stageResult{}, errors.New("an immutable captured snapshot is required")
	}
	if p.LocalFolder != 0 {
		target, ok, err := h.files.Item(p.Item)
		if err != nil {
			return stageResult{}, err
		}
		if !ok {
			target, ok, err = h.files.Item(p.Parent)
			if err != nil {
				return stageResult{}, err
			}
		}
		if !ok || target.Folder != p.LocalFolder {
			return stageResult{}, errors.New("the original folder was removed; the capture was preserved and cannot be uploaded to another folder")
		}
	}
	root, err := os.OpenRoot(filepath.Join(h.State, "Captured"))
	if err != nil {
		return stageResult{}, err
	}
	defer root.Close()
	info, err := root.Lstat(p.Captured)
	if err != nil {
		return stageResult{}, err
	}
	if !info.Mode().IsRegular() {
		return stageResult{}, errors.New("capture is not a regular file")
	}
	selected, err := root.Open(p.Captured)
	if err != nil {
		return stageResult{}, err
	}
	source, err := winplatform.SnapshotReader(selected)
	selected.Close()
	if err != nil {
		return stageResult{}, err
	}
	defer source.Close()
	info, err = source.Stat()
	if err != nil {
		return stageResult{}, err
	}
	var entry files.Entry
	conflict := false
	if p.Item == 0 {
		entry, err = h.files.CreateSnapshot(ctx, p.Parent, p.Name, false, source, info.Size(), info.ModTime(), key)
	} else {
		current, ok, itemErr := h.files.Item(p.Item)
		if itemErr != nil {
			return stageResult{}, itemErr
		}
		if !ok {
			entry, err = h.files.CreateSnapshot(ctx, p.Parent, p.Name, true, source, info.Size(), info.ModTime(), key)
			if err != nil {
				return stageResult{}, err
			}
			path, pathErr := h.files.ItemPath(entry.Item)
			if pathErr != nil {
				return stageResult{}, pathErr
			}
			source.Close()
			_ = root.Remove(p.Captured)
			return stageResult{Entry: entry, Path: path, Conflict: true}, nil
		}
		// CFAPI metadata crosses JSON as double-precision Windows ticks. Permit
		// that rounding while preserving intentional modification-time changes.
		delta := info.ModTime().Sub(current.ModTime)
		if delta < 0 {
			delta = -delta
		}
		if p.Recovery && delta <= 2*time.Microsecond {
			verifyCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
			unchanged, verifyErr := h.files.RecordUnchangedSnapshot(verifyCtx, current, source, info.Size(), key)
			cancel()
			if verifyErr != nil && !errors.Is(verifyErr, files.ErrVersionChanged) {
				return stageResult{}, verifyErr
			}
			if unchanged {
				path, pathErr := h.files.ItemPath(current.Item)
				if pathErr != nil {
					return stageResult{}, pathErr
				}
				source.Close()
				_ = root.Remove(p.Captured)
				return stageResult{Entry: current, Path: path}, nil
			}
		}
		if p.Node == 0 {
			return stageResult{}, errors.New("the capture must identify its original content version")
		}
		if current.Node != p.Node {
			err = files.ErrVersionChanged
		} else {
			entry, err = h.files.ImportSnapshot(ctx, current, source, info.Size(), info.ModTime(), key)
		}
		if errors.Is(err, files.ErrVersionChanged) {
			path, pathErr := h.files.ItemPath(p.Item)
			if pathErr != nil {
				return stageResult{}, pathErr
			}
			parentName := filepath.ToSlash(filepath.Dir(filepath.FromSlash(path)))
			parent, parentErr := h.files.Resolve(parentName)
			if parentErr != nil {
				return stageResult{}, parentErr
			}
			if _, err = source.Seek(0, io.SeekStart); err != nil {
				return stageResult{}, err
			}
			entry, err = h.files.CreateSnapshot(ctx, parent.Item, current.Name, true, source, info.Size(), info.ModTime(), key)
			conflict = err == nil
		}
	}
	if err != nil {
		return stageResult{}, err
	}
	path, err := h.files.ItemPath(entry.Item)
	if err != nil {
		return stageResult{}, err
	}
	source.Close()
	_ = root.Remove(p.Captured)
	return stageResult{Entry: entry, Path: path, Conflict: conflict}, nil
}

func (h *Host) OnEvent(string)                                 { h.revision.Add(1) }
func (h *Host) OnFolderScanFinished(string)                    { h.revision.Add(1) }
func (h *Host) OnDeviceDiscovered(string, *core.ListOfStrings) { h.revision.Add(1) }
func (h *Host) OnListenAddressesChanged(*core.ListOfStrings)   { h.revision.Add(1) }
func (h *Host) OnChange(*core.Change)                          { h.revision.Add(1) }
func (h *Host) OnMeasurementsUpdated()                         { h.revision.Add(1) }
func (h *Host) OnEngineStateChanged(state, message string) {
	h.revision.Add(1)
	if state != core.EngineStateRunning {
		h.gatewayStale.Store(true)
	}
	select {
	case h.events <- struct{}{}:
	default:
	}
}
func (h *Host) OnPendingDevicesChanged()                                       { h.revision.Add(1) }
func (h *Host) OnDevicePausesChanged(*core.ListOfStrings, *core.ListOfStrings) { h.revision.Add(1) }

// Each admitted operation retains its backend until completion. Transition cancellation happens before taking
// the write lease, so network requests release their leases promptly. Native captures never read placeholders.
func (h *Host) begin(ctx context.Context) (context.Context, func(), error) {
	h.gate.Lock()
	if h.transitioning || h.closed {
		h.gate.Unlock()
		return nil, nil, errors.New("Synctrain is switching modes or stopping")
	}
	h.mu.RLock()
	work, cancel := context.WithCancel(ctx)
	stop := context.AfterFunc(h.modeCtx, cancel)
	h.gate.Unlock()
	return work, func() { stop(); cancel(); h.mu.RUnlock() }, nil
}
func (h *Host) switchMode(ctx context.Context, mode string) error {
	h.transition.Lock()
	defer h.transition.Unlock()
	if mode != "files" && mode != "synchronize" {
		return errors.New("choose Synchronize or Files")
	}
	if h.settings.Mode == mode {
		return nil
	}
	if h.AfterModeChange != nil {
		defer h.AfterModeChange()
	}
	if h.BeforeModeChange != nil {
		if err := h.BeforeModeChange(ctx); err != nil {
			return err
		}
		// Let the old backend settle while native RPC is still admitted, then
		// refresh native identities to the committed nodes before freezing it.
		_, finish, err := h.begin(ctx)
		if err != nil {
			return err
		}
		drain, cancel := context.WithTimeout(ctx, 2*time.Minute)
		err = h.waitForFiles(drain)
		cancel()
		finish()
		if err != nil {
			return err
		}
		if err := h.BeforeModeChange(ctx); err != nil {
			return err
		}
	}
	h.gate.Lock()
	if h.closed {
		h.gate.Unlock()
		return errors.New("Synctrain is stopping")
	}
	h.transitioning = true
	h.cancelMode()
	h.gate.Unlock()
	h.mu.Lock()
	err := h.setMode(ctx, mode)
	mode = h.settings.Mode
	h.mu.Unlock()
	h.gate.Lock()
	h.modeCtx, h.cancelMode = context.WithCancel(context.Background())
	h.modeSnapshot = mode
	h.transitioning = false
	h.gate.Unlock()
	return err
}
func (h *Host) watchEngine(ctx context.Context) {
	timer := time.NewTicker(time.Second)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-h.events:
		case <-timer.C:
		}
		_, finish, err := h.begin(ctx)
		if err != nil {
			continue
		}
		h.gatewayMu.Lock()
		h.optionsMu.Lock()
		enabled := h.settings.Gateway
		h.optionsMu.Unlock()
		if h.client != nil {
			if h.gatewayStale.Swap(false) && h.gateway != nil {
				_ = h.gateway.Stop()
				h.gateway = nil
			}
			if !enabled && h.gateway != nil {
				if err := h.gateway.Stop(); err != nil {
					h.gatewayError = err.Error()
				} else {
					h.gateway = nil
					h.gatewayError = ""
				}
			}
			if enabled && h.gateway == nil && h.client.EngineState() == core.EngineStateRunning {
				if err := h.startGateway(); err != nil {
					h.gatewayError = err.Error()
				} else {
					h.gatewayError = ""
					h.revision.Add(1)
				}
			}
		}
		h.gatewayMu.Unlock()
		finish()
	}
}

func (h *Host) writeAtomic(name string, data []byte) error {
	file, err := os.CreateTemp(h.State, ".native-*")
	if err != nil {
		return err
	}
	defer os.Remove(file.Name())
	if _, err = file.Write(data); err == nil {
		err = file.Sync()
	}
	closeErr := file.Close()
	if err != nil {
		return err
	}
	if closeErr != nil {
		return closeErr
	}
	if err := os.Rename(file.Name(), filepath.Join(h.State, name)); err != nil {
		return err
	}
	return winplatform.FlushDirectory(h.State)
}

func (h *Host) notifyMode(mode string) error {
	if h.OnModeChange == nil {
		return nil
	}
	if mode == "" && h.files != nil {
		data, err := os.ReadFile(filepath.Join(h.State, "projection.json"))
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		var records []struct{ Entry files.Entry }
		if len(data) > 0 {
			if err := json.Unmarshal(data, &records); err != nil {
				return err
			}
		}
		type approved struct {
			Item int64
			Node uint64
		}
		clean := []approved{}
		for _, record := range records {
			current, ok, err := h.files.Item(record.Entry.Item)
			if err != nil {
				return err
			}
			if !ok || current.Node != record.Entry.Node {
				continue
			}
			state, err := h.files.State(current)
			if err != nil {
				return err
			}
			switch state {
			case "", "remote", "partial", "local", "kept":
				clean = append(clean, approved{current.Item, current.Node})
			}
		}
		id, err := h.files.ClientIdentity()
		if err != nil {
			return err
		}
		var checkpoint []map[string]json.RawMessage
		if len(data) > 0 {
			if err := json.Unmarshal(data, &checkpoint); err != nil {
				return err
			}
		}
		approvedNodes := map[int64]uint64{}
		for _, entry := range clean {
			approvedNodes[entry.Item] = entry.Node
		}
		for i, record := range records {
			if node, ok := approvedNodes[record.Entry.Item]; ok && node == record.Entry.Node {
				checkpoint[i]["Detaching"] = json.RawMessage("true")
			}
		}
		marked, err := json.Marshal(checkpoint)
		if err != nil {
			return err
		}
		if err := h.writeAtomic("projection.json", marked); err != nil {
			return err
		}
		data, err = json.Marshal(map[string]any{"ClientID": id, "Clean": clean})
		if err != nil {
			return err
		}
		if err := h.writeAtomic("provider-clean.json", data); err != nil {
			return err
		}
	}
	id := ""
	if mode == "files" && h.files != nil {
		var err error
		id, err = h.files.ClientIdentity()
		if err != nil {
			return err
		}
	}
	if err := h.OnModeChange(mode, id, h.Root); err != nil {
		return err
	}
	if mode == "" && h.files != nil {
		if err := h.writeAtomic("projection.json", []byte("[]")); err != nil {
			// The native owner has already detached. Rebuild ownership from
			// durable state rather than leaving Files mode without a provider.
			if h.OnFatalError != nil {
				h.OnFatalError(err)
			}
			return err
		}
	}
	return nil
}

func (h *Host) projectionState(method string, p request) (any, error) {
	switch method {
	case "projection.root.save":
		if len(p.Snapshot) > 16<<10 || !json.Valid(p.Snapshot) {
			return nil, errors.New("invalid native root ownership record")
		}
		return nil, h.writeAtomic("projection-root.json", p.Snapshot)
	case "projection.root.get":
		data, err := os.ReadFile(filepath.Join(h.State, "projection-root.json"))
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return json.RawMessage(data), err
	case "projection.load":
		data, err := os.ReadFile(filepath.Join(h.State, "projection.json"))
		if errors.Is(err, os.ErrNotExist) {
			return []any{}, nil
		}
		return json.RawMessage(data), err
	}
	return nil, errors.New("unknown projection state request")
}

// InitializeProvider admits provider RPC while keeping mode/root changes behind
// the same transition lock until initial native ownership is established.
func (h *Host) InitializeProvider(listen func() error) error {
	h.transition.Lock()
	defer h.transition.Unlock()
	if err := listen(); err != nil {
		return err
	}
	id, err := h.ProviderIdentity()
	if err != nil {
		return err
	}
	if h.OnModeChange != nil {
		return h.OnModeChange(h.settings.Mode, id, h.ProviderRoot())
	}
	return nil
}

// ProviderIdentity is used while mode changes are excluded during startup.
func (h *Host) ProviderIdentity() (string, error) {
	h.mu.RLock()
	defer h.mu.RUnlock()
	if h.files == nil {
		return "", nil
	}
	return h.files.ClientIdentity()
}

func expectedEntry(p request) []files.Entry {
	if p.ClientID == "" {
		return nil
	}
	return []files.Entry{{Item: p.Item, Node: p.Node}}
}

// Keep the backup until the restored mode and journal cleanup are committed.
// Recovery can retry after any interruption without consuming its only copy.
func (h *Host) restoreModeBackup(backup string) error {
	info, err := os.Lstat(backup)
	if err != nil {
		return err
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return errors.New("the mode backup is not a physical directory")
	}
	restored, err := os.MkdirTemp(h.State, ".files-restored-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(restored)
	if err := copyState(backup, restored); err != nil {
		return err
	}
	if err := winplatform.FlushDirectory(restored); err != nil {
		return err
	}
	current := filepath.Join(h.State, "Files")
	interrupted := filepath.Join(h.State, ".files-interrupted-"+fmt.Sprint(time.Now().UnixNano()))
	hadCurrent := true
	if err := os.Rename(current, interrupted); errors.Is(err, os.ErrNotExist) {
		hadCurrent = false
	} else if err != nil {
		return err
	}
	if err := os.Rename(restored, current); err != nil {
		if hadCurrent {
			return errors.Join(err, os.Rename(interrupted, current))
		}
		return err
	}
	if err := winplatform.FlushDirectory(h.State); err != nil {
		return err
	}
	if hadCurrent {
		_ = os.RemoveAll(interrupted)
	}
	return nil
}

func (h *Host) finishModeJournal(backup string) error {
	if err := os.Remove(filepath.Join(h.State, "mode-transition.json")); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	// A restored backup is removable only after journal deletion is durable.
	if err := winplatform.FlushDirectory(h.State); err != nil {
		return err
	}
	if backup != "" {
		return os.RemoveAll(backup)
	}
	return nil
}

// Configure the Explorer location before entering Files mode. The provider
// performs the NTFS and cloud-root checks before the mode is committed.
func (h *Host) configureRoot(path string) error {
	h.transition.Lock()
	defer h.transition.Unlock()
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.files != nil {
		return errors.New("switch out of Files mode before choosing another Explorer location")
	}
	if !filepath.IsAbs(path) {
		return errors.New("choose an absolute local folder path")
	}
	path = filepath.Clean(path)
	if info, err := os.Lstat(path); err == nil {
		if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			return errors.New("choose a physical directory")
		}
		entries, err := os.ReadDir(path)
		if err != nil {
			return err
		}
		if len(entries) != 0 {
			return errors.New("choose an empty directory")
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if h.client != nil && h.client.OverlappingFolderID(path) != "" {
		return errors.New("the location overlaps a synchronized folder")
	}
	previousRoot, previousSetting := h.Root, h.settings.FilesRoot
	h.Root, h.settings.FilesRoot = path, path
	if err := h.save(); err != nil {
		h.Root, h.settings.FilesRoot = previousRoot, previousSetting
		return err
	}
	h.revision.Add(1)
	return nil
}

// ProviderRoot is read before initial provider startup; mode callbacks carry the
// same value under the transition lease.
func (h *Host) ProviderRoot() string { h.mu.RLock(); defer h.mu.RUnlock(); return h.Root }

func (h *Host) projectionIssues() json.RawMessage {
	data, err := os.ReadFile(filepath.Join(h.State, "projection-issues.json"))
	if err != nil || len(data) > 128<<10 || !json.Valid(data) {
		return json.RawMessage("[]")
	}
	return data
}
func (h *Host) downloadFile(ctx context.Context, item int64, destination string) error {
	if !filepath.IsAbs(destination) {
		return errors.New("choose a full destination path")
	}
	destination, err := h.copyDestination(destination)
	if err != nil {
		return err
	}

	reader, err := h.files.OpenFile(item)
	if err != nil {
		return err
	}
	defer reader.Close()
	output, err := os.CreateTemp(filepath.Dir(destination), ".synctrain-download-")
	if err != nil {
		return err
	}
	temporary := output.Name()
	defer os.Remove(temporary)
	defer output.Close()
	buffer := make([]byte, 1<<20)
	for offset := int64(0); offset < reader.Size(); {
		count := int64(len(buffer))
		if count > reader.Size()-offset {
			count = reader.Size() - offset
		}
		read, err := reader.ReadAt(ctx, buffer[:count], offset)
		if read > 0 {
			if _, writeErr := output.Write(buffer[:read]); writeErr != nil {
				return writeErr
			}
			offset += int64(read)
		}
		if err != nil && !(errors.Is(err, io.EOF) && offset == reader.Size()) {
			return err
		}
		if read == 0 && offset < reader.Size() {
			return io.ErrNoProgress
		}
	}
	if err := output.Sync(); err != nil {
		return err
	}
	if err := output.Close(); err != nil {
		return err
	}
	return os.Rename(temporary, destination)
}

func (h *Host) waitForFiles(ctx context.Context) error {
	if h.files == nil {
		return nil
	}
	folders, err := h.files.Folders()
	if err != nil {
		return err
	}
	for _, folder := range folders {
		entry, err := h.files.Resolve(folder.DirName)
		if err != nil {
			return err
		}
		if err := h.files.Wait(ctx, entry.Item); err != nil {
			return err
		}
	}
	return nil
}
