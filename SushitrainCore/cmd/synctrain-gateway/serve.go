// Copyright (C) 2026 Gordon
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this file,
// You can obtain one at https://mozilla.org/MPL/2.0/.

package main

import (
	"context"
	"crypto/tls"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/gofrs/flock"
	"github.com/syncthing/syncthing/lib/build"
	"github.com/syncthing/syncthing/lib/config"
	"github.com/syncthing/syncthing/lib/events"
	"github.com/syncthing/syncthing/lib/ignore"
	"github.com/syncthing/syncthing/lib/locations"
	"github.com/syncthing/syncthing/lib/protocol"
	"github.com/syncthing/syncthing/lib/svcutil"
	"github.com/syncthing/syncthing/lib/syncthing"

	"t-shaped.nl/sushitrain/v2/gateway"
)

// The newest database schema the pinned engine knows (internal/db/sqlite currentSchemaVersion). A database
// written by a newer Syncthing is refused rather than opened: the engine would not migrate it back.
const supportedSchemaVersion = 6

// Database maintenance and retention of deleted items, as stock Syncthing defaults them
const (
	defaultDBMaintenanceInterval = 8 * time.Hour
	defaultDeleteRetention       = 10920 * time.Hour
)

// Container runtimes kill a process 10 s after asking it to stop: stopping the service and then Syncthing takes at
// most shutdownTimeout. What doesn't finish is recovered at the next start, from the service's journal and Syncthing's
// database.
const (
	shutdownTimeout        = 8 * time.Second
	serviceShutdownTimeout = 5 * time.Second
)

// serveOptions are the settings each start of Syncthing and the service uses
type serveOptions struct {
	service         gateway.Config
	socket          string
	adminGroup      bool
	pairHost        string
	pairPort        int
	dbMaintenance   time.Duration
	deleteRetention time.Duration
}

