# Files app integration

On iOS and iPadOS, Synctrain is a client of [gateways](gateway.md): Macs running Synctrain, or servers running
`synctrain-gateway`, that synchronize folders with Syncthing. Nothing runs Syncthing on the phone. The folders you choose
from a paired gateway appear in the app and in a **Synctrain** location in the Files app. Files can be opened, edited,
added, renamed, moved and deleted there (and from any app that opens documents through Files), and the changes go to the
gateway, which synchronizes them like any other change.

The Synctrain location exists while a gateway is paired: it is added with the first pairing, and removed when the last
gateway is forgotten. A Mac works this way in one of its two modes: see [On macOS](#on-macos).

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
  could not be sent are listed on the gateways page in Settings with **Export…** (a copy of the edit) and **Discard**.
- **Opening**: files are downloaded from the gateway in verified parts and kept as long as the system keeps them.
  **Keep on This Device** downloads an item, **Stop Keeping on This Device** removes the downloaded copy (never one
  with an edit that wasn't sent). Thumbnails come from the gateway.
- **Moving** a file to a folder on another gateway copies it there and then deletes the original; directories can't
  be moved between gateways (copy them, then delete them).
- **Packages** (directories a Mac shows as one document, like an Xcode project or an Icon Composer icon) show as one
  document in Files too, with the total size of their files, as in iCloud Drive; the app still opens them as folders.
  Opening a package downloads its directory tree, verifying each file, and hands the complete directory to Files.
  If the tree changes during the download, Files retries rather than receiving a mixture of versions.
  The phone's own types and those of
  Apple's and open formats always count, except a few Mac formats named with ordinary words (such as `.help` and
  `.workflow`). A gateway in Synctrain on a Mac also marks the types of every app installed there (a newly installed
  app's within a minute); `synctrain-gateway` marks none. iOS keeps the kind it first saw: a directory already on
  the phone as a folder stays one, a package renamed to a plain name stays a document, and a package copied in from
  where Files shows it as a folder (On My iPhone) stays a folder. **Rebuild Synctrain location** (below) shows each
  as what it is now.
- **Previous Versions…** (in a file's menu in Files and the app) combines the folder gateway with additional gateways
  enabled under Settings › **History Sources**. [Additional history sources](gateway.md#additional-history-sources)
  match by Syncthing folder ID and current path, with source names, dates, sizes, and independent connection errors.
  Open previews a downloaded version, and Save a Copy exports it through the share sheet. Restore downloads the
  archive and uploads it as a guarded edit to the folder's primary gateway; the destination archives its current
  contents according to its versioning. When destination versioning is off, those contents are not kept. Pending
  edits and intervening destination changes prevent restoration. Read-only folders support preview and export.
  The primary gateway needs the `versions` feature; additional sources need `versions.byPath`. Versioning on sources
  must use the default `.stversions` directory. External versioning, custom archive paths, and names before a rename
  are not browsed. The action extension uses the shared index and credentials independently of the app.


The gateway must be reachable (on the local network, a VPN or a public address you set up) for files that are not
on the phone to open, and for edits to complete. The phone connects to the address in the pairing code; to reach the
gateway another way, such as through a name that works away from its network, change **Address** on its page under
Settings › Gateways (`host` or `host:port`). The pairing stays. A gateway with a self-signed certificate (its key is
pinned by the pairing code) is checked against that key, so another service at the new address is refused. One with a
certificate the system trusts is checked by name instead, so only its port can be changed there: its credential would
go to whatever has a valid certificate for another name. To reach it at another name, pair with it again from a code
with that name.

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

A Mac in [Files in Finder mode](macos.md#two-modes) is a client of a gateway, like an iPhone: it runs no Syncthing, and its
folders stay on a gateway on another system that has all their files, which a server running `synctrain-gateway` or a Mac in
Synchronize mode can be. macOS fetches what you open, and can remove it again when space runs short.

Choose **Files in Finder** at the first start, or **Switch to Files in Finder…** in the app menu or under Settings › General.
macOS then asks you to allow Synctrain, in System Settings under General › Login Items & Extensions › File Providers. Pair with
a gateway in Synctrain's window, by pasting its link or choosing an image of its code. Open the gateway and **Add** a folder,
which appears in Finder under Locations.

Everything that works in Files works in Finder: opening files, editing, adding, renaming, moving and deleting them, **Keep on
This Device** and **Stop Keeping on This Device**, thumbnails, conflict copies and **Previous Versions…**. Browsing happens in
Finder: Synctrain's window is for gateways and folders.

- **While Synctrain is not running**, browsing and files on this Mac work. Long edits that the extension could not finish
  wait for it, so it stays in the Dock or the menu bar when its window closes. Turn on **Launch at login** in its window.
- **A gateway that is gone** doesn't keep its folders here: they stay listed on its page, and can be removed there.
- **Shortcuts** actions need Synchronize mode, as they work on this Mac's own folders, and say so.
- **Files opened with Synctrain**, or shared to it, aren't taken in: put them in the location instead.

Finder's `.DS_Store` and similar files stay on this Mac, and are never sent to a gateway. The ones that reached a gateway from
elsewhere are not shown. Files the location can't synchronize stay on this Mac, and a delete from another device doesn't take
the directory they are in until they are removed.

When its folder leaves Finder, Synctrain takes such files out of the location first, as macOS removes its copy of a folder
with everything in it, these included, and they are nowhere else. They go to a new directory named after the folder in
`~/Sync`, or in your home folder if `~/Sync` is itself synchronized, keeping the place they had in it. Synctrain says how many
there were and where they are now, and removes nothing. If any can't be moved, or Synctrain can't tell, the folder stays, so
that you can move them yourself and try again. A directory of such files that is left in the location all the same is kept on
this Mac without synchronizing, and when the folder comes back it is renamed to "Name (not synchronized)" and stays next to
the folder, which shows all of its own files again.

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

Gateway metadata is projected into `items`, with a persistent identifier for each item. Upgrades from the earlier
mixed local/gateway client import its identifiers from `index.sqlite`, matching gateway identities to the new index
without changing files or pending edits. New items on these devices use a separate identifier namespace, so a number
from the old index never points to an unrelated new row. The old index stays untouched. Each row has a change
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
discarded; Synctrain asks before doing that, and refuses while edits are unresolved. Before the location comes back,
every folder is listed again from its gateway (for up to a minute), so that packages show as the gateway marks them
now; folders of a gateway that can't be reached keep what the phone had, and Synctrain says so.
