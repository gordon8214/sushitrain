# Synctrain for Linux

Synctrain for Linux targets Fedora Workstation with GNOME. As on the Mac, a machine runs Synctrain in one of two modes:

- **Synchronize**: it runs Syncthing, and every folder syncs all of its contents. On Linux that is `synctrain-gateway`
  ([gateway.md](gateway.md)), which can also serve the machine's folders to phones and to machines in Files mode.
- **Files**: it runs no Syncthing. The folders of a gateway on another system, which has all their files, appear in
  `~/Synctrain`, and files are fetched from the gateway as they are read.

This page is about Files mode, which is `synctrain-files`. **So far it reads only**: files can be opened, kept on this
machine and let go of, but not changed. Saving into `~/Synctrain` fails with "Read-only file system".

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
| The index of the folders, and the credentials of gateways | `~/.local/state/synctrain/files` | `--state` |
| Files fetched from gateways, and downloads in progress | `~/.cache/synctrain` | `--cache` |
| The folders | `~/Synctrain` | `--mount` |
| The admin socket the commands below use | `$XDG_RUNTIME_DIR/synctrain/files.sock` | `--socket` |

`--cache-budget` sets how many MiB of fetched files are kept (5 GiB by default); the least recently opened go first
beyond that. Files kept on purpose (below) don't count. `--log-level debug` logs what the daemon does, including the
program behind each request it refuses.

Only one daemon runs with a state directory: a second one refuses to start.

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

## GNOME

GNOME treats `~/Synctrain` as a local disk, and some of its programs read every file they see:

- **Search (LocalSearch)** would read everything to index it. `~/Synctrain` has a `.trackerignore`, so it isn't
  indexed; searching in Nautilus there looks at names only.
- **Thumbnails**: programs that make thumbnails or extract metadata may read files that are here already, but are
  refused files that would have to be fetched, so browsing a folder of photos in Nautilus downloads nothing. Such files
  have no thumbnail until they are here.

## Limitations so far

- Nothing can be changed in `~/Synctrain`.
- Credentials are kept in a file only the owner can read (`credentials.json` in the state directory), not in GNOME's
  keyring.
- Symbolic links on the gateway are not shown. Previous versions can't be browsed here yet.

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