func serve(args []string) error {
	dbMaintenance, err := envDuration("STDBMAINTENANCEINTERVAL", defaultDBMaintenanceInterval)
	if err != nil {
		return err
	}
	deleteRetention, err := envDuration("STDBDELETERETENTIONINTERVAL", defaultDeleteRetention)
	if err != nil {
		return err
	}
	fs, cf := newFlagSet("serve")
	listen := fs.String("listen", envOr("SYNCTRAIN_GATEWAY_LISTEN", ":20920"), "address for the remote file service ($SYNCTRAIN_GATEWAY_LISTEN)")
	certFile := fs.String("cert", os.Getenv("SYNCTRAIN_GATEWAY_CERT"), "TLS certificate ($SYNCTRAIN_GATEWAY_CERT); a self-signed one is created when empty")
	keyFile := fs.String("key", os.Getenv("SYNCTRAIN_GATEWAY_KEY"), "TLS key ($SYNCTRAIN_GATEWAY_KEY)")
	proxy := fs.Bool("proxy", os.Getenv("SYNCTRAIN_GATEWAY_PROXY") == "1", "serve plain HTTP for a reverse proxy that terminates TLS ($SYNCTRAIN_GATEWAY_PROXY=1)")
	proxySPKI := fs.String("proxy-spki", os.Getenv("SYNCTRAIN_GATEWAY_PROXY_SPKI"), "public key hash clients pin for the proxy's certificate")
	publicHost := fs.String("public-host", os.Getenv("SYNCTRAIN_GATEWAY_HOST"), "host name clients connect to, for pairing codes ($SYNCTRAIN_GATEWAY_HOST)")
	publicPort := fs.Int("public-port", envInt("SYNCTRAIN_GATEWAY_PORT", 0), "port clients connect to, when it differs from the listening port ($SYNCTRAIN_GATEWAY_PORT)")
	name := fs.String("name", os.Getenv("SYNCTRAIN_GATEWAY_NAME"), "service name shown to clients ($SYNCTRAIN_GATEWAY_NAME)")
	spool := fs.Int64("content-cache-mib", int64(envInt("SYNCTRAIN_GATEWAY_CONTENT_CACHE_MIB", 8192)), "space for prepared content, in MiB")
	auditDays := fs.Int("audit-retention-days", envInt("SYNCTRAIN_GATEWAY_AUDIT_DAYS", 365), "days to keep the audit log (0 keeps it)")
	maintenance := fs.Duration("db-maintenance-interval", dbMaintenance, "how often Syncthing maintains its database; 0 disables it ($STDBMAINTENANCEINTERVAL)")
	retention := fs.Duration("db-delete-retention-interval", deleteRetention, "how long Syncthing's database remembers deleted items ($STDBDELETERETENTIONINTERVAL)")
	adminGroup := fs.Bool("admin-group", os.Getenv("SYNCTRAIN_GATEWAY_ADMIN_GROUP") == "1", "let members of the admin socket's group use it, not only its owner ($SYNCTRAIN_GATEWAY_ADMIN_GROUP=1)")
	if err := parseFlags(fs, args); err != nil {
		return err
	}
	if err := configureLogging(); err != nil {
		return err
	}

	// SIGINT and SIGTERM stop the gateway from here on, also while the database is migrated or opened
	ctx, stop := stopOnSignal()
	defer stop()

	pairHost, pairPort, err := pairingAddress(*publicHost, *publicPort, *listen)
	if err != nil {
		return err
	}
	if *proxy && *publicPort == 0 {
		slog.Warn("Pairing codes carry the listening port; set --public-port to the port the reverse proxy serves",
			"port", pairPort)
	}

	configDir, dataDir := baseDirs(cf.home)
	for _, dir := range []string{configDir, dataDir} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return err
		}
	}
	if err := locations.SetBaseDir(locations.ConfigBaseDir, configDir); err != nil {
		return err
	}
	if err := locations.SetBaseDir(locations.DataBaseDir, dataDir); err != nil {
		return err
	}
	slog.Info("Using Syncthing's home", "config", configDir, "data", dataDir)

	// One engine per home: take the lock before reading or writing anything else
	lock := flock.New(locations.Get(locations.LockFile))
	locked, err := lock.TryLock()
	if err != nil {
		return fmt.Errorf("taking the lock: %w", err)
	}
	if !locked {
		return errors.New("another Syncthing instance is using " + dataDir + "; stop it first")
	}
	defer func() {
		_ = lock.Unlock()
		_ = os.Remove(locations.Get(locations.LockFile))
	}()

	serviceName := *name
	if serviceName == "" {
		serviceName, _ = os.Hostname()
	}
	opts := serveOptions{
		service: gateway.Config{
			Home:           serviceHome(dataDir),
			ListenAddress:  *listen,
			CertFile:       *certFile,
			KeyFile:        *keyFile,
			Proxy:          *proxy,
			ProxySPKI:      *proxySPKI,
			ServiceName:    serviceName,
			Thumbnails:     gateway.NewCommandRenderer(),
			SpoolQuota:     *spool << 20,
			AuditRetention: time.Duration(*auditDays) * 24 * time.Hour,
			Logger:         slog.Default(),
		},
		socket:          adminSocket(cf.socket, dataDir),
		adminGroup:      *adminGroup,
		pairHost:        pairHost,
		pairPort:        pairPort,
		dbMaintenance:   *maintenance,
		deleteRetention: *retention,
	}
	for {
		restart, err := run(ctx, opts)
		if err != nil || !restart || ctx.Err() != nil {
			return err
		}
		// As stock Syncthing's monitor process would, after a restart from the web GUI or the REST API
		slog.Info("Restarting")
	}
}

