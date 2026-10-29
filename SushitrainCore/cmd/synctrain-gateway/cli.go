// Copyright (C) 2026 Gordon
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this file,
// You can obtain one at https://mozilla.org/MPL/2.0/.

package main

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"text/tabwriter"
	"time"

	"t-shaped.nl/sushitrain/v2/gateway"
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
	if err := parseFlags(fs, args); err != nil {
		return err
	}
	var st gateway.Status
	if err := gateway.AdminCall(cf.path(), "status", nil, &st); err != nil {
		return err
	}
	if *asJSON {
		return printJSON(st)
	}
	fmt.Printf("Service %s (%s), listening on %s\n", st.ServiceName, st.ServiceID, st.Address)
	fmt.Printf("Syncthing device %s\n", st.DeviceID)
	if st.SPKI != "" {
		fmt.Printf("Certificate public key (SPKI SHA-256): %s\n", st.SPKI)
	}
	fmt.Printf("Authority generation %d, %d clients, %d pending operations, %d pending pairings\n",
		st.AuthorityGeneration, st.Clients, st.PendingOperations, st.PendingPairings)
	fmt.Printf("Content cache: %d of %d MiB\n", st.ContentCacheBytes>>20, st.ContentCacheQuota>>20)
	table("FOLDER\tLABEL\tACCESS\tSTATE\tITEMS", func(w *tabwriter.Writer) {
		for _, e := range st.Exports {
			access := "read"
			if e.AllowWrite {
				access = "write"
			}
			fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%d\n", printable(e.FolderID), printable(e.Label), access, e.State, e.Items)
		}
	})
	return nil
}

// folderGrants collects repeated --folder ID:read|write options
type folderGrants map[string]string

func (g folderGrants) String() string { return "" }

// Set takes ID, ID:read or ID:write; folder IDs may contain colons themselves
func (g folderGrants) Set(value string) error {
	id, access := value, "read"
	if i := strings.LastIndex(value, ":"); i >= 0 {
		id, access = value[:i], value[i+1:]
	}
	if id == "" || (access != "read" && access != "write") {
		return fmt.Errorf("%q: give the folder as ID:read or ID:write", value)
	}
	g[id] = access
	return nil
}

func pairCommand(args []string) error {
	fs, cf := newFlagSet("pair")
	grants := folderGrants{}
	fs.Var(grants, "folder", "grant access to a folder: ID:read or ID:write (repeatable); read includes previous versions")
	yes := fs.Bool("yes", false, "approve the first client that submits the code without asking")
	png := fs.String("qr-png", "", "also write the pairing code as a PNG image to this file")
	timeout := fs.Duration("timeout", 10*time.Minute, "how long to wait for a client (at most until the code expires)")
	if err := parseFlags(fs, args); err != nil {
		return err
	}
	socket := cf.path()

	var session gateway.PairingSession
	if err := gateway.AdminCall(socket, "pairing.start", nil, &session); err != nil {
		return err
	}
	fmt.Println("Scan this code in Synctrain (Settings › Remote folders › Pair), or enter the link:")
	fmt.Println()
	if err := printQR(os.Stdout, session.URL); err != nil {
		return err
	}
	fmt.Println()
	fmt.Println(session.URL)
	fmt.Printf("\nThe code works once, until %s.\n", session.Expires.Local().Format(time.Kitchen))
	if *png != "" {
		if err := writeQRPNG(*png, session.URL); err != nil {
			return err
		}
	}

	deadline := time.Now().Add(*timeout)
	expiresFirst := !session.Expires.IsZero() && session.Expires.Before(deadline)
	if expiresFirst {
		deadline = session.Expires
	}
	request, err := waitForPairing(socket, session.SessionID, deadline)
	if errors.Is(err, errWaitedTooLong) {
		if expiresFirst {
			return errors.New("the code expired before a client submitted it; run pair again for a new code")
		}
		return fmt.Errorf("no client submitted the code within %s", *timeout)
	}
	if err != nil {
		return err
	}
	fmt.Printf("\n%q (%s, client %s) wants to pair.\n", request.ClientName, printable(request.Platform),
		printable(request.ClientID))
	if request.Replaces != "" {
		fmt.Printf("It uses the identity of the paired client %q, whose credential approving it replaces.\n",
			request.Replaces)
	}

	if len(grants) == 0 {
		var exports []gateway.Export
		if err := gateway.AdminCall(socket, "exports.list", nil, &exports); err != nil {
			return err
		}
		if len(exports) == 0 {
			fmt.Println("No folders are exported yet; the client can see nothing until you grant folders with \"clients permission\".")
		}
		for _, e := range exports {
			access := "read"
			if e.AllowWrite {
				access = "write"
			}
			grants[e.FolderID] = access
		}
	}
	fmt.Println("It will get access to:")
	for id, access := range grants {
		fmt.Printf("  %s (%s)\n", id, access)
	}
	if !*yes {
		fmt.Print("Approve? [y/N] ")
		answer, _ := bufio.NewReader(os.Stdin).ReadString('\n')
		if a := strings.ToLower(strings.TrimSpace(answer)); a != "y" && a != "yes" {
			if err := gateway.AdminCall(socket, "pairing.deny", map[string]string{"requestID": request.RequestID}, nil); err != nil {
				return err
			}
			fmt.Println("Denied.")
			return exitError(1)
		}
	}
	if err := gateway.AdminCall(socket, "pairing.approve", map[string]any{"requestID": request.RequestID,
		"grants": grants}, nil); err != nil {
		return err
	}
	fmt.Println("Approved. The client finishes pairing on its own.")
	return nil
}

