# MCP implementation verification

This records qualification of the uncommitted issue #67 implementation on
November 4, 2026. It is not a release or a notarization record.

## Verified

- macOS: the native Debug app builds for Apple Silicon with the project’s current
  SDK settings. Its helper contains both arm64 and x86_64 code; the embedded
  universal helper is signed with the app's development team and the dedicated
  `gordonbyrnes.Synctrain.MCP` identifier. A real SDK client discovers 93 tools
  through that packaged helper. The app accepts the signed helper and rejects an
  unsigned helper attempting to change enablement.
- A disposable Mac settings instance accepts MCP preference edits through its
  actual Swift adapter. A separate disposable Synchronize instance uses a fresh
  identity, scratch preferences/configuration, a loopback listener, and no peers.
  Through the signed helper, an SDK client verifies native status/diagnostics,
  browsing and bounded reads, folder/device creation and removal, pause/resume,
  rescan, a saved folder label, guarded content replacement, preserved history,
  and restoration of the selected version. Production identities and folders are
  not used. These checks verify the native APIs and persisted state, not visual
  presentation.
- The full macOS Go core suite passes with the repository's engine stamp and
  300-second per-process budget. Focused MCP/desktop-settings tests pass with the
  race detector. Swift package tests pass: 88 HostKit tests and 213 FileProviderKit
  tests. The changed Mac app is built after the final Swift changes.
- Windows: ARM64 and x64 development-signed MSIX packages build with the bundled
  helper and execution alias. Shared/native tests run on Windows 11 ARM64 using
  the native provider built by `Windows/build.ps1`. The MCP history regression
  covers preservation of history handlers in Files mode and protection of the
  current Explorer root after a location change. History state receives a
  protected Windows ACL; a Windows-specific test checks its permitted principals.
- Fedora 44 ARM64: focused MCP, attached-engine, and desktop-settings tests pass
  natively. The GTK app, daemons, helper, Nautilus module, and aarch64 RPM build.
  The package includes MCP setup documentation and Go dependency notices. The
  copied working tree's qualification RPM has a development version; it is not
  a published release artifact. Linux amd64 static executables cross-compile;
  an amd64 GTK/RPM runtime is not qualified by that check.
- Automated SDK clients use real stdio subprocesses and a disposable real
  engine/gateway. Tests cover pairing, accessible folders, reads, queued writes,
  selected-version restoration, revoked write grants, disabled/stopped hosts,
  invalid paths, unsupported modes, cancellation, bounded lists/logs/content,
  exact log timestamps, secret redaction, and concurrent settings edits that
  preserve unrelated native changes. Unsaved local edits remain in native queues.

## Native UI verification and remaining VM checks

After an initial locked-Mac failure, native app access became available. In the
isolated Mac settings fixture, the MCP page displayed the copied-client
configuration and its enabled switch. Disabling access visibly changed the
switch and a real SDK client then received `disabled`. In the isolated
Synchronize app, an MCP folder-label change appeared immediately in the open
sidebar, and an MCP application preference changed the visible “Folder access
from menu” control from “Open in Finder” to “Open in the app.” Native MCP
disablement was also checked in that app. Persistence across a host restart was
verified separately through the signed helper and saved configuration.

The Parallels window was accessible, but the provided computer-use screenshots
showed a black guest display for both Windows and Fedora. Click-driven native
settings checks in those VMs therefore remain unverified; their native builds,
SDK clients, and automated suites passed. No production installation or
configuration was changed to work around the capture restriction.

The remaining VM pass should enable/disable MCP in each native Settings page,
change a visible setting through MCP, verify the window refreshes, restart its
isolated host, and confirm the same state. Use disposable roots and fresh
identities; keep production credentials, provider domains, and sync folders out
of these checks.

## Code-quality fix verification

The follow-up review fixed ignore-pattern routing, complete authorization-header
redaction, exact file-read payloads and IPC integers, nested collection paging,
completed-operation eviction, and operation-scoped native pause cancellation.
File/history fixes retain validated inputs and export directories, stage selected
archives before saving current content, and publish snapshot exports exclusively.
Mac Files search runs off the main actor with a scan budget; reads reuse verified
cached content, and forgetting history retains a shared Files pairing. Windows
History Sources now stays on its detail page during refresh and provides Back.

Verification passed with the full core suite, the focused race suite, and real
engine regressions for same-second simple/staggered restore, stale export tokens,
Mac resource forks/attributes, canceled restores, and FIFO rejection. The signed
Mac app built and its packaged universal helper completed discovery and settings
round trips through a real SDK stdio client. Native Windows ARM64/x64 packages
built, with automated tests on ARM64 Windows. Native tests and the RPM build
passed on Fedora 45 ARM64, and Linux AMD64 static binaries built. The interactive
VM acceptance gap described above remains open.

## Reproduction

Run `make -C SushitrainCore test` for the full core suite (normally about a minute;
300-second hard budget per Go process). The focused suite normally takes under
ten seconds after compilation:

```sh
cd SushitrainCore
go test -race -count=1 -tags noassets -timeout 90s ./mcpapi ./mcpcontrol ./mcpserver ./desktopsettings
```

Use `Windows/build.ps1 -Architecture ARM64,x64 -DevelopmentSign`, followed by
`Windows/test.ps1 -Architecture ARM64` on native ARM64 Windows. The native test
suite normally takes about a minute and has a 180-second per-process budget.
On Fedora, build with the Go version required by `go.mod`, then run
`Linux/build/build-rpm.sh`. For Mac fixtures, use Debug's `SYNCTRAIN_TEST_ROOT`;
keep File Provider disabled unless explicitly testing a disposable provider
location. [MCP setup and exclusions](mcp.md) describe the supported transport and
runtime capability contract.
