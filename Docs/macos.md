# Synctrain on macOS

On a Mac, Synctrain is meant to replace Syncthing.app: one engine, configured from the app, and from its web GUI or its
REST API where a configuration has the GUI enabled. Since October 30, 2026 the iOS app no longer runs Syncthing: it is a
[client of gateways](file-provider.md) with a target of its own, `SynctrainMobile`, and `Synctrain` builds for macOS only.
The Go framework has only a macOS slice. iOS code still in the shared Go and Swift sources is unused, so where this page
compares the Mac app with iOS, it describes how the iOS app used to work and why the Mac differs from that.

The Mac app runs outside the App Sandbox (with the hardened runtime), so that it reaches folders anywhere, as Syncthing
does; that also means it can't come from the Mac App Store, and comes as a notarized disk image instead (see
[Signing and releasing](#signing-and-releasing)). Its extensions stay sandboxed, as macOS requires, and share the app's
group container (all are signed by the same team). macOS still asks before the app
reaches the Desktop, Documents and Downloads folders, external disks and network volumes, the first time a folder there
is synchronized; places that only Full Disk Access opens (such as other apps' data) are allowed in System Settings,
under Privacy & Security. Earlier versions were sandboxed; the app tells whether it is by its own entitlement.

## Engine settings

The app relies on some engine options and folder defaults: frequent progress updates, quick relay reconnects, a
one-hour rescan interval, no block index (`blockIndexing` off, which saves space), an empty release URL, and on iOS
ignored permissions and no file system watching. A new configuration also starts with usage and crash reporting off, and
without their addresses.

- **iOS** applies them at every start, together with moving folders back into the app's container, resuming their
  interrupted moves and turning off their scan progress. iOS has no web GUI or REST API, so nothing else can change
  them, and the app depends on them.
- **macOS** applies them only when it creates a new configuration. An existing configuration is used as it is, so a
  value changed in the web GUI or through the REST API lasts, and an existing Syncthing configuration keeps its
  settings, apart from reporting. Folders stay where they are and keep Syncthing's scan progress, which the web GUI
  shows. In a new configuration permissions sync, as in Syncthing (`ignorePerms` is off by default); configurations an
  earlier Synctrain created keep the value they have.

Usage and crash reporting are off until the user turns them on. A configuration that comes with them on, as one from
Syncthing.app can (crash reporting is on by default there), gets both turned off at its first start in the app. The
marker `synctrain-reporting-reset` next to the configuration records this, so that turning them on afterwards lasts,
whether in the app, the web GUI or through the REST API. The marker says that this happened for the folder, not who chose
what: a `config.xml` put in its place by hand later is used as it is, reporting included. Folders accessed through a
remote file service stay paused. Loading writes the configuration file only when one of these settings changed it.

While it runs, the app still changes the configuration in these ways:

- It pauses and resumes devices from its own list of paused devices, and pauses all of them while the app is suspended
  or connectivity is turned off for the current network. A device paused or resumed in the web GUI can therefore change
  back; reconciling the two is still to do.
- On iOS it sets the reconnect interval to one second in the foreground and 60 seconds in the background.

The engine never upgrades itself: the app is updated as a whole. Self-upgrade is turned off at runtime and compiled out
with the `noupgrade` build tag (the app also logs an error if a build lacks it), so the update settings in a
configuration don't matter. The gateway's Linux build uses the same tag.

## Advanced configuration

Advanced settings link to an editor of the engine's whole configuration, the counterpart of the web GUI's Advanced
dialog, on both platforms: the options, the GUI and LDAP settings (macOS only), each folder and device, and the defaults
for new ones. It is there for settings without a screen of their own, and warns that these are expert settings.

- The core lists the fields by reflection over Syncthing's configuration structs, under their names in the REST API
  (`advanced.go`), so that fields a new version of the fork adds appear by themselves. Booleans, numbers, text, lists
  (one item per line) and enumerations have their own controls, and the values of an enumeration are found by trying its
  numbers. File system types are the exception: nothing lists them, so the core offers the ones the app can use (not
  Syncthing's `fake`, for its tests, whose empty file system would look like every file was deleted) and the one a
  folder has. Nested structs become groups of fields; maps and lists of structs are edited as JSON.
- A section's edited fields are saved together, through the normal configuration change, so that other fields stay as
  they are by then. A value that doesn't parse, or that the configuration's own checks or a subscriber refuse, changes
  nothing and shows the error. So does a length of time the engine can't calculate with, going by a name that ends in a
  unit: the engine limits only some of these when it prepares a configuration (such as `scanProgressIntervalS`), and
  one that overflows no longer means what was entered. The GUI and LDAP sections keep to the rules of the GUI's settings
  page, and show its warnings when others can use the GUI without signing in. A field the engine corrects shows its new
  value after saving. Where a change only applies when the engine restarts (such as turning the GUI on or off), the
  editor offers to restart it.
- IDs are shown but can't be changed, as a new ID makes another folder or device. The same goes for a device's `paused`
  (the app sets that from its own list), and for what the engine keeps empty in defaults. The GUI's password and API key
  are never shown: left empty they stay as they are, and a new password is hashed. A folder's encryption passwords do
  show, in plain text in the JSON of its `devices`, as they do on the app's encryption screen.
- iOS has no GUI or LDAP sections, and hides what the app sets by itself or what does nothing there, such as a folder's
  path (`advancedFieldsHiddenOnIOS`).

## Engine options

On macOS, Advanced settings also link to a page with the options that had no native setting of their own (`options.go`,
`EngineOptionsView`): the minimum free disk space, how long temporary files are kept, how many folders work at once,
the limit on data for incoming requests, low priority, the temporary index threshold, sending the full index after an
upgrade, the audit log, feature flags, and usage and crash reporting. They are saved together. The core checks them
when saving, and only those that were edited since they were read: a value the page wouldn't accept stays until someone
changes it, and what the web GUI or the REST API changed meanwhile stays too. Most take effect at once.

- **Audit log.** Only Syncthing's command line writes the audit log by itself, so the app opens it when a run starts.
  It goes to the file given, added to, or to a new `audit-<time>.log` in the configuration folder at each start of the
  engine; those are removed after a week, as Syncthing does. The file has to be a full path, as Syncthing's `-` and `--`
  (standard output and error) go nowhere in an app, and a regular file, as opening a named pipe could wait forever.
  Syncthing stops if it can't open the file; the engine goes on without an audit log, and the page says so. A change
  needs an engine restart, which the page offers. The log has what Syncthing's does: the events as they are, with file
  names, device addresses, and in each `ConfigSaved` event the whole configuration, API key, password hash and folder
  encryption passwords included. The page says so, and refuses a file in a synchronized folder, where those would reach
  its devices. The default file is readable by its owner only.
- **Usage reporting.** Turning it on sets `urAccepted` to the current report version and sends a report at once, then
  one 30 minutes after each start of the engine and one a day. Syncthing's service also sends failure reports while it
  is on, as they happen, to the crash reporting address; the fork makes turning it off stop those at once, where
  Syncthing goes on until it restarts. The configurations the app makes have no reporting addresses, so turning
  reporting on in the app fills in Syncthing's. Turned on in the web GUI or through the REST API, they are filled in at
  the next start of the engine. `urURL` and `crURL` can be changed in the advanced configuration.
- **Crash reporting.** Syncthing's monitor process, which the app doesn't have, normally saves panics and uploads them.
  The app has the Go runtime save a crash to a `synctrain-crash-<time>.log` in the configuration folder
  (`debug.SetCrashOutput`) and sends it at the next start of the engine, as soon as the configuration is loaded, in the
  layout of Syncthing's crash reports, if crash reporting is on. If it is off, the crash is set aside, so that turning it
  on later doesn't send it. Logs are removed after a week. The framework is built with `-trimpath`, so that traces don't
  name folders on the computer that built it.
- Reports identify the engine as `v2.1.7`, the version the app sets, with the tag `synctrain` in the long version, which
  tells them from Syncthing's own builds. Syncthing's crash server refuses a long version it can't parse, so the app
  recalculates it after setting the version; at package init it still says `unknown-dev`.
- **Low priority** (`setLowPriority`) calls `setpriority` for the whole process, app included, at each start of the
  engine, and it is on by default, as in Syncthing. It is measurably in force (the process has a nice value of 9) but
  makes little difference on macOS. In a normally launched app the main thread stayed at priority 46 and the engine's
  threads at 31 with either value, and two processes spinning on every core, one at nice 9, did the same work (measured
  on October 30, 2026). A process can't lower its nice value again, so turning this off takes an app restart. A real
  priority setting is a separate matter (Syncthing.app sets the QoS of the backend process it launches).

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
- Advanced settings show whether they are on, their address and the API key (shown on request, copied so that
  clipboard managers that honor it don't keep it, and regenerated after a warning). Turning them on or off restarts
  the engine: they start with it, and one that runs keeps listening until then. Turning them on gives them a password
  if they have none, also after clearing the user name on purpose.
- Their settings page below that has the address and TLS, the user and password or LDAP server, the theme, the session
  cookie and metrics settings, and the insecure settings, with warnings. They are saved together, as each change makes
  the service listen again; that happens at once, with the engine left running. An address the service can't listen on
  is refused rather than saved, as the service could only log that it failed; so is the path of something that exists,
  which the service would remove to make a socket there. A socket it can't use at a start turns them off, as an address
  does. Clearing the user name turns signing in off and removes the password, and the page warns that any user or
  program on this Mac, or beyond it on a wider address, can then control them through the web pages' CSRF token. While
  `STGUIADDRESS` is set, the address and TLS can't be changed.
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

## Files in Finder

A folder can be on demand instead of synchronized in full: Finder shows all of it under Locations, and only what is opened
comes to this Mac, from the devices that have it. It is an option, off by default, under **Settings › Files in Finder**,
and other folders stay as they are. See [file-provider.md](file-provider.md#on-macos) for what it does; this is how.

The File Provider extension is sandboxed and has no engine, and there is one engine on a Mac, in the app. But the app also
runs the [remote file service](gateway.md), which is what the extension speaks to on iOS. So the Mac is its own gateway:

- **The service runs for this Mac alone** while the setting is on and the remote file service is off: it listens on
  `127.0.0.1`, and on a free port if its own is taken. With the remote file service on, it listens as it does for other
  devices, and the extension still connects over the loopback interface. `FinderAccess` pairs the app with its own
  service at the first start, with the service's key pinned, and keeps the port and the key up to date whenever the service
  starts or its certificate changes. The extension connects to it like to any gateway, and paired gateways work next to
  this Mac, in the same list.
- **An on-demand folder is a selective folder** whose `.stignore` also has the line `!*.sync-conflict-*`, which marks it
  (`onDemandPattern`). It stays where it is, for example in `~/Sync`. Its directory holds only what was written here, what
  no other device has, and conflict copies, which that line selects. Nothing keeps a record of it elsewhere.
- **The service writes to on-demand folders**, which it refuses for plain selective folders (`gateway/ondemand.go`). Three
  things differ from a complete replica. A new version builds on the global one, as this device usually has an ignored
  entry or none, and a scan would start a version vector that conflicts on every device. What was written is selected in
  the callback that commits it, and stays selected: a scan marks an ignored entry invalid, deletions included, and other
  devices then keep their old copy. So `RemoveSuperfluousSelectionEntries` leaves these folders alone, and deselecting is
  refused. And `!*.sync-conflict-*` keeps conflict copies indexed: when another device wins a conflict, the engine renames
  this device's edit to a conflict copy, which the last pattern would otherwise ignore, so that it would never reach
  anyone. What a write builds on is journaled when its preconditions hold (`writeBase`), as other devices' changes reach
  the index without going through the folder loop: a version that arrives before the file is indexed, even across a
  restart, stays concurrent with it, and devices keep both.
- **Deleting a file or directory this Mac doesn't have** downloads nothing. Moving does: an index entry without a file
  behind it would be announced as something this device can't provide. The item is selected, the operation waits until
  the engine has it, and it then moves as in a complete replica. A directory that would take more than 4 GiB is refused
  (`move_too_large`).
- **Switching a folder to on demand** (`SwitchToOnDemand`) must not lose what only this Mac has: making a folder
  selective ignores all of it, and this device stops offering it. So a file stays selected unless a connected device has
  that version in full, going by that device's own valid index entry, and the rest is removed. A device that merely ignores
  a file has an entry at the global version too; the fork leaves those out of a file's availability. With the folder
  paused, whatever is no longer the version found on another device stays as well. The switch is refused when no connected
  device has any of the files, when more than 10,000 would stay, when a device that shares the folder, connected or not,
  still needs a deletion (which this device would stop announcing), and when the folder has ignore patterns a selective
  folder can't express, as they would stop applying. Switching again finishes one that stopped halfway.
- **Switching back** removes the folder from Finder, refused while its edits are unresolved, and makes it synchronize in
  full again.
- **Changes run one at a time** (`FinderAccess.inOrder`), and the setting can't be turned off while Finder is the way to a
  folder, on demand or from a gateway: its files aren't on this Mac, and removing the location would discard what the
  system's copy holds that hasn't reached the service. A folder that other devices may only read has to allow changes
  first: allowing them for this Mac would also give write access to devices that were granted it.

The extension uses no background `URLSession` on macOS, unlike on iOS, where the system starts the app to finish a
session's events. macOS doesn't, and a session with a pinned key needs a process to answer its challenges. Nothing suspends
the extension here, and the app's outbox picks up an upload that it left.

macOS asks the user to allow a File Provider, in System Settings under General › Login Items & Extensions › File Providers.
Until then the domain is registered and Finder can't open it. It also doesn't accept an app in a temporary location, such as
a derived data path under `/private/tmp`: `NSFileProviderManager.add` fails with *The application cannot be used right
now*. A debug run with `SYNCTRAIN_TEST_ROOT` stays out of all this unless `SYNCTRAIN_TEST_FILE_PROVIDER` is set, because
domains and the group container aren't under its scratch directory.

Programs run from some shells have materializing dataless files turned off, and get `EDEADLK` (*Resource deadlock avoided*)
from the location. That is the process's I/O policy (`setiopolicy_np`), not the extension.

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

## Taking over Syncthing.app

When Syncthing (Syncthing.app's backend, in `~/Library/Application Support/Syncthing`) is set up on the Mac, the
app decides before its engine starts (`EngineSetupDecision`), so that two engines never run on one configuration and a
device identity never runs twice:

- **The first start** (with no configuration of its own, and no earlier version having run) offers to take over
  Syncthing's setup: the Mac keeps its device ID, folders, devices and database, and nothing synchronizes again. The
  same is offered later in the configuration settings (using Syncthing's configuration only, as the app's own engine
  runs then). A home without a usable device certificate and key, or from a newer Syncthing, is not taken over.
  - **Use Syncthing's configuration**: Syncthing's engine data (with its lock held) and Syncthing.app's preferences
    are copied (cloned, on APFS) into `~/Library/Application Support/Synctrain Takeover Backups/<time>/`; then
    Syncthing's folder becomes the app's configuration folder, and the app restarts on it.
  - **Import a copy**: Syncthing's configuration, keys and database are copied into the app's own folder, with
    Syncthing's lock held; Syncthing's own stay as they are. While the app runs, it keeps Syncthing's lock, so that
    Syncthing.app can't start on that identity next to it.
  - **Start with a new configuration**: a new device ID.
  Either takeover first turns off Syncthing.app's StartAtLogin setting and removes its login item (through System
  Events, which macOS asks to allow once; where that isn't allowed, in System Settings).
- **Syncthing running** (its folder's lock is held, or Syncthing.app runs): the takeover waits for it to quit. The app
  offers to quit it: Syncthing.app first, as it starts its backend again otherwise (it gets a SIGTERM if it doesn't quit
  within 15 seconds), then if needed the backend, through its REST API once it answers with the expected device ID.
- **The same identity** (Syncthing's configuration used directly, or a copy with its certificate) never starts while
  Syncthing runs: the app waits, whether or not a window shows it, and starts once Syncthing has quit. Another
  Syncthing, with another identity, runs alongside only once that is chosen; both then compete for the same ports. The
  app comes forward when the user has to choose.
- A debug run with `SYNCTRAIN_TEST_ROOT` takes over the Syncthing folder in its scratch directory, and changes nothing
  in the real app's or Syncthing.app's settings. Its preferences are in `Library/Preferences` there: `TestRun` changes
  what `UserDefaults.standard` returns before anything reads it, so that the app's code, `@AppStorage` and AppKit all
  use them, and the rest of the app treats them as usual. A test run stops rather than fall back to the real ones.

A taken-over configuration keeps its own web GUI setting, and its paused devices stay paused (the app pauses and
resumes devices from its own list, which the takeover fills). A receipt, `syncthing-takeover.json` in
`~/Library/Application Support/Synctrain`, records what was done: the choice, Syncthing's folder, the device ID,
Syncthing.app's StartAtLogin setting before, whether its login item was removed, the app's own paused devices before,
and where the backup is.

**Going back to Syncthing.app.** Syncthing.app must start on Synctrain's current engine data, never on an older
database with the same device identity: other devices already have this device's later changes, and would miss the
older database's own. After *Use Syncthing's configuration*, Syncthing's folder is Synctrain's current data. After
*Import a copy*, Synctrain's folder is, and goes back into Syncthing's (what syncthing-tools'
`return-to-syncthing-app` does). Quit Synctrain and keep it from starting at login, then turn Syncthing.app's
StartAtLogin setting and login item back on as the receipt records, and open it. Starting Syncthing.app on the backup,
or on Syncthing's own folder after an import, needs its database reset first (`syncthing debug reset-database`), which
exchanges every index anew. Going back is one-way when Synctrain's engine migrated the database to a newer schema than
Syncthing.app's, or when a folder uses a filesystem type only Synctrain has (the Trash, photo folders);
`return-to-syncthing-app` checks for both. Usage and crash reporting stay as Synctrain left them, which is off unless they
were turned on here.

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

## Signing and releasing

The Mac app is distributed as a disk image, signed with Developer ID and notarized, so that Gatekeeper accepts it on any
Mac.

The Release configuration signs the app and its extensions with Developer ID Application, with the hardened runtime
and without the `get-task-allow` entitlement. The app and the two File Provider extensions take a provisioning profile
each: they share gateway credentials in the Keychain through an access group (the app group's name), and macOS won't start
code that claims one without a profile that allows it. **An app signed without them doesn't start at all**, not even for
those who never use Files in Finder. The share extension needs none, as macOS accepts an app group that starts with the
team's ID without one, and without asking the user. Debug and iOS builds sign automatically, which creates the Debug
profiles. Both schemes run the Release configuration, so running the app on a Mac from Xcode takes
the certificate and the profiles too, or a scheme set to Debug; a Release run has no debugger, as it lacks `get-task-allow`. `make mac` and
`make build` don't sign, and take no certificate.

`make mac-release` archives the app and hands the archive to [`Scripts/notarize-mac.sh`](../Scripts/notarize-mac.sh),
which leaves the stapled app and `Synctrain-<version>.<build>.dmg` in `Build/release`:

1. It checks that all code in the app is signed by one team with Developer ID, with a secure timestamp and the hardened
   runtime and without `get-task-allow`; that the app is not sandboxed and its three extensions are; that they all use one
   app group with the team's prefix, which neither notarization nor Gatekeeper looks at; and that whatever claims a
   Keychain access group has a profile.
2. It has the app notarized and staples the ticket to it.
3. It puts the stapled app in a disk image, signs that, has it notarized and staples the ticket to it. So the image and
   the app copied out of it both open without a connection to Apple.
4. It validates both tickets and has Gatekeeper assess both (`spctl`).

Notarization usually takes minutes, but can take much longer on a team's first submissions, so the script waits for up
to two hours. If it fails, the archive is still there: fix the cause and run the script again on it, without rebuilding.

### Setting up

1. **Certificate.** The Account Holder creates it, as *Developer ID Application*, among the account's certificates in
   Xcode's settings or on the developer website. It stays in the login keychain of the Mac that builds releases. For CI,
   export it from Keychain Access as a `.p12` with a password.
2. **Notary credentials.** Create a Team key with Developer access in App Store Connect, under Users and Access,
   Integrations. It can be downloaded once. For a build on a Mac, save it in the keychain:

   ```bash
   xcrun notarytool store-credentials synctrain-notary --key AuthKey_<id>.p8 --key-id <id> --issuer <issuer id>
   ```

   ```bash
   NOTARY_KEYCHAIN_PROFILE=synctrain-notary make mac-release
   ```

3. **Provisioning profiles.** Under Certificates, Identifiers & Profiles on the developer website, create one profile of
   type *Developer ID* for each of these App IDs, with these names, which the project refers to. `make mac-release` checks
   for them first (`Scripts/notarize-mac.sh --check`). Install each by opening it in Xcode.

   | App ID | Profile name |
   | --- | --- |
   | `gordonbyrnes.Sushitrain` | Synctrain Developer ID |
   | `gordonbyrnes.Sushitrain.FileProvider` | Synctrain File Provider Developer ID |
   | `gordonbyrnes.Sushitrain.FileProviderUI` | Synctrain File Provider UI Developer ID |

4. **CI.** Turn workflows on in the repository's Actions tab: GitHub doesn't run them in a fork until then. Create an
   environment named `mac-release` in the repository's settings, allow it only tags matching `v*`, and add these secrets
   to that environment rather than to the repository, so that no branch can read them:

   | Secret | Value |
   | --- | --- |
   | `DEVELOPER_ID_P12` | The `.p12`, as base64 (`base64 -i developer-id.p12`) |
   | `DEVELOPER_ID_P12_PASSWORD` | Its password |
   | `DEVELOPER_ID_PROFILE_APP` | The app's profile, as base64 |
   | `DEVELOPER_ID_PROFILE_FILE_PROVIDER` | The File Provider extension's profile, as base64 |
   | `DEVELOPER_ID_PROFILE_FILE_PROVIDER_UI` | The File Provider UI extension's profile, as base64 |
   | `NOTARY_KEY_P8` | The `.p8` key, as base64 |
   | `NOTARY_KEY_ID` | The key's ID |
   | `NOTARY_ISSUER_ID` | The issuer ID shown above the keys |

None of these files belong in the repository, and Git ignores the certificate and key types. The workflow keeps them in a
temporary keychain and folder, which it removes after the build.

### Releasing

1. Raise `MARKETING_VERSION` and `CURRENT_PROJECT_VERSION` in all targets, and commit.
2. Push a tag named after them, such as `v2.8.86`. The `build` job creates a draft release, with the unsigned archives
   (`sushitrain-<commit>.zip`, which are not for distribution). The `mac-release` job then attaches the disk image.
3. On a Mac that never built the app, download the disk image and check it, then copy the app to Applications and check
   that as well:

   ```bash
   spctl -a -vv -t open --context context:primary-signature Synctrain-*.dmg
   ```

   ```bash
   spctl -a -vv /Applications/Synctrain.app
   ```

   Both should print `accepted` and `source=Notarized Developer ID`.
4. Publish the draft.
