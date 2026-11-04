# Synctrain MCP

Synctrain includes `synctrain-mcp`, a local stdio Model Context Protocol server.
It lets an assistant use structured tools to inspect and manage the running app
or service. It does not launch Synctrain, start an engine, mount folders, or enable
access by itself.

MCP is disabled initially. **Enabling it grants full management access**, including
changing settings, deleting accessible files, restoring versions, and changing
sharing permissions. File changes can propagate to other devices. Folder
permissions, native validation, restore safeguards, and pending-edit protections
still apply. Disabling MCP rejects new requests and cancels interruptible work;
it does not undo changes already committed.

## Enable and connect

Start Synctrain in its configured mode, then open **Settings → MCP** on macOS or
Linux, or **Settings → Enable MCP** on Windows. Copy the client configuration
from that page. For clients that accept `mcpServers` JSON:

```json
{
  "mcpServers": {
    "synctrain": {
      "command": "/Applications/Synctrain.app/Contents/MacOS/synctrain-mcp"
    }
  }
}
```

On Windows use `synctrain-mcp.exe`, the package's execution alias. On Linux use
`/usr/bin/synctrain-mcp`. Neither a Node nor a Python runtime is required.
Clients start their own helper subprocess. Closing a client stops its helper and
leaves Synctrain running. There is no MCP TCP listener and no network MCP port to
forward. Each helper talks to the app/service belonging to the same OS user.
The Mac app additionally authenticates its signed helper's team and identifier.

For a headless gateway, run the helper as the service's account on that host.
Pass the gateway's private admin socket explicitly when it is not the desktop
account's default:

```sh
synctrain-mcp --socket /path/to/remote-file-service/admin.sock --enable
synctrain-mcp --socket /path/to/remote-file-service/admin.sock
synctrain-mcp --socket /path/to/remote-file-service/admin.sock --disable
```

`--enable` and `--disable` are local administrative commands and exit immediately.
They are not MCP tools: an assistant cannot grant itself access through MCP.
The host must already be running. `--socket` also accepts a Windows Synctrain
named pipe. The `SYNCTRAIN_MCP_SOCKET` environment variable is an alternative.

Remote assistants can use an SSH subprocess as their stdio connection:

```json
{
  "mcpServers": {
    "synctrain-gateway": {
      "command": "ssh",
      "args": ["account@gateway", "/usr/bin/synctrain-mcp", "--socket", "/path/to/admin.sock"]
    }
  }
}
```

Use SSH's existing authentication. Do not put gateway credentials or API keys in
MCP configuration. Gateway file clients retain their existing pairings and
credential stores; enabling MCP never enables the optional web GUI or REST API.

## Discovery and tool coverage

Start with `synctrain_capabilities`. Discovery works even when the host is stopped
or MCP is disabled. A running enabled host reports each tool's supported state
for its platform and mode. Unsupported tools remain discoverable and return a
structured reason instead of performing a fallback action.

Resources:

| URI | Contents |
| --- | --- |
| `synctrain://capabilities` | Current host mode, enablement, runtime tool availability, and limits |
| `synctrain://tools` | Complete tool catalog, argument types, and side-effect classifications |
| `synctrain://setup` | Executable locations, enablement, and connection examples |
| `synctrain://troubleshooting` | Recovery steps for common error codes |

All tool names begin with `synctrain_`. Tools have strict input schemas and a
structured result containing `data`, or `error` with `code`, `message`, and
`retryable`. Lists can additionally contain `nextCursor`. Read tools are marked
read-only; destructive tools explicitly describe deletion, replacement, or access
changes. Settings updates use the same validated editors as native controls.

