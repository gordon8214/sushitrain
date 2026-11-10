# Synctrain for Linux

Synctrain for Linux supports Fedora with GNOME or KDE Plasma, with a native interface for each desktop. As on the Mac, a machine runs Synctrain in one of two modes:

- **Synchronize**: it runs Syncthing, and every folder syncs all of its contents. Phones and machines in Files mode can
  reach its folders through the remote file service, once it is turned on.
- **Files**: it runs no Syncthing. The folders of a gateway on another system, which has all their files, appear in
  `~/Synctrain`, and files are fetched from the gateway as they are read.

The `synctrain` launcher opens the native window for the current desktop. `synctrain-desktop` shares setup and service
management between the windows; `synctrain-gateway` runs Syncthing in Synchronize mode
([gateway.md](gateway.md)), and `synctrain-files` is the daemon of Files mode. Each daemon is a systemd user service,
and only the service for the machine's mode is enabled. They run without the window. The window uses private Unix
sockets to manage the existing engine and Files service. The mode is in `~/.config/synctrain/mode`; desktop preview,
list/grid, hidden-file, and tray preferences are in `~/.config/synctrain/desktop.json`.

## Management interface

The app opens Overview and keeps one navigation list while switching between management and preferences. Manage contains Overview, Folders, Devices, Remote Access, and Activity; Preferences contains General, Files & Storage, Network, Synchronization, Integrations, Defaults, and Advanced; Tools contains Browse Files, History, and About. Files mode hides engine-only pages and controls. Settings shortcuts select General, and there is no separate Settings navigation mode or Back to App action.

Folders manages synchronized or mounted folders. Browse Files remains an optional in-app tool; Overview provides system file-manager access. Settings search is available from every page and shows applicable results. Existing unsaved-draft and Save protections apply when moving between management and preference pages. See [the shared UI acceptance record](desktop-ui-acceptance.md) for verification and remaining platform limits.

Progress is live in Synchronize mode: Overview shows how many files are being received and sent, with the bytes done, speed and time remaining, and opens lists of each file being received (by folder) or sent (by device). A syncing folder shows how much of it is here and how much remains, and a file's information shows its download while it arrives. These pages update every second or two while they are shown. The speeds come from `engine.transfers`: a direction's speed from the bytes every connection received or sent over the last 10 seconds, as in Syncthing's own GUI, and a file's from its own progress. In Files mode, Activity lists the downloads from gateways with their state and verified blocks.

## Installing

