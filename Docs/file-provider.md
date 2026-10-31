# Files app integration

On iOS and iPadOS, Synctrain is a client of [gateways](gateway.md): Macs running Synctrain, or servers running
`synctrain-gateway`, that synchronize folders with Syncthing. Nothing runs Syncthing on the phone. The folders you choose
from a paired gateway appear in the app and in a **Synctrain** location in the Files app. Files can be opened, edited,
added, renamed, moved and deleted there (and from any app that opens documents through Files), and the changes go to the
gateway, which synchronizes them like any other change.

The Synctrain location exists while a gateway is paired: it is added with the first pairing, and removed when the last
gateway is forgotten. On a Mac, which runs Syncthing itself, it is an option that needs no gateway: see
[On macOS](#on-macos).

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

## On macOS

Synctrain on a Mac can show folders in Finder without keeping all of their files. It is off by default, and folders stay
synchronized in full until you choose otherwise. It needs no gateway: the Mac's own engine serves Finder, and fetches files
from your other devices. [macos.md](macos.md#files-in-finder) describes how.

Turn on **Show folders in Finder** under Settings › Files in Finder. macOS then asks you to allow Synctrain, in System
Settings under General › Login Items & Extensions › File Providers. **Switch to On Demand** next to a folder brings it to
Finder, under Locations. Files a connected device has are removed from this Mac; those that only this Mac has stay. The
folder stays where it was, for example in `~/Sync`, holding only those. **Synchronize All** switches back, which downloads
all of it. Folders of another Mac or a server can be added too: pair with them on the same page, as on an iPhone.
**Show folders in Finder** can only be turned off again once no folder is on demand or from a gateway, as nothing else
reaches their files.

Everything that works in Files works in Finder: opening files on demand, editing, adding, renaming, moving and deleting
them, **Keep on This Device** and **Stop Keeping on This Device**, thumbnails, conflict copies and **Previous Versions…**.
Because the engine is the Mac's own, some things differ from a folder on a gateway:

- **Opening** a file this Mac doesn't have needs a connected device that has it.
- **Editing and deleting** such a file need no other device, and download nothing. Saving an edit from an app first
  downloads the file, as apps read a file before writing it, so an app can't edit a file nobody can supply.
- **Renaming or moving** an item this Mac doesn't have downloads it first, so the change waits for a device that has it.
  A directory that would take more than 4 GiB is refused: copy what you need instead.
- **Previous Versions…** lists what this Mac archived, which is little in a folder it mostly doesn't hold. The versions other
  devices keep can't be reached through Syncthing.
- **While Synctrain is not running**, browsing and files on this Mac work, and edits wait. Start it at login to avoid that.
- **Names** with `*`, control characters, or a space at the start or end can't be changed in an on-demand folder, nor can
  such files be added there: they stay on this Mac without synchronizing.

Finder's `.DS_Store` and similar files stay on this Mac, and are never sent to other devices. The ones that reached a
gateway from elsewhere are not shown. Files the location can't synchronize, such as those names, stay in their directory,
which can't then be deleted from another device until they are removed.

Such a file also stays when its folder leaves Finder: the system can't remove the directory it is in, so the folder's
directory stays in the location, holding only those files. Synctrain says how many there are and where, and nothing is
removed: they can be files of yours that never reached another device. When the folder comes back, that directory is
renamed to "Name (not synchronized)" and stays next to the folder, which shows all of its own files again.

For now, a file edited in Finder stays in the folder as well as in the system's copy, and takes later changes from other
devices, until the folder is switched back. Deleted paths stay listed in its `.stignore`, so that the deletion stays
announced.

## How it works

The extension is a replicated File Provider extension that talks to gateways itself, so Files works while the app isn't
running. File Provider extensions on iOS may use about 20 MB of memory and only run while the system needs them. The app
and the extension share the app group container, and don't depend on each other at run time. On a Mac without another
gateway, the app is the gateway: the extension connects to the remote file service in the app, over the loopback interface.
The two apps share this code, in `SynctrainClient`.

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