| Feature area | Tools and workflows |
| --- | --- |
| Health and diagnostics | `status`, `diagnostics`, `statistics`, `logs`, `folder_errors`, `folder_differences`, `file_inspect`, `logging_facilities`, `logging_level_set`, `logging_levels_reset` |
| Folder management | `folders_list`, `folder_add`, `folder_remove`, `folder_share`, `folder_pause`, `folder_resume`; inspect/edit with `settings_get` and `settings_save` |
| Device management | `devices_list`, `device_add`, `device_remove`, `device_pause`, `device_resume`; inspect/edit with settings sections |
| Invitations | `invitations_list`, `folder_invitations_list`, `invitation_dismiss`, `invitation_ignore`, `invitation_unignore`, `ignored_devices_list`; accept with device/folder creation and sharing |
| Sync controls | `rescan`, `rescan_all`, `pause_all`, `resume_all`, `engine_restart`, `ignores_get`, `ignores_set`, `folder_revert`, `folder_override`, `deleted_files_list`, `pause_policy_get`, `pause_policy_set`, `resource_priority_set` |
| File access | `files_browse`, `files_search`, `file_inspect`, `file_read`, `file_download`, `file_materialize`, `file_pin` |
| File mutations | `directory_create`, `file_import`, `file_capture_restore`, `file_replace`, `file_rename`, `file_move`, `file_delete`; copy with download/export followed by import |
| Versions | `versions_list`, `version_restore`, `version_download`, `version_copy`, `version_snapshots_list`, `version_snapshot_download` |
| Independent history | `history_sources_list`, `history_source_pair`, `history_source_enable`, `history_source_forget`, `history_versions_list`, `history_version_download` |
| Gateway administration | `gateway_settings_get`, `gateway_settings_set`, `gateway_exports_list`, `gateway_export_set`, `gateway_export_remove`, `gateway_clients_list`, `gateway_client_revoke`, `gateway_permission_set`, `gateway_pairing_requests`, `gateway_pairing_approve`, `gateway_pairing_deny` |
| Gateway clients | `gateways_list`, `gateway_pair`, `gateway_forget`, `gateway_folders_list`, `folder_add`, `folder_remove` |
| On-demand work | `pending_changes_list`, `pending_change_export`, `pending_change_discard`, `storage_get`, `storage_budget_set`, `transfers_list`; native queues retain unsent edits |
| Settings and defaults | `settings_get`, `settings_save`, `settings_close`; sections described below |
| Long operations | `operation_list`, `operation_get`, `operation_cancel` |

## Platform and mode capabilities

“Sync” means Synchronize mode, including a headless Linux gateway. “Files” means
Files in Finder, Files in Explorer, or Linux Files. A gateway-backed client has
only its existing folder grants; it cannot administer the remote gateway's engine
or settings. To administer that host, connect its own MCP helper over SSH.

| Capability | Mac Sync | Mac Files | Windows Sync | Windows Files | Linux Sync/gateway | Linux Files |
| --- | --- | --- | --- | --- | --- | --- |
| Engine identity, devices, synchronization, networking | Yes | Unavailable: no engine | Yes | Unavailable: no engine | Yes | Unavailable: no engine |
| Engine settings, folder/device defaults, metadata/versioning | Yes | No local engine | Yes | No local engine | Yes | No local engine |
| Browse/search/read accessible files | Yes | Yes | Yes | Yes | Yes | Yes |
| File creation, replacement, rename/move, deletion | Yes | With write grants | Regular-file moves; directory moves require the native file manager | With write grants | Yes | With write grants |
| Native local version listing/restoration | When versioning is enabled | Through gateway | When versioning is enabled | Through gateway | When versioning is enabled | Through gateway |
| Platform snapshot metadata inspection/export | Complete macOS archives | Through gateway capabilities | Archive metadata may be unavailable | Through gateway capabilities | Complete archives when the source supports them | Through gateway capabilities |
| On-demand pinning/materialization and queued edits | Not an on-demand mode | Materialization only; Finder owns retention and completion | Not an on-demand mode | Cloud Files | Not an on-demand mode | Files service/FUSE |
| Host gateway exports, clients, grants, pairing approvals | Yes | No local gateway | Yes | No local gateway | Yes | No local gateway |
| Gateway listener settings | Native supported settings | No local gateway | Enable/disable; host fixes the listener | No local gateway | Supported settings; explicit deployment flags remain fixed | No local gateway |

The live capability resource is authoritative for individual operations. A stopped
engine, offline gateway, unsupported versioning type, revoked permission, or
missing File Provider location can make an otherwise supported operation
temporarily unavailable. Partial history-source failures do not hide other sources.

Engine settings sections include `engine`, `connections`, `bandwidth`, `gui`,
`proxy`, `defaults/folder`, `defaults/device`, `defaults/versioning`,
`defaults/ignores`, `folder/ID`, `device/ID`, `versioning/ID`, and native advanced
sections such as `advanced/options` and `advanced/ldap`. Basic editors include
`basic/folder/ID` and `basic/device/ID`. Open a section, change only intended
fields, save its token, or close it. Tokens expire after 30 minutes or an engine
restart. Blank secret fields preserve existing credentials.

