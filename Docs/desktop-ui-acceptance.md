# Shared desktop UI and acceptance record

Date: November 5, 2026. Branch: `file-provider`. No production installation was performed.

## Navigation contract

`SushitrainCore/desktopui/catalog.json` is the authoritative catalog. Stable IDs, display-label keys, array order, mode requirements, detail sections, file-action groups, and legacy aliases are shared by all four native adapters:

| Frontend | Catalog delivery | Native adapter |
|---|---|---|
| macOS | App bundle resource, decoded by `SynctrainHostKit.DesktopUICatalog` | SwiftUI split navigation, forms, menus, and guarded editor panels |
| Windows | Linked app resource copied from the same source JSON | WinUI `NavigationView`, command bars, standard controls, and child editors |
| GNOME | Go embedded resource | libadwaita navigation split view, navigation pages, preferences groups, and dialogs |
| KDE | Qt resource compiled from the same source JSON | Kirigami drawer, scrollable pages, forms, dialogs, and native overflow menus |

Main destinations are Overview, Folders, Devices, Files, Remote Access, History, Activity, and bottom-anchored Settings. Files has expandable folder shortcuts ordered by case-insensitive label and stable identity. Settings contains General, Files & Storage, Network, Synchronization, Integrations, Defaults, Advanced, and About in both modes. Unsupported sections explain their mode or backend requirement rather than disappearing.

Mac Settings remains a separate window reached from the sidebar, app menu, menu bar, and Command-comma. Windows and Linux replace app navigation with the eight settings categories and Back to App, which restores the preceding app destination.

## Canonical feature locations

| Existing feature | Canonical location |
|---|---|
| Overall state, attention, synchronization/gateway summary, storage summary | Overview, in that order |
| Folder selection and management | Folders; details use Overview, Synchronization, Sharing, Versioning, Ignore Patterns, Advanced |
| Requests, configured devices, sharing, connection configuration | Devices; details use Overview, Shared Folders, Connection Settings, Advanced |
| Browsing, search, previews, imports, file mutations | Files; action groups follow Open/Preview, offline availability, rename/move, Previous Versions, information, Delete |
| Service exports, permissions, paired clients | Remote Access in Synchronize mode |
| Pairing, gateways, available remote folders | Remote Access in Files mode; mounted-folder management remains in Folders |
| Previous versions, deleted files, archive/history sources | History and contextual Previous Versions shortcuts; existing version APIs, no global history index |
| Transfers, pending changes, errors, recovery, logs | Activity |
| Identity, mode, startup, app/window presence | Settings → General |
| Browsing, previews/thumbnails, cache/offline storage, locations | Settings → Files & Storage |
| Connectivity/discovery/relays, connection options, proxy, bandwidth | Settings → Network |
| Pausing, resource priority, scanning/database/performance, reporting | Settings → Synchronization |
| File manager, menu/tray folder access, MCP | Settings → Integrations |
| New folders/reset/versioning/devices/ignores | Settings → Defaults |
| GUI/REST/authentication, configuration, expert settings, diagnostics | Settings → Advanced |
| Versions, support, licenses | Settings → About |

Contextual and shell/tray shortcuts remain available. Legacy routes and remembered Mac settings categories are mapped through catalog aliases. Window navigation does not start or stop the background service or register File Provider domains.

## Data and editing behavior

The Mac Files window reads the existing metadata index and coordinator state. Folder navigation, filtering, pagination, information, and history selection do not materialize file contents. Explicit Open/Preview/offline operations use the existing provider, transfers, and remote services. Rename, move, import, new-folder, delete, and recovery reuse existing backend operations.

New SwiftUI navigation and browser state use `@Observable`. Browser state is scoped to stable folder/parent identities. Returning from details retains native views or snapshots, selection, filters, loaded ranges, focus, and scroll position. Successful mutations invalidate cached metadata; mode/endpoint changes discard incompatible snapshots. Stale asynchronous results are rejected.

Simple preferences apply immediately. Controls keep or restore the last confirmed value when persistence fails and show the error. Multi-field settings use Save/Cancel, keep failed drafts, block repeated saves, and retain backend validation and restart prompts. Navigation and window-close guards protect unsaved drafts, including invalid text. Mac nested staged editors use modal panels to avoid an unguarded Back action.

The native Windows settings/engine facade and Mac in-process host accessor expose existing native operations without requiring MCP to be enabled. External MCP authorization and helper/socket restrictions remain in their existing backends. Synchronized-file rename, move, and identity-bearing delete validate the identity captured when the action was selected inside the existing folder coordinator; a replacement file is preserved.

## Automated verification

Every run was bounded. Focused Swift tests had a five-minute budget, Go subsets six minutes, Qt tests 60 seconds, GTK navigation tests 60 seconds after compiling their test binary, and platform builds 25 minutes. Separate cold compilation budgets were used instead of treating compilation time as a test hang.

