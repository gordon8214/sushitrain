// Copyright (C) 2026 Gordon
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this file,
// You can obtain one at https://mozilla.org/MPL/2.0/.

// Command synctrain-gateway runs Syncthing with Synctrain's remote file service, without a GUI of its own, and
// manages the running service. See Docs/gateway.md.
package main

import (
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"runtime/debug"
	"strings"
	"unicode"

	"github.com/syncthing/syncthing/lib/locations"
)

// Set with -ldflags "-X main.version=…"
var version = ""

func usage() {
	fmt.Fprint(os.Stderr, `Usage: synctrain-gateway <command> [options]

Commands:
  serve       Run Syncthing and the remote file service
  status      Show the service's state
  pair        Pair a client: show a pairing code and approve the client
  clients     List clients; revoke one; set a client's folder access
  exports     List, add or remove exported folders
  devices     List, add or remove Syncthing devices
  folders     List folders; share a folder with a device
  version     Show the version

Run "synctrain-gateway <command> -h" for a command's options.
`)
}

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	command, args := os.Args[1], os.Args[2:]
	var err error
	switch command {
	case "serve":
		err = serve(args)
	case "status":
		err = statusCommand(args)
	case "pair":
		err = pairCommand(args)
	case "clients":
		err = clientsCommand(args)
	case "exports":
		err = exportsCommand(args)
	case "devices":
		err = devicesCommand(args)
	case "folders":
		err = foldersCommand(args)
	case "version", "--version", "-v":
		fmt.Println(versionString())
	case "help", "-h", "--help":
		usage()
	default:
		usage()
		os.Exit(2)
	}
	if err != nil {
		var exit exitError
		if errors.As(err, &exit) {
			os.Exit(int(exit))
		}
		fmt.Fprintln(os.Stderr, "synctrain-gateway:", err)
		os.Exit(1)
	}
}

type exitError int

func (e exitError) Error() string {
	return fmt.Sprintf("exit status %d", int(e))
}

func versionString() string {
	v := version
	if v == "" {
		v = "dev"
		if info, ok := debug.ReadBuildInfo(); ok {
			for _, setting := range info.Settings {
				if setting.Key == "vcs.revision" && len(setting.Value) >= 12 {
					v = "dev-" + setting.Value[:12]
				}
			}
		}
	}
	return "synctrain-gateway " + v
}

// baseDirs returns Syncthing's configuration and data directories: the --home flag or STHOMEDIR for both, or else
// where stock Syncthing looks for this user (its default location, or a legacy one already in use)
func baseDirs(flagValue string) (configDir, dataDir string) {
	home := flagValue
	if home == "" {
		home = os.Getenv("STHOMEDIR")
	}
	if home != "" {
		if abs, err := filepath.Abs(home); err == nil {
			home = abs
		}
		return home, home
	}
	return locations.GetBaseDir(locations.ConfigBaseDir), locations.GetBaseDir(locations.DataBaseDir)
}

// serviceHome is where the remote file service keeps its database, staged uploads, content cache and admin socket:
// below Syncthing's data directory
func serviceHome(dataDir string) string {
	return filepath.Join(dataDir, "synctrain-gateway")
}

// adminSocket returns the admin socket path: the --socket flag, SYNCTRAIN_GATEWAY_ADMIN, or in the service's
// directory
func adminSocket(flagValue, dataDir string) string {
	if flagValue != "" {
		return flagValue
	}
	if env := os.Getenv("SYNCTRAIN_GATEWAY_ADMIN"); env != "" {
		return env
	}
	return filepath.Join(serviceHome(dataDir), "admin.sock")
}

// clientFlags are the options every management command takes
type clientFlags struct {
	home   string
	socket string
}

func newFlagSet(name string) (*flag.FlagSet, *clientFlags) {
	fs := flag.NewFlagSet(name, flag.ExitOnError)
	cf := &clientFlags{}
	fs.StringVar(&cf.home, "home", "", "Syncthing home directory (default $STHOMEDIR, or where stock Syncthing looks)")
	fs.StringVar(&cf.socket, "socket", "", "admin socket of the running service (default $SYNCTRAIN_GATEWAY_ADMIN, or synctrain-gateway/admin.sock in Syncthing's data directory)")
	return fs, cf
}

func (cf *clientFlags) path() string {
	_, dataDir := baseDirs(cf.home)
	return adminSocket(cf.socket, dataDir)
}

// parseFlags parses a command's options; no command takes other arguments
func parseFlags(fs *flag.FlagSet, args []string) error {
	_ = fs.Parse(args) // exits on errors
	if fs.NArg() > 0 {
		fmt.Fprintf(fs.Output(), "unexpected argument %q\n", fs.Arg(0))
		fs.Usage()
		return exitError(2)
	}
	return nil
}

// printable replaces control characters (such as terminal escape sequences) and bidirectional text controls in text
// that clients or other devices chose, so that printing it can't change what the terminal shows
func printable(s string) string {
	return strings.Map(func(r rune) rune {
		if unicode.IsControl(r) || unicode.Is(unicode.Bidi_Control, r) {
			return unicode.ReplacementChar
		}
		return r
	}, s)
}

func envOr(name, fallback string) string {
	if v := strings.TrimSpace(os.Getenv(name)); v != "" {
		return v
	}
	return fallback
}