// run starts Syncthing and the service, and runs them until ctx is done or Syncthing stops. It reports whether
// Syncthing stopped to be restarted (from its web GUI or REST API).
func run(ctx context.Context, opts serveOptions) (restart bool, err error) {
	if err := checkSchema(locations.Get(locations.Database)); err != nil {
		return false, err
	}
	slog.Info(versionString(), "engine", build.LongVersion)
	cert, err := loadIdentity()
	if err != nil {
		return false, err
	}
	id := protocol.NewDeviceID(cert.Certificate[0])

	// The event logger and the configuration keep running while Syncthing stops, until this run ends
	runCtx, cancel := context.WithCancel(context.Background())
	defer cancel()
	evLogger := events.NewLogger()
	go evLogger.Serve(runCtx)
	configFile := locations.Get(locations.ConfigFile)
	if _, err := os.Stat(configFile); errors.Is(err, os.ErrNotExist) {
		slog.Warn("No Syncthing configuration in the home; creating a new one (to take over an existing Syncthing, "+
			"stop and pass its home with --home)", "path", configFile)
	}
	cfg, err := syncthing.LoadConfigAtStartup(configFile, cert, evLogger, false, false)
	if err != nil {
		return false, fmt.Errorf("loading the configuration: %w", err)
	}
	go func() { _ = cfg.Serve(runCtx) }()

	if err := syncthing.TryMigrateDatabase(ctx, opts.deleteRetention); err != nil {
		if ctx.Err() != nil {
			return false, nil
		}
		return false, fmt.Errorf("migrating the database: %w", err)
	}
	sdb, err := syncthing.OpenDatabase(locations.Get(locations.Database), opts.deleteRetention)
	if err != nil {
		return false, fmt.Errorf("opening the database: %w", err)
	}
	if ctx.Err() != nil {
		sdb.Close()
		return false, nil
	}
	app, err := syncthing.New(cfg, sdb, evLogger, cert, syncthing.Options{
		NoUpgrade:             true,
		DBMaintenanceInterval: opts.dbMaintenance,
	})
	if err != nil {
		sdb.Close()
		return false, err
	}
	// Syncthing closes the database when it stops, also when it fails to start
	if err := app.Start(); err != nil {
		return false, err
	}
	slog.Info("Syncthing started", "device", id.String())

	svc, admin, err := startService(ctx, opts, app, cfg, evLogger, id)
	if err != nil {
		shutdown(nil, nil, app)
		return false, err
	}

	exit := make(chan svcutil.ExitStatus, 1)
	go func() { exit <- app.Wait() }()
	select {
	case <-ctx.Done():
		shutdown(admin, svc, app)
		return false, nil
	case status := <-exit:
		stopped := shutdown(admin, svc, app)
		switch status {
		case svcutil.ExitSuccess:
			return false, nil
		case svcutil.ExitRestart, svcutil.ExitUpgrade:
			if !stopped {
				return false, errors.New("the remote file service did not stop for the restart")
			}
			return true, nil
		}
		return false, fmt.Errorf("syncthing stopped: %v", app.Error())
	}
}

// startService starts the remote file service and its admin socket for a running Syncthing
func startService(ctx context.Context, opts serveOptions, app *syncthing.App, cfg config.Wrapper,
	evLogger events.Logger, id protocol.DeviceID) (*gateway.Service, *gateway.AdminServer, error) {
	backend := &gateway.EngineBackend{App: app, Config: cfg, Events: evLogger, ID: id, IsSelective: isSelective(app, cfg)}
	svc, err := gateway.New(opts.service, backend)
	if err != nil {
		return nil, nil, err
	}
	if err := svc.Start(); err != nil {
		_ = svc.Close()
		return nil, nil, err
	}
	admin := gateway.NewAdminServer()
	svc.RegisterAdmin(admin, opts.pairHost, opts.pairPort)
	registerHostAdmin(admin, cfg, app, id)
	if err := os.MkdirAll(filepath.Dir(opts.socket), 0o700); err != nil {
		_ = svc.Close()
		return nil, nil, err
	}
	if err := admin.Listen(ctx, opts.socket, opts.adminGroup); err != nil {
		_ = svc.Close()
		return nil, nil, fmt.Errorf("listening on the admin socket: %w", err)
	}
	slog.Info("Admin socket ready", "path", opts.socket)
	return svc, admin, nil
}

