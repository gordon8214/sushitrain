# Synctrain for Windows

The Windows port uses a native WinUI desktop app, a per-user Go host, and a native
Cloud Files provider. The host links the same Syncthing engine, gateway, verified
content reader, local index, write queue, conflicts, and version-history code as
the existing platforms.

## Management interface

The app opens Overview and keeps one navigation list while switching between management and preferences. Manage contains Overview, Folders, Devices, Remote Access, and Activity; Preferences contains General, Files & Storage, Network, Synchronization, Integrations, Defaults, and Advanced; Tools contains Browse Files, History, and About. Files mode hides engine-only pages and controls. Settings shortcuts select General, and there is no separate Settings navigation mode or Back to App action.

Folders manages synchronized or mounted folders. Browse Files remains an optional in-app tool; Overview provides system file-manager access. Settings search is available from every page and shows applicable results. Existing unsaved-draft and Save protections apply when moving between management and preference pages. See [the shared UI acceptance record](desktop-ui-acceptance.md) for verification and remaining platform limits.

## Build and install

Build on local NTFS with Windows 11, .NET 10, Go 1.27, Visual Studio's WinUI and C++
desktop workloads, the ARM64/x64 C++ tools, and Windows SDK 10.0.26100.0. Parallels
shared folders are source-transfer locations, not build or Cloud Files locations.

```powershell
Windows\build.ps1 -Architecture ARM64,x64 -DevelopmentSign
```

The outputs are under `Windows\build\ARM64\Packages` and
`Windows\build\x64\Packages`. Dependencies are included in the self-contained
MSIX. `-DevelopmentSign` uses a development certificate in the current user's
certificate store and exports only its public certificate. It never exports a
private signing key. For a development installation, run the certificate trust
step elevated once:

```powershell
Windows\install.ps1 -Package <package.msix> -DevelopmentCertificate <public.cer>
```

Quit Synctrain from its tray menu before installing an update so the package files
are released.

A production package needs a trusted signing certificate whose publisher matches
the manifest. An unsigned CI package is a build artifact, not a publicly
installable release. Do not distribute the development certificate as a trusted
production publisher.

## Features

| Capability | Windows implementation |
| --- | --- |
| Folder and device management | Native forms, sharing, encryption, ignore rules, pause/resume, and errors |
| Engine settings and diagnostics | Native settings, logs, statistics, search, and the local engine GUI |
| Remote Files hosting | Gateway exports, permissions, pairing approval, and client revocation |
| Remote Files browsing | Virtualized lists, filtering, create, rename, delete, and save a copy |
| On-demand Explorer access | Cloud Files placeholders, verified ranged hydration, and local-save capture |
| Offline access | Keep on device and cache-budget eviction of clean unpinned files |
| Recovery and history | Pending-change export/discard, recovered captures, versions, restore, and copy |
| Shell integration | Thumbnail previews, availability/pending status, Previous Versions, and Recycle Bin |
| Desktop lifecycle | Tray, login startup, close to background, and explicit Quit |

## Runtime ownership

The app starts one background supervisor per signed-in Windows user. Closing the
window leaves the tray and background agent active. Quit stops them. The startup
setting controls Windows' login startup task. There is no machine service and no
operation after sign-out.

A Windows job owns each worker and its child provider. A crash closes the process
tree, and the supervisor applies bounded restart backoff. Each client authenticates
the named-pipe server's Windows user before sending data. Pipes accept only the
current user; private state has a protected user/SYSTEM ACL. Gateway credentials
use Windows Credential Manager.

State is under `%LOCALAPPDATA%\Synctrain`; ordinary state is excluded from MSIX
virtualization so a package update preserves it. The default Explorer location is
`%USERPROFILE%\Synctrain`. A provider identity binds its placeholders to the stable
local-index identity, preventing a fresh index from treating old placeholders as
its own entries.

The protected state also records the root's NTFS file identity. If a package
upgrade removes the shell registration, the provider can register that same root
again. Older installations can recover only a complete tree whose paths and NTFS
identities match the protected checkpoint; an unrelated or incomplete tree is
preserved and refused.

Package unregistration can remove dehydrated placeholders and convert hydrated
files and directories into ordinary objects. A durable recovery marker separates
that cleanup from user deletions. Recovery recreates missing placeholders, restores
parent directories before replaying child captures, and verifies surviving content
against the shared reader before rebinding it. Unchanged survivors do not create
uploads or new versions. A failed verification preserves the capture for recovery.

## Files mode

Only local NTFS locations are supported. Reparse-point locations and overlapping
cloud roots are refused. Each placeholder identifies its local item and content
node. Fetch callbacks request bounded ranges from the shared, hash-verified reader;
errors never complete missing bytes as successful data.

Before importing a local save, the provider makes a complete immutable capture
outside the Cloud Files tree. This avoids recursive hydration through the host.
The shared core installs the capture and its receipt atomically, and retains failed
or conflicting changes for recovery. The provider checkpoints its identity before
acknowledging the receipt. Renames, directory creation, remote moves, and intentional
mode cleanup also have replay handling. NTFS file identities and USNs distinguish
local saves from provider metadata changes. Remote dehydration and deletion protect
the affected file against concurrent writers and replacement.

Removing a selected folder waits for its native captures to settle as well as the
shared write queue. Captures retain their original local folder identity; selecting
another folder at the same Explorer path cannot redirect a preserved edit to it.