On a Fedora host with Go (the release in `SushitrainCore/go.mod`, newer than Fedora's) and `rpm-build`:

```bash
Linux/build/build-rpm.sh
# KDE Plasma (the common package supplies services and the launcher)
sudo dnf install SushitrainCore/build/rpm/$(rpm --eval '%{_arch}')/synctrain-common-*.rpm \
  SushitrainCore/build/rpm/$(rpm --eval '%{_arch}')/synctrain-kde-*.rpm
# GNOME: install synctrain-common and synctrain (without the -kde suffix).
```

The package supports x86-64 and ARM64 (`LINUX_ARCH=amd64` or `arm64`, built on the target architecture).
The `synctrain-common` RPM contains the shared daemons, services, lifecycle helper, launcher,
and desktop entry. `synctrain` contains the GTK/libadwaita window and Nautilus module;
`synctrain-kde` contains the Qt 6/Kirigami window and Dolphin plugins. Both frontends can be
installed together. Installing KDE alone does not require GTK or Nautilus.

The `synctrain` command selects Qt in a KDE session and GTK elsewhere, falling back to the
installed frontend. `synctrain-kde` and `synctrain-gtk` select explicitly; `SYNCTRAIN_DESKTOP=kde`
or `gnome` changes the launcher's preference. Pairing links, `--background`, and `versions <path>`
are forwarded unchanged. KDE also accepts `open <path>` and `status <path>`.

The Go release must match `SushitrainCore/go.mod`. KDE builds need CMake, Ninja, a C++20 compiler,
Qt 6 Base/Declarative/Multimedia/PDF development packages, Extra CMake Modules, and KDE Frameworks
Kirigami, I18n, CoreAddons, DBusAddons, KIO, Notifications, StatusNotifierItem, and Prison.
On Fedora these are the matching `qt6-*-devel`, `kf6-*-devel`, and `extra-cmake-modules` packages
listed in `Linux/build/Containerfile.kde`. GTK builds require `gtk4-devel` and `libadwaita-devel`;
Nautilus requires `nautilus-devel` and `json-glib-devel`.

`Linux/build/build-rpm.sh` builds both interfaces by default. Set `SYNCTRAIN_DESKTOPS=kde` or
`gnome` to build only one. Missing development libraries, or `SYNCTRAIN_CONTAINER_BUILD=1`,
use the Fedora 44 Podman build containers. The runtime package supports x86-64 and ARM64;
build desktop components on the target architecture. The names and logo retain the README's
licensing restrictions.

Build individual components with `make -C SushitrainCore linux`, `linux-window`, `linux-kde`,
or `linux-nautilus`. `linux-kde-test` runs the Qt transport and Dolphin tests, normally around
six seconds and bounded to 60 seconds. `linux-nautilus-test` runs the native Nautilus tests.

Removing the package leaves `syncthing.service` masked if Synctrain took it over: `systemctl --user unmask
syncthing.service` gives it back (with a Syncthing at least as new as Synctrain's engine).

## The window

The first time, Synctrain asks what the machine does. Its main menu switches to the other mode later.

**Synchronize** starts Syncthing at every login. With Syncthing already set up here (`~/.local/state/syncthing`, or the
legacy `~/.config/syncthing`), Synctrain takes it over in place, with the same device ID, folders and database, so
other devices see no change:

1. `syncthing.service` is stopped and masked, so that it doesn't start again; a Syncthing that systemd didn't start (as
   from Syncthing Tray) is asked to stop through its REST API.
2. The configuration, identity and database are copied next to the home, to `syncthing.before-synctrain-<time>`
   (reflinks on btrfs, which take no space until either side changes).
3. `synctrain-gateway.service` starts on the home. If anything fails before that, what ran before is put back. Once
   Synctrain's engine has started on the home, it may have changed the database, so `syncthing.service` stays masked,
   and the window says where the copy of the home is.

Syncthing Tray keeps working against the same REST API, but turn off its launcher (Settings › Launcher) if it starts
Syncthing itself, or it competes for the home at the next login. A Syncthing run as a system service
(`syncthing@<user>`) has to be disabled by an administrator first. Going back to the stock Syncthing needs a version at
least as new as Synctrain's engine: stop Synctrain (switch to Files mode, or `systemctl --user disable --now
synctrain-gateway`), unmask `syncthing.service` and start it.

In Synchronize mode, native pages manage folders, devices, sharing, encryption passwords, ignores, failed and
deleted files, scanning, pause/resume, and all supported engine settings. Common settings have dedicated controls;
Advanced Settings exposes the complete engine schema, including defaults. Editors keep drafts until Save, show
validation errors, and ask before discarding changes. The web GUI and REST API are optional and can stay disabled.

On GNOME, sections use hierarchical navigation below the sidebar locations, following the
[GNOME browsing guidelines](https://developer.gnome.org/hig/patterns/nav/browsing.html).
Settings, folder and device tools, file actions, directories, search results, and Previous Versions
use `AdwNavigationPage` in a shared `AdwNavigationView`, with a header bar for each page.
The header shows the current page title and a Back arrow at the leading edge. Back returns to the
immediate parent: for example, Previous Versions returns to file actions before returning to the
file list. Parent views retain their widgets, scroll position, filters, selection, and browser location;
views with interrupted requests or changed data reload when revisited.

Inline editors use the same header Back arrow, with Save or Add Folder as their explicit action.
Back asks before discarding unsaved edits, and failed saves keep the draft and validation error visible.
Successful saves return to the immediate parent. Action dialogs keep their normal Cancel and affirmative
buttons, following the [GNOME dialog guidelines](https://developer.gnome.org/hig/patterns/feedback/dialogs.html).
Clean pages support libadwaita's Escape, Alt+Left, mouse Back, and swipe navigation (directions follow
the locale). With unsaved edits, the header arrow, Escape, Alt+Left, and mouse Back share the discard
guard; native gestures and collapsed-sidebar navigation cannot silently discard a draft.
Navigation is blocked while saving. Back controls include accessible labels and destination tooltips.

The GTK navigation regression test uses an isolated service socket and a virtual display. With cached
GTK dependencies it normally takes a few seconds, with a 20-second test timeout and a three-minute
outer budget including compilation:

```bash
cd SushitrainCore
timeout 180s xvfb-run -a dbus-run-session -- go test -trimpath -timeout 20s -run TestNavigation ./cmd/synctrain
```

The native file browser supports paged lists and grids, search, thumbnails, import, move, rename, creating folders,
Save a Copy, and local Trash. Files mode also supports Keep on This Device, offline edits, and pending-change review.
Images, audio, video, PDF pages, and UTF-8 text have native previews; other documents and HTML open in the default app.
Image previews have a configurable size limit. Disposable preview copies have a 1 GiB budget and a 512 MiB per-file
limit; recently opened copies are retained for external apps, and day-old copies expire. Pinned files and unsent edits
are outside this disposable cache.

**History Sources** pairs read-only history gateways without mounting their folders. Previous Versions combines local
history and independent results from paired gateways by Syncthing folder ID and path. A source failure does not hide
other results. Preview and Save a Copy bind the download to the selected archive. Restoring checks the destination
again after downloading and preserves its current content. Files mode refuses replacement over pending local edits.
The Nautilus and Dolphin Previous Versions commands open the corresponding native browser. History credentials use Secret Service when available,
with a private credential-file fallback when no keyring service is available.

**Automatic Pause** observes NetworkManager's metered/cellular status and the desktop's Power Saver profile. Each
policy is optional. Pauses made with native controls or explicit REST Pause requests survive policy changes and restarts.
Resume clears the manual pause while an active automatic policy keeps synchronization paused. Reading and saving the
effective REST configuration does not create manual pause intent. An enabled policy pauses while its required signal is
unknown. Resource Priority uses systemd CPU and I/O weights for the background service.

The optional tray uses StatusNotifierItem/AppIndicator. GNOME requires an AppIndicator extension to display it. The
window and background services remain usable without a tray host. Tray settings include login startup, Show,
Pause All, Resume All, Settings, and Quit and Stop. Closing a window leaves the services running.

**Remote Files** manages the remote file service:

- It is off until you turn it on. Then it listens on port 20920. **Address for Clients** is where pairing codes send
  clients: the machine's name and that port unless you give another, as `host` or `host:port` (the port when a router
  forwards another one), such as a name that reaches the machine from outside its network. Clients paired already
  keep the address they were given; change it on each of them.
- **Shared folders** are what clients may be given: not shared, read only (which includes previous versions), or read
  and write.
- **Pair…** shows a code to scan in Synctrain on an iPhone, iPad or Mac, with a link to paste in Synctrain on a machine
  in Files mode. When a client submits it, you choose which folders it gets, and approve or deny.
- **Clients** lists paired phones and machines; each one's access per folder can be changed, and a client revoked.
- **Keep Running After Logging Out** turns on systemd's linger for your user, so that phones reach the folders while
  nobody is logged in.

**Files** starts `synctrain-files.service`. Its pages are the folders in `~/Synctrain` (Add…, Remove from This Device),
the gateways (Pair… with a link from the gateway, Change Address, Forget), changes that aren't on their gateway yet
(Export…, Discard), and the cache's size. **Previous Versions** of a file, opened from Nautilus (or
`synctrain versions <path>`), lists what the folder's versioning keeps on the gateway, to open, restore or save a copy
of.

**Switching modes** follows the Mac's order, and the mode changes last, so a switch that stops halfway can be repeated:

- From Files to Synchronize, changes being sent are waited for (two minutes at most); if any isn't on its gateway then,
  nothing changes. Otherwise each folder is taken out of `~/Synctrain`, files that were only here are moved out first
  (as when a folder is removed, below), and the cache is emptied. The pairings with gateways stay, for switching back.
- From Synchronize to Files, Syncthing stops and its folders stay where they are, to synchronize again when the machine
  switches back. `syncthing.service` stays masked. Clients of the remote file service can't reach the folders meanwhile.

## Files mode

The rest of this page is about Files mode, which is `synctrain-files`. Everything the window does is also a command.

### Running

```bash
synctrain-files serve
```

It mounts the folders at `~/Synctrain` and follows their gateways until it is stopped (`SIGINT` or `SIGTERM`, or
unmounting `~/Synctrain`, as Nautilus's sidebar does). The package installs it as a systemd user service, which the
window enables; without the package, with the binary in `/usr/bin`:

```bash
cp Linux/systemd/synctrain-files.service ~/.config/systemd/user/
systemctl --user enable --now synctrain-files
```

| What | Where | Option |
| --- | --- | --- |
| The index of the folders, and changes not on the gateway yet | `~/.local/state/synctrain/files` | `--state` |
| Files fetched from gateways, and downloads in progress | `~/.cache/synctrain` | `--cache` |
| The folders | `~/Synctrain` | `--mount` |
| The admin socket the commands below use | `$XDG_RUNTIME_DIR/synctrain/files.sock` | `--socket` |

`--cache-budget` sets how many MiB of fetched files are kept (5 GiB by default); the least recently opened go first
beyond that. Files kept on purpose (below) don't count. `--log-level debug` logs what the daemon does, including the
program behind each request it refuses.

Only one daemon runs with a state directory: a second one refuses to start.

The credentials of gateways are kept in the desktop's keyring (GNOME Keyring), and the daemon never asks to unlock
it: while it is locked, it waits. Without a keyring (on a server, say), or with `--credentials file`, they are kept in
`credentials.json` in the state directory, which only the user can read; they move into the keyring once there is one.

### Gateways and folders

1. On the gateway, start pairing: Pair… in Synctrain's Remote Files on a Linux machine in Synchronize mode,
   `synctrain-gateway pair --folder <folder>:read` on a server, or Settings › Remote file service on a Mac in Synchronize
   mode. It shows a `synctrain+pair://` link.
2. Here: `synctrain-files pair '<link>'`, then approve the request on the gateway.
3. `synctrain-files folders --gateway <service ID>` lists the gateway's folders for this machine, and
   `synctrain-files folders --gateway <service ID> --add <folder>` adds one. It appears in `~/Synctrain`, named after
   its label.

`synctrain-files status` shows the gateways and folders. `synctrain-files folders --remove <ID>` takes a folder out of
`~/Synctrain` (it stays on the gateway), and `synctrain-files gateways --forget <service ID>` forgets a gateway whose
folders are all removed. `synctrain-files gateways --gateway <service ID> --address <host[:port]>` changes where this
machine reaches a gateway, as when another name reaches it away from its network; the pairing stays, and the address is
kept even if the gateway doesn't answer there just then (it says so). The window's Change Address does the same. Only
the port of a gateway paired without a pinned key (one whose certificate is trusted normally, by its name) can change:
another name takes pairing again. A gateway on this machine can't be paired with: Files mode shows the folders of
another system.

### Files

Listing needs no network: the index follows each folder's changes on the gateway, and they show as they arrive. A file
is fetched when it is read, in blocks, each checked against the hashes Syncthing keeps of the file; a read waits only for
the blocks it covers, so opening a large video and seeking in it doesn't wait for the whole file. A file read to the end
is kept in the cache. A program that opened a file before it changed on the gateway goes on reading the version it
opened.

With the gateway out of reach, files in the cache open as usual; others fail with an input/output error after about
90 seconds.

**Keep on this device** fetches a file, or everything in a directory (including what is added to it later), and keeps
it regardless of the cache's budget:

```bash
synctrain-files keep ~/Synctrain/Documents/Taxes
synctrain-files keep --stop ~/Synctrain/Documents/Taxes
synctrain-files state ~/Synctrain/Documents/*.pdf
```

The same through extended attributes: `setfattr -n user.synctrain.keep -v 1 <path>` (or `-v 0`), and
`getfattr -n user.synctrain.state <path>`, which is `remote`, `partial`, `local` or `kept`.

### Changing files

Files and directories in `~/Synctrain` are changed as on a local disk, in folders the gateway lets this machine write
(in others, a write fails with "Read-only file system"):

- **A change is kept here before the program that made it is told it succeeded**, so a crash or a power loss loses
  nothing it was told was saved. It goes to the gateway once the folder has been quiet for two seconds, in the order it
  was made, and waits here while the gateway is out of reach. `synctrain-files wait <path>` waits until changes are on
  the gateway; `user.synctrain.state` is `pending` until then.
- **Saves keep the file's identity.** Editors and tools save through temporary files and renames (GNOME's apps,
  LibreOffice, vim, `sed -i`, `rsync`): such a save reaches the gateway as one new version of the file, which keeps its
  history there, and the temporary names never do. A save that leaves the content as it was sends nothing.
- **Conflicts:** a file changed on the gateway while it was being edited here is kept as both: the edit becomes a copy
  named as Syncthing names conflict copies (`report.sync-conflict-20261102-101500-ABCDEFG.odt`), next to the gateway's
  version. A name taken on the gateway meanwhile does the same for a new file. Removing or moving a file that changed on
  the gateway is undone, which leaves the gateway's version.
- **Stays here:** LibreOffice's `.~lock.…#`, vim's swap files and Emacs's `#…#` files, which last as long as an editing
  session, and files the gateway's ignore patterns exclude. `user.synctrain.state` is `local-only`.
- **Can't be done here:** symbolic links, hard links and device nodes ("Operation not permitted"); moving an item to
  another folder (`mv` copies it and removes the original); exchanging two items in one rename. Modes and owners are the
  mount's, and setting them does nothing.
- A program that still has a file open after it was removed or replaced (as LibreOffice keeps a document it saved
  through a temporary file) can go on writing to it; that is dropped when it closes, as on a local disk.

`synctrain-files pending` lists changes that aren't on the gateway yet. One the gateway refuses (the folder became
read-only, say), one whose outcome can't be known (the gateway's database was restored from a backup after it was
sent), and one to a file that was being written when the daemon stopped (its content may be incomplete) are kept for
you to decide: `pending --export <ID> <file>` saves a copy of what it would write, and `pending --discard <ID>` gives
it up, so that the file shows the gateway's version again. Nothing is sent twice.