// shutdown closes the admin socket, stops the service and then Syncthing, waiting at most shutdownTimeout in all. It
// reports whether everything stopped in time.
func shutdown(admin *gateway.AdminServer, svc *gateway.Service, app *syncthing.App) bool {
	start := time.Now()
	stopped := true
	if admin != nil {
		admin.Close()
	}
	if svc != nil {
		closed := within(serviceShutdownTimeout, func() {
			if err := svc.Close(); err != nil {
				slog.Warn("Stopping the remote file service", "error", err)
			}
		})
		if !closed {
			slog.Warn("The remote file service did not stop in time; its operations continue at the next start")
			stopped = false
		}
	}
	// Returns at once when Syncthing stopped already
	if !within(shutdownTimeout-time.Since(start), func() { app.Stop(svcutil.ExitSuccess) }) {
		slog.Warn("Syncthing did not stop in time")
		stopped = false
	}
	return stopped
}

// within runs fn and reports whether it returned within d; fn keeps running if it didn't
func within(d time.Duration, fn func()) bool {
	done := make(chan struct{})
	go func() {
		fn()
		close(done)
	}()
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-done:
		return true
	case <-timer.C:
		return false
	}
}

// stopOnSignal returns a context that is cancelled at SIGINT or SIGTERM. A second signal exits at once.
func stopOnSignal() (context.Context, context.CancelFunc) {
	ctx, cancel := context.WithCancel(context.Background())
	signals := make(chan os.Signal, 2)
	signal.Notify(signals, os.Interrupt, syscall.SIGTERM)
	go func() {
		sig := <-signals
		slog.Info("Stopping", "signal", sig.String())
		cancel()
		sig = <-signals
		slog.Warn("Stopping immediately", "signal", sig.String())
		os.Exit(svcutil.ExitError.AsInt())
	}()
	return ctx, func() {
		signal.Stop(signals)
		cancel()
	}
}

// loadIdentity loads the device certificate, or creates a new device identity when the home has none. A certificate
// or key that exists but can't be loaded is an error: a new identity would make this a different device to all others.
func loadIdentity() (tls.Certificate, error) {
	certFile, keyFile := locations.Get(locations.CertFile), locations.Get(locations.KeyFile)
	_, certErr := os.Stat(certFile)
	_, keyErr := os.Stat(keyFile)
	if errors.Is(certErr, os.ErrNotExist) && errors.Is(keyErr, os.ErrNotExist) {
		slog.Warn("No device certificate in the home; creating a new device identity (to take over an existing "+
			"Syncthing, stop and pass its home with --home)", "path", certFile)
		return syncthing.GenerateCertificate(certFile, keyFile)
	}
	cert, err := tls.LoadX509KeyPair(certFile, keyFile)
	if err != nil {
		return tls.Certificate{}, fmt.Errorf("loading the device certificate (%s, %s): %w", certFile, keyFile, err)
	}
	return cert, nil
}

// checkSchema refuses databases a newer Syncthing wrote
func checkSchema(dbDir string) error {
	entries, err := os.ReadDir(dbDir)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if !strings.HasSuffix(entry.Name(), ".db") {
			continue
		}
		version, err := schemaVersion(filepath.Join(dbDir, entry.Name()))
		if err != nil {
			return fmt.Errorf("reading the schema of %s: %w", entry.Name(), err)
		}
		if version > supportedSchemaVersion {
			return fmt.Errorf("%s has database schema %d, newer than this engine supports (%d); use a matching Syncthing version",
				entry.Name(), version, supportedSchemaVersion)
		}
	}
	return nil
}

func schemaVersion(path string) (int, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return 0, err
	}
	uri := url.URL{Scheme: "file", Path: abs, RawQuery: "mode=ro"}
	db, err := sql.Open(gateway.SQLiteDriver, uri.String())
	if err != nil {
		return 0, err
	}
	defer db.Close()
	var version sql.NullInt64
	err = db.QueryRow(`SELECT MAX(schema_version) FROM schemamigrations`).Scan(&version)
	if err != nil && strings.Contains(err.Error(), "no such table") {
		return 0, nil
	}
	return int(version.Int64), err
}

