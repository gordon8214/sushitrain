// Copyright (C) 2024 Tommy van der Vorst
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this file,
// You can obtain one at https://mozilla.org/MPL/2.0/.
package sushitrain

import (
	"archive/zip"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"iter"
	"log/slog"
	"math"
	"net/url"
	"os"
	"path"
	"reflect"
	"runtime"
	"runtime/pprof"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/gofrs/flock"
	"github.com/syncthing/syncthing/lib/build"
	"github.com/syncthing/syncthing/lib/config"
	"github.com/syncthing/syncthing/lib/events"
	"github.com/syncthing/syncthing/lib/fs"
	"github.com/syncthing/syncthing/lib/locations"
	"github.com/syncthing/syncthing/lib/model"
	"github.com/syncthing/syncthing/lib/osutil"
	"github.com/syncthing/syncthing/lib/protocol"
	"github.com/syncthing/syncthing/lib/syncthing"

	"t-shaped.nl/sushitrain/v2/sthome"
)

type Client struct {
	cert                       *tls.Certificate
	Delegate                   ClientDelegate
	filesPath                  string
	IgnoreEvents               bool
	IsUsingCustomConfiguration bool
	Server                     *StreamingServer

	connectedDeviceAddresses map[string]string
	downloadProgress         map[string]map[string]*model.PullerProgress // folderID, path => progress
	uploadProgress           map[string]map[string]map[string]int        // deviceID, folderID, path => block count
	foldersDownloading       map[string]bool
	ResolvedListenAddresses  map[string][]string
	mutex                    sync.Mutex
	extraneousIgnored        []string
	Measurements             *Measurements
	defaultLogLevel          slog.Level
	appLock                  *flock.Flock
	lockedPath               string
	lockMutex                sync.Mutex
	remoteManaged            *remoteManagedSet
	startupProblem           atomic.Value // string, one of the StartupProblem constants

	// The engine, restarted in place (engine.go). lifecycle serializes loading, starting, restarting and stopping, and
	// is never held while calling the delegate.
	run            atomic.Pointer[engineRun]
	lifecycle      sync.Mutex
	stopRequested  bool
	resetDeltaIdxs bool // for the next run that starts
	status         atomic.Pointer[engineStatus]

	// Engine states waiting to be told to the delegate, in order (engine.go)
	notifyMutex   sync.Mutex
	notifications []engineStatus
	notifying     bool

	// The file provider watcher follows the running engine (fileprovider_events.go)
	watcherMutex         sync.Mutex
	fileProviderDelegate FileProviderDelegate
	watchedRun           *engineRun

	// The remote file service serves one run; a restart stops it (gateway_mobile.go)
	remoteMutex   sync.Mutex
	remoteService *RemoteFileService
	remoteClosing chan struct{} // closed once a service that did not stop in time has stopped
}

type Change struct {
	FolderID string
	Path     string
	Action   string
	ShortID  string
	Time     *Date
}

const measurementStaleAfterDurationSeconds = 60.0

type Measurement struct {
	latency float64
	when    time.Time
}

type Measurements struct {
	mutex        sync.Mutex
	measurements map[string]Measurement
	client       *Client
	isMeasuring  bool
}

type ClientDelegate interface {
	OnEvent(event string)
	OnFolderScanFinished(folderID string)
	OnDeviceDiscovered(deviceID string, addresses *ListOfStrings)
	OnListenAddressesChanged(addresses *ListOfStrings)
	OnChange(change *Change)
	OnMeasurementsUpdated()
	// The engine restarted, stopped or failed (one of the EngineState constants, with an error message when it failed).
	// Called from any thread, and not subject to IgnoreEvents.
	OnEngineStateChanged(state string, message string)
}

var (
	ErrStillLoading = errors.New("still loading")

	// ErrInstanceRunning means another Syncthing (or another copy of the app) uses the configuration directory
	ErrInstanceRunning = errors.New("The app cannot be started, as it appears it is already running. If this error persists, try restarting your device.")
)

// Why the engine could not start, when the user can do something about it (see Client.StartupProblem)
const (
	// The configuration file can't be parsed; it can be set aside (Client.SetConfigurationAside)
	StartupProblemDamagedConfiguration = "damaged-configuration"
	// The configuration file is from a newer Syncthing
	StartupProblemNewerConfiguration = "newer-configuration"
	// The device certificate or key is missing or damaged; only a new identity (Client.ClearIdentity) gets past it
	StartupProblemDamagedIdentity = "damaged-identity"
)

const (
	ConfigFileName       = "config.xml"
	ExportConfigFileName = "exported-config.xml"
	CertFileName         = "cert.pem"
	KeyFileName          = "key.pem"
)

func NewClient(configPath string, filesPath string, saveLog bool) *Client {
	// Set version info
	build.Version = "v2.1.7"
	build.Host = "t-shaped.nl"
	build.User = "sushitrain"

	// Set up logging
	var logOutWriter io.Writer = os.Stdout
	if saveLog {
		logFilePath := path.Join(filesPath, fmt.Sprintf("%s.log", time.Now().UTC().Format("synctrain-2006-2-1-15-04-05")))
		// Unbuffered, so that the file has every line when the app stops; the log handler writes a line at a time
		if logFile, err := os.Create(logFilePath); err != nil {
			fmt.Println(err)
		} else {
			logOutWriter = io.MultiWriter(os.Stdout, logFile)
		}
	}
	defaultLogLevel := configureLogging(logOutWriter, saveLog)

	// Set up default locations
	locations.SetBaseDir(locations.DataBaseDir, configPath)
	locations.SetBaseDir(locations.ConfigBaseDir, configPath)
	locations.SetBaseDir(locations.UserHomeBaseDir, filesPath)
	slog.Info("paths", "databaseDir", configPath, "filesDir", filesPath)

	// Check for custom user-provided config file
	isUsingCustomConfiguration := false
	customConfigFilePath := path.Join(filesPath, ConfigFileName)
	if info, err := os.Stat(customConfigFilePath); err == nil {
		if !info.IsDir() {
			slog.Info("config XML exists in files dir, using it at", "path", customConfigFilePath)
			locations.Set(locations.ConfigFile, customConfigFilePath)
			isUsingCustomConfiguration = true
		}
	}

	// Check for custom user-provided identity
	customCertPath := path.Join(filesPath, CertFileName)
	customKeyPath := path.Join(filesPath, KeyFileName)
	if keyInfo, err := os.Stat(customKeyPath); err == nil {
		if !keyInfo.IsDir() {
			if certInfo, err := os.Stat(customCertPath); err == nil {
				if !certInfo.IsDir() {
					slog.Info("found user-provided identity files, using those")
					locations.Set(locations.CertFile, customCertPath)
					locations.Set(locations.KeyFile, customKeyPath)
					isUsingCustomConfiguration = true
				}
			}
		}
	}

	return &Client{
		Delegate:                   nil,
		cert:                       nil,
		Server:                     nil,
		foldersDownloading:         make(map[string]bool, 0),
		connectedDeviceAddresses:   make(map[string]string, 0),
		IsUsingCustomConfiguration: isUsingCustomConfiguration,
		filesPath:                  filesPath,
		IgnoreEvents:               false,
		uploadProgress:             make(map[string]map[string]map[string]int),
		downloadProgress:           make(map[string]map[string]*model.PullerProgress),
		ResolvedListenAddresses:    make(map[string][]string),
		extraneousIgnored:          make([]string, 0),
		Measurements:               nil,
		defaultLogLevel:            defaultLogLevel,
	}
}

func (clt *Client) SetExtraneousIgnored(names []string) {
	clt.extraneousIgnored = names
}

func (clt *Client) SetExtraneousIgnoredJSON(js []byte) error {
	var names []string
	if err := json.Unmarshal(js, &names); err != nil {
		return err
	}
	clt.SetExtraneousIgnored(names)
	return nil
}

func (clt *Client) isExtraneousIgnored(name string) bool {
	// Always ignore files that are prefixed with .syncthing. or ~syncthing~, these are considered 'Syncthing private'
	// See https://docs.syncthing.net/users/syncing.html#temporary-files
	if strings.HasPrefix(name, ".syncthing.") || strings.HasPrefix(name, "~syncthing~") {
		return true
	}

	// Must be an equal match for now
	return slices.Contains(clt.extraneousIgnored, name)
}

