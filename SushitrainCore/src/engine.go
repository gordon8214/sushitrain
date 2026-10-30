// Copyright (C) 2026 Gordon
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this file,
// You can obtain one at https://mozilla.org/MPL/2.0/.
package sushitrain

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/syncthing/syncthing/lib/build"
	"github.com/syncthing/syncthing/lib/config"
	"github.com/syncthing/syncthing/lib/events"
	"github.com/syncthing/syncthing/lib/locations"
	"github.com/syncthing/syncthing/lib/model"
	"github.com/syncthing/syncthing/lib/svcutil"
	"github.com/syncthing/syncthing/lib/syncthing"
	"github.com/syncthing/syncthing/lib/upgrade"

	"t-shaped.nl/sushitrain/v2/sthome"
)

// What the engine is doing, as Client.EngineState reports it and ClientDelegate.OnEngineStateChanged announces it
const (
	EngineStateLoaded            = "loaded"
	EngineStateRunning           = "running"
	EngineStateRestarting        = "restarting"
	EngineStateShutdownRequested = "shutdown-requested"
	EngineStateFailed            = "failed"
	EngineStateStopped           = "stopped"
)

var (
	// ErrRestarting means the engine is being restarted; try again once it runs
	ErrRestarting = errors.New("the engine is restarting")
	// ErrEngineStopped means the engine stopped or failed; restarting it (Client.RestartEngine) may help
	ErrEngineStopped = errors.New("the engine is not running")
)

const (
	// How long Syncthing may take to stop, as the gateway allows it
	engineStopTimeout = 8 * time.Second
	// How long the remote file service may take to stop: its HTTP server gets 5 seconds and its operations 3 more
	remoteServiceStopTimeout = 10 * time.Second
	// Default retention of deleted items, from Syncthing's command line default
	// (https://github.com/syncthing/syncthing/commit/7a76685d7ef277d2a4aaccf9685d55323ad1d6de)
	dbDeleteRetentionInterval = 10920 * time.Hour
)

// engineRun is one run of the engine: Syncthing with its event logger, configuration and database. Restarting tears a
// run down and starts a new one. The client, its lock, streaming server and measurements stay.
type engineRun struct {
	// Ends the run's event listener and file provider watcher, and the waits of calls into the run
	ctx    context.Context
	cancel context.CancelFunc

	evLogger    events.Logger
	stopLog     context.CancelFunc
	logDone     chan struct{}
	config      config.Wrapper
	stopCfg     context.CancelFunc
	cfgDone     chan struct{} // closed once the configuration is no longer served, after its last save
	listenDone  chan struct{} // closed once the event listener returned
	app         *syncthing.App
	appDone     chan struct{}                       // closed once the app stopped
	closeDB     func()                              // closes the database of a run that never started (the app closes it otherwise)
	internals   atomic.Pointer[syncthing.Internals] // set once the app has started
	firstRun    bool
	stopTimeout atomic.Bool // the app did not stop in time; its configuration and logger are left alone

	// Configuration changes hold this for reading; the teardown takes it to stop them
	writes sync.RWMutex
	closed bool
	// Whether the configuration and the event logger stopped (closeRun ran)
	released bool
}

func (r *engineRun) isClosed() bool {
	r.writes.RLock()
	defer r.writes.RUnlock()
	return r.closed
}

// engineStatus is what the engine is doing, with an error message when it failed
type engineStatus struct {
	state   string
	message string
}

// currentRun is the run Synctrain works with: the running one, or while restarting or after stopping, the last one.
// Nil before Load.
func (clt *Client) currentRun() *engineRun {
	return clt.run.Load()
}

// internals is the running engine's model. It is nil until the engine first started, and never nil again afterwards:
// while the engine restarts, or after it stopped, it is the last engine's, whose calls fail. Calls that wait for the
// engine should also stop waiting when its run ends (see engineRun.ctx).
func (clt *Client) internals() *syncthing.Internals {
	if r := clt.run.Load(); r != nil {
		return r.internals.Load()
	}
	return nil
}

