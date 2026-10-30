# Synctrain on macOS

On a Mac, Synctrain is meant to replace Syncthing.app: one engine, configured from the app, and from its web GUI or its
REST API where a configuration has the GUI enabled. This page records where the Mac app differs from iOS and why.

The Mac app runs outside the App Sandbox (with the hardened runtime), so that it reaches folders anywhere, as Syncthing
does; that also means it can't come from the Mac App Store. Its share extension stays sandboxed, as macOS requires of
extensions, and shares the app's group container (both are signed by the same team). macOS still asks before the app
reaches the Desktop, Documents and Downloads folders, external disks and network volumes, the first time a folder there
is synchronized; places that only Full Disk Access opens (such as other apps' data) are allowed in System Settings,
under Privacy & Security. Earlier versions were sandboxed; the app tells whether it is by its own entitlement.

## Engine settings

The app relies on some engine options and folder defaults: frequent progress updates, quick relay reconnects, a
one-hour rescan interval, no block index (`blockIndexing` off, which saves space), no crash or usage reporting (with
the reporting and release URLs left empty), and on iOS ignored permissions and no file system watching.

- **iOS** applies them at every start, together with moving folders back into the app's container, resuming their
  interrupted moves and turning off their scan progress. iOS has no web GUI or REST API, so nothing else can change
  them, and the app depends on them.
- **macOS** applies them only when it creates a new configuration. An existing configuration is used as it is, so a
  value changed in the web GUI or through the REST API lasts, and an existing Syncthing configuration keeps its
  settings. Folders stay where they are and keep Syncthing's scan progress, which the web GUI shows. In a new
  configuration permissions sync, as in Syncthing (`ignorePerms` is off by default); configurations an earlier
  Synctrain created keep the value they have.

On both, usage reporting stays off at every start (`urAccepted` is -1): the engine is a fork, which must not report to
Syncthing's servers as an unknown build. Folders accessed through a remote file service stay paused. Loading writes the
configuration file only when one of these settings changed it.

While it runs, the app still changes the configuration in these ways:

- It pauses and resumes devices from its own list of paused devices, and pauses all of them while the app is suspended
  or connectivity is turned off for the current network. A device paused or resumed in the web GUI can therefore change
  back; reconciling the two is still to do.
- On iOS it sets the reconnect interval to one second in the foreground and 60 seconds in the background.

The engine never upgrades itself: the app is updated as a whole. Self-upgrade is turned off at runtime and compiled out
with the `noupgrade` build tag (the app also logs an error if a build lacks it), so the update settings in a
configuration don't matter. The gateway's Linux build uses the same tag.

## Web GUI and REST API

On macOS the engine runs Syncthing's web GUI and REST API, as Syncthing.app does, so that tools and scripts reach it
the same way:

- A new configuration has them on, on 127.0.0.1:8384 (and Syncthing's sync port, 22000), with a random API key and a
  password nobody knows: only the API key opens them, not the web pages' CSRF token, which any user of this Mac could
  get. A configuration without the marker `synctrain-gui-enabled` next to it (one an earlier version of the app made,
  or a folder chosen in the settings) gets them turned on once, the same way, but only where no other computer can
  reach them: on a loopback address with Syncthing's host check, or behind a password it already has. Turned off
  afterwards, they stay off. A configuration taken over from Syncthing.app keeps its own setting.
- If their address is in use at a start (as by Syncthing.app still running), they move to a free port on the same host
  instead of stopping the engine, and move back once it is free again (unless their address was changed meanwhile);
  the marker records where they belong. If they can't listen on their address at all, they are turned off. Either
  way the app's settings say so. An address set with `STGUIADDRESS` is left alone.
- Advanced settings show whether they are on, their address and the API key (shown on request, and copied so that
  clipboard managers that honor it don't keep it). Turning them on or off restarts the engine: they start with it,
  and one that runs keeps listening until then. Turning them on gives them a password if they have none.
- The web GUI's pages are not built in (the engine is built with `noassets`), so its address shows an empty page. The
  REST API works in full, for example `curl -H "X-API-Key: <key>" http://127.0.0.1:8384/rest/system/status`.

On iOS they stay off in the configurations the app makes, and the app has no setting for them (a custom configuration
can have them on; they can be turned off, not on). iOS gives the app no steady time in the background to serve them,
and there are no pages to serve.

## Starting the engine

The engine takes Syncthing's lock (`syncthing.lock` in its configuration directory) before it reads or writes anything
there, and keeps it while the app runs, so it never works next to another Syncthing on the same directory.

It creates a configuration or a device identity only when there is none. A configuration file that is damaged, empty,
unreadable or from a newer version, or a certificate without its key, stops the start and is left as it is; the app
says why and offers what helps (for a damaged configuration, setting it aside and starting with a new one that keeps
the device identity). A configuration from an older Syncthing is archived as `config.xml.v<version>` before it is
migrated, and a database a newer Syncthing wrote is refused.

## Where things are

Outside the App Sandbox, the Mac app keeps the engine's configuration, keys and database in
`~/Library/Application Support/Synctrain` (or the configuration folder chosen in the settings), its log files in
`~/Library/Logs/Synctrain`, and its caches (thumbnails, and the remote file service's content spool) in
`~/Library/Caches/<bundle id>`, where they can be removed at any time. The app excludes nothing from Time Machine. The
sandboxed versions before kept everything in the app's container, which the first start outside it takes over (below),
and iOS keeps everything in the app's container.

Outside the sandbox the app reaches folders and files by their paths, so it keeps no security-scoped bookmarks: folder
paths come straight from the configuration, and the configuration folder chosen in the settings (and the remote file
service's certificate files) are remembered by path. A chosen configuration folder that can't be found, as on a disk
that is not connected, stops the start instead of being replaced by a new one; the app offers to try again or to use
the default location.

Folders are the user's, anywhere (in the home directory or on another volume), as with Syncthing:

- A new folder goes in a new directory named after its label (the one the offering device uses, or else its ID) in the
  configuration's default folder path, as in the web GUI. A new configuration's default folder path is `~/Sync`, and
  so is the place used when it is empty or relative. The name loses leading dots, path separators and invisible
  characters, and a name that is taken gets a number. When the default folder path is a folder or inside one, or no
  number is left, the home directory is used instead. So a new folder never takes over an existing directory, becomes a
  hidden one, or lands inside another folder. Adding a folder shows where it will go; Existing folder picks another
  place, with a warning when it overlaps a folder.
- The app does not add a folder that contains, or is inside, the engine's configuration or database directory (the web
  GUI and the REST API don't check this).
- Remove folder, which deletes the files too, is offered only for folders inside `~/Sync` that are neither inside nor
  around another folder; the directory Syncthing would remove, and where its links lead, must pass too. Other folders
  are unlinked and their files removed in Finder. The home directory, its parents and the engine's own directories are
  never removed, on iOS either.
- No folder counts as another app's (external), and none is excluded from Time Machine. On iOS, new folders are still
  left out of the device backup by default.

## Moving out of the App Sandbox

The first start outside the sandbox takes over what the sandboxed app kept in its container
(`~/Library/Containers/<bundle id>/Data`), before the engine starts:

- **Preferences.** Both preference files are first backed up in
  `~/Library/Application Support/Synctrain Migration Backups/<time>/`; then the container's values that are not set yet
  are added (but not AppKit's window and open panel state). Its bookmarks become paths when they are first used.
- **Engine data**, unless a configuration folder was chosen: its data stays where it is, and only the remote file
  service's certificate copies are taken over. Otherwise the configuration, keys, databases and the remote file
  service's state are copied (cloned, on APFS) into `Synctrain.migrating`, with the container's lock held, checked file
  by file (and the device certificate and key by their SHA-256), and renamed into place as
  `~/Library/Application Support/Synctrain`, with a receipt (`container-migration.json`) naming what was copied and the
  certificate's SHA-256. Caches stay behind; a destination with no engine data (such as what a failed attempt left) is
  set aside as `Synctrain.premigration-<time>`.
- **When it can't.** If the container can't be read (macOS may ask to allow that), the preferences can't be taken over,
  an earlier version still runs, or the copy fails, the copy is removed and the engine doesn't start, as it would start
  anew with another device identity. The app says why and offers to try again. A destination that has engine data of
  its own is used as it is; the app says so once when its device identity differs from the container's.
- **The container** is left as it is. It is the copy to go back to, and can be removed once all is well.
- **Folders in the container** keep their paths and keep working. The app offers, once and in the configuration
  settings, to move them to a folder the user chooses: each is renamed (never copied) into a new folder there, named
  after its label, and paused while it moves. A journal in the configuration folder (`folder-relocations.json`) lets
  the next start finish a move the app stopped in the middle of.

## Restarting and shutting down

Restarting Syncthing (from the web GUI or the REST API's `/rest/system/restart`, from Troubleshooting in the app's
settings, or on macOS from the app menu or the menu bar menu) restarts the engine inside the app. It stops the remote
file service (paired devices reconnect once it is back) and Syncthing, loads the saved configuration again, and starts
a new engine with the same lock, device identity and streaming server, and the remote file service again. The app's
screens stay open; changes made while it restarts fail with a message that it is restarting, and the File Provider
hears of every folder once afterwards. If the new engine can't start, the app says why and offers to try again (also
from the menu bar menu on macOS).

A shutdown from the web GUI or the REST API's `/rest/system/shutdown` quits the Mac app, rather than leaving it open
with the engine stopped (on iOS, which has no web GUI, Restart Syncthing starts it again). Quitting the app stops the
remote file service and then the engine, waiting for each a limited time; what they could not finish is recovered at
the next start.