| Target | Result and scope |
|---|---|
| macOS Go framework | Universal macOS framework build passed after the native accessor and checked-delete changes |
| macOS app/extensions | Debug app and all extensions built with Xcode 27.2; signing disabled for build verification |
| iOS compatibility | `SynctrainMobile` iOS Simulator Debug build passed; desktop-only UI remains conditional |
| Swift | 90 HostKit tests passed, including catalog identities/order/capability checks and existing navigation policy tests |
| Go on Mac and Linux ARM64 | `desktopui`, `desktopapi`, `desktopsettings`, `enginecontrol`, `hostbridge` passed; catalog validation and replacement-preserving delete regression included |
| Windows packages | ARM64 and x64 Release MSIX builds passed with zero warnings/errors |
| Windows ARM64 tests | Native shared/backend suite passed; focused native facade, MCP isolation, catalog, and checked-delete tests passed after the safety changes |
| GTK on Linux ARM64 | Native test binary compiled; `TestNavigation` passed under Wayland in 0.77 seconds after the review fixes |
| Qt/KDE on Linux ARM64 | All three CTest suites passed in 11.22 seconds, including actual QML switch rollback, mode-change snapshots, distinct Folders/Remote Access, editor guards, stale loads, and existing Dolphin lifecycle tests |
| Linux packages | Fedora 45 ARM64 GTK, common, and KDE RPMs built, including Nautilus/Dolphin integration |
| Source checks | Changed Swift files passed strict Swift formatting; changed Go files passed gofmt; `git diff --check` passed |

GTK navigation tests cover both modes, ordered catalog sections, unavailable capabilities, native Back, draft protection, invalid saves, settings refresh, retained browser state, query-specific search filters, and metadata requery after invalidation. Qt tests cover empty and populated mocked responses, unavailable/error responses, queued activation, settings tokens/restart prompts, dirty close handling, pagination, browser restoration, and native QML layout at wide and narrow sizes. Windows tests exercise native editor access while MCP is disabled, failed-save token retention, successful saves, safe editor close after mode changes, and native metadata access.

Representative bounded commands (run in the repository unless stated otherwise):

```sh
# Core directory; bound the process to six minutes.
go test -count=1 -timeout 300s ./desktopui ./desktopapi ./desktopsettings ./enginecontrol ./hostbridge

# Five-minute process budget.
swift test --package-path SynctrainHostKit

# 25-minute process budgets, separate derived-data directories.
xcodebuild -scheme Synctrain -configuration Debug -destination platform=macOS \
  -derivedDataPath build/desktop-ui/DerivedData CODE_SIGNING_ALLOWED=NO build
xcodebuild -scheme SynctrainMobile -configuration Debug -sdk iphonesimulator \
  -destination 'generic/platform=iOS Simulator' -derivedDataPath build/desktop-ui/MobileDerivedData \
  CODE_SIGNING_ALLOWED=NO build

# Linux ARM64 mirror; timeout is GNU timeout on Linux.
timeout 60 ctest --test-dir SushitrainCore/build/kde-arm64-native --output-on-failure
# After compiling the GTK test binary, run with a disposable D-Bus session.
timeout 90 env GDK_BACKEND=wayland WAYLAND_DISPLAY=wayland-1 XDG_RUNTIME_DIR=/run/user/1000 \
  dbus-run-session -- SushitrainCore/build/desktop-ui-gtk.test \
  -test.run TestNavigation -test.v -test.timeout 60s
```

Windows package builds used `Windows/build.ps1 -Architecture ARM64,x64`; native ARM64 regression testing used `Windows/test.ps1 -Architecture ARM64` and the focused Go subset. Linux packages used `SYNCTRAIN_PACKAGE_DESCRIBE=v2.8.85-0-gdesktopui Linux/build/build-rpm.sh`. Build/test scripts were run in isolated source mirrors in the existing VMs, with explicit outer deadlines and polled output.

## Code-quality follow-up

A fresh review identified one P1 and seven P2 findings, all fixed without committing:

- Mac Files validates the captured metadata revision inside the move/delete write transaction and retains the original Move snapshot while its picker is open. The store parameters are optional, preserving existing provider callers.
- Desktop route URLs now encode case-sensitive destination IDs in a query parameter, round-trip all catalog destinations, and accept the earlier host/path form.
- Windows editor entry supersedes pending parent renders; search results have stable folder/path identities; encrypted receive-only folders explicitly warn that Delete Unexpected Items deletes local content.
- GTK management callbacks invalidate retained Files metadata after successful changes. A native regression exercises folder removal, its confirmation, the successful RPC callback, and the refreshed browser.
- KDE transfer progress is attached when its own row is created, avoiding collisions with folder or device labels. A regression delays statistics until colliding labels have appeared.

Follow-up verification passed: 90 HostKit tests; 19 outbox-store tests, including two stale mutation cases; the GTK navigation suite; all three Qt suites; Mac app/extensions, iOS Simulator, Windows ARM64/x64 MSIX, and ARM64 Linux package builds. The initial Mac compile failure from the Move presentation wrapper and the GTK test's lookup of an obsolete container were corrected before the passing runs. No test timeout was raised or test disabled.