func (clt *Client) CurrentConfigDirectory() string {
	return locations.GetBaseDir(locations.ConfigBaseDir)
}

// ConfigurationFilePath is the path of the configuration file the engine loads
func (clt *Client) ConfigurationFilePath() string {
	return locations.Get(locations.ConfigFile)
}

func (c *Client) ClearIdentity() error {
	if err := c.acquireLock(); err != nil {
		return err
	}
	// Either file may be gone already, from an earlier attempt that removed only the other
	certPath := locations.Get(locations.CertFile)
	slog.Warn("Removing certificate", "path", certPath)
	if err := os.Remove(certPath); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}

	keyPath := locations.Get(locations.KeyFile)
	slog.Warn("Removing key", "path", keyPath)
	if err := os.Remove(keyPath); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

// acquireLock takes the instance lock in the data directory, once per process, and keeps it until the process ends.
// Nothing in the directory may be changed without it: another Syncthing using the directory would be working with it.
func (clt *Client) acquireLock() error {
	clt.lockMutex.Lock()
	defer clt.lockMutex.Unlock()
	lockPath := locations.Get(locations.LockFile)
	if clt.appLock != nil {
		if lockPath != clt.lockedPath {
			return fmt.Errorf("the configuration directory changed from %s to %s; restart the app", clt.lockedPath,
				lockPath)
		}
		return nil
	}

	if err := os.MkdirAll(path.Dir(lockPath), 0o700); err != nil {
		return err
	}
	slog.Info("Attempting to obtain application lock at", "path", lockPath)
	lock := flock.New(lockPath)
	locked, err := lock.TryLock()
	if err != nil {
		return fmt.Errorf("failed to obtain lock: %w", err)
	} else if !locked {
		return ErrInstanceRunning
	}
	clt.appLock = lock
	clt.lockedPath = lockPath
	return nil
}

// StartupProblem says why the last Load failed, when the user can do something about it: one of the StartupProblem
// constants, or empty. The files involved are left as they were.
func (clt *Client) StartupProblem() string {
	problem, _ := clt.startupProblem.Load().(string)
	return problem
}

// SetConfigurationAside renames a damaged configuration file to config.xml.broken-<time>, so that the next start
// creates a new configuration with the same device identity. Returns the new name of the file. Only possible after Load
// failed on a damaged configuration of the app's own (not one placed in the files directory).
func (clt *Client) SetConfigurationAside() (string, error) {
	if clt.StartupProblem() != StartupProblemDamagedConfiguration || clt.IsUsingCustomConfiguration {
		return "", errors.New("only a damaged configuration of the app's own can be set aside")
	}
	if err := clt.acquireLock(); err != nil {
		return "", err
	}
	configPath := locations.Get(locations.ConfigFile)
	asidePath := configPath + ".broken-" + time.Now().UTC().Format("20060102-150405")
	slog.Warn("Setting the configuration aside", "path", configPath, "newPath", asidePath)
	// A link and a removal rather than a rename, which would replace an earlier copy set aside in the same second
	if err := os.Link(configPath, asidePath); err != nil {
		return "", err
	}
	if err := os.Remove(configPath); err != nil {
		return "", err
	}
	return asidePath, nil
}

func (clt *Client) ExportConfigurationFile() error {
	cfg := clt.cfg().RawCopy()
	homeDir := locations.GetBaseDir(locations.UserHomeBaseDir)
	customConfigFilePath := path.Join(homeDir, ExportConfigFileName)
	fd, err := osutil.CreateAtomic(customConfigFilePath)
	if err != nil {
		return err
	}

	if err := cfg.WriteXML(osutil.LineEndingsWriter(fd)); err != nil {
		fd.Close()
		return err
	}

	if err := fd.Close(); err != nil {
		return err
	}
	return nil
}

func (clt *Client) handleEvent(evt events.Event) {
	switch evt.Type {
	case events.DeviceDiscovered:
		clt.mutex.Lock()
		if !clt.IgnoreEvents && clt.Delegate != nil {
			data := evt.Data.(map[string]interface{})
			devID := data["device"].(string)
			addresses := data["addrs"].([]string)
			clt.mutex.Unlock()
			clt.Delegate.OnDeviceDiscovered(devID, &ListOfStrings{data: addresses})
		} else {
			clt.mutex.Unlock()
		}

	case events.FolderRejected:
		// FolderRejected is deprecated
		break

	case events.StateChanged:
		// Keep track of which folders are in syncing state. We need to know whether we are idling or not
		data := evt.Data.(map[string]interface{})
		folder := data["folder"].(string)
		state := data["to"].(string)
		previousState, _ := data["from"].(string)
		folderTransferring := (state == model.FolderSyncing.String() || state == model.FolderSyncWaiting.String() || state == model.FolderSyncPreparing.String())

		clt.mutex.Lock()
		clt.foldersDownloading[folder] = folderTransferring
		if !clt.IgnoreEvents && clt.Delegate != nil {
			delegate := clt.Delegate
			clt.mutex.Unlock()
			// Forward completion before general UI events can be throttled. A short
			// scan may start and finish between two UI updates.
			if previousState == model.FolderScanning.String() && state != previousState {
				delegate.OnFolderScanFinished(folder)
			}
			delegate.OnEvent(evt.Type.String())
		} else {
			clt.mutex.Unlock()
		}

	case events.ListenAddressesChanged:
		clt.mutex.Lock()
		if !clt.IgnoreEvents && clt.Delegate != nil {
			addrs := make([]string, 0)
			data := evt.Data.(map[string]interface{})
			addressSpec := data["address"].(*url.URL)
			wanAddresses := data["wan"].([]*url.URL)
			lanAddresses := data["lan"].([]*url.URL)

			for _, wa := range wanAddresses {
				addrs = append(addrs, wa.String())
			}
			for _, la := range lanAddresses {
				addrs = append(addrs, la.String())
			}
			clt.ResolvedListenAddresses[addressSpec.String()] = addrs

			// Get all current addresses and send to client
			currentResolved := make([]string, 0)
			for _, addrs := range clt.ResolvedListenAddresses {
				currentResolved = append(currentResolved, addrs...)
			}
			clt.mutex.Unlock()
			clt.Delegate.OnListenAddressesChanged(List(currentResolved))
		} else {
			clt.mutex.Unlock()
		}

	case events.DeviceConnected:
		data := evt.Data.(map[string]string)
		devID := data["id"]
		address := data["addr"]

		clt.mutex.Lock()
		clt.connectedDeviceAddresses[devID] = address

		if !clt.IgnoreEvents && clt.Delegate != nil {
			clt.mutex.Unlock()
			clt.Delegate.OnEvent(evt.Type.String())
		} else {
			clt.mutex.Unlock()
		}

	case events.LocalChangeDetected, events.RemoteChangeDetected:
		data := evt.Data.(map[string]string)
		modifiedBy, ok := data["modifiedBy"]
		if !ok {
			modifiedBy = clt.DeviceID()
		}

		clt.mutex.Lock()
		if !clt.IgnoreEvents && clt.Delegate != nil {
			go clt.Delegate.OnChange(&Change{
				FolderID: data["folder"],
				ShortID:  modifiedBy,
				Action:   data["action"],
				Path:     data["path"],
				Time:     &Date{time: evt.Time},
			})
			clt.mutex.Unlock()
			clt.Delegate.OnEvent(evt.Type.String())
		} else {
			clt.mutex.Unlock()
		}

	case events.LocalIndexUpdated, events.DeviceDisconnected, events.ConfigSaved,
		events.ClusterConfigReceived, events.FolderResumed, events.FolderPaused:
		// Just deliver the event
		clt.mutex.Lock()
		if !clt.IgnoreEvents && clt.Delegate != nil {
			clt.mutex.Unlock()
			clt.Delegate.OnEvent(evt.Type.String())
		} else {
			clt.mutex.Unlock()
		}

	case events.DownloadProgress:
		clt.mutex.Lock()
		clt.downloadProgress = evt.Data.(map[string]map[string]*model.PullerProgress)
		if !clt.IgnoreEvents && clt.Delegate != nil {
			clt.mutex.Unlock()
			clt.Delegate.OnEvent(evt.Type.String())
		} else {
			clt.mutex.Unlock()
		}

	case events.RemoteDownloadProgress:
		peerData := evt.Data.(map[string]interface{})
		peerID := peerData["device"].(string)
		folderID := peerData["folder"].(string)
		state := peerData["state"].(map[string]int) // path: number of blocks downloaded

		clt.mutex.Lock()
		if _, ok := clt.uploadProgress[peerID]; !ok {
			clt.uploadProgress[peerID] = make(map[string]map[string]int)
		}

		if _, ok := clt.uploadProgress[peerID][folderID]; !ok {
			clt.uploadProgress[peerID][folderID] = make(map[string]int)
		}

		clt.uploadProgress[peerID][folderID] = state

		if !clt.IgnoreEvents && clt.Delegate != nil {
			clt.mutex.Unlock()
			clt.Delegate.OnEvent(evt.Type.String())
		} else {
			clt.mutex.Unlock()
		}

	case events.ItemFinished, events.ItemStarted:
		// Ignore these events
		break

	default:
		slog.Debug("event", "type", evt.Type.String(), "event", evt)
	}
}

