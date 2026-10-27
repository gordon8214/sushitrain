# Files app integration

On iOS and iPadOS, Synctrain adds a **Synctrain** location to the Files app. Every folder stored in the app appears in
it as a directory, including the files that are only on other devices. Files can be opened, edited, added, renamed, moved
and deleted there (and from any app that opens documents through Files), and the changes are synchronized like any
other change.

External folders, photo folders and receive-encrypted folders are not part of the location.

## Using it

Turn the location on in Files (Browse, then tap Synctrain under Locations). The **Files app** page in Synctrain's
settings shows whether it is on and lists files that need attention.

What works depends on whether Synctrain is running, because only the app runs Syncthing:

- **Always**: browsing, opening files that are on this device, and adding, editing, renaming, moving and deleting them.
  Changes are written to the folder right away; Syncthing picks them up the next time Synctrain runs (it scans the
  changed paths, and selects new items in on-demand folders).
- **While Synctrain runs** (on the screen, or for about half a minute after leaving it): opening files that are only
  on other devices. These are fetched from another device through the app. When a file is opened while Synctrain is
  not running, Files says so and Synctrain lists it under **Requested downloads**, with a button to show it in Files.
- **Keep on This Device** and **Stop Keeping on This Device** (in on-demand folders) are carried out when Synctrain
  runs. Stopping only happens when another connected device has all of the item; otherwise Synctrain keeps it and says
  why on the Files app page. Files' own downloaded copy is removed as well; while it is still in use (for instance
  shown in Files), Synctrain tries again for a day, and **Remove Download** in Files removes it right away.

Saving a file that is only on other devices first downloads it (Files shows the save as waiting until Synctrain has the
file); the edit is then an ordinary change of that version, not a second, conflicting one.

Items kept on this device in on-demand folders have a checkmark badge, and conflict copies a warning badge. Files get
thumbnails (images, videos, PDFs and other documents the system can preview) from the file on this device. While
Synctrain runs, files that are only on other devices get them too: videos are streamed (only the parts needed for the
first frame are fetched), other files are fetched up to 16 MB.

Deleting removes the item on all devices. Only items kept on this device can be renamed, moved or deleted; in on-demand
folders, a directory can only be renamed or moved when it is kept on this device as a whole. Items can also be moved to
another folder, which other devices see as a removal from one folder and an addition to the other.

### Limitations

- Receive-only folders are read-only. Symbolic links are not shown.
- Files that are only on other devices get no thumbnail while Synctrain is not running, nor when they are larger than
  16 MB (except videos).
- Work left by the Files app while Synctrain is not running (scans, selections, keeping and stopping) is carried out
  when Synctrain next runs, which includes its background refreshes.
- There is no macOS version yet.

## How it works

The extension is a replicated File Provider extension. It does not run Syncthing: File Provider extensions on iOS may
use about 20 MB of memory and only run while the system needs them, and the app keeps running Syncthing on its own.

### Shared container

Internal folders are stored in the app group container (`Synctrain/Folders/<folder ID>`), so both the app and the
extension can reach them. Folders in the app's documents directory are moved there once (a rename on the same volume;
nothing is downloaded again). The container also holds:

- `FileProvider/index.sqlite`: a mirror of Syncthing's global index for the exposed folders, which the extension serves
  items from, and a queue of work the extension leaves for the app.
- `FileProvider/heartbeat.json`: the port and token of the app's loopback file server, and until when the app is awake.
- `Synctrain/Staging`: where the extension writes files before moving them into place.

### Index

The app updates the mirror from Syncthing's database sequence, driven by index events (debounced) and full passes. Each
row has a change sequence; the system's sync anchor is the index generation plus that sequence. Item identifiers are row
IDs, and stay the same when Syncthing reports a file that was deleted and created again.

A change made through the Files app is recorded as *pending* together with the folder's Syncthing sequence at that time.
Index records from before the change cannot undo it; the record Syncthing produces when it scans the change is adopted
without counting as a new version, and any other newer record (for instance after Syncthing resolved a conflict)
replaces it.

### Changes from the Files app

The extension writes into the folder with atomic renames (`RENAME_EXCL` to add, `RENAME_SWAP` to replace), so Syncthing
never sees a partial file, and queues work for the app: scan the changed paths, and select new items in on-demand
folders. When Synctrain is not running, the work waits until it does.

When an edit is based on a version that has changed on another device since, it is kept as a conflict copy named the
way Syncthing names them (or rejected when the system asks for that, from iOS 26).

In an on-demand folder, Syncthing ignores a path until it is selected, and would treat a plain rename as a deletion. A
rename there links the new name instead (a directory is cloned, which takes no extra space); the app selects it, removes
the old name and has both scanned. Anything that arrived in an old directory in the meantime is moved over first. The
old name stays selected, because Syncthing marks the entries of newly ignored paths as invalid, and other devices
disregard those.

Moving an item to another folder is a rename on disk (both folders are on the same volume). The item keeps its
identifier; the index leaves tombstones in the old folder so that its records from before the move do not bring it back,
and the new folder selects (if on-demand) and scans it.

### Opening files

Files on this device are cloned into the system's copy (`fclonefileat`, so they take no extra space). Other files are
fetched through the app's loopback server (`127.0.0.1`, protected by a token that changes on every start), which reads
them from the local copy if it is current and otherwise from connected devices, block by block. The server supports
range requests, so video thumbnails only fetch the blocks AVFoundation reads; as AVFoundation cannot set the
authorization header, the token may also be passed as a `token` query parameter.

Synctrain does not hand requested files to the system as background downloads: the system runs those later, usually
while the app is suspended again, and while they wait or fail Files shows the location as paused.

## Debugging

Debug builds of the extension log every call to `Library/Caches/FileProviderExtension.log` in the app group container.
Unlike the system log, which needs root to collect from a device, it can be copied from a device with:

```
xcrun devicectl device copy from --device <device> --domain-type appGroupDataContainer \
  --domain-identifier <app group> --source Library/Caches/FileProviderExtension.log --destination .
```

**Rebuild Synctrain location** on the Files app page removes the location and sets it up again from a new index. Files
on the device are not affected.
