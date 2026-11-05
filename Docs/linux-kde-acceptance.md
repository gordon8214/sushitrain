# KDE acceptance record

Qualification date: November 4, 2026. Source: the uncommitted `file-provider` tree based on
`10b39f2`. The implementation includes both native frontends, the shared lifecycle helper,
metadata-only desktop status methods, Dolphin actions/overlays, Places maintenance, and RPM/CI integration.
This record distinguishes completed checks from the remaining manual qualification matrix.

## Environment and artifacts

Interactive checks used only the **Fedora KDE** Parallels VM: Fedora 45, aarch64,
Qt 6.11.2, Kirigami 6.31.0, and Dolphin 26.08.2. KDE binaries/plugins were also built and
tested in the Fedora 44 container. The RPM packaging host was Fedora 45, so these local
artifacts carry an `fc45` release suffix. CI retains Fedora 44 for both architectures.

The RPM version is `2.8.85-167.g10b39f2.dirty.fc45.aarch64`. Local artifacts and logs are in
`build/kde-acceptance/rpm` and `build/kde-acceptance/logs` in the repository checkout.

| RPM | SHA-256 |
| --- | --- |
| `synctrain-common` | `ecd7ab39d7378dc9e4ebcf1413e1ab5009667c3661f36e894ca67e9ea673362e` |
| `synctrain-kde` | `6836af3df1e79099d086082d032cdd71a3c21c2504eaa4d5bd66f53d9e9f0565` |
| `synctrain` (GNOME) | `b831fb2396e99e50dc8422b1c30939cae6e75d0901070a803423dd6a71525159` |

## Completed checks

| Area | Evidence and result |
| --- | --- |
| Builds | Go services/helper, GTK/libadwaita, and Qt/Kirigami/Dolphin components compiled on ARM64. Fedora 44 KDE container build passed. Package builds stayed within the 30-minute budget. |
| Go regression subset | The subset documented in `linux.md` passed with a 360-second outer timeout and 300-second per-package deadline. Files took 56.564 seconds; FUSE took 8.046 seconds. Logs: `kde-go-tests-final.log`. |
| Lifecycle failures | Tests cover takeover failure before engine startup, leaving the previous engine masked after possible database migration, mode-last persistence, startup rollback, and refusing to leave Files when pending edits cannot send. `kde-lifecycle-final.log` passed. |
| Qt/plugin regressions | All three suites passed in 5.51 seconds with a 60-second outer budget and 30-second individual deadlines. Cases include missing sockets, partial responses, exact 64-bit values, trickled-response deadlines, canceled owners, obsolete badges, service loss, cold discovery, plugin metadata/loading, draft preservation, pairing cancellation, permissions, and pagination. |
| RPM upgrade | Installed the previous monolithic RPM, then upgraded to common + KDE. The old monolith was replaced. Identity, configuration, folders, archives, and mode survived. Added the GNOME package alongside KDE afterward. |
| Package layout | `rpm -qf` confirms launcher/helper/services belong to common, GTK belongs to GNOME, and Qt/plugins belong to KDE. One application-menu desktop entry. `rpm -V` passes for common/KDE. KDE/common requirements contain no GTK/Nautilus dependency; the Fedora 44 KDE test container has neither GTK 4 nor Nautilus installed. |
| Launcher | KDE menu activation opens Qt with both packages installed. Bounded launcher checks cover KDE/GNOME selection, explicit override, argument preservation, and KDE-only fallback from a GNOME session. |
| Native synchronization | Created a folder, accepted a device, shared the folder, paused/resumed, and synchronized real files with an isolated peer. Divergent initial content produced a preserved Syncthing conflict copy. |
| Local history | Enabled simple versioning through Qt, received two file revisions, listed Previous Versions, previewed UTF-8 content, exported with the KDE Save dialog, and restored. Export and restored content matched the chosen revision; displaced content remained archived. |
| Files mode | Switched through Qt and GTK, paired an isolated gateway, added its folder, and browsed the mount. Pairing survived mode changes. A retained file remained readable while the peer was offline. An offline replacement stayed queued and arrived at the peer after reconnection. |
| Dolphin | Loaded both installed plugins. Used the native context menu for View Sync Status and Keep. Single-file Previous Versions and Synchronize-mode Rescan were present. Observed remote, pending, cached, and retained availability badges changing asynchronously. |
| Window independence | Closed the Qt window and verified the selected user service stayed active. Dolphin actions and Places remained available. Places were added/removed with the mounted folder lifecycle. |
| GTK regression | The installed GTK frontend displayed the same engine identity and synchronized folder totals, restarted the service, and switched to Files using the extracted lifecycle package. |
| Background/tray | `synctrain --background` creates the Plasma tray without opening a window. Native Show, Settings, Pause All, Resume All, and Quit and Stop actions are provided. Show and Settings activation, Pause All/Resume All, and confirmed Quit and Stop were exercised. The peer's paused state changed to true and back to false; shutdown stopped both services. |