// The pairing.wait method's answer when no request came while it waited
const noPairingRequestYet = "no pairing request yet"

var errWaitedTooLong = errors.New("no pairing request before the deadline")

// waitForPairing waits until deadline for a client to submit the code of a pairing session
func waitForPairing(socket, sessionID string, deadline time.Time) (gateway.PendingPairing, error) {
	type result struct {
		request gateway.PendingPairing
		err     error
	}
	for {
		remaining := time.Until(deadline)
		if remaining <= 0 {
			return gateway.PendingPairing{}, errWaitedTooLong
		}
		// The service answers when a request arrives, or after a while without one; a call still waiting at the
		// deadline is abandoned
		done := make(chan result, 1)
		go func() {
			var r result
			r.err = gateway.AdminCall(socket, "pairing.wait", map[string]string{"sessionID": sessionID}, &r.request)
			done <- r
		}()
		timer := time.NewTimer(remaining)
		select {
		case r := <-done:
			timer.Stop()
			if r.err == nil {
				return r.request, nil
			}
			if !strings.Contains(r.err.Error(), noPairingRequestYet) {
				return gateway.PendingPairing{}, r.err
			}
		case <-timer.C:
			return gateway.PendingPairing{}, errWaitedTooLong
		}
	}
}

func clientsCommand(args []string) error {
	fs, cf := newFlagSet("clients")
	revoke := fs.String("revoke", "", "revoke the client with this ID")
	client := fs.String("client", "", "client ID, with --folder and --access")
	folder := fs.String("folder", "", "folder ID, with --client and --access")
	access := fs.String("access", "", "read (including previous versions), write, or none, with --client and --folder")
	asJSON := fs.Bool("json", false, "print JSON")
	if err := parseFlags(fs, args); err != nil {
		return err
	}
	socket := cf.path()
	switch {
	case *revoke != "":
		return gateway.AdminCall(socket, "clients.revoke", map[string]string{"clientID": *revoke}, nil)
	case *client != "":
		a := *access
		switch a {
		case "read", "write":
		case "none":
			a = ""
		default:
			return errors.New("--client takes --folder and --access read, write or none")
		}
		if *folder == "" {
			return errors.New("--client takes --folder and --access read, write or none")
		}
		return gateway.AdminCall(socket, "clients.permission", map[string]string{"clientID": *client,
			"folderID": *folder, "access": a}, nil)
	}
	var clients []gateway.Client
	if err := gateway.AdminCall(socket, "clients.list", nil, &clients); err != nil {
		return err
	}
	if *asJSON {
		return printJSON(clients)
	}
	table("CLIENT\tNAME\tPLATFORM\tLAST SEEN\tSTATE\tFOLDERS", func(w *tabwriter.Writer) {
		for _, c := range clients {
			state := "paired"
			if c.Revoked {
				state = "revoked"
			}
			seen := "never"
			if !c.LastSeen.IsZero() {
				seen = c.LastSeen.Local().Format(time.DateTime)
			}
			var folders []string
			for id, a := range c.Permissions {
				folders = append(folders, id+":"+a)
			}
			fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\n", printable(c.ClientID), printable(c.Name), printable(c.Platform),
				seen, state, printable(strings.Join(folders, ",")))
		}
	})
	return nil
}

