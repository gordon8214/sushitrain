// Copyright (C) 2026 Gordon
// SPDX-License-Identifier: MPL-2.0

// Package mcpapi defines the engine-independent, allowlisted MCP contract.
package mcpapi

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
)

const MaxResult = 1 << 20
const MaxContent = 64 << 10

type Argument struct {
	Type        string `json:"type"`
	Description string `json:"description"`
	Required    bool   `json:"required"`
}
type Route struct {
	Method    string
	Fields    map[string]string
	Constants map[string]any
}
type Tool struct {
	Name        string              `json:"name"`
	Description string              `json:"description"`
	ReadOnly    bool                `json:"readOnly"`
	Destructive bool                `json:"destructive"`
	Long        bool                `json:"long"`
	Arguments   map[string]Argument `json:"arguments"`
	Synchronize Route               `json:"-"`
	Files       Route               `json:"-"`
}
type Call struct {
	Name      string         `json:"name"`
	Arguments map[string]any `json:"arguments"`
}
type Error struct {
	Code      string `json:"code"`
	Message   string `json:"message"`
	Retryable bool   `json:"retryable"`
}

func (e *Error) Error() string            { return e.Message }
func Failure(code, message string) *Error { return &Error{Code: code, Message: message} }

type Result struct {
	Data       any    `json:"data,omitempty"`
	Error      *Error `json:"error,omitempty"`
	NextCursor string `json:"nextCursor,omitempty"`
	Truncated  bool   `json:"truncated,omitempty"`
}

// A compact declaration still produces a distinct, strict schema per tool.
// Field names after '=' are native adapter fields; '?' marks optional inputs.
func route(method, fields string) Route {
	r := Route{Method: method, Fields: map[string]string{}, Constants: map[string]any{}}
	for _, field := range strings.Fields(fields) {
		parts := strings.SplitN(strings.TrimSuffix(field, "?"), "=", 2)
		if len(parts) == 2 {
			r.Fields[parts[0]] = parts[1]
		}
	}
	return r
}
func action(value string) Route {
	r := route("engine.action", "folder=Folder? device=Device? id=ID? path=Path?")
	r.Constants["Action"] = value
	return r
}

