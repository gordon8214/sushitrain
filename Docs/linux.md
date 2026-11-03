# Synctrain for Linux

Synctrain for Linux targets Fedora Workstation with GNOME. As on the Mac, a machine runs Synctrain in one of two modes:

- **Synchronize**: it runs Syncthing, and every folder syncs all of its contents. On Linux that is `synctrain-gateway`
  ([gateway.md](gateway.md)), which can also serve the machine's folders to phones and to machines in Files mode.
- **Files**: it runs no Syncthing. The folders of a gateway on another system, which has all their files, appear in
  `~/Synctrain`, and files are fetched from the gateway as they are read.

This page is about Files mode, which is `synctrain-files`.

## Building

On a Linux host with Go (see `SushitrainCore/go.mod` for the version):

```bash
make -C SushitrainCore linux
```

This builds `SushitrainCore/build/linux-amd64/synctrain-files` and `synctrain-gateway`, both statically linked. The
daemon needs `fuse3` (`fusermount3`), which Fedora Workstation has.

## Running

```bash
synctrain-files serve
```

It mounts the folders at `~/Synctrain` and follows their gateways until it is stopped (`SIGINT` or `SIGTERM`, or
unmounting `~/Synctrain`, as Nautilus's sidebar does). As a systemd user service, with the binary in `/usr/bin`:

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

## Gateways and folders

1. On the gateway, start pairing: `synctrain-gateway pair --folder <folder>:read` on a Linux server, or Settings ›
   Remote file service on a Mac in Synchronize mode. It shows a `synctrain+pair://` link.
2. Here: `synctrain-files pair '<link>'`, then approve the request on the gateway.
3. `synctrain-files folders --gateway <service ID>` lists the gateway's folders for this machine, and
   `synctrain-files folders --gateway <service ID> --add <folder>` adds one. It appears in `~/Synctrain`, named after
   its label.

`synctrain-files status` shows the gateways and folders. `synctrain-files folders --remove <ID>` takes a folder out of
`~/Synctrain` (it stays on the gateway), and `synctrain-files gateways --forget <service ID>` forgets a gateway whose
folders are all removed. A gateway on this machine can't be paired with: Files mode shows the folders of another system.

## Files

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

## Changing files

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

## GNOME

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
items in `~/Synctrain`, and a Synctrain column to the list view. It needs `nautilus-python`:

```bash
sudo dnf install nautilus-python
mkdir -p ~/.local/share/nautilus-python/extensions
cp Linux/nautilus/synctrain.py ~/.local/share/nautilus-python/extensions/
nautilus -q
```

## Limitations so far

- Symbolic links on the gateway are not shown.

## Development

The client is Go in `SushitrainCore`: `remoteapi` (the API's types, shared with the gateway), `remoteclient` (a client
of a gateway), `files` (the index, sync and cache) and `files/fusefs` (the mount), with the daemon in
`cmd/synctrain-files`. `remoteclient` and `files` are tested against a real gateway in-process (`gateway/gatewaytest`),
also on macOS; the FUSE tests run on Linux with `/dev/fuse`:

```bash
cd SushitrainCore
go test -tags noassets ./remoteapi ./remoteclient ./files/...
make linux-test   # test executables to run on another Linux host
```

The client follows the Swift client in `SynctrainFileProviderKit` and `Docs/remote-api/invariants.md`: a change to the
API needs both.