func envInt(name string, fallback int) int {
	if v, err := strconv.Atoi(os.Getenv(name)); err == nil {
		return v
	}
	return fallback
}

func envDuration(name string, fallback time.Duration) (time.Duration, error) {
	v := strings.TrimSpace(os.Getenv(name))
	if v == "" {
		return fallback, nil
	}
	d, err := time.ParseDuration(v)
	if err != nil {
		return 0, fmt.Errorf("%s: %w", name, err)
	}
	return d, nil
}

// pairingAddress is where clients reach the service, for pairing codes
func pairingAddress(host string, port int, listen string) (string, int, error) {
	host = strings.TrimSpace(host)
	if _, _, err := net.SplitHostPort(host); err == nil {
		return "", 0, fmt.Errorf("--public-host %q contains a port; give the port with --public-port", host)
	}
	host = strings.TrimSuffix(strings.TrimPrefix(host, "["), "]")
	listenHost, listenPort, _ := net.SplitHostPort(listen)
	if port == 0 {
		port, _ = strconv.Atoi(listenPort)
	}
	if host == "" {
		host = listenHost
		if host == "" || host == "0.0.0.0" || host == "::" {
			host, _ = os.Hostname()
		}
	}
	return host, port, nil
}

// isSelective reports whether a folder's ignore patterns keep only a selection of its files on this device (a
// pattern that ignores everything, usually after exceptions, as on-demand folders use). It answers yes when the
// patterns can't be read, so that such a folder is never written.
func isSelective(app *syncthing.App, cfg config.Wrapper) func(string) bool {
	return func(folderID string) bool {
		_, patterns, err := app.Internals.Ignores(folderID)
		if err != nil {
			// Syncthing has no patterns for folders that don't run (paused ones): read them from the folder
			folder, ok := cfg.Folder(folderID)
			if !ok {
				return true
			}
			matcher := ignore.New(folder.Filesystem())
			if err := matcher.Load(".stignore"); err != nil && !errors.Is(err, os.ErrNotExist) {
				return true
			}
			patterns = matcher.Patterns()
		}
		return slices.ContainsFunc(patterns, ignoresEverything)
	}
}

// ignoresEverything reports whether a parsed ignore pattern ("(?d)(?i)!pattern") ignores every file
func ignoresEverything(pattern string) bool {
	pattern = strings.TrimPrefix(pattern, "(?d)")
	pattern = strings.TrimPrefix(pattern, "(?i)")
	switch pattern {
	case "*", "**", "/*", "/**", "**/*", "**/**":
		return true
	}
	return false
}