func Catalog() []Tool {
	var out []Tool
	add := func(name, description, fields string, read, destructive, long bool, sync, files Route) {
		t := Tool{Name: "synctrain_" + name, Description: description, ReadOnly: read, Destructive: destructive, Long: long, Arguments: map[string]Argument{}, Synchronize: sync, Files: files}
		for _, spec := range strings.Fields(fields) {
			optional := strings.HasSuffix(spec, "?")
			spec = strings.TrimSuffix(spec, "?")
			pair := strings.SplitN(spec, ":", 2)
			kind := "string"
			if len(pair) == 2 {
				kind = pair[1]
			}
			t.Arguments[pair[0]] = Argument{Type: kind, Required: !optional, Description: descriptions[pair[0]]}
		}
		if read {
			t.Arguments["cursor"] = Argument{Type: "string", Description: "Opaque continuation from the previous result with the same arguments."}
			t.Arguments["limit"] = Argument{Type: "integer", Description: "Page size, 1–500; default 100."}
		}
		for _, r := range []Route{t.Synchronize, t.Files} {
			for key := range r.Fields {
				if _, ok := t.Arguments[key]; !ok {
					delete(r.Fields, key)
				}
			}
		}
		if name == "folder_add" {
			field := t.Arguments["path"]
			field.Description = "Absolute native path of the synchronized folder (omit to use the app default)."
			t.Arguments["path"] = field
		}
		out = append(out, t)
	}
	none := Route{}
	add("capabilities", "Inspect platform, mode, enablement, runtime capabilities, and setup instructions.", "", true, false, false, none, none)
	add("status", "Inspect identity, engine health and restart requirements, or Files service state.", "", true, false, false, route("engine.status", ""), route("status", ""))
	add("pause_policy_get", "Inspect automatic pause policies and the current effective pause reason.", "", true, false, false, route("policy.get", ""), none)
	add("pause_policy_set", "Set metered, cellular, and power-saver pause policies; manual pauses remain independent.", "metered:boolean cellular:boolean powerSaver:boolean", false, false, false, route("policy.save", "metered=Metered cellular=Cellular powerSaver=PowerSaver"), none)
	add("resource_priority_set", "Set native synchronization resource priority: normal, utility, or background.", "priority", false, false, false, route("resources.set", "priority=Priority"), none)
	add("diagnostics", "Inspect discovery, resolved listeners, relay/NAT settings, and connection health without credentials.", "", true, false, false, route("engine.diagnostics", ""), none)
	add("folder_differences", "Inspect local/global counts, needed paths, and receive-only changes.", "folder", true, false, false, route("engine.differences", "folder=Folder"), none)
	add("version_snapshots_list", "List platform-history metadata completeness and exact snapshot validators.", "folder path", true, false, false, route("engine.snapshots", "folder=Folder path=Path"), none)
	add("version_snapshot_download", "Export a complete selected platform-history snapshot archive, retaining native metadata.", "folder path version token destination", false, false, true, route("engine.exportSnapshot", "folder=Folder path=Path version=ID token=Token destination=Name"), none)
	add("statistics", "Inspect transfer rates, totals, and synchronization progress.", "", true, false, false, route("engine.stats", ""), route("mcp.files.storage", ""))
	add("logging_facilities", "Inspect native log facilities and their current levels.", "", true, false, false, action("logFacilities"), none)
	logRoute := action("logLevel")
	logRoute.Fields["facility"] = "Name"
	logRoute.Fields["level"] = "ID"
	add("logging_level_set", "Set a log facility's level until restart: debug, info, warn, or error.", "facility level", false, false, false, logRoute, none)
	add("logging_levels_reset", "Reset runtime logging overrides to native defaults.", "", false, false, false, action("resetLogLevels"), none)
	add("transfers_list", "Inspect active and resumable on-demand transfers without lease credentials.", "", true, false, false, none, route("mcp.files.transfers", ""))
	add("logs", "Read a bounded, credential-redacted page of engine log messages.", "after?", true, false, false, route("engine.logs", "after=Offset"), none)
	add("folders_list", "List configured folders, sharing, errors, and current state.", "", true, false, false, route("engine.folders", ""), route("mcp.files.folders", ""))
	add("folder_add", "Add a synchronized folder or attach a gateway folder in Files mode. Use id/path for Synchronize; serviceId/id for Files.", "id path? name? type? device? password? serviceId?", false, false, false, route("engine.addFolder", "id=ID path=Path name=Name type=Action device=Device password=Password"), route("mcp.files.add", "serviceId=ServiceID id=FolderID"))
	add("folder_remove", "Remove a folder configuration or detach a Files folder. File deletion is a separate operation; preserve unsent edits.", "folder", false, true, true, action("remove"), route("mcp.files.remove", "folder=Item"))
	add("folder_share", "Share or unshare a synchronized folder with a device.", "folder device enabled:boolean password?", false, true, false, route("engine.share", "folder=Folder device=Device enabled=Enabled password=Password"), none)
	for _, entity := range []string{"folder", "device"} {
		for _, a := range []string{"pause", "resume"} {
			add(entity+"_"+a, a+" a synchronized "+entity+" using the host's manual pause policy.", entity, false, false, false, action(a), none)
		}
	}
	add("devices_list", "List paired Syncthing devices and their connection state.", "", true, false, false, route("engine.devices", ""), none)
	add("device_add", "Add a Syncthing device by its full device ID.", "id", false, false, false, route("engine.addDevice", "id=ID"), none)
	add("device_remove", "Remove a Syncthing device and its sharing configuration.", "device", false, true, false, action("remove"), none)
	add("invitations_list", "List pending device invitations.", "", true, false, false, route("engine.pending", ""), none)
	add("folder_invitations_list", "List folder invitations from a configured device. Accept with folder_add and folder_share.", "device", true, false, false, route("engine.pendingFolders", "device=Device"), none)
	for _, a := range []string{"dismiss", "ignore", "unignore"} {
		add("invitation_"+a, a+" a pending Syncthing device invitation.", "id", false, a == "ignore", false, action(a), none)
	}
	add("ignored_devices_list", "List ignored device invitations.", "", true, false, false, route("engine.ignored", ""), none)
	for _, a := range []string{"pauseAll", "resumeAll", "restart", "rescanAll"} {
		name := map[string]string{"pauseAll": "pause_all", "resumeAll": "resume_all", "restart": "engine_restart", "rescanAll": "rescan_all"}[a]
		add(name, "Request "+name+" through the existing engine lifecycle and pause coordination.", "", false, false, true, action(a), none)
	}
	add("rescan", "Rescan a synchronized folder or contained relative path.", "folder path?", false, false, true, action("rescan"), none)
	add("ignores_get", "Inspect a folder's ignore patterns.", "folder", true, false, false, action("ignores"), none)
	r := action("saveIgnores")
	r.Fields["patterns"] = "Path"
	add("ignores_set", "Replace a folder's ignore patterns using native validation.", "folder patterns", false, true, false, r, none)
	add("folder_errors", "Inspect failed items in a synchronized folder.", "folder", true, false, false, action("failed"), none)
	add("deleted_files_list", "Inspect deleted items retained by the engine.", "folder", true, false, false, action("deleted"), none)
	add("folder_revert", "Revert local changes in a receive-only folder. This can replace or delete local files.", "folder", false, true, true, action("revert"), none)
	add("folder_override", "Override remote changes using a send-only folder's local state.", "folder", false, true, true, action("override"), none)
	add("files_browse", "Browse a folder-relative directory, or a Files parent item ID. Results contain stable item and revision identifiers.", "folder? path? parent:integer?", true, false, false, route("engine.browse", "folder=Folder path=Path"), route("mcp.files.children", "parent=Parent"))
	add("files_search", "Search indexed file names in a folder or Files subtree.", "query folder? parent:integer?", true, false, true, route("engine.search", "query=Name folder=Folder"), route("mcp.files.search", "query=Name parent=Parent"))
	add("file_inspect", "Inspect file metadata, local/global state, content availability, and conflict status.", "folder? path? item:integer?", true, false, false, route("engine.inspect", "folder=Folder path=Path"), route("mcp.files.inspect", "item=Item"))
	add("file_read", "Read at most 64 KiB, as UTF-8 or base64, without returning authenticated URLs.", "folder? path? item:integer? offset:integer? bytes:integer?", true, false, false, route("engine.read", "folder=Folder path=Path offset=Offset bytes=Limit"), route("mcp.files.read", "item=Item offset=Offset bytes=Limit"))
	add("file_download", "Save a copy to an explicit absolute destination, refusing unsafe replacement.", "destination folder? path? item:integer? node:integer? token?", false, false, true, route("engine.exportCurrent", "folder=Folder path=Path destination=Name"), route("mcp.files.download", "item=Item node=Node destination=Path"))
	add("directory_create", "Create a directory inside an accessible folder.", "name folder? path? parent:integer?", false, false, false, route("engine.mkdir", "folder=Folder path=Path name=Name"), route("mcp.files.mkdir", "parent=Parent name=Name"))
	add("file_capture_restore", "Capture the current target revision before replacing synchronized content. Pass the returned token to file_replace.", "folder path", true, false, false, route("engine.captureRestore", "folder=Folder path=Path"), none)
	add("file_replace", "Replace existing file content from a local source, preserving history. Synchronize requires a captured target token; Files requires the current node revision.", "source folder? path? item:integer? node:integer? token?", false, true, true, route("engine.replace", "folder=Folder path=Path source=Name token=Token"), route("mcp.files.replace", "item=Item node=Node source=Path"))
	add("file_import", "Import a local source into a folder using the existing write/outbox path.", "source name? folder? path? parent:integer?", false, false, true, route("engine.import", "folder=Folder path=Path source=ID name=Name"), route("mcp.files.import", "parent=Parent source=Path name=Name"))
	add("file_rename", "Rename a file. Files mode requires the node revision returned by browsing.", "name folder? path? item:integer? node:integer? token?", false, true, true, route("engine.rename", "folder=Folder path=Path name=Name token=Token"), route("mcp.files.rename", "item=Item node=Node name=NewName"))
	add("file_move", "Move a file within its folder; destination is a contained relative path.", "destination folder? path? item:integer? node:integer? token?", false, true, true, route("engine.move", "folder=Folder path=Path destination=Name token=Token"), route("mcp.files.move", "item=Item node=Node destination=Path"))
	add("file_delete", "Delete a file through native deletion and permissions. This propagates to other devices.", "folder? path? item:integer? node:integer?", false, true, true, action("deleteFile"), route("mcp.files.delete", "item=Item node=Node"))
	add("versions_list", "List previous versions, including platform metadata and source identity where available.", "folder? path? item:integer?", true, false, false, route("engine.versions", "folder=Folder path=Path"), route("mcp.files.versions", "item=Item"))
	add("version_restore", "Restore the selected version, preserving current content and rejecting stale targets or pending edits.", "version token folder? path? item:integer?", false, true, true, route("engine.restoreVersion", "folder=Folder path=Path version=ID token=Token"), route("mcp.files.versionRestore", "item=Item version=Version token=Token"))
	add("version_download", "Export the selected archived version to an absolute destination.", "version token destination folder? path? item:integer?", false, false, true, route("engine.exportVersion", "folder=Folder path=Path version=ID token=Token destination=Name"), route("mcp.files.versionExport", "item=Item version=Version token=Token destination=Path"))
	add("version_copy", "Save a previous version as a new sibling file in Files mode.", "item:integer version token?", false, false, true, none, route("mcp.files.versionCopy", "item=Item version=Version token=Token"))
	for _, op := range []struct {
		name, method, fields string
		read, destructive    bool
	}{
		{"history_sources_list", "history.sources", "", true, false},
		{"history_source_pair", "history.pair", "link=Link", false, false},
		{"history_source_enable", "history.enable", "id=ID enabled=Enabled", false, false},
		{"history_source_forget", "history.forget", "id=ID", false, true},
		{"history_versions_list", "history.list", "folder=Folder path=Path", true, false},
		{"history_version_download", "history.export", "id=ID folder=Folder path=Path version=Version destination=Destination token=ExpectedTag", false, false},
	} {
		fields := ""
		for _, f := range strings.Fields(op.fields) {
			key := strings.SplitN(f, "=", 2)[0]
			if key == "enabled" {
				key += ":boolean"
			}
			fields += key + " "
		}
		r := route(op.method, op.fields)
		add(op.name, "Manage independent read-only history gateways: "+op.name+".", fields, op.read, op.destructive, !op.read, r, r)
	}
	add("gateways_list", "Inspect paired gateways and reachability, without credentials.", "", true, false, false, route("service.status", ""), route("mcp.files.gateways", ""))
	add("gateway_pair", "Pair Files mode with a gateway using a supplied pairing link; waits for approval on the gateway.", "link", false, false, true, none, route("mcp.files.pair", "link=Link"))
	add("gateway_forget", "Forget a paired gateway after its folders and pending edits are safely handled.", "serviceId", false, true, false, none, route("mcp.files.forget", "serviceId=ServiceID"))
	add("gateway_folders_list", "List folders a gateway permits this Files client to access.", "serviceId", true, false, false, none, route("mcp.files.available", "serviceId=ServiceID"))
	add("gateway_settings_get", "Inspect remote file service settings and runtime state.", "", true, false, false, route("service.status", ""), none)
	add("gateway_settings_set", "Update the remote file service's supported settings. Fixed deployment flags remain authoritative.", "enabled:boolean listen? publicHost? publicPort:integer?", false, true, true, route("service.set", "enabled=enabled listen=listen publicHost=publicHost publicPort=publicPort"), none)
	for _, op := range []struct {
		name, method, fields, args string
		read, destructive          bool
	}{
		{"gateway_exports_list", "exports.list", "", "", true, false},
		{"gateway_export_set", "exports.set", "folder=folderID enabled=allowWrite", "folder enabled:boolean", false, true},
		{"gateway_export_remove", "exports.remove", "folder=folderID", "folder", false, true},
		{"gateway_clients_list", "clients.list", "", "", true, false},
		{"gateway_client_revoke", "clients.revoke", "id=clientID", "id", false, true},
		{"gateway_permission_set", "clients.permission", "id=clientID folder=folderID access=access", "id folder access", false, true},
		{"gateway_pairing_requests", "pairing.pending", "", "", true, false},
		{"gateway_pairing_approve", "pairing.approve", "id=requestID grants=grants", "id grants:object", false, true},
		{"gateway_pairing_deny", "pairing.deny", "id=requestID", "id", false, false},
	} {
		add(op.name, "Manage the local gateway: "+op.name+".", op.args, op.read, op.destructive, false, route(op.method, op.fields), none)
	}
	add("file_pin", "Keep or release on-demand content through the platform provider.", "item:integer node:integer? keep:boolean", false, false, true, none, route("mcp.files.keep", "item=Item node=Node keep=Keep"))
	add("file_materialize", "Fetch an on-demand file and wait for current content availability.", "item:integer", false, false, true, none, route("mcp.files.wait", "item=Item"))
	add("pending_changes_list", "Inspect queued local edits and transfer completion or conflict state.", "", true, false, false, none, route("mcp.files.pending.list", ""))
	add("pending_change_export", "Export an unsent edit to a safe absolute destination.", "id destination", false, false, true, none, route("mcp.files.pending.export", "id=ID destination=Dest"))
	add("pending_change_discard", "Discard a queued local edit. This gives up that local change.", "id", false, true, false, none, route("mcp.files.pending.discard", "id=ID"))
	add("storage_get", "Inspect on-demand cache usage, pinned content, and budget.", "", true, false, false, none, route("mcp.files.storage", ""))
	add("storage_budget_set", "Set the disposable content cache budget in bytes; pinned files and unsent edits are preserved.", "bytes:integer", false, false, false, none, route("mcp.files.storage.budget", "bytes=Bytes"))
	add("settings_get", "Open a validated settings section. Returns field types, choices, secrets redacted, and an expiring editor token. Sections: engine, connections, bandwidth, gui, proxy, defaults/folder, defaults/device, defaults/versioning, defaults/ignores, folder/ID, device/ID, versioning/ID, advanced/options, advanced/ldap.", "section", true, false, false, route("settings.get", "section=Section"), route("mcp.preferences.get", "section=Section"))
	add("settings_save", "Save only explicitly supplied fields using an editor token and native validation; values use the field's string representation. Blank secret values preserve existing secrets.", "token values:object", false, true, false, route("settings.save", "token=Token values=Values"), route("mcp.preferences.save", "token=Token values=Values"))
	add("settings_close", "Release an unused settings editor.", "token", false, false, false, route("settings.close", "token=Token"), route("mcp.preferences.close", "token=Token"))
	for _, op := range []string{"get", "list", "cancel"} {
		fields := "id"
		if op == "list" {
			fields = ""
		}
		add("operation_"+op, "Inspect or cancel a host-owned MCP operation. Cancellation does not roll back committed changes.", fields, op != "cancel", op == "cancel", false, none, none)
	}
	return out
}

