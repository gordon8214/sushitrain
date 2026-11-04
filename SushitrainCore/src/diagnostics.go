// Copyright (C) 2026 Gordon
// SPDX-License-Identifier: MPL-2.0
package sushitrain

import (
	"encoding/json"
	"errors"
	"github.com/syncthing/syncthing/lib/protocol"
)

// DiagnosticsJSON snapshots discovery/listener state without exposing API keys.
func (c *Client) DiagnosticsJSON() ([]byte, error) {
	if c.EngineState() != EngineStateRunning || c.cfg() == nil {
		return nil, ErrStillLoading
	}
	options := c.cfg().Options()
	c.mutex.Lock()
	resolved := map[string][]string{}
	for name, addresses := range c.ResolvedListenAddresses {
		resolved[name] = append([]string{}, addresses...)
	}
	c.mutex.Unlock()
	return json.Marshal(map[string]any{"engineVersion": Version(), "listening": c.IsListening(), "configuredListeners": options.RawListenAddresses, "resolvedListeners": resolved, "globalDiscovery": options.GlobalAnnEnabled, "localDiscovery": options.LocalAnnEnabled, "discoveryServers": options.RawGlobalAnnServers, "relays": options.RelaysEnabled, "nat": options.NATEnabled, "connectedDevices": c.ConnectedPeerCount()})
}
func (f *Folder) FileStateJSON(name string) ([]byte, error) {
	if !finderPathAllowed(name) {
		return nil, errors.New("invalid contained file path")
	}
	internals := f.client.internals()
	if internals == nil {
		return nil, ErrStillLoading
	}
	local, hasLocal, err := internals.LocalFileInfo(f.FolderID, name)
	if err != nil {
		return nil, err
	}
	global, hasGlobal, err := internals.GlobalFileInfo(f.FolderID, name)
	if err != nil {
		return nil, err
	}
	summary := func(info protocol.FileInfo, found bool) any {
		if !found {
			return nil
		}
		return map[string]any{"size": info.Size, "modifiedNs": info.ModTime().UnixNano(), "deleted": info.Deleted, "invalid": info.IsInvalid(), "sequence": info.Sequence, "version": info.Version.String(), "type": info.Type.String()}
	}
	return json.Marshal(map[string]any{"local": summary(local, hasLocal), "global": summary(global, hasGlobal)})
}