Local follow-up logs use the `review-` prefix: `review-hostkit-tests.log`, `review-outbox-tests.log`, `review-mac-build.log`, `review-ios-build.log`, `review-windows-build.log`, and `review-format.log`. The final native interaction gaps below remain open; build and fixture evidence does not close them.

## Native interaction evidence

These checks are distinct from automated fixtures and builds:

| Frontend/mode | Observed native interaction | Remaining acceptance |
|---|---|---|
| Mac Synchronize | Separate Debug bundle and isolated real engine, external discovery/relay/NAT disabled, disposable folder. Sidebar/shortcuts, Overview groups, Cmd-comma, settings categories, invalid connection draft/error, successful save, category/window-close discard prompts, Keep Editing, integrations relocation. Stale rename rejected with draft retained; fresh rename and move succeeded and appeared at the new location. | Full connected-device/service/history/recovery workflows, spoken VoiceOver, and Intel Mac execution |
| Mac Files | Separate Debug metadata fixture with offline gateway and 600 files; no provider registration or credentials. Main window, folder disclosure, filtering, Load More, selected item from page two, return from History preserving state, action order, information sheet, separate Settings, unavailable sections. Light and dark appearance were observed across the Mac checks. | Live gateway pairing, actual File Provider Open/Preview/download/offline/edit/restore workflows, shell integration, and populated recovery |
| KDE Files | Native Plasma window rendered with the shared sidebar, bottom Settings, folder disclosure, status/gateway/storage fixture data. Screenshot recorded. | Guest input automation failed, so native clicking/typing, live services, keyboard, resizing, appearance, and both-mode workflow acceptance remain open |
| KDE Synchronize | Automated native Qt/QML/backend tests and package builds only | Native end-to-end interaction remains open |
| GNOME, both modes | GTK native widget/navigation tests passed under Wayland on the KDE VM; GTK executable launched in the GNOME VM | GNOME session was locked. No product-window interaction or GNOME visual acceptance was obtained |
| Windows, both modes | Native ARM64 backend/Cloud Files tests and ARM64/x64 packages; isolated Debug fixture prepared | A firewall prompt from `winhost.test` covered the guest and guest input automation failed. No firewall permission was granted. Interactive WinUI/shell acceptance remains open |

All Windows fixture host/UI processes were stopped through their isolated pipe/path checks. Mac fixture apps were quit. Linux presentation fixtures were bounded to five minutes. Production identities, pairings, folder configuration, File Provider domains, and installations were not used for these checks.

Screenshots and logs are local, ignored evidence under `build/desktop-ui/`:

- [Mac Files Overview](../build/desktop-ui/screenshots/mac-files-overview.png)
- [Mac Files retained pagination/selection](../build/desktop-ui/screenshots/mac-files-paginated-selection.png)
- [Mac Files Settings](../build/desktop-ui/screenshots/mac-files-settings-general.png)
- [Mac Synchronize Overview](../build/desktop-ui/screenshots/mac-sync-overview.png)
- [Mac Settings close guard](../build/desktop-ui/screenshots/mac-sync-close-guard.png)
- [Mac failed settings draft](../build/desktop-ui/screenshots/mac-sync-failed-save-draft.png)
- [Mac stale rename rejection](../build/desktop-ui/screenshots/mac-sync-stale-rename.png)
- [Mac successful move](../build/desktop-ui/screenshots/mac-sync-moved-file.png)
- [Mac Integrations](../build/desktop-ui/screenshots/mac-sync-settings-integrations.png)
- [KDE Files rendering](../build/desktop-ui/screenshots/kde-files-overview.png)
- [GNOME locked-session blocker](../build/desktop-ui/screenshots/gnome-session-locked.png)
- [Windows interaction blocker](../build/desktop-ui/screenshots/windows-interaction-blocked.png)

Local logs include `core-build.log`, `mac-build.log`, `ios-build.log`, `swift-tests.log`, `go-tests.log`, `windows-build.log`, `windows-tests.log`, and `windows-safety.log`. Local copies of the final Qt and GTK results are `qt-tests.log` and `gtk-navigation.log`. Linux mirror logs include `test-native.log`, `SushitrainCore/build/gtk-navigation.log`, `SushitrainCore/build/desktop-ui-go-tests.log`, and `build-rpm.log`.

## Acceptance still open

Implementation, builds, and bounded regression checks are complete; the full native interaction matrix is not. Windows x64 execution, Linux x86-64 builds/execution, Intel Mac execution, actual GNOME interaction, both-mode Windows/KDE interaction, full accessibility narration, and live cross-platform remote/shell/provider workflows require further qualification. These gaps must not be treated as proven parity from the fixtures or successful builds.

Native control choices follow the requested [Apple settings](https://developer.apple.com/documentation/foundation/adding-a-settings-interface-to-your-app), [Windows app settings](https://learn.microsoft.com/en-us/windows/apps/design/app-settings/guidelines-for-app-settings), [GNOME sidebars](https://developer.gnome.org/hig/patterns/nav/sidebars.html), and [KDE layout/navigation](https://develop.kde.org/hig/layout_and_nav/) guidance. Visual and interaction conformity outside the observed checks remains part of the open acceptance matrix.