// listenToEvents handles a run's events until the run ends
func (clt *Client) listenToEvents(r *engineRun, sub events.Subscription) {
	defer close(r.listenDone)
	defer sub.Unsubscribe()

	for {
		select {
		case <-r.ctx.Done():
			return
		case evt, ok := <-sub.C():
			// An event that arrives as the run ends is not handled, as the state it would change is reset
			if !ok || r.ctx.Err() != nil {
				return
			}
			clt.handleEvent(evt)
		}
	}
}

func (clt *Client) IsUploading() bool {
	clt.mutex.Lock()
	defer clt.mutex.Unlock()

	for devID, uploadsPerFolder := range clt.uploadProgress {
		// Skip peers that are not connected
		peer := clt.PeerWithID(devID)
		if peer == nil || !peer.IsConnected() {
			continue
		}

		for _, uploads := range uploadsPerFolder {
			if len(uploads) > 0 {
				return true
			}
		}
	}
	return false
}

func (clt *Client) UploadingToPeers() *ListOfStrings {
	clt.mutex.Lock()
	defer clt.mutex.Unlock()

	peers := make([]string, 0)
	for peerID, uploadsPerFolder := range clt.uploadProgress {
		// Skip peers that are not connected
		peer := clt.PeerWithID(peerID)
		if peer == nil || !peer.IsConnected() {
			continue
		}

		peerHasUploads := false
		for _, uploads := range uploadsPerFolder {
			if len(uploads) > 0 {
				peerHasUploads = true
				break
			}
		}
		if peerHasUploads {
			peers = append(peers, peerID)
		}
	}
	return List(peers)
}

func (clt *Client) UploadingFilesForPeerAndFolder(deviceID string, folderID string) *ListOfStrings {
	clt.mutex.Lock()
	defer clt.mutex.Unlock()

	// Skip peers that are not connected
	peer := clt.PeerWithID(deviceID)
	if peer == nil || !peer.IsConnected() {
		return &ListOfStrings{}
	}

	if uploads, ok := clt.uploadProgress[deviceID]; ok {
		if files, ok := uploads[folderID]; ok {
			return List(KeysOf(files))
		}
	}
	return &ListOfStrings{}
}

func (clt *Client) UploadingFoldersForPeer(deviceID string) *ListOfStrings {
	clt.mutex.Lock()
	defer clt.mutex.Unlock()

	// Skip peers that are not connected
	peer := clt.PeerWithID(deviceID)
	if peer == nil || !peer.IsConnected() {
		return &ListOfStrings{}
	}

	if uploads, ok := clt.uploadProgress[deviceID]; ok {
		return List(KeysOf(uploads))
	}
	return &ListOfStrings{}
}

func (clt *Client) GetLastPeerAddress(deviceID string) string {
	clt.mutex.Lock()
	defer clt.mutex.Unlock()

	if addr, ok := clt.connectedDeviceAddresses[deviceID]; ok {
		return addr
	}
	return ""
}

func (clt *Client) DownloadingFolders() *ListOfStrings {
	clt.mutex.Lock()
	defer clt.mutex.Unlock()
	return List(KeysOf(clt.downloadProgress))
}

func (clt *Client) DownloadingPathsForFolder(folderID string) *ListOfStrings {
	clt.mutex.Lock()
	defer clt.mutex.Unlock()

	if paths, ok := clt.downloadProgress[folderID]; ok {
		return List(KeysOf(paths))
	}
	return &ListOfStrings{}
}

func (clt *Client) IsDownloading() bool {
	clt.mutex.Lock()
	defer clt.mutex.Unlock()

	for _, isTransferring := range clt.foldersDownloading {
		if isTransferring {
			return true
		}
	}
	return false
}

func (clt *Client) HasLegacyDatabase() bool {
	if _, err := os.Lstat(locations.Get(locations.LegacyDatabase)); err != nil {
		// No old database
		return false
	}
	return true
}

func (clt *Client) HasMigratedLegacyDatabase() bool {
	if _, err := os.Lstat(clt.migratedLegacyDatabasePath()); err != nil {
		// No old database
		return false
	}
	return true
}

// Load loads the identity and the configuration, and opens and migrates the database, so that the configuration can be
// read and changed before the engine starts. This can take a while, and is done once; Start starts the engine.
func (clt *Client) Load(resetDeltaIdxs bool) error {
	clt.lifecycle.Lock()
	defer clt.lifecycle.Unlock()

	if clt.run.Load() != nil {
		return errors.New("client already loaded")
	}
	clt.startupProblem.Store("")

	// Some early chores
	osutil.MaximizeOpenFileLimit()

	// Print final locations
	slog.Info("config", "path", locations.Get(locations.ConfigFile))
	slog.Info("cert", "publicPath", locations.Get(locations.CertFile), "keyPath", locations.Get(locations.KeyFile))

	// Check if we are the only instance running, before reading or writing anything else
	if err := clt.acquireLock(); err != nil {
		return err
	}

	// Ensure that we have a certificate and key; only create them when neither exists
	cert, created, err := sthome.LoadIdentity(locations.Get(locations.CertFile), locations.Get(locations.KeyFile))
	if err != nil {
		if errors.Is(err, sthome.ErrIdentity) {
			clt.startupProblem.Store(StartupProblemDamagedIdentity)
		}
		return err
	}
	if created {
		slog.Warn("Created a new device identity", "path", locations.Get(locations.CertFile))
	}
	clt.cert = &cert

	// Load or create the config
	slog.Info("loading config file", "path", locations.Get(locations.ConfigFile))
	remoteManaged, err := loadRemoteManaged(remoteManagedPath())
	if err != nil {
		return err
	}
	clt.remoteManaged = remoteManaged

	clt.resetRunState()
	clt.resetDeltaIdxs = resetDeltaIdxs
	r, err := clt.prepareRun()
	if err != nil {
		return err
	}
	clt.run.Store(r)
	clt.setStatus(EngineStateLoaded, "", false)
	return nil
}

// Start starts the engine that Load prepared, and the streaming server
func (clt *Client) Start() error {
	clt.lifecycle.Lock()
	r := clt.run.Load()
	if r == nil || clt.EngineState() != EngineStateLoaded {
		clt.lifecycle.Unlock()
		return errors.New("call Client.Load first")
	}

	// These stay across restarts: the streaming server's port and URLs remain valid
	if clt.Measurements == nil {
		clt.Measurements = NewMeasurements(clt)
	}
	if clt.Server == nil {
		server, err := NewServer(clt.Measurements)
		if err != nil {
			clt.lifecycle.Unlock()
			return err
		}
		server.client = clt
		clt.Server = server
	}

	defer clt.lifecycle.Unlock()
	if err := clt.startRun(r); err != nil {
		clt.setStatus(EngineStateFailed, err.Error(), true)
		return err
	}
	// The app goes on starting itself; it hears of later changes
	clt.setStatus(EngineStateRunning, "", false)
	return nil
}