A folder whose changes aren't on the gateway can't be removed. Files that stay here are moved out when it is, to a new
directory named after the folder in `~/Sync` (or in your home folder if `~/Sync` is itself synchronized).

### GNOME

Each folder is a bookmark in the sidebar of Nautilus and of the file chooser while it is here. Changes that can't be
sent, and files that can't be opened because their gateway is out of reach, are notified (at most once in ten
minutes). When Déjà Dup is installed, `~/Synctrain` is added to the folders it leaves out of backups.

GNOME treats `~/Synctrain` as a local disk, and some of its programs read every file they see:

- **Search (LocalSearch)** would read everything to index it. `~/Synctrain` has a `.trackerignore`, so it isn't
  indexed; searching in Nautilus there looks at names only.
- **Thumbnails** come from the gateway: when Nautilus lists a directory, the daemon puts the gateway's thumbnails of
  its images (and, when the gateway has ffmpeg and pdftoppm, videos and PDFs) in GNOME's thumbnail cache. GNOME's own
  thumbnailers and metadata extractors may read files that are here already, but are refused files that would have to
  be fetched, so browsing a folder of photos downloads nothing.

**The Nautilus extension** adds Keep on This Device, Stop Keeping on This Device and Previous Versions… to the menu of
items in `~/Synctrain`, and a Synctrain column to the list view. Previous Versions also applies to individual files
in synchronized folders. The package bundles a compiled native module; Python and `nautilus-python` are not required.
Restart Nautilus with `nautilus -q` after installation or upgrade.