On macOS, `app`/`files` exposes the supported application preferences. On Linux,
these sections expose shared desktop preferences, with conflict-aware persistence
and native UI refresh. Windows exposes engine editors and Files cache settings;
its startup approval and Explorer location remain native workflows. OS-granted
permissions, login startup approval, certificate picking, onboarding/takeover,
and workflows requiring a native review remain native interactions. Mode changes
remain native because they can stop services, detach an OS provider, and require
review of pending edits. Generate a new gateway pairing invitation in the native
UI or gateway CLI; MCP accepts an invitation and approves/denies pending clients,
but does not return invitation secrets. macOS supports materialization requests;
persistent Finder pinning remains a Finder action because the app has no native
API to set the user's retention policy. `file_pin` is unsupported there. Windows
synchronized directory moves remain a file-manager action because the core has no
exclusive directory rename primitive on that platform. The helper
does not replace configurations wholesale, edit arbitrary files outside explicit
file-operation sources/destinations, execute arbitrary internal RPC, or extract
stored credentials. Independent history sources are read-only. Download their selected version to
an explicit temporary destination, then use `file_capture_restore`/`file_replace`
in Synchronize mode, or `file_replace` with a current item/node in Files mode.
This composition deliberately exposes both the selected archive validator and the
current replacement target. Primary version restores use `version_restore`.
A full platform snapshot can be exported with metadata; replacing from an ordinary
local file does not claim to restore all snapshot metadata.

These are exclusions, not alternate transports.

## Identity, limits, and completion

- Use discovered folder/service/item IDs. Files mutations require the current
  node revision. Synchronize replacement requires the target token from
  `file_capture_restore`; rename/move uses the file identity from inspection.
  A selected version uses its listing's revision/entity tag as `token`.
- File paths are contained, forward-slash folder-relative paths. Creating a
  synchronized folder takes an absolute native path. Import sources and export
  destinations are explicit absolute paths. Moves stay inside their folder.
- Lists default to 100 items, with a maximum of 500 per page. Reuse `nextCursor`
  with the same tool/filter arguments. Snapshots expire after five minutes and
  have an 8 MiB per-list and 32 MiB aggregate budget.
- Results are bounded to 1 MiB. Inline reads are at most 64 KiB and return UTF-8
  or base64, byte offsets, and EOF state. Export/download large content to disk.
- Long tools return `operationId` and continue if the client disconnects. Use
  `operation_cancel` to cancel host-owned work. Poll `operation_get` for state, progress,
  and the final structured result. Completed records expire after one hour or a
  host restart. Native queued changes remain in their existing durable journals.
- `committed` means the gateway confirmed the mutation. `queued` means it is
  preserved locally and still awaiting its gateway. Inspect pending changes
  before retrying. A failed or canceled call can have already queued/committed
  effects; cancellation never promises rollback.
- Logs, settings, errors, and support results redact credentials, authorization
  headers, pairing invitations, and authenticated content URLs. Ordinary editor
  and revision tokens are retained because clients need them for coordination.

## Build and verify

The Mac build embeds and signs the helper before signing the containing app.
`Windows/build.ps1` includes it in ARM64/x64 packages. `make -C SushitrainCore
gateway-linux` and `linux` build it beside the service binaries; the RPM installs
it in `/usr/bin`. Install both executables for a headless gateway.

The MCP suites use an official SDK client over a real stdio subprocess. They
exercise strict discovery, access control, pagination, cancellation, redaction,
and a disposable real engine/gateway Files workflow including version restore.
Linux additionally tests engine management and concurrent settings updates through
MCP. They never use production sync folders or credentials.

```sh
cd SushitrainCore
go test -race -count=1 -tags noassets -timeout 90s ./mcpapi ./mcpcontrol ./mcpserver
```

These focused suites normally take under ten seconds after compilation. The full
core suite uses the repository's 300-second budget. Native Windows tests use
`Windows/test.ps1`; Linux runs the shared tests on the target host and builds
the GTK window/RPM. Cross-compilation alone does not establish provider or native
UI behavior. See [the acceptance record](mcp-acceptance.md) for actual platform verification.

## Troubleshooting

`unavailable`: start Synctrain yourself, verify its selected mode, and check the
IPC endpoint. `disabled`: enable MCP in native settings. `unsupported`: inspect
capabilities and use a host with the required engine/provider. `gateway_offline`:
restore connectivity and inspect queued changes before repeating writes.
`permission_denied`: inspect folder/client grants. `stale_revision` or
`cursor_expired`: refresh the listing/editor and retry with its new identity.

On Linux the two modes share MCP enablement in
`$XDG_CONFIG_HOME/synctrain/mcp.json` (normally `~/.config/synctrain/mcp.json`).
On Windows it is in the host's private state directory. On macOS it is in
`~/Library/Application Support/Synctrain/MCP/settings.json`. These are owned by
the host; configure them through native controls or the explicit local CLI.
Diagnostics and discovery do not require enabling the web GUI.
