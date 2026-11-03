// Copyright (C) 2026 Gordon
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this file,
// You can obtain one at https://mozilla.org/MPL/2.0/.

package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"text/tabwriter"
	"time"

	"t-shaped.nl/sushitrain/v2/adminsock"
	"t-shaped.nl/sushitrain/v2/files"
)

func printJSON(v any) error {
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	return enc.Encode(v)
}

func table(header string, rows func(w *tabwriter.Writer)) {
	w := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
	fmt.Fprintln(w, header)
	rows(w)
	w.Flush()
}

func statusCommand(args []string) error {
	fs, cf := newFlagSet("status")
	asJSON := fs.Bool("json", false, "print JSON")
	if err := fs.Parse(args); err != nil {
		return err
	}
	var st files.Status
	if err := adminsock.Call(cf.path(), "status", nil, &st); err != nil {
		return err
	}
	if *asJSON {
		return printJSON(st)
	}
	fmt.Printf("%s, folders in %s\n", st.Version, st.Mount)
	printGateways(st.Gateways)
	if len(st.Folders) > 0 {
		fmt.Println()
		printFolders(st.Folders)
	}
	return nil
}

func printGateways(gateways []files.Gateway) {
	table("GATEWAY\tADDRESS\tSERVICE\tFOLDERS\tSTATE", func(w *tabwriter.Writer) {
		for _, g := range gateways {
			state := g.State
			if state == "" {
				state = "ok"
			}
			fmt.Fprintf(w, "%s\t%s:%d\t%s\t%d\t%s\n", g.Name, g.Host, g.Port, g.ServiceID, g.Folders, state)
		}
	})
}

func printFolders(folders []files.Folder) {
	table("ID\tDIRECTORY\tGATEWAY\tFOLDER\tACCESS\tSTATE", func(w *tabwriter.Writer) {
		for _, f := range folders {
			access := "read"
			if f.CanWrite {
				access = "write"
			}
			state := f.State
			switch {
			case state != "":
			case !f.Listed:
				state = "listing"
			default:
				state = "ok"
			}
			fmt.Fprintf(w, "%d\t%s\t%s\t%s\t%s\t%s\n", f.ID, f.DirName, f.Gateway, f.RemoteID, access, state)
		}
	})
}

func pairCommand(args []string) error {
	fs, cf := newFlagSet("pair")
	fs.Usage = func() {
		fmt.Fprintln(os.Stderr, "Usage: synctrain-files pair [options] <synctrain+pair:// link>")
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		fs.Usage()
		return flag.ErrHelp
	}
	fmt.Println("Asking the gateway to pair; approve this device there.")
	var g files.Gateway
	if err := adminsock.Call(cf.path(), "pair", map[string]string{"link": fs.Arg(0)}, &g); err != nil {
		return err
	}
	fmt.Printf("Paired with %s (%s). Add its folders with: synctrain-files folders --gateway %s --add <folder>\n",
		g.Name, g.ServiceID, g.ServiceID)
	return nil
}

func gatewaysCommand(args []string) error {
	fs, cf := newFlagSet("gateways")
	forget := fs.String("forget", "", "forget the gateway with this service ID (remove its folders first)")
	asJSON := fs.Bool("json", false, "print JSON")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *forget != "" {
		return adminsock.Call(cf.path(), "gateways.forget", map[string]string{"serviceID": *forget}, nil)
	}
	var st files.Status
	if err := adminsock.Call(cf.path(), "status", nil, &st); err != nil {
		return err
	}
	if *asJSON {
		return printJSON(st.Gateways)
	}
	printGateways(st.Gateways)
	return nil
}

func foldersCommand(args []string) error {
	fs, cf := newFlagSet("folders")
	gateway := fs.String("gateway", "", "a gateway's service ID: list its folders, or add one of them")
	add := fs.String("add", "", "add the gateway's folder with this ID")
	remove := fs.String("remove", "", "remove the folder with this ID (as listed) from this device; it stays on the gateway")
	asJSON := fs.Bool("json", false, "print JSON")
	if err := fs.Parse(args); err != nil {
		return err
	}
	switch {
	case *remove != "":
		id, err := strconv.ParseInt(*remove, 10, 64)
		if err != nil {
			return errors.New("--remove takes a folder's ID, as listed")
		}
		var removal files.Removal
		if err := adminsock.Call(cf.path(), "folders.remove", map[string]int64{"id": id}, &removal); err != nil {
			return err
		}
		if removal.Moved > 0 {
			fmt.Printf("%d files that were only on this machine are in %s\n", removal.Moved, removal.Leftovers)
		}
		return nil
	case *add != "":
		if *gateway == "" {
			return errors.New("--add needs --gateway")
		}
		var f files.Folder
		if err := adminsock.Call(cf.path(), "folders.add", map[string]string{"serviceID": *gateway, "folderID": *add},
			&f); err != nil {
			return err
		}
		fmt.Printf("Added %s as %s\n", f.Label, f.DirName)
		return nil
	case *gateway != "":
		var remote []files.RemoteFolder
		if err := adminsock.Call(cf.path(), "folders.remote", map[string]string{"serviceID": *gateway},
			&remote); err != nil {
			return err
		}
		if *asJSON {
			return printJSON(remote)
		}
		table("FOLDER\tLABEL\tACCESS\tHERE AS", func(w *tabwriter.Writer) {
			for _, f := range remote {
				access := "read"
				if f.Write {
					access = "write"
				}
				fmt.Fprintf(w, "%s\t%s\t%s\t%s\n", f.ID, f.Label, access, f.DirName)
			}
		})
		return nil
	}
	var st files.Status
	if err := adminsock.Call(cf.path(), "status", nil, &st); err != nil {
		return err
	}
	if *asJSON {
		return printJSON(st.Folders)
	}
	printFolders(st.Folders)
	return nil
}