The isolated peer used its own identity/home and `kde-qualification` folder under
`build/kde-qualification-peer`. VM test data is under `/home/gordon/Projects/kde-acceptance`.
Production sync folders and gateways on the host were not used. The disposable peer was stopped
after qualification; the VM remains in Synchronize mode with its isolated data and installed window available.

## Indexing and hydration

Initially, Dolphin displayed the gateway-generated image thumbnail while Files reported
`downloadedBytes: 0`, `used: 0`, and `kept: 0`; Baloo was running. Explicit Keep then downloaded
exactly the selected text file's 17 bytes.

A forced `balooctl6 index` check exposed an additional reader: that command can extract content
in its own process. The safeguard now covers `balooctl` as well as the background Baloo workers.
After rebuilding, installing, and restarting Files, indexing the test paths left the counter at
zero; an uncached text file remained `remote`, and a partially cached binary remained `partial`.
The counter stayed zero during subsequent observation. This counts actual file-content response
bytes and intentionally excludes metadata and gateway thumbnail traffic.

`TestBalooCommandCannotHydrateRemoteContent` uses a real FUSE mount and a separately executed
reader: the indexer-named process is refused remote content, while the ordinary file reader succeeds.
The focused KDE/Baloo FUSE tests pass in about one second. KIO thumbnail-worker identification is
tested separately from ordinary KIO file operations.

## Remaining qualification

- The x86-64 CI matrix is updated but was not executed in this uncommitted session.
- Full manual parity qualification remains open for encrypted/untrusted sharing, every advanced
  engine setting, metered/cellular/Power Saver transitions, PDF/audio/video previews, the full
  import/move/Trash matrix, and simultaneous local/remote history with independent source failures.
  Shared backend tests cover history and policy contracts; that does not substitute for these UI checks.
- GTK checks ran in the named KDE VM. A separate GNOME session was not used.
- The VM contains development packages. The dependency check and GTK-free container tests do not
  claim that all GTK packages were removed from the running VM.
- Parallels root transport caused Fedora systemd package triggers to print transient-service
  transport errors. User-session daemon reload, service control, RPM verification, and runtime checks
  succeeded afterward.
- An exploratory pending-edit mode switch used an insufficient 15-second CLI wrapper and timed out.
  The operation finished after the peer reconnected and the edit sent. It is not recorded as a
  successful timing test; pending-edit refusal is covered by the bounded lifecycle regression test.

## Code quality follow-up

The subsequent `code-quality --fix` pass corrected draft cancellation, settings-token ownership,
startup preference ordering, queued activation, preview-window cleanup and cache leases,
default-application materialization/retention, restart prompts, ignored-directory badges,
asynchronous Places updates, cold Dolphin action execution, and incomplete native dependency detection.

The documented Go subset passed again: Files took 57.742 seconds, FUSE 9.082 seconds, and
engine control (including an isolated engine/ignored-directory endpoint test) 2.104 seconds.
The Places concurrency regression also passed with the race detector in 1.057 seconds.
All three Qt/plugin suites passed in 6.70 seconds. These now exercise the production QML
Cancel/Save signal path, delayed preferences, overlapping/abandoned editors, pending activations,
preview deletion and cache-lock release, local default-app URLs, and a constructed Dolphin
plugin's cold multi-item menu. The same 360-second Go and 60-second native test budgets apply.

Both native frontends and all three RPMs built successfully. A build with Qt Quick installed
and a simulated incomplete CMake dependency set exercised the Fedora 44 KDE container fallback.
An extra Fedora 44 GTK-container bootstrap was canceled during its initial dependency download;
it was not a passing build or test. GTK compilation used the installed Fedora 45 development
toolchain. The original interactive matrix above was not repeated after this follow-up.

Follow-up artifacts and logs are in `build/kde-code-quality`; its `SHA256SUMS`, `source-sha256.json`,
and `review.json` identify the artifacts, reviewed source, and applied findings. The RPM hash table
above continues to identify the earlier interactively qualified build.
