# Remote file service (gateway)

The remote file service lets Synctrain on iOS browse, open and edit a folder in the Files app without running
Syncthing on the phone. A host that runs Syncthing anyway (a Mac running Synctrain, or a Linux server) serves the
folder over HTTPS; the phone becomes a client of that host, and the host's Syncthing engine keeps synchronizing with
all other devices as usual. Design: [remote-file-provider-plan.md](remote-file-provider-plan.md). API contract:
[remote-api](remote-api/openapi.yaml).

The host is called the gateway below. It must be running and reachable for the phone to open files that are not on
the phone, and for edits to complete. Edits made while it is unreachable wait on the phone and are sent later.

## What the gateway needs

- **One engine.** The gateway is Syncthing: it uses the host's Syncthing configuration, keys, database and device ID.
  It never runs next to another Syncthing instance on the same home directory (it takes Syncthing's lock first and
  refuses to start otherwise).
- **Writable folders** must be ordinary send-receive folders on the host's own filesystem that the host keeps in full
  (not on-demand or selective). Read-only exports can be any unencrypted folder; files the host doesn't keep are
  streamed from other devices.
- **HTTPS.** Either a certificate clients trust normally (such as Let's Encrypt), or a self-signed certificate whose
  public key the pairing code carries (clients then accept exactly that key). Arbitrary certificates are never
  accepted. The gateway can also sit behind a reverse proxy (below).
- **A reachable address**: the local network, a VPN, or a public address you configure. The gateway doesn't open
  ports or use Syncthing relays for this.

## Pairing

Pairing creates one credential per phone, with access to the folders you grant (read or read and write; read
includes the folder's previous versions).

1. On the gateway, start pairing. It shows a QR code and a `synctrain+pair://…` link that works once, for ten minutes.
2. In Synctrain on the phone, scan it (Settings › Remote folders › Pair).
3. Approve the request on the gateway, choosing the folders.

Revoke a phone's credential on the gateway at any time; its edits that haven't been sent stay on the phone and can be
exported there.

## Synctrain on macOS

Settings › Remote file service: turn it on, choose the port and certificate, export folders, pair phones, and see
pending operations and clients. The Mac must stay awake and Synctrain running (it can start at login without a
window); while the service runs, Synctrain keeps macOS from napping it. Sleep is an ordinary outage. Thumbnails are
rendered with Quick Look.

## Linux (`synctrain-gateway`)

`synctrain-gateway` is a single executable that runs Syncthing (with its usual web GUI and REST API, as its
`config.xml` sets them) and the remote file service. Build it with `make gateway-linux` in `SushitrainCore`
(`build/linux-amd64/synctrain-gateway`, statically linked).

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
Restoring and copying happen on the gateway, through the same journal as other edits: the version is copied, the
replaced contents are archived as usual (the trash can then keeps those instead of the restored version), and only
the file's contents are written, not platform metadata kept with the version. Version reads are recorded in the audit
log. An edit or restore that writes the contents a file already has (as when a phone saves a file it only opened)
archives nothing, so the history doesn't fill up with copies of the same contents, unless the file has extended
attributes, which only its archived copy would keep.

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
synctrain-gateway exports --add Documents --write
synctrain-gateway pair --folder Documents:write
synctrain-gateway clients
synctrain-gateway clients --revoke <client ID>
synctrain-gateway devices --add <device ID> --name laptop
synctrain-gateway folders --share Documents --device <device ID>
```

### Replacing a stock Syncthing

The gateway can take over an existing Syncthing home in place: same device ID, configuration and database, so other
devices see no change. Before switching:

1. Stop the stock Syncthing, and back up the home directory (configuration, `cert.pem`, `key.pem`, `index-v2/`)
   together with the folders. A filesystem snapshot is best.
2. Use a gateway built with the same Syncthing version or newer. It refuses databases with a newer schema than its
   engine supports, as a downgrade would damage them.
3. Start `synctrain-gateway serve` with the same home.

To return to the stock Syncthing, stop the gateway and start the stock Syncthing on the same home (with a version no
older than the gateway's engine). `<home>/synctrain-gateway/` can be removed; phones then show their remote folders as
unreachable until they are switched back to Syncthing or paired with another gateway.

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