func (clt *Client) PerformMaintenanceBlocking() error {
	r := clt.currentRun()
	if r == nil || r.internals.Load() == nil {
		return ErrStillLoading
	}
	if r.isClosed() {
		return clt.errNotRunning()
	}
	// The database service drops the request when it has stopped (or is busy), and then never answers
	select {
	case err := <-r.app.StartMaintenance():
		return err
	case <-r.ctx.Done():
		return clt.errNotRunning()
	}
}

func (clt *Client) LastMaintenanceTime() *Date {
	r := clt.currentRun()
	if r == nil || r.internals.Load() == nil {
		return nil
	}

	t := r.app.LastMaintenanceTime()
	if t.IsZero() {
		return nil
	}
	return &Date{time: t}
}

func (clt *Client) SetFSWatchingEnabledForAllFolders(enabled bool) error {
	return clt.changeConfiguration(func(cfg *config.Configuration) {
		for _, fc := range cfg.Folders {
			fc.FSWatcherEnabled = enabled
			cfg.SetFolder(fc)
		}
	})
}

// loadEngineConfig loads the configuration, or creates one when the file doesn't exist, and applies the app's settings.
// A file that exists but can't be used is never replaced: the error wraps sthome.ErrConfiguration,
// sthome.ErrNewerConfiguration or sthome.ErrUnreadableConfiguration. The settings are applied to a copy first, and the
// file is written only when they changed it. The caller serves the returned configuration.
//
// On iOS the app's settings are applied at every start: there is no web GUI or REST API to change them, and the app
// depends on them. On macOS they are only the defaults of a new configuration, so that changes made in the web GUI or
// through the REST API last, and an existing Syncthing configuration is used as it is. On both, usage reporting stays
// off and remote-managed folders stay paused.
func loadEngineConfig(cfgFile string, cert tls.Certificate, logger events.Logger, filesPath string,
	remoteManaged *remoteManagedSet, ios bool) (config.Wrapper, error) {
	cfg, _, err := sthome.LoadConfig(cfgFile, cert, logger, false, func(conf *config.Configuration) {
		conf.GUI.Enabled = false // Don't need the web UI, we have our own :-)
		applyAppDefaults(conf, ios)
	})
	if err != nil {
		return nil, err
	}

	// A configuration with no path is never saved, also not when it stops being served
	myID := protocol.NewDeviceID(cert.Certificate[0])
	staging := config.Wrap("", cfg.RawCopy(), myID, events.NoopLogger)
	stagingCtx, stopStaging := context.WithCancel(context.Background())
	stagingDone := make(chan struct{})
	go func() {
		defer close(stagingDone)
		_ = staging.Serve(stagingCtx)
	}()
	defer func() {
		stopStaging()
		<-stagingDone
	}()

	waiter, err := staging.Modify(func(conf *config.Configuration) {
		if ios {
			applyAppDefaults(conf, ios)
			applyIOSFolderSettings(conf, filesPath)
		}

		// This engine is a fork, which must not send usage and failure reports to Syncthing's servers as an unknown
		// build. (Crash uploads are the work of Syncthing's monitor process, which the app doesn't have.)
		conf.Options.URAccepted = -1

		// Remote-managed folders stay paused, even if the file was edited to resume them
		remoteManaged.enforce(conf)
	})
	if err != nil {
		return nil, err
	}
	waiter.Wait()

	// Write the file only when something changed
	if applied := staging.RawCopy(); !reflect.DeepEqual(applied, cfg.RawCopy()) {
		cfg = config.Wrap(cfgFile, applied, myID, logger)
		if err := cfg.Save(); err != nil {
			return nil, err
		}
	}

	// From now on, no change may resume them
	cfg.Subscribe(remoteManaged)
	return cfg, nil
}

// applyAppDefaults sets the options and folder defaults the app wants: on iOS at every start, on macOS for a new
// configuration only. On macOS, folders keep Syncthing's default scan progress, which the web GUI shows.
func applyAppDefaults(conf *config.Configuration, ios bool) {
	conf.Options.CREnabled = false               // No crash reporting for now
	conf.Options.URAccepted = -1                 // No usage reporting for now
	conf.Options.ProgressUpdateIntervalS = 1     // We want to update the user often, it improves the experience and is worth the compute cost
	conf.Options.CRURL = ""                      // No crash reporting for now
	conf.Options.URURL = ""                      // No usage reporting for now
	conf.Options.ReleasesURL = ""                // The app updates itself, the engine doesn't
	conf.Defaults.Folder.RescanIntervalS = 3600  // Rescan every hour by default
	conf.Options.RelayReconnectIntervalM = 1     // Set this to one minute (from the default 10) because on mobile networks this is more often necessary
	conf.Defaults.Folder.FSWatcherEnabled = !ios // Enable watching by default but not on iOS
	conf.Defaults.Folder.BlockIndexing = false   // Save space by default
	if ios {
		conf.Defaults.Folder.IgnorePerms = true // iOS doesn't expose permissions to users
	}
}

// applyIOSFolderSettings keeps the folders in the app's container where they belong on iOS, resumes their interrupted
// moves to the shared container, and turns off their scan progress
func applyIOSFolderSettings(conf *config.Configuration, filesPath string) {
	// The absolute path to the app's container that has the synchronized folders changes on each run. Therefore we
	// re-set the absolute folder path here to [app documents directory]/[folder ID] if we don't have a folder marker in
	// the old location but do have one in the new.
	for _, folderConfig := range conf.Folders {
		if isNativeFilesystem(folderConfig.FilesystemType) {
			standardPath := path.Join(filesPath, folderConfig.ID)
			if folderConfig.Path != standardPath && folderConfig.Path != managedFolderPath(folderConfig.ID) {
				slog.Warn("configured folder path differs from expected path", "configured", folderConfig.Path, "expected", standardPath)

				oldMarkerPath := path.Join(folderConfig.Path, folderConfig.MarkerName)
				if _, err := os.Stat(oldMarkerPath); errors.Is(err, os.ErrNotExist) {
					newMarkerPath := path.Join(standardPath, folderConfig.MarkerName)
					if _, err := os.Stat(newMarkerPath); errors.Is(err, os.ErrNotExist) {
						slog.Warn("marker does not exist at either old or new location, not changing anything", "oldMarkerPath", oldMarkerPath, "newMarkerPath", newMarkerPath)
					} else {
						slog.Warn("marker does not exist at old location and exists at new location, resetting standard path", "oldMarkerPath", oldMarkerPath, "newMarkerPath", newMarkerPath, "standardPath", standardPath)
						folderConfig.Path = standardPath
						conf.SetFolder(folderConfig)
					}
				}
			}

			if recoverManagedFolder(&folderConfig, filesPath) {
				conf.SetFolder(folderConfig)
			}
		}

		// The app currently doesn't do anything with scan progress update events, and enabling progress reports apparently
		// leads to some extra memory usage (see https://github.com/syncthing/syncthing/pull/10221), so disable it.
		if folderConfig.ScanProgressIntervalS >= 0 {
			folderConfig.ScanProgressIntervalS = -1
			conf.SetFolder(folderConfig)
		}
	}
}

/** Returns our node's device ID */
func (clt *Client) DeviceID() string {
	return protocol.NewDeviceID(clt.cert.Certificate[0]).String()
}

/** Returns our node's short device ID */
func (clt *Client) ShortDeviceID() string {
	return protocol.NewDeviceID(clt.cert.Certificate[0]).Short().String()
}

func (clt *Client) deviceID() protocol.DeviceID {
	return protocol.NewDeviceID(clt.cert.Certificate[0])
}

func (clt *Client) Folders() *ListOfStrings {
	if clt.cfg() == nil {
		return nil
	}

	return List(Map(clt.cfg().FolderList(), func(folder config.FolderConfiguration) string {
		return folder.ID
	}))
}

func (clt *Client) FolderWithID(id string) *Folder {
	if clt.cfg() == nil {
		return nil
	}

	fi, ok := clt.cfg().Folders()[id]
	if !ok {
		return nil // Folder with this ID does not exist
	}

	return &Folder{
		client:   clt,
		FolderID: fi.ID,
	}
}

