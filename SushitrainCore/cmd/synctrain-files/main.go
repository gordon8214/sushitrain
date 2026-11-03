// Copyright (C) 2026 Gordon
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this file,
// You can obtain one at https://mozilla.org/MPL/2.0/.

// Command synctrain-files shows the folders of gateways as a directory, files fetched as they are read (Files mode
// of Synctrain for Linux), and manages the running daemon. See Docs/linux.md.
package main

import (
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"runtime/debug"
)

// Set with -ldflags "-X main.version=…"
var version = ""

func usage() {
	fmt.Fprint(os.Stderr, `Usage: synctrain-files <command> [options]

Commands:
  serve       Mount the folders and follow their gateways
  status      Show the gateways and folders
  pair        Pair with a gateway, from its pairing link
  gateways    List paired gateways; forget one
  folders     List folders here; list a gateway's folders; add or remove one
  keep        Keep files on this device (or stop keeping them)
  state       Show whether files are on this device, or changed here
  pending     List changes not on the gateway yet; export or discard one
  wait        Wait until changes to files are on the gateway
  version     Show the version

Run "synctrain-files <command> -h" for a command's options.
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
	case "gateways":
		err = gatewaysCommand(args)
	case "folders":
		err = foldersCommand(args)
	case "keep":
		err = keepCommand(args)
	case "state":
		err = stateCommand(args)
	case "pending":
		err = pendingCommand(args)
	case "wait":
		err = waitCommand(args)
	case "version", "--version", "-v":
		fmt.Println(versionString())
	case "help", "-h", "--help":
		usage()
	default:
		usage()
		os.Exit(2)
	}
	if err != nil {
		if errors.Is(err, flag.ErrHelp) {
			os.Exit(2)
		}
		fmt.Fprintln(os.Stderr, "synctrain-files:", err)
		os.Exit(1)
	}
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
	return "synctrain-files " + v
}

// xdgDir is an XDG base directory: the variable, or its default below the home directory
func xdgDir(variable, fallback string) string {
	if dir := os.Getenv(variable); filepath.IsAbs(dir) {
		return dir
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, fallback)
}

// Where things are by default (Docs/linux.md)
func defaultStateDir() string {
	return filepath.Join(xdgDir("XDG_STATE_HOME", ".local/state"), "synctrain", "files")
}
func defaultCacheDir() string { return filepath.Join(xdgDir("XDG_CACHE_HOME", ".cache"), "synctrain") }
func defaultMountDir() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, "Synctrain")
}

// defaultSocket is the admin socket: in the runtime directory, which only this user can enter, or else in the state
// directory
func defaultSocket() string {
	if dir := os.Getenv("XDG_RUNTIME_DIR"); filepath.IsAbs(dir) {
		return filepath.Join(dir, "synctrain", "files.sock")
	}
	return filepath.Join(defaultStateDir(), "files.sock")
}

// commonFlags are the flags every command that talks to the daemon has
type commonFlags struct {
	socket *string
}

func newFlagSet(name string) (*flag.FlagSet, commonFlags) {
	fs := flag.NewFlagSet("synctrain-files "+name, flag.ContinueOnError)
	cf := commonFlags{socket: fs.String("socket", "", "admin socket (default "+defaultSocket()+")")}
	return fs, cf
}

func (cf commonFlags) path() string {
	if *cf.socket != "" {
		return *cf.socket
	}
	if env := os.Getenv("SYNCTRAIN_FILES_SOCKET"); env != "" {
		return env
	}
	return defaultSocket()
}