func absolutePaths(paths []string) ([]string, error) {
	var result []string
	for _, p := range paths {
		abs, err := filepath.Abs(p)
		if err != nil {
			return nil, err
		}
		result = append(result, abs)
	}
	return result, nil
}

func keepCommand(args []string) error {
	fs, cf := newFlagSet("keep")
	stop := fs.Bool("stop", false, "stop keeping them, removing their downloaded copies")
	fs.Usage = func() {
		fmt.Fprintln(os.Stderr, "Usage: synctrain-files keep [--stop] <path>…")
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		return err
	}
	paths, err := absolutePaths(fs.Args())
	if err != nil || len(paths) == 0 {
		fs.Usage()
		return flag.ErrHelp
	}
	for _, p := range paths {
		if err := adminsock.Call(cf.path(), "keep", map[string]any{"path": p, "keep": !*stop}, nil); err != nil {
			return fmt.Errorf("%s: %w", p, err)
		}
	}
	return nil
}

func stateCommand(args []string) error {
	fs, cf := newFlagSet("state")
	if err := fs.Parse(args); err != nil {
		return err
	}
	paths, err := absolutePaths(fs.Args())
	if err != nil || len(paths) == 0 {
		return errors.New("usage: synctrain-files state <path>…")
	}
	table("STATE\tPATH", func(w *tabwriter.Writer) {
		for _, p := range paths {
			var st files.ItemState
			if err := adminsock.Call(cf.path(), "state", map[string]string{"path": p}, &st); err != nil {
				fmt.Fprintf(w, "error\t%s: %v\n", p, err)
				continue
			}
			fmt.Fprintf(w, "%s\t%s\n", st.State, p)
		}
	})
	return nil
}

func pendingCommand(args []string) error {
	fs, cf := newFlagSet("pending")
	export := fs.String("export", "", "export the edit with this ID (as listed) to the file given after it")
	discard := fs.String("discard", "", "discard the edit with this ID: the file shows what the gateway has again")
	asJSON := fs.Bool("json", false, "print JSON")
	if err := fs.Parse(args); err != nil {
		return err
	}
	switch {
	case *export != "":
		id, err := strconv.ParseInt(*export, 10, 64)
		if err != nil || fs.NArg() != 1 {
			return errors.New("usage: synctrain-files pending --export <ID> <file>")
		}
		dest, err := filepath.Abs(fs.Arg(0))
		if err != nil {
			return err
		}
		return adminsock.Call(cf.path(), "pending.export", map[string]any{"id": id, "dest": dest}, nil)
	case *discard != "":
		id, err := strconv.ParseInt(*discard, 10, 64)
		if err != nil {
			return errors.New("--discard takes an edit's ID, as listed")
		}
		return adminsock.Call(cf.path(), "pending.discard", map[string]int64{"id": id}, nil)
	}
	var edits []files.Edit
	if err := adminsock.Call(cf.path(), "pending.list", nil, &edits); err != nil {
		return err
	}
	if *asJSON {
		return printJSON(edits)
	}
	if len(edits) == 0 {
		fmt.Println("Every change is on its gateway.")
		return nil
	}
	table("ID\tSTATE\tCHANGE\tPATH\tREASON", func(w *tabwriter.Writer) {
		for _, e := range edits {
			fmt.Fprintf(w, "%d\t%s\t%s\t%s\t%s\n", e.ID, e.State, e.Kind, e.Path, e.Error)
		}
	})
	return nil
}

func waitCommand(args []string) error {
	fs, cf := newFlagSet("wait")
	timeout := fs.Duration("timeout", 10*time.Minute, "how long to wait at most")
	if err := fs.Parse(args); err != nil {
		return err
	}
	paths, err := absolutePaths(fs.Args())
	if err != nil || len(paths) == 0 {
		return errors.New("usage: synctrain-files wait [--timeout 10m] <path>…")
	}
	for _, p := range paths {
		if err := adminsock.Call(cf.path(), "wait", map[string]any{"path": p, "timeout": *timeout}, nil); err != nil {
			return fmt.Errorf("%s: %w", p, err)
		}
	}
	return nil
}