func (clt *Client) ConnectedPeerCount() int {
	if clt.internals() == nil {
		return 0
	}

	if clt.cfg() == nil || clt.internals() == nil {
		return 0
	}

	devIDs := clt.cfg().Devices()
	connected := 0
	for devID := range devIDs {
		if devID == clt.deviceID() {
			continue
		}
		if clt.internals().IsConnectedTo(devID) {
			connected++
		}
	}
	return connected
}

func (clt *Client) Peers() *ListOfStrings {
	if clt.cfg() == nil {
		return nil
	}

	return List(Map(clt.cfg().DeviceList(), func(device config.DeviceConfiguration) string {
		return device.DeviceID.String()
	}))
}

func (clt *Client) PeerWithID(deviceID string) *Peer {
	devID, err := protocol.DeviceIDFromString(deviceID)

	if err != nil {
		return nil
	}

	return &Peer{
		client:   clt,
		deviceID: devID,
	}
}

func (clt *Client) PeerWithShortID(shortID string) *Peer {
	for _, dc := range clt.cfg().DeviceList() {
		if dc.DeviceID.Short().String() == shortID {
			return &Peer{
				client:   clt,
				deviceID: dc.DeviceID,
			}
		}
	}
	return nil
}

// This function sets all the listed device to the desired pause state, and all other devices to the opposite state.
func (clt *Client) SetDevicesPaused(peers *ListOfStrings, pause bool) error {
	ids := peers.data

	return clt.changeConfiguration(func(cfg *config.Configuration) {
		for _, dc := range cfg.Devices {
			listed := slices.ContainsFunc(ids, func(v string) bool {
				did, err := protocol.DeviceIDFromString(v)
				return err == nil && dc.DeviceID.Equals(did)
			})

			shouldPause := listed
			if !pause {
				shouldPause = !shouldPause
			}

			if dc.Paused != shouldPause {
				dc.Paused = shouldPause
				cfg.SetDevice(dc)
			}
		}
	})
}

func (clt *Client) changeConfiguration(block config.ModifyFunction) error {
	r := clt.currentRun()
	if r == nil {
		return ErrStillLoading
	}
	// A configuration that is no longer served would never apply the change
	r.writes.RLock()
	defer r.writes.RUnlock()
	if r.closed {
		return clt.errNotRunning()
	}

	waiter, err := r.config.Modify(block)
	if err != nil {
		return err
	}
	waiter.Wait()

	return r.config.Save()
}

func (clt *Client) AddPeer(deviceID string) error {
	addedDevice, err := protocol.DeviceIDFromString(deviceID)
	if err != nil {
		return err
	}

	deviceConfig := clt.cfg().DefaultDevice()
	deviceConfig.DeviceID = addedDevice

	return clt.changeConfiguration(func(cfg *config.Configuration) {
		cfg.SetDevice(deviceConfig)
	})
}

func (clt *Client) AddSpecialFolder(folderID string, fsType string, folderPath string, folderType string) error {
	if clt.internals() == nil {
		return ErrStillLoading
	}

	ft := config.FolderTypeSendReceive
	ft.UnmarshalText([]byte(folderType))

	folderConfig := clt.cfg().DefaultFolder()
	folderConfig.ID = folderID
	folderConfig.FilesystemType = config.FilesystemType(fsType)
	folderConfig.Type = ft
	folderConfig.Path = folderPath
	folderConfig.Label = folderID
	folderConfig.Paused = false

	// Add to configuration
	err := clt.changeConfiguration(func(cfg *config.Configuration) {
		cfg.SetFolder(folderConfig)
	})
	if err != nil {
		return err
	}

	return nil
}

// Leave path empty to add folder at default location
func (clt *Client) AddFolder(folderID string, folderPath string, createAsOnDemand bool, createAsReceiveEncrypted bool) error {
	if clt.internals() == nil {
		return ErrStillLoading
	}

	if createAsReceiveEncrypted && createAsOnDemand {
		// This is not allowed
		panic("invalid combination of createAsReceiveEncrypted and createAsOnDemand specified")
	}

	folderConfig := clt.cfg().DefaultFolder()
	folderConfig.ID = folderID
	folderConfig.Label = folderID
	if len(folderPath) == 0 {
		defaultPath, err := clt.defaultFolderPath(folderID)
		if err != nil {
			return err
		}
		folderConfig.Path = defaultPath
	} else {
		folderConfig.Path = folderPath
	}
	folderConfig.Paused = false

	if createAsReceiveEncrypted {
		folderConfig.Type = config.FolderTypeReceiveEncrypted
	}

	// Add to configuration
	err := clt.changeConfiguration(func(cfg *config.Configuration) {
		cfg.SetFolder(folderConfig)
	})
	if err != nil {
		return err
	}

	if !createAsReceiveEncrypted {
		// Set default ignores for on-demand sync
		if createAsOnDemand {
			return clt.internals().SetIgnores(folderID, []string{"*"})
		} else {
			// Create empty .stignore anyway because there may be an old one lingering around
			return clt.internals().SetIgnores(folderID, []string{})
		}
	} else {
		return nil
	}
}

func (clt *Client) SetNATEnabled(enabled bool) error {
	return clt.changeConfiguration(func(cfg *config.Configuration) {
		cfg.Options.NATEnabled = enabled
	})
}

func (clt *Client) IsNATEnabled() bool {
	return clt.cfg().Options().NATEnabled
}

func (clt *Client) SetSTUNEnabled(enabled bool) error {
	return clt.changeConfiguration(func(cfg *config.Configuration) {
		if enabled {
			cfg.Options.StunKeepaliveMinS = 20
		} else {
			cfg.Options.StunKeepaliveMinS = 0
		}
	})
}

func (clt *Client) IsSTUNEnabled() bool {
	return clt.cfg().Options().StunKeepaliveMinS > 0
}

func (clt *Client) SetRelaysEnabled(enabled bool) error {
	return clt.changeConfiguration(func(cfg *config.Configuration) {
		cfg.Options.RelaysEnabled = enabled
	})
}

func (clt *Client) IsRelaysEnabled() bool {
	return clt.cfg().Options().RelaysEnabled
}

func (clt *Client) SetLocalAnnounceEnabled(enabled bool) error {
	return clt.changeConfiguration(func(cfg *config.Configuration) {
		cfg.Options.LocalAnnEnabled = enabled
	})
}

func (clt *Client) IsLocalAnnounceEnabled() bool {
	return clt.cfg().Options().LocalAnnEnabled
}

func (clt *Client) SetGlobalAnnounceEnabled(enabled bool) error {
	return clt.changeConfiguration(func(cfg *config.Configuration) {
		cfg.Options.GlobalAnnEnabled = enabled
	})
}

func (clt *Client) IsGlobalAnnounceEnabled() bool {
	return clt.cfg().Options().GlobalAnnEnabled
}

func (clt *Client) SetAnnounceLANAddresses(enabled bool) error {
	return clt.changeConfiguration(func(cfg *config.Configuration) {
		cfg.Options.AnnounceLANAddresses = enabled
	})
}

func (clt *Client) IsAnnounceLANAddressesEnabled() bool {
	return clt.cfg().Options().AnnounceLANAddresses
}

func (clt *Client) IsBandwidthLimitedInLAN() bool {
	return clt.cfg().Options().LimitBandwidthInLan
}

func (clt *Client) SetBandwidthLimitedInLAN(enabled bool) error {
	return clt.changeConfiguration(func(cfg *config.Configuration) {
		cfg.Options.LimitBandwidthInLan = enabled
	})
}

// Syncthing stores bandwidth limits in KiB/s (despite the 'Kbps' field names), the app shows Mbit/s
const kibPerSecondPerMbitPerSecond = 1_000_000.0 / 8.0 / 1024.0

// Rounds to whole Mbit/s, but never turns a limit into zero (which would read as 'no limit')
func kibPerSecondToMbitsPerSecond(kib int) int {
	if kib <= 0 {
		return 0
	}
	return max(1, int(math.Round(float64(kib)/kibPerSecondPerMbitPerSecond)))
}