func exportsCommand(args []string) error {
	fs, cf := newFlagSet("exports")
	add := fs.String("add", "", "export this folder")
	write := fs.Bool("write", false, "with --add: allow clients with write access to change it")
	remove := fs.String("remove", "", "stop exporting this folder")
	asJSON := fs.Bool("json", false, "print JSON")
	if err := parseFlags(fs, args); err != nil {
		return err
	}
	socket := cf.path()
	switch {
	case *add != "":
		return gateway.AdminCall(socket, "exports.set", gateway.Export{FolderID: *add, AllowWrite: *write}, nil)
	case *remove != "":
		return gateway.AdminCall(socket, "exports.remove", gateway.Export{FolderID: *remove}, nil)
	}
	var exports []gateway.Export
	if err := gateway.AdminCall(socket, "exports.list", nil, &exports); err != nil {
		return err
	}
	if *asJSON {
		return printJSON(exports)
	}
	table("FOLDER\tACCESS", func(w *tabwriter.Writer) {
		for _, e := range exports {
			access := "read"
			if e.AllowWrite {
				access = "write"
			}
			fmt.Fprintf(w, "%s\t%s\n", printable(e.FolderID), access)
		}
	})
	return nil
}

func devicesCommand(args []string) error {
	fs, cf := newFlagSet("devices")
	add := fs.String("add", "", "add (or update) the device with this ID")
	name := fs.String("name", "", "with --add: the device's name")
	address := fs.String("address", "", "with --add: comma-separated addresses (default dynamic)")
	remove := fs.String("remove", "", "remove the device with this ID")
	asJSON := fs.Bool("json", false, "print JSON")
	if err := parseFlags(fs, args); err != nil {
		return err
	}
	socket := cf.path()
	switch {
	case *add != "":
		params := map[string]any{"id": *add, "name": *name}
		var addresses []string
		for _, a := range strings.Split(*address, ",") {
			if a = strings.TrimSpace(a); a != "" {
				addresses = append(addresses, a)
			}
		}
		if len(addresses) > 0 {
			params["addresses"] = addresses
		}
		return gateway.AdminCall(socket, "devices.add", params, nil)
	case *remove != "":
		return gateway.AdminCall(socket, "devices.remove", map[string]string{"id": *remove}, nil)
	}
	var devices []map[string]any
	if err := gateway.AdminCall(socket, "devices.list", nil, &devices); err != nil {
		return err
	}
	if *asJSON {
		return printJSON(devices)
	}
	table("DEVICE\tNAME\tCONNECTED\tPAUSED", func(w *tabwriter.Writer) {
		for _, d := range devices {
			fmt.Fprintf(w, "%v\t%s\t%v\t%v\n", d["id"], printable(fmt.Sprint(d["name"])), d["connected"], d["paused"])
		}
	})
	return nil
}

func foldersCommand(args []string) error {
	fs, cf := newFlagSet("folders")
	share := fs.String("share", "", "share this folder with --device")
	unshare := fs.String("unshare", "", "stop sharing this folder with --device")
	device := fs.String("device", "", "device ID, with --share or --unshare")
	asJSON := fs.Bool("json", false, "print JSON")
	if err := parseFlags(fs, args); err != nil {
		return err
	}
	socket := cf.path()
	if (*share != "" || *unshare != "") && *device == "" {
		return errors.New("--share and --unshare take --device")
	}
	switch {
	case *share != "":
		return gateway.AdminCall(socket, "folders.share", map[string]any{"folderID": *share, "deviceID": *device}, nil)
	case *unshare != "":
		return gateway.AdminCall(socket, "folders.share", map[string]any{"folderID": *unshare, "deviceID": *device,
			"unshare": true}, nil)
	}
	var folders []map[string]any
	if err := gateway.AdminCall(socket, "folders.list", nil, &folders); err != nil {
		return err
	}
	if *asJSON {
		return printJSON(folders)
	}
	table("FOLDER\tLABEL\tTYPE\tSTATE\tPATH", func(w *tabwriter.Writer) {
		for _, f := range folders {
			fmt.Fprintf(w, "%s\t%s\t%v\t%v\t%s\n", printable(fmt.Sprint(f["id"])), printable(fmt.Sprint(f["label"])),
				f["type"], f["state"], printable(fmt.Sprint(f["path"])))
		}
	})
	return nil
}