When switching modes, captures finish while the old backend still admits requests.
The shared queue drains before native cleanup. Cleanup requires proof that each
version is synchronized; open writers, pending captures, and local-only content
prevent removal. Intentional native cleanup is recorded before touching the tree
so a crash cannot upload those removals as user deletions. A failed backend switch
restores the prior Files index and its selected folders.

Synchronized media remain ordinary files, with native image/media preview controls in the app.

## Verification

```powershell
Windows\test.ps1 -Architecture ARM64
```

`TestCloudFilesRoundTrip` runs the production host RPC and native provider against
a disposable real Syncthing engine and gateway. It tests hydration larger than one
transfer chunk, local create/edit/rename/delete, preserved-timestamp saves, pinning,
cache eviction, conflict copies, capture-receipt replay, real CFAPI unregistration,
read-only survivors, offline parent moves, interrupted directory creation, remote
updates, and detachment. The fixture uses direct CFAPI registration
for its temporary root so it can run without installing a test package. The
packaged shell registration and installed WinUI app require separate VM testing.

The packaged thumbnail and command registrations can be checked against a paired
image placeholder with `Windows\tools\test-packaged-shell.ps1 -Path <image>`.
It checks activation of all three COM classes and verifies that requesting a
thumbnail leaves the placeholder's full-content allocation unchanged.

`Windows\tools\vm.sh script.ps1` runs a PowerShell script as the signed-in user of
the local Parallels Windows 11 development VM. Transfer sources to a local guest
checkout before building. Tests must use disposable folders and gateway identities.

Explorer integration is declared through the package's `windows.cloudFiles` and
out-of-process COM extensions. Thumbnail extraction requests a bounded gateway
preview instead of reading the placeholder's full data stream. Custom status
properties show pending, failed, local-only, and availability states. The Previous
Versions command activates the native app with a client-bound item identity.

### Local acceptance record, November 3, 2026

The ARM64 Windows 11 Parallels VM built both development-signed, self-contained
MSIX packages at version 1.0.13.0 with zero warnings and zero errors. ARM64 was
installed and tested. The x64 Cloud Files round trip passed under Windows ARM64
emulation; native x64 hardware remains unqualified.

The final uncached native suite passed 550 tests and subtests. It includes the
production CFAPI round trip, capture isolation between removed/reselected folders,
captures whose original parent has disappeared, native capture settling before
folder removal, Windows paths, credentials, named pipes, mode transitions, and
supervisor shutdown. Separate Windows engine metadata-history tests, shared Mac
tests, and the Mac XCframework build passed; the shared core also cross-built for
Linux.

Installed-package checks passed against a real gateway running on the Mac:
hydration and upload while the native window was hidden; native pin/unpin;
activation of all three shell COM classes; thumbnail extraction with placeholder
allocation unchanged at zero; all eight navigation pages; browsing, filtering,
accessible file names, and image preview; close to background; and reopening the
same UI process. A populated package update and both mode-switch directions were
also exercised during development. Disposable selections and pairings were
removed after testing.

Automation initially used a stale UI Automation visibility property after hiding
the window. A fresh process/window-handle check verified that hiding succeeded,
the background agent stayed alive, and reactivation restored the existing window.
Direct tray-menu Quit interaction remains a manual acceptance check because VM
input automation could not reliably select that native popup. The menu entries
and host shutdown path were verified separately. Production signing and remote
CI execution were not performed.

Development packages and public certificates are in
`Windows/build/artifacts/ARM64` and `Windows/build/artifacts/x64`, with
`Windows/build/artifacts/SHA256SUMS`. Local verification logs are retained in
`Windows/build/verification`; these generated directories are ignored by Git.

### Code quality fix pass, November 3, 2026

Version 1.0.14.0 preserves the current placeholder and directory subtree when a
new case-colliding remote name is enumerated first. Projection refuses another
item's identity, and cleanup verifies the native item and node before deletion.
The native regression fails against the previous provider and passes with the
fix, including unchanged placeholder identities and no remote delete requests.

Move replay now recognizes an already completed destination before requiring
its old parent. Recovered-capture export and Save a Copy compare Windows' final
directory names obtained through handles, including NTFS junctions; an ordinary
outside export still succeeds and retains its source. The first junction fix
failed its regression because Go's path resolution left the junction unresolved;
the handle-based correction and affected host-suite rerun passed.

The tray posts the required null message after its popup returns, and save
suggestions handle reserved names before the first extension, including
`NUL.tar.gz`. Both self-contained MSIX packages built with zero warnings and
errors. Their Go host and native provider payload hashes match the verified
builds. ARM64 1.0.14.0 was installed and launched with a responsive native window
and healthy background host; it remains running in the VM.

There are 552 passing Windows tests/subtests across the full run and affected
host-suite rerun. The x64 collision, recovery, capture-settling, and junction
regressions passed under ARM emulation. The shared Mac files suite and rebuilt
Mac XCframework passed. Direct repeated tray-menu interaction and Quit remain
manual acceptance checks; native x64 hardware and production signing retain the
limits recorded above. Logs use the `quality-` prefix in
`Windows/build/verification`, and the updated packages and checksums are in
`Windows/build/artifacts`.

## Assistant access

The bundled [MCP server](mcp.md) provides structured inspection and management
through local stdio. Start Synctrain and explicitly enable MCP in Settings;
enabling it grants full management access. Headless gateways use the documented
local enable/disable command and can connect remote clients through SSH.