func mbitsPerSecondToKiBPerSecond(mbits int) int {
	if mbits <= 0 {
		return 0
	}
	return int(math.Round(float64(mbits) * kibPerSecondPerMbitPerSecond))
}

func (clt *Client) GetBandwidthLimitUpMbitsPerSec() int {
	return kibPerSecondToMbitsPerSecond(clt.cfg().Options().MaxSendKbps)
}

func (clt *Client) GetBandwidthLimitDownMbitsPerSec() int {
	return kibPerSecondToMbitsPerSecond(clt.cfg().Options().MaxRecvKbps)
}

func (clt *Client) SetBandwidthLimitsMbitsPerSec(down int, up int) error {
	down = max(0, down)
	up = max(0, up)

	return clt.changeConfiguration(func(cfg *config.Configuration) {
		// Only rewrite a limit that changed, so a limit set elsewhere that isn't a whole number of Mbit/s
		// survives a change to the other one
		if down != kibPerSecondToMbitsPerSecond(cfg.Options.MaxRecvKbps) {
			cfg.Options.MaxRecvKbps = mbitsPerSecondToKiBPerSecond(down)
		}
		if up != kibPerSecondToMbitsPerSecond(cfg.Options.MaxSendKbps) {
			cfg.Options.MaxSendKbps = mbitsPerSecondToKiBPerSecond(up)
		}
	})
}

type Progress struct {
	BytesTotal int64
	BytesDone  int64
	FilesTotal int64
	Percentage float32
}

func (clt *Client) UploadProgressForPeerFolderPath(deviceID string, folderID string, path string) *Progress {
	clt.mutex.Lock()
	defer clt.mutex.Unlock()

	if uploads, ok := clt.uploadProgress[deviceID]; ok {
		if files, ok := uploads[folderID]; ok {
			if blocksTransferred, ok := files[path]; ok {
				info, ok, err := clt.internals().GlobalFileInfo(folderID, path)
				if !ok || err != nil {
					return nil
				}

				bytesTotal := info.Size
				if bytesTotal == 0 {
					return nil
				}
				bytesDone := min(bytesTotal, int64(blocksTransferred)*int64(info.BlockSize()))

				return &Progress{
					BytesTotal: bytesTotal,
					BytesDone:  bytesDone,
					FilesTotal: 1,
					Percentage: float32(float64(bytesDone) / float64(bytesTotal)),
				}
			}
		}
	}
	return nil
}

func (clt *Client) GetTotalUploadProgress() *Progress {
	clt.mutex.Lock()
	defer clt.mutex.Unlock()

	if clt.uploadProgress == nil {
		return nil
	}

	var totalBytes int64 = 0
	var transferredBytes int64 = 0
	var totalFiles int64 = 0

	for _, info := range clt.uploadProgress {
		for folderID, finfo := range info {
			for path, blocksTransferred := range finfo {
				info, ok, err := clt.internals().GlobalFileInfo(folderID, path)
				if !ok || err != nil {
					continue
				}
				totalBytes += info.Size
				bytesDone := min(info.Size, int64(blocksTransferred)*int64(info.BlockSize()))
				transferredBytes += bytesDone
				totalFiles += 1
			}
		}
	}

	if totalBytes == 0 {
		return nil
	}

	return &Progress{
		BytesTotal: totalBytes,
		BytesDone:  transferredBytes,
		FilesTotal: totalFiles,
		Percentage: float32(float64(transferredBytes) / float64(totalBytes)),
	}
}

func (clt *Client) GetTotalDownloadProgress() *Progress {
	clt.mutex.Lock()
	defer clt.mutex.Unlock()

	if clt.downloadProgress == nil {
		return nil
	}

	var doneBytes, totalBytes int64
	doneBytes = 0
	totalBytes = 0
	fileCount := 0
	for _, info := range clt.downloadProgress {
		for _, fileInfo := range info {
			doneBytes += fileInfo.BytesDone
			totalBytes += fileInfo.BytesTotal
			fileCount++
		}
	}

	if totalBytes == 0 {
		return nil
	}

	return &Progress{
		BytesTotal: totalBytes,
		BytesDone:  doneBytes,
		FilesTotal: int64(fileCount),
		Percentage: float32(doneBytes) / float32(totalBytes),
	}
}

func (clt *Client) GetDownloadProgressForFile(path string, folder string) *Progress {
	clt.mutex.Lock()
	defer clt.mutex.Unlock()

	if clt.downloadProgress == nil {
		return nil
	}

	if folderInfo, ok := clt.downloadProgress[folder]; ok {
		if fileInfo, ok := folderInfo[path]; ok {
			return &Progress{
				BytesTotal: fileInfo.BytesTotal,
				BytesDone:  fileInfo.BytesDone,
				FilesTotal: 1,
				Percentage: float32(fileInfo.BytesDone) / float32(fileInfo.BytesTotal),
			}
		}
	}

	return nil
}

func (clt *Client) GetName() (string, error) {
	devID := clt.deviceID()

	selfConfig, ok := clt.cfg().Devices()[devID]
	if !ok {
		return "", errors.New("cannot find myself")
	}
	return selfConfig.Name, nil
}

func (clt *Client) SetName(name string) error {
	devID := clt.deviceID()

	selfConfig, ok := clt.cfg().Devices()[devID]
	if !ok {
		return errors.New("cannot find myself")
	}
	selfConfig.Name = name

	return clt.changeConfiguration(func(cfg *config.Configuration) {
		cfg.SetDevice(selfConfig)
	})
}

func (clt *Client) Statistics() (*FolderStats, error) {
	if clt.internals() == nil {
		return nil, ErrStillLoading
	}

	globalTotal := FolderCounts{}
	localTotal := FolderCounts{}

	for _, folder := range clt.cfg().FolderList() {
		globalFolderSize, err := clt.internals().GlobalSize(folder.ID)
		if err != nil {
			return nil, err
		}
		localFolderSize, err := clt.internals().LocalSize(folder.ID)
		if err != nil {
			return nil, err
		}

		globalTotal.add(newFolderCounts(globalFolderSize))
		localTotal.add(newFolderCounts(localFolderSize))
	}

	return &FolderStats{
		Global: &globalTotal,
		Local:  &localTotal,
	}, nil
}

type SearchResultDelegate interface {
	Result(entry *Entry)
	IsCancelled() bool
}

// Zip interleaves the iterator value with the error. The iteration ends
// after a non-nil error.
func zipError[T any](it iter.Seq[T], errFn func() error) iter.Seq2[T, error] {
	return func(yield func(T, error) bool) {
		for v := range it {
			if !yield(v, nil) {
				break
			}
		}
		if err := errFn(); err != nil {
			var zero T
			yield(zero, err)
		}
	}
}

/*
* Search for files by name in the global index. Calls back the delegate up to `maxResults` times with a result in no
particular order, unless/until the delegate returns true from IsCancelled. Set maxResults to <=0 to collect all results.
*/
func (clt *Client) Search(text string, delegate SearchResultDelegate, maxResults int, folderID string, prefix string) error {
	if clt.internals() == nil {
		return ErrStillLoading
	}

	text = strings.ToLower(text)
	resultCount := 0

	for _, folder := range clt.cfg().FolderList() {
		if delegate.IsCancelled() {
			return nil
		}
		if folderID != "" && folder.ID != folderID {
			continue
		}

		folderObject := Folder{
			client:   clt,
			FolderID: folder.ID,
		}

		for f, err := range zipError(clt.internals().AllGlobalFiles(folder.ID)) {
			if err != nil {
				return err
			}

			if delegate.IsCancelled() {
				return nil
			}

			// Check prefix
			if !strings.HasPrefix(f.Name, prefix) {
				continue
			}

			pathParts := strings.Split(f.Name, "/")
			lowerFileName := strings.ToLower(pathParts[len(pathParts)-1])

			if !f.Deleted && strings.Contains(lowerFileName, text) {
				entry, err := folderObject.GetFileInformation(f.Name)
				if err == nil {
					resultCount += 1
					delegate.Result(entry)
					if maxResults > 0 && resultCount >= maxResults {
						return nil
					}
				}
			}
		}
	}
	return nil
}