var descriptions = map[string]string{
	"folder": "Syncthing folder ID in Synchronize; local folder identity in Files (from folders_list).",
	"device": "Full Syncthing device ID.", "path": "Contained folder-relative path; never an absolute path or '..'.",
	"id": "Stable identifier returned by the related discovery tool.", "item": "Local Files item ID returned by browsing.",
	"node":    "Files node revision returned by browsing; required for mutations of existing items.",
	"token":   "Opaque revision/editor token returned by discovery. Never invent this value.",
	"version": "Exact version ID returned by versions_list.", "destination": "Explicit destination; absolute for exports, folder-relative for moves.",
	"source": "Absolute path of a local file to import.", "serviceId": "Gateway service identity returned by gateways_list.",
	"link":   "Sensitive synctrain+pair:// invitation; never echoed in results or logs.",
	"values": "Map of field names from settings_get to their string values; untouched fields are preserved.",
	"grants": "Map of folder IDs to read/write permissions approved for the pending client.",
	"offset": "Nonnegative content byte offset.", "bytes": "Content bytes (1–65536), or cache budget bytes as documented.",
}

func Find(name string) (Tool, bool) {
	for _, t := range Catalog() {
		if t.Name == name {
			return t, true
		}
	}
	return Tool{}, false
}
func (t Tool) Validate(args map[string]any) error {
	for k, a := range t.Arguments {
		if _, ok := args[k]; a.Required && !ok {
			return Failure("invalid_arguments", "Missing argument: "+k)
		}
	}
	for k, v := range args {
		a, ok := t.Arguments[k]
		if !ok {
			return Failure("invalid_arguments", "Unknown argument: "+k)
		}
		valid := false
		switch a.Type {
		case "string":
			_, valid = v.(string)
		case "boolean":
			_, valid = v.(bool)
		case "object":
			_, valid = v.(map[string]any)
		case "integer":
			n, ok := v.(float64)
			valid = ok && n == float64(int64(n)) && n >= 0 && n <= 9007199254740991
		}
		if !valid {
			return Failure("invalid_arguments", fmt.Sprintf("%s must be %s", k, a.Type))
		}
		if s, ok := v.(string); ok && len(s) > MaxContent {
			return Failure("invalid_arguments", "Argument too large: "+k)
		}
		if k == "token" && a.Required && strings.TrimSpace(v.(string)) == "" {
			return Failure("invalid_arguments", "Supply the selected version's nonempty validator.")
		}
	}
	if n, ok := args["limit"].(float64); ok && (n < 1 || n > 500) {
		return Failure("invalid_arguments", "limit must be 1–500")
	}
	return nil
}
func (t Tool) Schema() map[string]any {
	properties := map[string]any{}
	required := []string{}
	for name, a := range t.Arguments {
		p := map[string]any{"type": a.Type, "description": a.Description}
		if a.Type == "string" {
			p["maxLength"] = MaxContent
			if name == "token" && a.Required {
				p["minLength"] = 1
				p["pattern"] = `\S`
			}
		}
		if a.Type == "integer" {
			p["minimum"] = 0
			p["maximum"] = 9007199254740991
		}
		if name == "limit" {
			p["minimum"] = 1
			p["maximum"] = 500
		}
		if name == "values" {
			p["additionalProperties"] = map[string]any{"type": "string"}
		}
		properties[name] = p
		if a.Required {
			required = append(required, name)
		}
	}
	return map[string]any{"type": "object", "properties": properties, "required": required, "additionalProperties": false}
}
func (r Route) Params(args map[string]any) json.RawMessage {
	p := map[string]any{}
	for k, v := range r.Constants {
		p[k] = v
	}
	for k, to := range r.Fields {
		if v, ok := args[k]; ok {
			if to == "Item" || to == "Parent" || to == "Offset" {
				if text, ok := v.(string); ok {
					if n, err := strconv.ParseInt(text, 10, 64); err == nil {
						v = n
					}
				}
			}
			p[to] = v
		}
	}
	b, _ := json.Marshal(p)
	return b
}