// cfg is the engine's configuration (nil before Load). While restarting it is the last run's, which can be read but not
// changed.
func (clt *Client) cfg() config.Wrapper {
	if r := clt.run.Load(); r != nil {
		return r.config
	}
	return nil
}

// EngineState is what the engine is doing: one of the EngineState constants, or empty before Load
func (clt *Client) EngineState() string {
	if status := clt.status.Load(); status != nil {
		return status.state
	}
	return ""
}

// EngineError says why the engine failed (EngineStateFailed), or is empty
func (clt *Client) EngineError() string {
	if status := clt.status.Load(); status != nil {
		return status.message
	}
	return ""
}

// EngineCanRestart reports whether RestartEngine can start the engine again after it failed or was shut down
func (clt *Client) EngineCanRestart() bool {
	state := clt.EngineState()
	if state != EngineStateFailed && state != EngineStateShutdownRequested {
		return false
	}
	r := clt.currentRun()
	return r != nil && !r.stopTimeout.Load()
}

// errNotRunning is the error for a call that a closed run refuses
func (clt *Client) errNotRunning() error {
	if clt.EngineState() == EngineStateRestarting {
		return ErrRestarting
	}
	return ErrEngineStopped
}

// setStatus records what the engine is doing. The caller holds lifecycle, so that changes are recorded in the order
// they happen. When notify is set, the delegate is told, in the same order, from a goroutine that holds no locks.
func (clt *Client) setStatus(state string, message string, notify bool) {
	clt.status.Store(&engineStatus{state: state, message: message})
	slog.Info("Engine state", "state", state, "message", message)
	if !notify {
		return
	}
	clt.notifyMutex.Lock()
	defer clt.notifyMutex.Unlock()
	clt.notifications = append(clt.notifications, engineStatus{state: state, message: message})
	if !clt.notifying {
		clt.notifying = true
		go clt.deliverNotifications()
	}
}

func (clt *Client) deliverNotifications() {
	for {
		clt.notifyMutex.Lock()
		if len(clt.notifications) == 0 {
			clt.notifying = false
			clt.notifyMutex.Unlock()
			return
		}
		status := clt.notifications[0]
		clt.notifications = clt.notifications[1:]
		clt.notifyMutex.Unlock()

		if delegate := clt.Delegate; delegate != nil {
			delegate.OnEngineStateChanged(status.state, status.message)
		}
	}
}

// prepareRun loads the configuration and opens the database for a new run, ready to start. On failure it undoes what
// it did, and records what the user can do about a configuration that can't be used.
func (clt *Client) prepareRun() (_ *engineRun, err error) {
	clt.startupProblem.Store("")
	r := &engineRun{logDone: make(chan struct{}), cfgDone: make(chan struct{}), appDone: make(chan struct{}),
		firstRun: clt.run.Load() == nil}
	r.ctx, r.cancel = context.WithCancel(context.Background())
	r.evLogger = events.NewLogger()
	var logCtx context.Context
	logCtx, r.stopLog = context.WithCancel(context.Background())
	go func() {
		defer close(r.logDone)
		_ = r.evLogger.Serve(logCtx)
	}()
	defer func() {
		if err != nil {
			if r.closeDB != nil {
				r.closeDB()
			}
			r.cancel()
			r.stopLog()
			<-r.logDone
		}
	}()

	cfg, err := loadEngineConfig(locations.Get(locations.ConfigFile), *clt.cert, r.evLogger, clt.filesPath,
		clt.remoteManaged, build.IsIOS)
	if err != nil {
		if errors.Is(err, sthome.ErrConfiguration) {
			clt.startupProblem.Store(StartupProblemDamagedConfiguration)
		} else if errors.Is(err, sthome.ErrNewerConfiguration) {
			clt.startupProblem.Store(StartupProblemNewerConfiguration)
		}
		return nil, err
	}

	// Refuse a database a newer Syncthing wrote, which this one would not migrate back
	dbPath := locations.Get(locations.Database)
	if err := sthome.CheckSchema(dbPath); err != nil {
		return nil, err
	}
	// It really wants to set up a temporary API while migrating...
	if err := syncthing.TryMigrateDatabase(r.ctx, dbDeleteRetentionInterval); err != nil {
		slog.Warn("failed to migrate legacy database", "cause", err)
		return nil, err
	}
	sdb, err := syncthing.OpenDatabase(dbPath, dbDeleteRetentionInterval)
	if err != nil {
		return nil, err
	}
	r.closeDB = func() { sdb.Close() }

	// The app is updated as a whole; self-upgrade is also compiled out (the noupgrade tag), which is what blocks the REST
	// API's upgrade request
	if !upgrade.DisabledByCompilation && !testing.Testing() {
		slog.Error("This build can upgrade the engine through the REST API; build it with the noupgrade tag")
	}
	app, err := syncthing.New(cfg, sdb, r.evLogger, *clt.cert, syncthing.Options{
		NoUpgrade:      true,
		ProfilerAddr:   "",
		ResetDeltaIdxs: clt.resetDeltaIdxs,

		// Disable periodic database maintenance, we will schedule it ourselves at a convenient time
		DBMaintenanceInterval: 0,
	})
	if err != nil {
		return nil, err
	}
	r.app = app

	// Only now serve the configuration, which saves it when it stops: a failed load leaves the file as it was
	r.config = cfg
	var cfgCtx context.Context
	cfgCtx, r.stopCfg = context.WithCancel(context.Background())
	go func() {
		defer close(r.cfgDone)
		_ = cfg.Serve(cfgCtx)
	}()
	return r, nil
}