func (clt *Client) GetEnoughConnections() int {
	return clt.cfg().Options().ConnectionLimitEnough
}

func (clt *Client) SetEnoughConnections(enough int) error {
	return clt.changeConfiguration(func(cfg *config.Configuration) {
		cfg.Options.ConnectionLimitEnough = enough
	})
}

// To make Syncthing 'not listening' we set the listen address to localhost. Setting it to empty will not do much, as
// the default will be reloaded (which is 'default', and which means 'listen')
const (
	NoListenAddress = "tcp://127.0.0.1:22000"
)

func (clt *Client) IsListening() bool {
	addrs := clt.cfg().Options().ListenAddresses()
	return len(addrs) > 0 && addrs[0] != NoListenAddress
}

func (clt *Client) SetListening(listening bool) error {
	return clt.changeConfiguration(func(cfg *config.Configuration) {
		if listening {
			cfg.Options.RawListenAddresses = []string{"default"}
		} else {
			cfg.Options.RawListenAddresses = []string{NoListenAddress}
		}
	})
}

/** Returns true if any device is currently offering the folder with the specified ID as encrypted folder. */
func (clt *Client) IsPendingFolderOfferedReceiveEncrypted(folderID string) (isOffered bool, err error) {
	if clt.internals() == nil {
		return false, ErrStillLoading
	}

	var anyOffers = false
	var anyOfferReceiveEncrypted = false
	peers := clt.cfg().DeviceList()
	for _, peer := range peers {
		peerPendingFolderIDs, err := clt.internals().PendingFolders(peer.DeviceID)
		if err != nil {
			continue
		}

		if peerPendingFolder, ok := peerPendingFolderIDs[folderID]; ok {
			for _, offeringPeer := range peerPendingFolder.OfferedBy {
				anyOffers = true
				if offeringPeer.ReceiveEncrypted {
					anyOfferReceiveEncrypted = true
					break
				}
			}
		}

		if anyOfferReceiveEncrypted {
			break
		}
	}

	return anyOfferReceiveEncrypted && anyOffers, nil
}

func (clt *Client) pendingFolders() (map[string][]string, error) {
	if clt.internals() == nil {
		return nil, ErrStillLoading
	}

	peers := clt.cfg().DeviceList()
	fids := map[string][]string{}
	for _, peer := range peers {
		peerFids, err := clt.internals().PendingFolders(peer.DeviceID)
		if err != nil {
			return nil, err
		}
		for peerFid := range peerFids {
			existing := fids[peerFid]
			existing = append(existing, peer.DeviceID.String())
			fids[peerFid] = existing
		}
	}

	return fids, nil
}

func (clt *Client) PendingFolderIDs() (*ListOfStrings, error) {
	if clt.internals() == nil {
		return nil, ErrStillLoading
	}

	pfs, err := clt.pendingFolders()
	if err != nil {
		return nil, err
	}
	return List(KeysOf(pfs)), nil
}

func (clt *Client) DevicesPendingFolder(folderID string) (*ListOfStrings, error) {
	if clt.internals() == nil {
		return nil, ErrStillLoading
	}

	pfs, err := clt.pendingFolders()
	if err != nil {
		return nil, err
	}

	if devs, ok := pfs[folderID]; ok {
		return List(devs), nil
	}
	return List([]string{}), nil
}

func (clt *Client) SetReconnectIntervalS(secs int) error {
	slog.Info("set reconnect interval", "interval", secs)
	return clt.changeConfiguration(func(cfg *config.Configuration) {
		cfg.Options.ReconnectIntervalS = secs
	})
}

func (clt *Client) ListenAddresses() *ListOfStrings {
	return List(clt.cfg().Options().RawListenAddresses)
}

func (clt *Client) SetListenAddresses(addrs *ListOfStrings) error {
	return clt.changeConfiguration(func(cfg *config.Configuration) {
		cfg.Options.RawListenAddresses = addrs.data
	})
}

func (clt *Client) DiscoveryAddresses() *ListOfStrings {
	return List(clt.cfg().Options().RawGlobalAnnServers)
}

func (clt *Client) SetDiscoveryAddresses(addrs *ListOfStrings) error {
	return clt.changeConfiguration(func(cfg *config.Configuration) {
		cfg.Options.RawGlobalAnnServers = addrs.data

		// When setting an empty list of servers the expectation is that discovery stops. However on restart and reading
		// the config file, the default value of this field will be filled in (as there are no `globalAnnounceServer`
		// elements in the XML). Thus we also disable it here. As there is a separate toggle to enable it, we don't do
		// that here when a nonempty list is set.
		if len(addrs.data) == 0 {
			cfg.Options.GlobalAnnEnabled = false
		}
	})
}

func (clt *Client) StunAddresses() *ListOfStrings {
	if clt.cfg().Options().StunKeepaliveMinS < 1 {
		return List(make([]string, 0))
	}
	return List(clt.cfg().Options().RawStunServers)
}

func (clt *Client) SetStunAddresses(addrs *ListOfStrings) error {
	return clt.changeConfiguration(func(cfg *config.Configuration) {
		cfg.Options.RawStunServers = addrs.data

		// When setting an empty list of servers the expectation is that STUN stops. However on restart and reading
		// the config file, the default value of this field will be filled in (as there are no `stunServer` elements in
		// the XML). Thus we also disable it here. As there is a separate toggle to enable it, we don't do that here when
		// a nonempty list is set.
		if len(addrs.data) == 0 {
			cfg.Options.StunKeepaliveMinS = 0 // Disable
		}
	})
}

func (clt *Client) IsDiskSpaceSufficient() bool {
	if minFree := clt.cfg().Options().MinHomeDiskFree; minFree.Value > 0 {
		dbPath := locations.Get(locations.Database)
		if usage, err := fs.NewFilesystem(fs.FilesystemTypeBasic, dbPath).Usage("."); err == nil {
			if err = config.CheckFreeSpace(minFree, usage); err != nil {
				return false
			}
		}
	}
	return true
}

func IsValidDeviceID(devID string) bool {
	_, err := protocol.DeviceIDFromString(devID)
	return err == nil
}

func Version() string {
	return build.Version
}

func LogInfo(message string) {
	slog.Info("[App] " + message)
}

func LogWarn(message string) {
	slog.Warn("[App] " + message)
}

func ShortDeviceID(devID string) string {
	did, err := protocol.DeviceIDFromString(devID)
	if err != nil {
		return "(invalid device ID)"
	}
	return did.Short().String()
}

func (c *Client) migratedLegacyDatabasePath() string {
	return locations.Get(locations.LegacyDatabase) + "-migrated"
}

func (c *Client) ClearMigratedLegacyDatabase() error {
	if err := c.acquireLock(); err != nil {
		return err
	}
	dbPath := c.migratedLegacyDatabasePath()
	slog.Warn("Removing v1 index", "path", dbPath)
	return os.RemoveAll(dbPath)
}

func (c *Client) ClearLegacyDatabase() error {
	if err := c.acquireLock(); err != nil {
		return err
	}
	dbPath := locations.Get(locations.LegacyDatabase)
	slog.Warn("Removing v1 index", "path", dbPath)
	return os.RemoveAll(dbPath)
}

func (c *Client) ClearDatabase() error {
	if err := c.acquireLock(); err != nil {
		return err
	}
	dbPath := locations.Get(locations.Database)
	slog.Warn("Removing v2 index", "path", dbPath)
	return os.RemoveAll(dbPath)
}

func (c *Client) WriteSupportBundle(path string, appInfo []byte) error {
	out, err := os.Create(path)
	if err != nil {
		return err
	}
	defer out.Close()
	err = c.generateSupportBundle(out, appInfo)
	if err != nil {
		return err
	}
	return nil
}

