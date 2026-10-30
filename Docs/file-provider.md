# Files app integration

On iOS and iPadOS, Synctrain is a client of [gateways](gateway.md): Macs running Synctrain, or servers running
`synctrain-gateway`, that synchronize folders with Syncthing. Nothing runs Syncthing on the phone. The folders you choose
from a paired gateway appear in the app and in a **Synctrain** location in the Files app. Files can be opened, edited,
added, renamed, moved and deleted there (and from any app that opens documents through Files), and the changes go to the
gateway, which synchronizes them like any other change.

The Synctrain location exists while a gateway is paired: it is added with the first pairing, and removed when the last
gateway is forgotten. On macOS there is no location: Synctrain runs Syncthing itself there.

## Using it

Turn the location on in Files (Browse, then tap Synctrain under Locations). Settings › Gateways shows whether it is on.

Everything goes through the gateway over HTTPS, whether or not Synctrain is running:

- **Pairing**: on first launch, or Settings › Gateways › Pair with a Gateway, scan the gateway's code (or paste its
  link) and approve the phone on the gateway.
- **Choosing folders**: Settings › Folders lists what each gateway lets this phone access. **Add** brings a folder to
  this phone, in the app and in Files; **Remove from This Device** takes it away again, and it stays on the gateway.
  Removing is refused until the folder's edits are sent, exported or discarded, and asks first when Files holds changes
  it hasn't handed over.
- **Saving**: an edit is reported as saved only once the gateway has committed it. While the gateway can't be reached,
  Files keeps the edit and tries again; Synctrain sends it as well when it runs. A large edit still being sent after a
  minute (or when Synctrain is about to be suspended) is sent on in the background, also while neither Files nor
  Synctrain runs; Files shows it as waiting until the gateway has committed it. A save that leaves the contents as the
  phone downloaded them (an app saving a file it only opened) sends nothing, so the file and its history on the
  gateway stay as they are. An edit based on a version that changed meanwhile is kept as a conflict copy. Edits that
  could not be sent are listed on the Files app page with **Export…** (a copy of the edit) and **Discard**.
- **Opening**: files are downloaded from the gateway in verified parts and kept as long as the system keeps them.
  **Keep on This Device** downloads an item, **Stop Keeping on This Device** removes the downloaded copy (never one
  with an edit that wasn't sent). Thumbnails come from the gateway.
- **Moving** a file to a folder on another gateway copies it there and then deletes the original; directories can't
  be moved between gateways (copy them, then delete them).
- **Previous Versions…** (in a file's menu in Files) lists the versions the folder's Syncthing versioning keeps on the
  gateway, newest first. Versions whose modification time and size equal those of the next newer one (Syncthing
  archives a version when only a file's metadata changes) are left out. Tap a version to preview it (and share it from
  the preview). **Restore** replaces the file with the version on the gateway: the current contents are kept as a
  version (with trash can versioning, in place of the restored one) unless they are the same as the version's. A file
  that changed on the gateway since the phone last saw it is not overwritten, and a file with an edit that hasn't been
  sent can't be restored until it has. **Save a Copy** puts the
  version next to the file as "Name (date time).ext". Both happen on the gateway, so nothing is uploaded; they copy
  the file's contents only, not extended attributes or resource forks that the version may have kept. Versions are
  found by the file's current name, so versions from before a rename aren't shown. This needs a gateway with the
  `versions` feature and versioning turned on for the folder, keeping versions in the default `.stversions` (not
  external versioning, nor a custom versions path). The sheet is shown by the
  **SynctrainFileProviderUI** extension, which reads the index and the gateway's credential from the app group.

The gateway must be reachable (on the local network, a VPN or a public address you set up) for files that are not
on the phone to open, and for edits to complete.

The gateway must be reachable (on the local network, a VPN or a public address you set up) for files that are not
on the phone to open, and for edits to complete.

## Viewing files in the app

The Folders tab browses the same index Files uses, so it also works offline, and follows changes as they arrive. Search
finds files and directories by name in the folders on the phone. It doesn't look inside files, and a folder is searchable
once its first listing has finished.

Tapping a file shows it in Quick Look, or plays it. Audio and video play from the phone once downloaded; files of 64 MB
or more that aren't on the phone yet play from the gateway while they stay there, and can be seeked in. Thumbnails come
from the gateway, and stay available offline once shown. Viewing is read-only: edits go through Files, and **Show in
Files** (in a file's menu) takes you there.

Files opened in the app go into the same cache the extension uses. Settings sets how much space it may take (5 GB by
default), and removes what was used longest ago beyond that. Files you chose to keep in Files are the system's copies, and
aren't affected.

## Limitations

- Tags, favorites and the order in Recents are kept on this device only; gateways don't know about them.
- Symbolic links are not shown. Folders the gateway only lets this phone read are read-only in Files too.
- There is no macOS version yet.

## How it works

The extension is a replicated File Provider extension that talks to gateways itself, so Files works while the app isn't
running. File Provider extensions on iOS may use about 20 MB of memory and only run while the system needs them. The app
and the extension share the app group container, and don't depend on each other at run time.

### Shared container

- `FileProvider/gateway.sqlite`: the index the extension serves items from and the app lists, with each folder's gateway
  metadata and the outbox of edits.
- `FileProvider/Remote/Content`: verified cached files, named after their content identity.
- `FileProvider/Remote/Staged`: immutable staged edits, until the gateway has committed them.
- `FileProvider/Remote/Transfers`: partial downloads, with their manifests and verified ranges.

### Index

Gateway metadata is projected into `items`, with a stable row ID as each item's identifier. Each row has a change
sequence, and the system's sync anchor is the index generation plus that sequence. Items that are gone stay as tombstones
for 30 days, for what the system hasn't asked about yet.

### Changes from the Files app

Contents are staged immutably, the change is recorded in the outbox, and the extension drives it to the gateway's commit
inside the call. A change the gateway hasn't committed is reported as not synchronized, and the system's next attempt
continues the same operation. A row with an unconfirmed change keeps showing it, as an overlay, until the gateway answers.

When an edit is based on a version that has changed on the gateway since, it is kept as a conflict copy named the way
Syncthing names them (or rejected when the system asks for that). Conflict copies carry the start of this device's
client ID, where Syncthing would put a device ID.

### Opening files

A staged edit, or a verified cached copy, is cloned into the system's copy, which takes no extra space. Other files are
downloaded through a read lease, every block verified against the lease's manifest, and published to the cache with an
atomic rename. As files there are immutable and named after their contents, the app and the extension can share the cache
without coordinating; `remote_transfers` keeps them from downloading the same file twice.

`AVPlayer` can't send the credential or apply the gateway's pinned key, and credentials never go into URLs, so streamed
media reaches the player through an `AVAssetResourceLoader`, reading through the gateway client up to 1 MiB at a time
(`RemoteContentStream`). Streamed bytes aren't verified block by block, so they are never stored in the cache.

## Debugging

Debug builds of the extension log every call to `Library/Caches/FileProviderExtension.log` in the app group container.
Unlike the system log, which needs root to collect from a device, it can be copied from a device with:

```
xcrun devicectl device copy from --device <device> --domain-type appGroupDataContainer \
  --domain-identifier <app group> --source Library/Caches/FileProviderExtension.log --destination .
```

**Rebuild Synctrain location** under Settings › Gateways removes the location and sets it up again, starting a new index
generation. Files on gateways are not affected, but changes made in Files that have not reached Synctrain yet are
discarded; Synctrain asks before doing that, and refuses while edits are unresolved.