// startRun starts a prepared run: its event listener, the file provider watcher, and Syncthing, and then watches it.
// The caller holds lifecycle.
func (clt *Client) startRun(r *engineRun) error {
	// Both subscribe before Syncthing starts, so that they see its first events
	sub := r.evLogger.Subscribe(events.AllEvents)
	r.listenDone = make(chan struct{})
	go clt.listenToEvents(r, sub)
	clt.followWithFileProviderWatcher(r)

	if err := r.app.Start(); err != nil {
		// The app closed the database and stopped what it had started
		r.closeDB = nil
		close(r.appDone)
		clt.followWithFileProviderWatcher(nil)
		clt.closeRun(r)
		return err
	}
	r.internals.Store(r.app.Internals)
	// A reset of the delta indexes was requested for the next run that starts
	clt.resetDeltaIdxs = false
	go clt.supervise(r)
	return nil
}

// supervise waits for a run's Syncthing to stop, and restarts it when the web GUI or the REST API asked for that
func (clt *Client) supervise(r *engineRun) {
	status := r.app.Wait()
	close(r.appDone)

	clt.lifecycle.Lock()
	defer clt.lifecycle.Unlock()
	// Stopped by the app (Stop, RestartEngine), or replaced already
	if clt.stopRequested || clt.run.Load() != r || r.isClosed() {
		return
	}
	switch status {
	case svcutil.ExitRestart, svcutil.ExitUpgrade:
		_ = clt.restartLocked(r)
	case svcutil.ExitSuccess:
		// The web GUI or the REST API asked for a shutdown; the app quits
		clt.teardown(r)
		clt.setStatus(EngineStateShutdownRequested, "", true)
	default:
		clt.teardown(r)
		message := fmt.Sprintf("Syncthing stopped (exit status %v)", status)
		if err := r.app.Error(); err != nil {
			message = err.Error()
		}
		clt.setStatus(EngineStateFailed, message, true)
	}
}

// restartLocked tears down a run and starts a new one in its place, reporting the progress. The caller holds lifecycle.
func (clt *Client) restartLocked(r *engineRun) error {
	clt.setStatus(EngineStateRestarting, "", true)
	if err := clt.replaceRunLocked(r); err != nil {
		clt.setStatus(EngineStateFailed, err.Error(), true)
		return err
	}
	clt.setStatus(EngineStateRunning, "", true)
	return nil
}

func (clt *Client) replaceRunLocked(r *engineRun) error {
	if !clt.teardown(r) {
		return errors.New("Syncthing did not stop in time; quit and reopen the app")
	}
	next, err := clt.prepareRun()
	if err != nil {
		return err
	}
	if err := clt.startRun(next); err != nil {
		return err
	}
	// Published once started, so the model is never nil to those who saw one before
	clt.run.Store(next)
	return nil
}