// registerHostAdmin adds Syncthing device and folder management to the admin socket
func registerHostAdmin(admin *gateway.AdminServer, cfg config.Wrapper, app *syncthing.App, self protocol.DeviceID) {
	type device struct {
		ID        string   `json:"id"`
		Name      string   `json:"name"`
		Addresses []string `json:"addresses,omitempty"`
		Connected bool     `json:"connected"`
		Paused    bool     `json:"paused"`
	}
	type folder struct {
		ID      string   `json:"id"`
		Label   string   `json:"label"`
		Path    string   `json:"path"`
		Type    string   `json:"type"`
		Paused  bool     `json:"paused"`
		State   string   `json:"state"`
		Devices []string `json:"devices"`
	}
	modify := func(fn func(*config.Configuration) error) error {
		var inner error
		waiter, err := cfg.Modify(func(c *config.Configuration) { inner = fn(c) })
		if err != nil {
			return err
		}
		if inner != nil {
			return inner
		}
		waiter.Wait()
		return cfg.Save()
	}
	// A device ID to add, remove or share with; never the gateway's own, whose entry Syncthing needs as it is
	otherDevice := func(value string) (protocol.DeviceID, error) {
		id, err := protocol.DeviceIDFromString(value)
		if err != nil {
			return protocol.EmptyDeviceID, err
		}
		if id == self {
			return protocol.EmptyDeviceID, errors.New("that is this gateway's own device ID")
		}
		return id, nil
	}
	admin.Register("devices.list", func(context.Context, json.RawMessage) (any, error) {
		var result []device
		for _, d := range cfg.RawCopy().Devices {
			if d.DeviceID == self {
				continue
			}
			result = append(result, device{ID: d.DeviceID.String(), Name: d.Name, Addresses: d.Addresses,
				Connected: app.Internals.IsConnectedTo(d.DeviceID), Paused: d.Paused})
		}
		return result, nil
	})
	admin.Register("devices.add", func(_ context.Context, params json.RawMessage) (any, error) {
		var p device
		if err := json.Unmarshal(params, &p); err != nil {
			return nil, err
		}
		id, err := otherDevice(p.ID)
		if err != nil {
			return nil, err
		}
		return nil, modify(func(c *config.Configuration) error {
			d, _, ok := c.Device(id)
			if !ok {
				d = c.Defaults.Device.Copy()
				d.DeviceID = id
			}
			if p.Name != "" {
				d.Name = p.Name
			}
			if len(p.Addresses) > 0 {
				d.Addresses = p.Addresses
			}
			c.SetDevice(d)
			return nil
		})
	})
	admin.Register("devices.remove", func(_ context.Context, params json.RawMessage) (any, error) {
		var p device
		if err := json.Unmarshal(params, &p); err != nil {
			return nil, err
		}
		id, err := otherDevice(p.ID)
		if err != nil {
			return nil, err
		}
		return nil, modify(func(c *config.Configuration) error {
			if _, _, ok := c.Device(id); !ok {
				return errors.New("no such device")
			}
			devices := c.Devices[:0]
			for _, d := range c.Devices {
				if d.DeviceID != id {
					devices = append(devices, d)
				}
			}
			c.Devices = devices
			for i := range c.Folders {
				kept := c.Folders[i].Devices[:0]
				for _, fd := range c.Folders[i].Devices {
					if fd.DeviceID != id {
						kept = append(kept, fd)
					}
				}
				c.Folders[i].Devices = kept
			}
			return nil
		})
	})
	admin.Register("folders.list", func(context.Context, json.RawMessage) (any, error) {
		var result []folder
		for _, f := range cfg.RawCopy().Folders {
			state, _, _ := app.Internals.FolderState(f.ID)
			entry := folder{ID: f.ID, Label: f.Label, Path: f.Path, Type: f.Type.String(), Paused: f.Paused, State: state}
			for _, d := range f.Devices {
				if d.DeviceID != self {
					entry.Devices = append(entry.Devices, d.DeviceID.String())
				}
			}
			result = append(result, entry)
		}
		return result, nil
	})
	admin.Register("folders.share", func(_ context.Context, params json.RawMessage) (any, error) {
		var p struct {
			FolderID string `json:"folderID"`
			DeviceID string `json:"deviceID"`
			Unshare  bool   `json:"unshare"`
		}
		if err := json.Unmarshal(params, &p); err != nil {
			return nil, err
		}
		id, err := otherDevice(p.DeviceID)
		if err != nil {
			return nil, err
		}
		return nil, modify(func(c *config.Configuration) error {
			f, _, ok := c.Folder(p.FolderID)
			if !ok {
				return errors.New("no such folder")
			}
			if _, _, ok := c.Device(id); !ok {
				return errors.New("add the device first")
			}
			kept := f.Devices[:0]
			for _, fd := range f.Devices {
				if fd.DeviceID != id {
					kept = append(kept, fd)
				}
			}
			f.Devices = kept
			if !p.Unshare {
				f.Devices = append(f.Devices, config.FolderDeviceConfiguration{DeviceID: id})
			}
			c.SetFolder(f)
			return nil
		})
	})
}
