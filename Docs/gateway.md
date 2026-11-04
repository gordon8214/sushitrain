# Remote file service (gateway)

The remote file service is how Synctrain on iOS reaches folders at all: the phone runs no Syncthing, and browses,
opens and edits them in the app and the Files app. A host that runs Syncthing anyway (a Mac running Synctrain, or a Linux server) serves the
folder over HTTPS; the phone becomes a client of that host, and the host's Syncthing engine keeps synchronizing with
all other devices as usual. Design: [remote-file-provider-plan.md](remote-file-provider-plan.md). API contract:
[remote-api](remote-api/openapi.yaml).

The host is called the gateway below. It must be running and reachable for the phone to open files that are not on
the phone, and for edits to complete. Edits made while it is unreachable wait on the phone and are sent later.

## What the gateway needs

- **One engine.** The gateway is Syncthing: it uses the host's Syncthing configuration, keys, database and device ID.
  It never runs next to another Syncthing instance on the same home directory (it takes Syncthing's lock first and
  refuses to start otherwise).
- **Writable folders** must be ordinary send-receive folders on the host's own filesystem that the host keeps in full. One
  whose ignore patterns exclude everything by default is read-only for clients, whatever they were granted. Read-only exports
  can be any unencrypted folder; files the host doesn't keep are streamed from other devices. A gateway is what a phone, or a
  [Mac in Files in Finder mode](macos.md#two-modes), has instead of the files, so it should have all of them.
- **HTTPS.** Either a certificate clients trust normally (such as Let's Encrypt), or a self-signed certificate whose
  public key the pairing code carries (clients then accept exactly that key). Arbitrary certificates are never
  accepted. The gateway can also sit behind a reverse proxy (below).
- **A reachable address**: the local network, a VPN, or a public address you configure. The gateway doesn't open
  ports or use Syncthing relays for this.

## Pairing

Pairing creates one credential per phone, with access to the folders you grant (read or read and write; read
includes the folder's previous versions).

1. On the gateway, start pairing. It shows a QR code and a `synctrain+pair://…` link that works once, for ten minutes.
2. In Synctrain on the phone, scan it (on first launch, or Settings › Gateways › Pair with a Gateway).
3. Approve the request on the gateway, choosing the folders.

Revoke a phone's credential on the gateway at any time; its edits that haven't been sent stay on the phone and can be
exported there.

## Synctrain on macOS

In Synchronize mode, Settings › Remote file service: turn it on, choose the port and certificate, export folders, pair
phones, and see pending operations and clients. The Mac must stay awake and Synctrain running (it can start at login without a
window); while the service runs, Synctrain keeps macOS from napping it. Sleep is an ordinary outage. Thumbnails are rendered
with Quick Look. A Mac in [Files in Finder mode](macos.md#two-modes) runs no engine, so it can't be a gateway.

## Linux (`synctrain-gateway`)

`synctrain-gateway` is a single executable that runs Syncthing (with its usual web GUI and REST API, as its
`config.xml` sets them) and the remote file service. Build it with `make gateway-linux` in `SushitrainCore`
(`build/linux-<architecture>/synctrain-gateway`, statically linked). The default architecture is the build host's;
use `LINUX_ARCH=amd64` or `LINUX_ARCH=arm64` to select a target explicitly.

```
synctrain-gateway serve --home /var/syncthing/config --listen :20920 \
    --cert /var/syncthing/config/https-cert.pem --key /var/syncthing/config/https-key.pem \
    --public-host nas.example.org
```

| Option | Environment | Meaning |
| --- | --- | --- |
| `--home` | `STHOMEDIR` | Syncthing's home: `config.xml`, `cert.pem`, `key.pem`, `index-v2/`. Without it, the directories stock Syncthing would use for the user (including its legacy locations) |
| `--listen` | `SYNCTRAIN_GATEWAY_LISTEN` | Address of the remote file service (default `:20920`) |
| `--cert`, `--key` | `SYNCTRAIN_GATEWAY_CERT`, `SYNCTRAIN_GATEWAY_KEY` | Certificate files; reloaded when they change. Without them, a self-signed certificate is created |
| `--public-host`, `--public-port` | `SYNCTRAIN_GATEWAY_HOST`, `SYNCTRAIN_GATEWAY_PORT` | Where phones connect, for pairing codes (the host without a port; behind a reverse proxy, set the proxy's port) |
| `--proxy`, `--proxy-spki` | `SYNCTRAIN_GATEWAY_PROXY=1`, `SYNCTRAIN_GATEWAY_PROXY_SPKI` | Plain HTTP for a reverse proxy |
| `--service` | `SYNCTRAIN_GATEWAY_SERVICE` | `on` or `off`: whether the remote file service runs, which `synctrain-gateway service` then can't change. By default it runs as that last set it |
| `--service-default` | `SYNCTRAIN_GATEWAY_SERVICE_DEFAULT` | `on` (the default) or `off`: whether the remote file service runs before `synctrain-gateway service` first turns it on or off |
| `--socket` | `SYNCTRAIN_GATEWAY_ADMIN` | Admin socket (default `<home>/synctrain-gateway/admin.sock`) |
| `--admin-group` | `SYNCTRAIN_GATEWAY_ADMIN_GROUP=1` | Let members of the socket's group use it too (it is the owner's only otherwise) |
| `--db-maintenance-interval`, `--db-delete-retention-interval` | `STDBMAINTENANCEINTERVAL`, `STDBDELETERETENTIONINTERVAL` | As for stock Syncthing (default `8h` and `10920h`) |
| | `STGUIADDRESS`, `STLOGLEVEL`, `STTRACE` | As for stock Syncthing |

The service keeps its own database, staged uploads and content cache in `<home>/synctrain-gateway/` (in Syncthing's
data directory when the home isn't given; never inside Syncthing's database). Folders whose deletions go to the system
Trash (`basic-trash`) can't be exported for writing on Linux; use Syncthing's versioning instead.

Phones can browse, preview, restore and copy the previous versions of a file that the folder's Syncthing versioning
keeps (simple, staggered or trash can; external versioning can't be listed). Turn versioning on for every exported
folder whose history phones should see, and leave its versions path empty: versions in a custom path aren't shown,
as that path may be shared with another folder (or point into one) that a phone has no access to. Read access to a
folder includes its history: a phone that may only read a folder can still see and preview its previous versions.
The gateway API can restore and copy its own archives through the same journal as other edits: the version is copied, the
replaced contents are archived as usual (the trash can then keeps those instead of the restored version), and only
the file's contents are written, not platform metadata kept with the version. Version reads are recorded in the audit
log. An edit or restore that writes the contents a file already has (as when a phone saves a file it only opened)
archives nothing, so the history doesn't fill up with copies of the same contents, unless the file has extended
attributes, which only its archived copy would keep.

### Additional history sources

On macOS, iPhone and iPad, Settings > History Sources can include archives from other paired gateways. Enable a
paired gateway or choose **Pair with a History Source…**. History-only pairing grants access without adding its
folders to Finder or Files, downloading its index, or changing the file currently stored there. Existing gateways
are not automatically enabled as additional sources; the current file's own gateway is always included.

Files match by their Syncthing folder ID and current relative path, not by the folder label or a gateway's item ID.
The source needs `versions.byPath` in its capabilities (update Synctrain or `synctrain-gateway` if the browser asks).
Read permission is enough, and archives can be downloaded even when the current file has been deleted on the source.
The default `.stversions` restriction also applies to additional sources. Renames are not traced to older names.

Previous Versions combines sources by date and shows their names and independent connection errors. Open and Save a
Copy download the archive directly. Restore replaces the file at its current destination, using the Mac engine or
an upload to the folder's primary gateway. It checks the destination revision before replacing anything and refuses
pending edits. The replaced contents are archived according to the destination's versioning; when it is off, they
are not kept. An upload that cannot finish stays in Pending Edits and is never reported as a completed restore.
Source settings are stored on each device, shared with its Files action extension; credentials remain in the Keychain.
Path-based lists include a strong archive validator. Downloads use `If-Match`, so a replaced archive slot is rejected
even if its length and original modification time are unchanged. Older primary gateways retain their existing
server-side restore operation; additional history sources require the updated path-based API.

On iOS, large restore uploads can move to a distinct background session for the app or the Files action extension;
the containing app finishes the extension's events after it stops. The staged edit remains in the shared outbox until
the destination confirms it. On a synchronizing Mac, archiving before publication is protected by a durable journal
and a hard-linked recovery copy of the original (including its platform metadata). Recovery runs before the engine's
scanners and pullers start. A pending marker in the engine configuration directory also blocks startup until a volume
with an interrupted restore is available, even if the folder was reconfigured. Completed recovery removes its
artifacts and journal durably before removing that marker. A filesystem unable to preserve that hard link refuses the restore before displacing
the original; Open and Save a Copy remain available. Local archives retain native restoration, including saved
platform metadata; cross-host downloads restore file contents.

When the home has no `cert.pem` and `key.pem`, the gateway logs a warning and creates a new device identity (and a
new `config.xml` when there is none); a certificate or key that exists but can't be read stops it instead. A restart
from Syncthing's web GUI or REST API restarts the engine and the service within the process. `SIGINT` or `SIGTERM`
stops both within 8 seconds (container runtimes allow 10); a second signal exits at once.

Manage the running service with the same executable (it talks to the admin socket; anyone who can open the socket can
manage the service). The gateway refuses a socket directory that another user owns, that everyone can write without
the sticky bit, or that its group can write without `--admin-group`, since someone could put a socket of their own in
its place. It also keeps `<home>/synctrain-gateway/` readable by its own user only.

```
synctrain-gateway status
synctrain-gateway service --off
synctrain-gateway service --on --listen :20921 --public-host nas.example.org    # unless serve's options set them
synctrain-gateway exports --add Documents --write
synctrain-gateway pair --folder Documents:write
synctrain-gateway clients
synctrain-gateway clients --revoke <client ID>
synctrain-gateway devices --add <device ID> --name laptop
synctrain-gateway folders --share Documents --device <device ID>
```

The remote file service can be turned off and on while Syncthing runs (`synctrain-gateway service`, or the window of
Synctrain for Linux), and its address and public host changed with it. Those settings are kept in
`<home>/synctrain-gateway/service.json`; what serve's options or environment set can't be changed this way. While the
service is off, Syncthing and the admin socket run as usual, and the service's commands answer that it is off. On a
Linux desktop, Synctrain runs the gateway with the service off until it is turned on: see [linux.md](linux.md).
Without `--service on`, a service that can't start (its port is taken, say) leaves Syncthing running and says why in
`synctrain-gateway service`; a server that should stop instead runs with `--service on`.

### Replacing a stock Syncthing

The gateway can take over an existing Syncthing home in place: same device ID, configuration and database, so other
devices see no change. Before switching:

1. Stop the stock Syncthing, and back up the home directory (configuration, `cert.pem`, `key.pem`, `index-v2/`)
   together with the folders. A filesystem snapshot is best.
2. Use a gateway built with the same Syncthing version or newer. It refuses databases with a newer schema than its
   engine supports, as a downgrade would damage them.
3. Start `synctrain-gateway serve` with the same home.

To return to the stock Syncthing, stop the gateway and start the stock Syncthing on the same home (with a version no
older than the gateway's engine). `<home>/synctrain-gateway/` can be removed; phones then show their folders as unreachable
until they are removed or paired with another gateway.

### Container example

An image based on the official one (whose entrypoint drops privileges with `PUID`/`PGID`):

```dockerfile
FROM syncthing/syncthing:2.1
COPY synctrain-gateway /bin/synctrain-gateway
ENTRYPOINT ["/bin/entrypoint.sh", "/bin/synctrain-gateway", "serve"]
```

Publish the service port next to Syncthing's (`20920/tcp`), and mount the admin socket's directory if you want to run
the management commands from the host (with `SYNCTRAIN_GATEWAY_ADMIN_GROUP=1` when the host user reaches it through
a shared group, as on TrueNAS).

## Reverse proxies

A proxy that terminates TLS must: pass `Authorization`, `Range`, `If-Range`, `ETag`, `Tus-*` and `Upload-*` headers
unchanged; allow `PATCH`, `HEAD`, `DELETE` and `OPTIONS`; not buffer request or response bodies; use timeouts longer
than 60 seconds (the longest long poll); and never redirect `/v1` paths. Clients see the proxy's certificate: either a
normally trusted one, or pass its public-key hash with `--proxy-spki` so pairing codes carry it.

```nginx
location /v1/ {
    proxy_pass http://127.0.0.1:20920;
    proxy_http_version 1.1;
    proxy_request_buffering off;
    proxy_buffering off;
    proxy_read_timeout 300s;
    proxy_send_timeout 300s;
    client_max_body_size 0;
}
```

## Recovery and consistency

- Edits are published inside the folder's Syncthing loop, where scanning and pulling can't interfere, and indexed
  immediately; a client is told an edit is committed only then. A crash at any point is recovered from the service's
  journal at the next start, without applying an edit twice.
- An edit based on a version that is no longer current is kept as a conflict copy, named as Syncthing names them, or
  rejected when Files asks for that. Deletes and moves of changed items are rejected, leaving the data intact.
- If Syncthing's database or the service's database is reset or restored from a backup, the gateway starts a new
  *authority generation*: phones take a fresh listing, and edits they couldn't confirm are shown for review rather
  than sent again.
- Content is verified against Syncthing's block hashes before it is served, whether it comes from the gateway's disk
  or from another device. Previous versions, which only get previewed, are served from the gateway's disk as they
  are.
- The audit log (in the service database) records which client did what, by client, operation and item ID (no file
  names).

## macOS extended attributes

The shared Syncthing engine defaults to Apple's syncable-attribute policy for
folders that send or sync extended attributes. In the Syncthing web folder settings,
**macOS extended attributes** selects **Syncable attributes (recommended)** or
**All attributes** (`xattrFilter.macOSMode`: `syncable` or `all`). Explicit filter
rules retain their first-match, default-deny override behavior. Size limits remain
configurable; zero means unlimited.

Finder info, resource forks, tags, comments, text encoding, and quarantine are
included by Apple's policy. Unmarked application metadata is generally excluded.
The NAS still relays and archives foreign Mac metadata; existing historical
attributes remain archived, but Mac receivers apply only attributes permitted by
their current policy. Excluded source attributes do not enter future NAS history.
Use All attributes when archival completeness is required. Folders with extended
attribute synchronization disabled remain disabled.

### Complete Mac snapshots for BetterFinder

The `versions.snapshot` capability adds `GET /v1/folders/{folderID}/versions/{versionID}/snapshot?path=…`.
Path-based listings include `metadataStatus` and, for complete archives, `snapshotETag`.
The snapshot request requires that validator in `If-Match`; ordinary read access is sufficient.
The tar stream contains a versioned manifest, the data fork, and numbered binary extended-attribute entries,
including the resource fork. Identities, lengths, and SHA-256 digests are checked before any file is published.
Missing platform-history records, absent Darwin capture, and corrupt records never authorize partial recovery.
An explicitly captured empty attribute set is complete. Existing content endpoints remain compatible.

On macOS, the app provides a private, authenticated history bridge to BetterFinder. It owns pairings and credentials;
BetterFinder links only SynctrainHostKit, lists the enabled history sources, and materializes verified copies.
Restore targets the synchronized file on this Mac through the engine's guarded restore transaction. It restores
all archived Darwin attributes, without applying the current sync filter to the recovered copy. Normal subsequent
synchronization still follows the folder's existing policies. Attributes excluded before archiving cannot be recovered.

The bridge admits only this user's Apple-signed BetterFinder from team UF6P8KZZN4, and BetterFinder verifies the
host's signature in return. Its versioned requests are bounded; version handles expire with the host/configuration
or after one hour. Temporary materializations are released after copying and swept after interrupted sessions.
The app's existing history windows and other-platform clients keep their existing content-only export behavior.
