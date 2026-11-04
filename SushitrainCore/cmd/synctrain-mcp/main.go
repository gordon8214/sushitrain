// Copyright (C) 2026 Gordon
// SPDX-License-Identifier: MPL-2.0
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"t-shaped.nl/sushitrain/v2/adminsock"
	"t-shaped.nl/sushitrain/v2/mcpserver"
)

var version = "dev"

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "synctrain-mcp:", err)
		os.Exit(1)
	}
}
func run() error {
	socket := flag.String("socket", "", "explicit private Synctrain IPC endpoint (never starts a host)")
	enable := flag.Bool("enable", false, "enable full MCP management access on the running host and exit")
	disable := flag.Bool("disable", false, "disable MCP access on the running host and exit")
	showVersion := flag.Bool("version", false, "print version and exit")
	flag.Parse()
	if *showVersion {
		fmt.Println(version)
		return nil
	}
	if *enable && *disable {
		return fmt.Errorf("choose --enable or --disable")
	}
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt)
	defer cancel()
	endpoint := func() (string, error) {
		if *socket != "" {
			return *socket, nil
		}
		return defaultEndpoint(ctx)
	}
	if *enable || *disable {
		path, err := endpoint()
		if err != nil {
			return err
		}
		ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
		defer cancel()
		var result json.RawMessage
		if err := adminsock.CallContext(ctx, path, "mcp.settings", map[string]bool{"enabled": *enable}, &result); err != nil {
			return err
		}
		fmt.Println(string(result))
		return nil
	}
	return mcpserver.New(version, mcpserver.SocketBackend(endpoint)).Run(ctx, &mcp.StdioTransport{})
}
