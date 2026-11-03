# Synctrain for Linux

Synctrain for Linux targets Fedora Workstation with GNOME. As on the Mac, a machine runs Synctrain in one of two modes:

- **Synchronize**: it runs Syncthing, and every folder syncs all of its contents. Phones and machines in Files mode can
  reach its folders through the remote file service, once it is turned on.
- **Files**: it runs no Syncthing. The folders of a gateway on another system, which has all their files, appear in
  `~/Synctrain`, and files are fetched from the gateway as they are read.

Three programs make it up: `synctrain` is the window, `synctrain-gateway` runs Syncthing in Synchronize mode
([gateway.md](gateway.md)), and `synctrain-files` is the daemon of Files mode. Each daemon is a systemd user service,
and only the one of the machine's mode is enabled. They run without the window, which only manages them and keeps no
state of its own; the mode is in `~/.config/synctrain/mode`.

## Installing

On a Fedora host with Go (the release in `SushitrainCore/go.mod`, newer than Fedora's) and `rpm-build`:

```bash
Linux/build/build-rpm.sh
sudo dnf install SushitrainCore/build/rpm/x86_64/synctrain-*.rpm
```

The window links GTK 4 and libadwaita. Without `gtk4-devel` and `libadwaita-devel` installed, the script builds it in a
rootless podman container (`Linux/build/build-window.sh`, Fedora with those packages, using the host's Go and module
cache). The package needs `fuse3`, and recommends `nautilus-python` (the Nautilus extension), `ffmpeg-free` and
`poppler-utils` (video and PDF thumbnails for phones). The names Sushitrain and Synctrain and the logo aren't under the
MPL (README, License), so the package is for your own machines.

To build the programs only: `make -C SushitrainCore linux` (the two daemons, statically linked) and
`make -C SushitrainCore linux-window` (the window, with GTK's development packages).

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

In Synchronize mode, the window shows the device ID, Syncthing's folders and devices, and opens the web GUI, where
folders and devices are added and Syncthing is set up as usual. **Remote Files** manages the remote file service:

- It is off until you turn it on. Then it listens on port 20920; the host name in pairing codes is the machine's name
  unless you give another (the one phones reach it at).
- **Shared folders** are what clients may be given: not shared, read only (which includes previous versions), or read
  and write.
- **Pair…** shows a code to scan in Synctrain on an iPhone, iPad or Mac, with a link to paste in Synctrain on a machine
  in Files mode. When a client submits it, you choose which folders it gets, and approve or deny.
- **Clients** lists paired phones and machines; each one's access per folder can be changed, and a client revoked.
- **Keep Running After Logging Out** turns on systemd's linger for your user, so that phones reach the folders while
  nobody is logged in.

**Files** starts `synctrain-files.service`. Its pages are the folders in `~/Synctrain` (Add…, Remove from This Device),
the gateways (Pair… with a link from the gateway, Forget), changes that aren't on their gateway yet (Export…, Discard),
and the cache's size. **Previous Versions** of a file, opened from Nautilus (or `synctrain versions <path>`), lists what
the folder's versioning keeps on the gateway, to open, restore or save a copy of.

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
folders are all removed. A gateway on this machine can't be paired with: Files mode shows the folders of another system.

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
items in `~/Synctrain`, and a Synctrain column to the list view. The package installs it; it needs `nautilus-python`
(`sudo dnf install nautilus-python`, then `nautilus -q`). Without the package:

```bash
mkdir -p ~/.local/share/nautilus-python/extensions
cp Linux/nautilus/synctrain.py ~/.local/share/nautilus-python/extensions/
nautilus -q
```

## Limitations so far

- Symbolic links on the gateway are not shown in Files mode.
- One mode per machine: a machine in Synchronize mode can't also show another gateway's folders.

## Development

The Go code is in `SushitrainCore`: `remoteapi` (the remote file API's types, shared with the gateway), `remoteclient`
(a client of a gateway), `files` (the index, sync and cache of Files mode) and `files/fusefs` (the mount), with the
daemon in `cmd/synctrain-files`; `gatewayadmin` (what the gateway's admin socket answers) and `cmd/synctrain-gateway`;
and the window in `cmd/synctrain` (gotk4 and libadwaita, Linux only). `remoteclient` and `files` are tested against a
real gateway in-process (`gateway/gatewaytest`), also on macOS; the FUSE tests run on Linux with `/dev/fuse`:

```bash
cd SushitrainCore
go test -tags noassets ./remoteapi ./adminsock ./remoteclient ./files/... ./gatewayadmin ./sthome \
    ./cmd/synctrain-gateway ./cmd/synctrain-files
make linux-test   # test executables to run on another Linux host
```

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