// RestartEngine stops the engine and starts it again, as the web GUI's restart does, or starts it again after it failed
// or was shut down. ClientDelegate.OnEngineStateChanged reports the progress. Returns once the engine runs again.
func (clt *Client) RestartEngine() error {
	clt.lifecycle.Lock()
	defer clt.lifecycle.Unlock()
	r := clt.run.Load()
	if r == nil || clt.stopRequested {
		return ErrStillLoading
	}
	if r.internals.Load() == nil && !r.isClosed() {
		// Loaded but not started
		return ErrStillLoading
	}
	return clt.restartLocked(r)
}

// Stop stops the engine for good, as the app quits. Calling it again does nothing.
func (clt *Client) Stop() {
	clt.lifecycle.Lock()
	defer clt.lifecycle.Unlock()
	if clt.stopRequested {
		return
	}
	clt.stopRequested = true
	if r := clt.run.Load(); r != nil {
		clt.teardown(r)
	}
	clt.setStatus(EngineStateStopped, "", true)
}

// teardown stops a run: no more configuration changes, then the remote file service and the file provider watcher, then
// Syncthing, and then the configuration (after its last save) and the event logger. It reports whether Syncthing has
// stopped; if it didn't in time, its configuration and logger are left running for it, and a later call waits again.
// The lock in the data directory stays. The caller holds lifecycle.
func (clt *Client) teardown(r *engineRun) bool {
	r.writes.Lock()
	alreadyClosed := r.closed
	r.closed = true
	r.writes.Unlock()

	if !alreadyClosed {
		clt.stopRemoteService()
		clt.followWithFileProviderWatcher(nil)
	}

	if r.internals.Load() != nil {
		// Returns at once when Syncthing stopped already
		go r.app.Stop(svcutil.ExitSuccess)
		select {
		case <-r.appDone:
			r.stopTimeout.Store(false)
		case <-time.After(engineStopTimeout):
			slog.Warn("Syncthing did not stop in time")
			r.stopTimeout.Store(true)
			return false
		}
	} else if r.closeDB != nil {
		// Never started: the app never took over the database
		r.closeDB()
		r.closeDB = nil
	}
	clt.closeRun(r)
	clt.resetRunState()
	return true
}

// resetRunState forgets what the client learned from a run's events; the ignored extraneous files are a setting and
// stay
func (clt *Client) resetRunState() {
	clt.mutex.Lock()
	clt.connectedDeviceAddresses = make(map[string]string)
	clt.downloadProgress = make(map[string]map[string]*model.PullerProgress)
	clt.uploadProgress = make(map[string]map[string]map[string]int)
	clt.foldersDownloading = make(map[string]bool)
	clt.ResolvedListenAddresses = make(map[string][]string)
	clt.mutex.Unlock()
	if clt.Measurements != nil {
		clt.Measurements.reset()
	}
}

// closeRun ends a run's event listener, stops serving its configuration (waiting for its last save) and then its event
// logger, which closes the remaining subscriptions. Calling it again does nothing.
func (clt *Client) closeRun(r *engineRun) {
	r.writes.Lock()
	r.closed = true
	released := r.released
	r.released = true
	r.writes.Unlock()
	if released {
		return
	}

	r.cancel()
	if r.listenDone != nil {
		<-r.listenDone
	}
	if r.stopCfg != nil {
		r.stopCfg()
		<-r.cfgDone
	}
	if r.stopLog != nil {
		r.stopLog()
		<-r.logDone
	}
}

// scanFolderSubdirs scans a folder and waits for the scan, but stops waiting when the engine stops: a folder that
// stopped before its first scan never answers
func (clt *Client) scanFolderSubdirs(folderID string, subdirs []string) error {
	r := clt.currentRun()
	if r == nil || r.internals.Load() == nil {
		return ErrStillLoading
	}
	if r.isClosed() {
		return clt.errNotRunning()
	}
	done := make(chan error, 1)
	go func() {
		done <- r.internals.Load().ScanFolderSubdirs(folderID, subdirs)
	}()
	select {
	case err := <-done:
		return err
	case <-r.ctx.Done():
		return clt.errNotRunning()
	}
}
