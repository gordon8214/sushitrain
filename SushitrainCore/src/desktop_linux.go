// Copyright (C) 2026 Gordon
// SPDX-License-Identifier: MPL-2.0

package sushitrain

import (
	"context"
	"crypto/tls"
	"os"
	"path/filepath"
	"slices"
	"sync"

	"github.com/syncthing/syncthing/lib/config"
	"github.com/syncthing/syncthing/lib/dialer"
	"github.com/syncthing/syncthing/lib/events"
	"github.com/syncthing/syncthing/lib/locations"
	"github.com/syncthing/syncthing/lib/model"
	"github.com/syncthing/syncthing/lib/syncthing"
)

// PrepareDesktopEngine applies persisted safety fences before the gateway starts
// networking. It never creates or starts another engine.
func PrepareDesktopEngine(cfg config.Wrapper) error {
	RegisterTrashHandler(linuxTrash{})
	proxy, err := loadProxyFile(proxyFilePath())
	if err != nil {
		return err
	}
	parsed, err := proxy.proxy()
	if err != nil {
		return err
	}
	dialer.SetProxy(parsed)
	home, err := os.UserHomeDir()
	if err != nil {
		return err
	}
	conf := cfg.RawCopy()
	changed, finished, err := finishRelocations(&conf, relocationJournalPath(), home)
	if err != nil {
		return err
	}
	if err := recoverHistoryRestores(conf.FolderMap(), filepath.Join(locations.GetBaseDir(locations.ConfigBaseDir), "history-restores")); err != nil {
		return err
	}
	if changed {
		waiter, err := cfg.Modify(func(current *config.Configuration) { *current = conf })
		if err != nil {
			return err
		}
		waiter.Wait()
		if err := cfg.Save(); err != nil {
			return err
		}
	}
	if len(finished) > 0 {
		return changeRelocations(func(pending []folderRelocation) []folderRelocation {
			return slices.DeleteFunc(pending, func(r folderRelocation) bool { return slices.Contains(finished, r) })
		})
	}
	return nil
}

// AttachDesktopEngine reuses the native core's validated operations on an engine
// owned by the gateway. The caller owns its lock, configuration, database and
// lifecycle. Call detach before stopping those resources; never call Load, Start,
// Stop or RestartEngine on this attached client.
func AttachDesktopEngine(ctx context.Context, app *syncthing.App, cfg config.Wrapper, logger events.Logger,
	cert tls.Certificate) (*Client, func(), error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, nil, err
	}
	managed, err := loadRemoteManaged(remoteManagedPath())
	if err != nil {
		return nil, nil, err
	}
	proxy, err := loadProxyFile(proxyFilePath())
	if err != nil {
		return nil, nil, err
	}
	c := &Client{cert: &cert, filesPath: home, folderLayout: homeFolders,
		foldersDownloading: map[string]bool{}, connectedDeviceAddresses: map[string]string{},
		uploadProgress: map[string]map[string]map[string]int{}, downloadProgress: map[string]map[string]*model.PullerProgress{},
		ResolvedListenAddresses: map[string][]string{}, remoteManaged: managed}
	c.proxy.Store(proxy)
	r := &engineRun{app: app, config: cfg, evLogger: logger, guiStarted: cfg.GUI().Enabled, proxy: proxy,
		listenDone: make(chan struct{})}
	r.ctx, r.cancel = context.WithCancel(ctx)
	r.internals.Store(app.Internals)
	c.run.Store(r)
	c.setStatus(EngineStateRunning, "", false)
	c.Measurements = NewMeasurements(c)
	server, err := NewServer(c.Measurements)
	if err != nil {
		r.cancel()
		return nil, nil, err
	}
	server.client = c
	c.Server = server
	go c.listenToEvents(r, logger.Subscribe(events.AllEvents))
	var once sync.Once
	detach := func() {
		once.Do(func() {
			r.writes.Lock()
			r.closed = true
			r.writes.Unlock()
			r.cancel()
			server.Close()
			<-r.listenDone
		})
	}
	return c, detach, nil
}