RPM upgrades remove the old package-owned Python extension. If you previously copied `synctrain.py` manually to
`~/.local/share/nautilus-python/extensions/`, remove that specific copy before restarting Nautilus to avoid duplicate
menu entries. Other Python extensions and `nautilus-python` can remain installed.

Without the package, build on the target Linux architecture and install the module to Nautilus's extension directory:

```bash
make -C SushitrainCore linux-nautilus
sudo install -Dm755 SushitrainCore/build/linux-$(go env GOARCH)/libsynctrain-nautilus.so \
  "$(pkg-config --variable=extensiondir libnautilus-extension-4)/libsynctrain-nautilus.so"
nautilus -q
```

### KDE Plasma and Dolphin

The Qt window provides both modes and uses the same private admin sockets, configuration,
credentials, and engine as the GNOME window. Native previews use Qt, and the tray is a Plasma
StatusNotifierItem. Closing the window leaves services running; Quit and Stop explicitly stops them.

The KDE package installs two native Dolphin plugins: Synctrain context-menu actions and status
overlays. Enable **Synctrain** under Dolphin's **Configure Dolphin → Context Menu**, and enable
icon overlays if disabled. Close and reopen Dolphin after installation or upgrade; `kquitapp6 dolphin`
closes its current windows. No Python extension is required.

