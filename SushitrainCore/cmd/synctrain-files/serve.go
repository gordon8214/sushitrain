// Copyright (C) 2026 Gordon
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this file,
// You can obtain one at https://mozilla.org/MPL/2.0/.

//go:build linux

package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/gofrs/flock"
	"t-shaped.nl/sushitrain/v2/adminsock"
	"t-shaped.nl/sushitrain/v2/desktopsettings"
	"t-shaped.nl/sushitrain/v2/files"
	"t-shaped.nl/sushitrain/v2/files/fusefs"
	"t-shaped.nl/sushitrain/v2/mcpcontrol"
	"t-shaped.nl/sushitrain/v2/mcpfiles"
)

func serve(args []string) error {
	fs, cf := newFlagSet("serve")
	stateDir := fs.String("state", defaultStateDir(), "index and edits not sent yet")
	cacheDir := fs.String("cache", defaultCacheDir(), "content fetched from gateways")
	mount := fs.String("mount", defaultMountDir(), "where the folders are shown")
	budget := fs.Int64("cache-budget", files.DefaultCacheBudget>>20, "MiB of content to keep, beyond kept files")
	logLevel := fs.String("log-level", envOr("SYNCTRAIN_FILES_LOG_LEVEL", "info"), "debug, info, warn or error")
	fuseDebug := fs.Bool("fuse-debug", false, "log every FUSE request")
	credentials := fs.String("credentials", "auto", "where to keep gateways' credentials: keyring, file, or auto "+
		"(the keyring when there is one)")
	allowLocal := fs.Bool("allow-local-gateway", false, "allow pairing with a gateway on this machine (for tests)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	for _, dir := range []*string{stateDir, cacheDir, mount} {
		abs, err := filepath.Abs(*dir)
		if err != nil {
			return err
		}
		*dir = abs
	}
	var level slog.Level
	if err := level.UnmarshalText([]byte(*logLevel)); err != nil {
		return fmt.Errorf("log level: %w", err)
	}
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: level}))
	slog.SetDefault(logger)

	if err := os.MkdirAll(*stateDir, 0o700); err != nil {
		return err
	}
	// One daemon per index: a second one would mount over the first and fight it for the database
	lock := flock.New(filepath.Join(*stateDir, "files.lock"))
	if locked, err := lock.TryLock(); err != nil {
		return err
	} else if !locked {
		return errors.New("another synctrain-files runs with " + *stateDir)
	}
	defer lock.Unlock()

	store, err := credentialStore(*credentials, *stateDir, logger)
	if err != nil {
		return err
	}
	notices := newNotifier(logger)
	service, err := files.Open(files.Config{
		StateDir:          *stateDir,
		CacheDir:          *cacheDir,
		Credentials:       store,
		CacheBudget:       *budget << 20,
		AllowLocalGateway: *allowLocal,
		Logger:            logger,
		Notify:            notices.notify,
	})
	if err != nil {
		return err
	}
	defer service.Close()
	m, err := fusefs.MountService(*mount, service, fusefs.Options{Logger: logger, Debug: *fuseDebug})
	if err != nil {
		return fmt.Errorf("mounting at %s: %w", *mount, err)
	}
	logger.Info("Mounted", "path", *mount, "version", versionString())
	if err := service.Start(); err != nil {
		unmount(m, *mount, logger)
		return err
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	foldersChanged := func() {
		if folders, err := service.Folders(); err == nil {
			syncBookmarks(*mount, folders, logger)
		}
	}
	foldersChanged()
	excludeFromBackups(*mount, logger)
	admin := adminsock.NewServer()
	registerAdmin(admin, service, *mount, foldersChanged)
	mcpfiles.Register(admin, func(ctx context.Context, call func(context.Context, *files.Service, string) (any, error)) (any, error) {
		result, err := call(ctx, service, *mount)
		if err == nil {
			foldersChanged()
		}
		return result, err
	})
	desktopsettings.Register(admin, desktopsettings.Path())
	if _, err := mcpcontrol.Register(admin, mcpcontrol.DefaultSettingsPath(), func() string { return "files" }); err != nil {
		admin.Close()
		unmount(m, *mount, logger)
		return err
	}
	socket := cf.path()
	if err := os.MkdirAll(filepath.Dir(socket), 0o700); err != nil {
		unmount(m, *mount, logger)
		return err
	}
	if err := admin.Listen(ctx, socket, false); err != nil {
		unmount(m, *mount, logger)
		return fmt.Errorf("listening on the admin socket: %w", err)
	}
	defer admin.Close()
	notifySystemd("READY=1")

	signals := make(chan os.Signal, 2)
	signal.Notify(signals, syscall.SIGINT, syscall.SIGTERM)
	unmounted := make(chan struct{})
	go func() {
		m.Wait()
		close(unmounted)
	}()
	select {
	case sig := <-signals:
		logger.Info("Stopping", "signal", sig)
		notifySystemd("STOPPING=1")
		unmount(m, *mount, logger)
	case <-unmounted:
		// Unmounted from outside (Nautilus's sidebar, fusermount3 -u): a clean stop
		logger.Info("The mount was unmounted; stopping")
	}
	return nil
}

// unmount unmounts, lazily if files in the mount are still open: the kernel finishes it when they are closed
func unmount(m *fusefs.Mount, dir string, logger *slog.Logger) {
	done := make(chan error, 1)
	go func() { done <- m.Unmount() }()
	select {
	case err := <-done:
		if err == nil {
			return
		}
		logger.Info("Unmounting lazily, as files are open", "error", err)
	case <-time.After(5 * time.Second):
	}
	if out, err := exec.Command("fusermount3", "-uz", dir).CombinedOutput(); err != nil {
		logger.Warn("Could not unmount", "path", dir, "error", err, "output", strings.TrimSpace(string(out)))
	}
}

func envOr(name, fallback string) string {
	if v := os.Getenv(name); v != "" {
		return v
	}
	return fallback
}

// notifySystemd tells systemd about the daemon's state (sd_notify), when it runs as a Type=notify unit
func notifySystemd(state string) {
	socket := os.Getenv("NOTIFY_SOCKET")
	if socket == "" {
		return
	}
	if strings.HasPrefix(socket, "@") {
		socket = "\x00" + socket[1:]
	}
	conn, err := net.DialUnix("unixgram", nil, &net.UnixAddr{Name: socket, Net: "unixgram"})
	if err != nil {
		return
	}
	defer conn.Close()
	_, _ = conn.Write([]byte(state))
}