func (c *Client) generateSupportBundle(writer io.Writer, appInfo []byte) error {
	zipWriter := zip.NewWriter(writer)
	defer zipWriter.Close() // We might close twice but that's alright

	// Write app support info
	if len(appInfo) > 0 {
		appInfo = []byte(redactLog(string(appInfo)))
		appInfoWriter, err := zipWriter.CreateHeader(&zip.FileHeader{
			Name:     "info-app.json",
			Modified: time.Now(),
			Method:   zip.Deflate,
		})
		if err != nil {
			return err
		}
		_, err = appInfoWriter.Write(appInfo)
		if err != nil {
			return err
		}
	}

	// Write log tail, and the warnings and errors, which debug output can push out of the tail
	logFiles := []struct {
		name  string
		lines []syncthing.LogLine
	}{
		{"log-tail.txt", syncthing.LogLines(time.Time{})},
		{"log-warnings.txt", syncthing.LogWarnings(time.Time{})},
	}
	for _, logFile := range logFiles {
		logFileWriter, err := zipWriter.CreateHeader(&zip.FileHeader{
			Name:     logFile.name,
			Modified: time.Now(),
			Method:   zip.Deflate,
		})
		if err != nil {
			return err
		}
		if err := writeLogLines(logFileWriter, logFile.lines); err != nil {
			return err
		}
	}

	// Write a general info JSON
	infoJson := make(map[string]any)
	infoJson["version"] = build.Version
	infoJson["shortDeviceID"] = c.ShortDeviceID()
	infoJson["isUsingCustomConfiguration"] = c.IsUsingCustomConfiguration
	infoJson["isIgnoringEvents"] = c.IgnoreEvents
	infoJson["extraneousIgnored"] = c.extraneousIgnored
	infoJson["hasLegacyDatabase"] = c.HasLegacyDatabase()
	infoJson["hasMigratedLegacyDatabase"] = c.HasMigratedLegacyDatabase()
	infoJson["connectedPeerCount"] = c.ConnectedPeerCount()
	infoJson["bundleGeneratedAt"] = time.Now().Format(time.RFC3339)
	infoJson["redactedConfig"] = c.getRedactedConfigFile()
	infoJson["numGoroutines"] = runtime.NumGoroutine()
	infoJson["numCPUs"] = runtime.NumCPU()

	var rlimit syscall.Rlimit
	if err := syscall.Getrlimit(syscall.RLIMIT_NOFILE, &rlimit); err == nil {
		infoJson["currentFileDescriptorLimit"] = rlimit.Cur
		infoJson["maxFileDescriptorLimit"] = rlimit.Max
	}

	var m runtime.MemStats
	runtime.ReadMemStats(&m)
	infoJson["allocatedHeapBytes"] = m.Alloc
	infoJson["sysMemoryBytes"] = m.Sys - m.HeapReleased

	jsonData, err := json.MarshalIndent(infoJson, "", "\t")
	if err != nil {
		return err
	}
	jsonData = []byte(redactLog(string(jsonData)))
	jsonWriter, err := zipWriter.CreateHeader(&zip.FileHeader{
		Name:     "info.json",
		Modified: time.Now(),
		Method:   zip.Deflate,
	})
	if err != nil {
		return err
	}
	_, err = jsonWriter.Write(jsonData)
	if err != nil {
		return err
	}

	// Go profiles
	profileNames := []string{"goroutine", "block", "mutex", "heap", "goroutineleak"}
	for _, profileName := range profileNames {
		if p := pprof.Lookup(profileName); p != nil {
			goroutineWriter, err := zipWriter.CreateHeader(&zip.FileHeader{
				Name:     profileName + ".pprof",
				Modified: time.Now(),
				Method:   zip.Deflate,
			})
			if err != nil {
				return err
			}
			_ = p.WriteTo(goroutineWriter, 0)
		}
	}

	if err = zipWriter.Close(); err != nil {
		return err
	}
	return nil
}

func (c *Client) getRedactedConfigFile() config.Configuration {
	rawConf := c.cfg().RawCopy()
	rawConf.GUI.APIKey = "•••"
	if rawConf.GUI.Password != "" {
		rawConf.GUI.Password = "•••"
	}
	if rawConf.GUI.User != "" {
		rawConf.GUI.User = "•••"
	}

	for fi, f := range rawConf.Folders {
		for di, d := range f.Devices {
			if len(d.EncryptionPassword) > 0 {
				d.EncryptionPassword = "•••"
				rawConf.Folders[fi].Devices[di] = d
			}
		}
	}

	return rawConf
}

func (clt *Client) IsNetworkTrafficLowPriority() bool {
	return clt.cfg().Options().TrafficClass == 4
}

func (clt *Client) SetNetworkTrafficLowPriority(lowPrio bool) error {
	return clt.changeConfiguration(func(cfg *config.Configuration) {
		if lowPrio {
			// This sets Syncthing traffic as DSCP 'lower effort'. The DSCP value for lower effort traffic is 1, but
			// the trafficClass setting is the full ToS / Traffic Class byte in IP, so we need to shift the desired
			// DSCP value to the left by 2 bits to account for the two least significant ECN (Explicit Congestion
			// Notification) bits: 1<<2 = 4.
			cfg.Options.TrafficClass = 4
		} else {
			cfg.Options.TrafficClass = 0
		}
	})
}

/** Returns the free disk space on the volume where the database is stored */
func GetFreeDiskSpaceMegaBytes() int {
	dbPath := locations.Get(locations.Database)
	if usage, err := fs.NewFilesystem(fs.FilesystemTypeBasic, dbPath).Usage("."); err == nil {
		return int(usage.Free / 1024 / 1024)
	}
	return 0
}

/** Returns the total size of the volume where the database is stored */
func GetTotalDiskSpaceMegaBytes() int {
	dbPath := locations.Get(locations.Database)
	if usage, err := fs.NewFilesystem(fs.FilesystemTypeBasic, dbPath).Usage("."); err == nil {
		return int(usage.Total / 1024 / 1024)
	}
	return 0
}

func NewMeasurements(clt *Client) *Measurements {
	return &Measurements{
		client:       clt,
		measurements: make(map[string]Measurement, 0),
		mutex:        sync.Mutex{},
		isMeasuring:  false,
	}
}

// reset forgets the measurements of an engine run that stopped
func (m *Measurements) reset() {
	m.mutex.Lock()
	defer m.mutex.Unlock()
	m.measurements = make(map[string]Measurement, 0)
}

func (m *Measurements) LatencyFor(deviceID string) float64 {
	m.mutex.Lock()
	defer m.mutex.Unlock()
	if measurement, ok := m.measurements[deviceID]; ok {
		if time.Since(measurement.when) > (measurementStaleAfterDurationSeconds * time.Second) {
			return math.NaN()
		}
		return measurement.latency
	}
	return math.NaN()
}

func (m *Measurements) actuallyMeasure() {
	ctx := context.Background()
	internals, cfg := m.client.internals(), m.client.cfg()
	if internals == nil || cfg == nil {
		return
	}
	devices := cfg.DeviceList()
	latencies := make(map[string]Measurement, 0)
	for _, device := range devices {
		if internals.IsConnectedTo(device.DeviceID) {
			start := time.Now()
			pingContext, cancel := context.WithTimeout(ctx, time.Second*1)

			// Make a faux request. This is expected to return a 'generic error' but we are not actually interested in the
			// requested block anyway, just in the time it takes to respond.
			fakeHash := [32]byte{}
			_, _ = internals.DownloadBlock(pingContext, device.DeviceID, "__fake_folder", "__fake_file_name_for_ping", 0, protocol.BlockInfo{Size: 1, Offset: 0, Hash: fakeHash[:]}, false)
			pingErr := pingContext.Err()
			cancel()
			if pingErr == nil {
				duration := time.Since(start)
				latencies[device.DeviceID.String()] = Measurement{
					latency: duration.Seconds(),
					when:    time.Now(),
				}
			} else {
				slog.Info("ping error", "cause", pingErr)
			}
		}
	}

	m.mutex.Lock()
	defer m.mutex.Unlock()
	m.measurements = latencies
	if m.client.Delegate != nil {
		m.client.Delegate.OnMeasurementsUpdated()
	}
}

func (m *Measurements) Measure() {
	m.mutex.Lock()
	wasMeasuring := m.isMeasuring
	if !wasMeasuring {
		m.isMeasuring = true
	}
	m.mutex.Unlock()

	if wasMeasuring {
		return
	}

	m.actuallyMeasure()

	m.mutex.Lock()
	m.isMeasuring = false
	m.mutex.Unlock()
}