Managed items offer Open in Synctrain and View Sync Status. Single files also offer Previous
Versions. Synchronized items offer Rescan, and Files-mode items offer Keep on This Device and
Stop Keeping. Compatible multiple selections are supported. Status badges report synchronization and
local availability; View Sync Status distinguishes cached from retained files. Unknown status never
shows a success badge. Status discovery is asynchronous, so a newly opened view may briefly have no badges.

Synctrain-owned Places bookmarks follow mounted folders even without an open window. User-created
bookmarks are preserved. Gateway thumbnails use the shared freedesktop thumbnail cache. Baloo and
KDE thumbnail workers cannot fetch remote content merely to index or preview a directory; normal
KIO file copies and explicit opens remain available.

If integration is absent, check `rpm -q synctrain-common synctrain-kde`, restart Dolphin, and
inspect `journalctl --user -u synctrain-files -u synctrain-gateway`. Verify that the selected mode's
service is running. `synctrain-kde status <absolute-path>` uses the same metadata as the badges.

## Limitations so far

- Symbolic links on the gateway are not shown in Files mode.
- One mode per machine: a machine in Synchronize mode can't also show another gateway's folders.

## Development

The Go code is in `SushitrainCore`: `remoteapi` (the remote file API's types, shared with the gateway), `remoteclient`
(a client of a gateway), `files` (the index, sync and cache of Files mode) and `files/fusefs` (the mount), with the
daemon in `cmd/synctrain-files`; `gatewayadmin` (what the gateway's admin socket answers) and `cmd/synctrain-gateway`;
the shared desktop lifecycle in `linuxdesktop` and `cmd/synctrain-desktop`; and the windows in
`cmd/synctrain` (gotk4/libadwaita) and `Linux/kde` (C++20/Qt 6/Kirigami). `remoteclient` and `files` are tested against a
real gateway in-process (`gateway/gatewaytest`), also on macOS; the FUSE tests run on Linux with `/dev/fuse`:

```bash
cd SushitrainCore
go test -tags noassets ./remoteapi ./adminsock ./remoteclient ./files/... ./gatewayadmin ./sthome \
    ./cmd/synctrain-gateway ./cmd/synctrain-files
make linux-test   # test executables to run on another Linux host
```

The focused desktop regression subset takes about a minute on the Fedora KDE ARM64 VM.
Run it with a six-minute outer budget and a five-minute per-package deadline:

```bash
cd SushitrainCore
timeout 360 go test -tags noassets ./linuxdesktop ./desktopapi ./desktopsettings ./enginecontrol \
    ./linuxpolicy ./history ./safecopy ./previewcache ./adminsock ./files ./files/fusefs \
    ./cmd/synctrain-files ./cmd/synctrain-gateway ./cmd/synctrain-desktop -count=1 -timeout 300s
```

Bound RPM builds with `timeout 1800 Linux/build/build-rpm.sh`. The CI matrix builds GNOME
and KDE components and packages on Fedora 44 for ARM64 and x86-64. Fedora 45 KDE acceptance
results and remaining qualification gaps are in [linux-kde-acceptance.md](linux-kde-acceptance.md).

`Linux/test/e2e-files.sh` runs a scratch gateway and Files daemon on one machine, in `/tmp`, without touching the
session's own. The window can be checked without a desktop through GTK's Broadway backend (`gtk4-broadwayd :5`, then
`GDK_BACKEND=broadway BROADWAY_DISPLAY=:5 synctrain` and http://127.0.0.1:8085 in a browser that is in view, since a
hidden page holds Broadway's frames back; a headless Chrome works too). `SYNCTRAIN_FILES_SOCKET`, `SYNCTRAIN_GATEWAY_ADMIN`
and `STHOMEDIR` point it at scratch daemons, and `XDG_CONFIG_HOME` at a scratch mode. Setting a mode up or switching
starts and stops the real units with `systemctl --user`: to try it without touching your own, put a stand-in
`systemctl` first on the window's `PATH` that runs scratch daemons (the window refuses when `STHOMEDIR` or
`SYNCTRAIN_GATEWAY_ADMIN` differ from your service manager's, as its units would use another home).

The client follows the Swift client in `SynctrainFileProviderKit` and `Docs/remote-api/invariants.md`: a change to the
API needs both.

## Assistant access

The bundled [MCP server](mcp.md) provides structured inspection and management
through local stdio. Start Synctrain and explicitly enable MCP in Settings;
enabling it grants full management access. Headless gateways use the documented
local enable/disable command and can connect remote clients through SSH.
