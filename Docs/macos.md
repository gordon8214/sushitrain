# Synctrain on macOS

On a Mac, Synctrain is meant to replace Syncthing.app: one engine, configured from the app, and from its web GUI or its
REST API where a configuration has the GUI enabled. That is [Synchronize mode](#two-modes), which most of this page is
about; in Files in Finder mode no engine runs. Since October 30, 2026 the iOS app no longer runs Syncthing: it is a
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
- The engine calls itself `v2.1.7-synctrain.N`, with the tag `synctrain` in the long version, in its GUI's About dialog,
  `/rest/system/version`, reports, logs and to other devices, which tells it from Syncthing's own builds.
  `SushitrainCore/Makefile` stamps the version, tags, builder and commit time through `-ldflags -X`, because `lib/build`
  works out the long version in its `init`; raise N there when `go.mod` moves to another commit of the fork. `lib/build`
  stops the process on a version it can't parse, and Syncthing's crash server refuses such a long version, so `make test`
  and `make test-engine` run with the same stamp. Without it, as in a plain `go test`, the version is `unknown-dev`.
- **Low priority** (`setLowPriority`) calls `setpriority` for the whole process, app included, at each start of the
  engine, and it is on by default, as in Syncthing. It is measurably in force (the process has a nice value of 9) but
  makes little difference on macOS. In a normally launched app the main thread stayed at priority 46 and the engine's
  threads at 31 with either value, and two processes spinning on every core, one at nice 9, did the same work (measured
  on October 30, 2026). A process can't lower its nice value again, so turning this off takes an app restart. A real
  priority setting is a separate matter (Syncthing.app sets the QoS of the backend process it launches).

## Folder, device and connection settings

On macOS every setting has a native page (iOS has no engine). The pages save their settings together, so that a folder
restarts once. As with Engine options, the core checks only what was edited since the page read it, so that a value the
page wouldn't accept stays until someone changes it, and what the web GUI or the REST API changed meanwhile stays too
(`foldersettings.go`, `versioningsettings.go`, `devicesettings.go`, `connectionoptions.go`, `SettingsPage`).

What the engine would silently replace when it prepares a configuration is refused instead, so that a page never ends
up showing something else than was entered: a watcher delay of 0, more than 256 files written at once, a reconnection
interval under 5 seconds, a priority over the internet that isn't above the one on the local network, automatically
accepting folders from an untrusted device.

- **Folders.** The Advanced tab links to Synchronization options and Metadata. The copy method offers what exists on
  macOS: another method fails every copy of a block, with `ENOTSUP`, as it is compiled for other systems only. A new
  marker name is checked against the disk first, as the folder would stop. Metadata options follow the folder's type,
  as in the web GUI: a send-only folder can't apply metadata and a receive-only folder can't send it. Only what is turned
  on is refused, so a folder that already has such a combination keeps working. Extended attribute rules replace the
  macOS mode altogether and deny whatever matches none of them, as the page says. Patterns the engine can't read would
  match nothing, so they are refused.
- **Versioning.** The page has External versioning, a versions path and the fork's platform history. A type that was
  just chosen writes all its values as shown, as they were not read from the configuration and would otherwise not count
  as edited. Whatever is saved, platform history goes unless the type is Staggered with versions in the default place, as
  the versioner won't start otherwise, and the file system type is set to basic, without which a path is used as it is,
  with no `~` and not relative to the folder. A versions path can't be in or around a synchronized folder, as versions
  would reach other devices and cleaning up removes files, nor where another folder keeps its versions, as files of the
  same name would replace each other. These checks go by what the disk sees: whatever the case of the letters, and
  following symbolic links. A new folder can't go in a versions path either. Ages that don't fit the engine's durations
  are refused, and the fork reads them as no limit, as they used to wrap around and remove every version. Ending platform
  history asks first. What was kept stays on disk,
  but nothing protects its files in `.stversions/.stversions/platform` from cleaning up anymore, and a version made while
  it was on may refuse to be restored.
- **Devices.** The Connection tab links to compression, limits, allowed networks and the rest. Limits are in Mbit/s, like
  the app's own, and a limit that wasn't edited keeps its exact value in KiB/s. The engine skips a network it can't read,
  which could lock a device out, so they are checked.
- **Connection options.** Advanced settings link to this page. Everything on it applies at once, including the local
  discovery port and address, so it only offers a restart for changes made elsewhere.
- **Defaults.** Advanced settings link to the defaults for new folders and devices, and the default ignore patterns, with
  the same sections. The engine checks a folder's settings only when it starts one, so the page refuses what would break
  every new folder: a relative location, patterns it can't read, and receive-encrypted as the type, which can't be changed
  afterwards. An empty location is stored as `~/Sync`, as the engine takes no path to mean its working directory for a
  folder it accepts by itself. It leaves out the marker name, as the engine only creates a marker of the default name, and
  a versions path, which all new folders would share. Folders in a file system of the app's own don't take default
  versioning. A new folder gets the default patterns in its `.stignore` before the engine sees it, so that its first scan
  keeps to them. A `.stignore` that is already there stays, which the add folder screen says, as what it lists isn't
  synchronized. Receive-encrypted folders have none.

## Folder status and failed items

The General tab of a folder shows its status, with the engine's own message when it stopped on an error. Before, that
message didn't reach the app: `Folder.State` returned the state and the error together, and gomobile returns no value
along with an error. It now returns the state, and `StateError` the message.

- **Failed items** are what a folder could not scan or synchronize, from the engine's `FolderErrors`. Each can be shown
  in Finder, at the nearest place that exists, as what failed may never have reached this device, or in the file browser.
  A folder with failed items reads as out of sync, and the menu bar shows them (see Menu bar status). The list follows
  the engine's events. Failures while scanning have no event of their own, so they show when the folder's state changes.
  A file can fail both a scan and a pull, and then shows once. Reading them takes sorting them all, so the menu bar does
  that off the main thread, at the pace the views refresh at.
- **Revert Local Changes** and **Override Changes** show with the number of items they concern, and ask first. Both report
  why they couldn't run, which the fork added, as the engine's own calls say nothing, and looking at the folder's state
  afterwards races with the folder. Revert also empties a receive-encrypted folder of what doesn't belong there, under the
  name Delete Unexpected Items, as in the web GUI. Override deletes on every device what this one doesn't have, so its
  confirmation says that an empty folder means everything.

## Connection requests

A device that tries to connect without being known here is turned away, and the engine remembers it. The app shows these
as connection requests, with what the device said about itself, in the start screen, the devices table and the menus, and
offers Add, Dismiss and Ignore (`pending.go`, `ConnectionRequestsView`). Dismissing forgets a request until the device
tries again, which is soon if it keeps trying. Ignoring has the engine turn it away from now on, and can be undone under
Advanced settings. A device's ignored folders are on its Connection tab.

Outside the app, a request adds to the Dock badge, and gets a notification once, even across restarts, until it ends. More
than three at once come as one, as a host can make up as many devices as it likes, and each shows the short ID, as a device
can call itself anything. Permission is asked for with the first request, and a request counts as announced only once the
system took the notification. Those IDs are kept apart from the user settings, as a change to those reaches the engine.
The engine calls `OnPendingDevicesChanged` outside the `IgnoreEvents` gate, as nobody would otherwise hear about one with no
window open. It reads a database, so the app doesn't ask at every event, and it asks at each start, as no event comes for
requests from before it. Clicking a notification only brings the app to the front, which `Connection Requests` in the menus
then takes further: opening that window takes SwiftUI's environment, which a notification handler doesn't have.

## File versions

Files in a folder with versioning have Previous Versions in their context menu and under Advanced in their info, with the
version's date and size, to open, save elsewhere or restore. Deleted Files in the folder's menu lists what is gone but has
versions, to restore in one go (`versions.go`, `FileVersionsView`). External versioning has no such thing, as the engine
doesn't know where its command puts files, and neither do paused folders, which have no versioner running, and
receive-encrypted folders, which only have encrypted names.

- Restoring works as in the web GUI: a file in the way becomes a version, and a link is removed. With platform history,
  its metadata comes back too, and a version from before it was turned on refuses to restore, for which Save As stays
  available. The fork finds the version before it moves anything, and Trash can reports what went wrong: it used to drop
  every error, which could take the file out of the folder and report success.
- Open and Save As copy only the contents. Open uses a temporary copy, so editing that changes nothing in the folder, and
  the next start of the app removes it. Save As won't replace a file in a synchronized folder, which would keep no version
  of it, and its temporary file is one that scanning skips.
- In a receive-only folder a restored file is a local change, which Revert Local Changes would remove, as the confirmation
  says.
- Deleted Files walks all of a folder's versions, and one that a cleanup removes during that ends the walk, so it is tried
  twice. Platform history keeps its own files among the versions, which a listing shows as files, so they are left out. The
  list shows when a file was last archived, which isn't when it was deleted: a deletion on this device archives nothing, so
  restoring can bring back an older version, as the confirmation says.

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
- The web GUI's pages are built into the engine, as in the gateway. They are the fork's own, folder browser and metadata
  settings included: its generated `lib/api/auto/gui.files.go`, from the commit in `go.mod`. Moving to another commit
  brings its pages along, as long as the fork regenerated that file with any change to `gui/`; nothing checks that yet.
  A `gui` folder in the configuration directory overrides them, as in Syncthing. They add 2.2 MB to the app (now 60 MB)
  and about 3 MB to its disk image (now 25 MB), measured on unsigned Release archives, which are arm64 only. The fork's
  `gui` folder as resources of the app would add about 7 MB and 2 MB, and a step of its own to copy it, as the module
  in `go.mod` doesn't hold the folder's `vendor` scripts.
- A new configuration's password is one nobody knows, so the web GUI stops at Syncthing's sign-in page until a user and
  password are chosen on the settings page above. The REST API needs only the key, for example
  `curl -H "X-API-Key: <key>" http://127.0.0.1:8384/rest/system/status`.

On iOS they stay off in the configurations the app makes, and the app has no setting for them (a custom configuration
can have them on; they can be turned off, not on). iOS gives the app no steady time in the background to serve them.

## Starting the engine

The engine takes Syncthing's lock (`syncthing.lock` in its configuration directory) before it reads or writes anything
there, and keeps it while the app runs, so it never works next to another Syncthing on the same directory.

It creates a configuration or a device identity only when there is none. A configuration file that is damaged, empty,
unreadable or from a newer version, or a certificate without its key, stops the start and is left as it is; the app
says why and offers what helps (for a damaged configuration, setting it aside and starting with a new one that keeps
the device identity). A configuration from an older Syncthing is archived as `config.xml.v<version>` before it is
migrated, and a database a newer Syncthing wrote is refused.

## Two modes

Synctrain does one of two things on a Mac, and never both. The two have nothing in common at run time, so the mode is
settled before anything starts (`AppEntry`, `AppMode`), and changing it restarts the app.

| | **Synchronize** | **Files in Finder** |
| --- | --- | --- |
| Engine | Runs; every folder synchronizes all of its contents | Does not start |
| Files | On this Mac | On a gateway on another system; macOS keeps what is opened |
| Gateway for other devices | Can be one | Can't be one |
| Finder's Synctrain location | None | Exists while a gateway is paired |

There are no partial folders. A file that hasn't arrived yet, or that a folder's ignore patterns exclude, can still be
viewed or streamed from a device that has it, but the folder holds everything else. A `.stignore` that ends in `*`, as
from an earlier version, is just a folder with ignore patterns: the normal ignore editor shows it, and it stays read-only
through a gateway.

A Mac where the engine has run stays in Synchronize mode without asking: one with a configuration, a chosen configuration
folder, `lastRunBuild` or a sandbox container. Anywhere else the first start asks, before an engine would make a device
identity and announce it. Choosing Synchronize restarts the app, which then offers to take over Syncthing as usual.

### Files in Finder mode

The app is another app in this mode (`FilesApp`): no engine, no device identity, and a window with the paired gateways,
their folders, edits that wait for review, and the location. It has no file browser, as Finder is that. `RemoteCoordinator`
and the File Provider extension do what they do on iOS, and share the code (`AppModel`, `SynctrainClient`).

The extension browses and opens files by itself. Its work with gateways is limited to what the system asks of it, though:
following changes and retrying edits are the app's, so it stays in the Dock or the menu bar when its window closes, and
**Launch at login** is worth turning on. The extension uses no background `URLSession` on macOS, unlike on iOS, where the
system starts the app to finish a session's events. macOS doesn't, and a session with a pinned key needs a process to
answer its challenges.

Without an engine the framework's logger goes nowhere, so `Log` writes to the unified log instead. The app's Shortcuts
actions need the engine, and can't be hidden in this mode: they and what looks things up for them stop first, saying so
(`AppMode.requireEngine`). Files that reach the app (from document types and the Share extension) are refused with an alert.

Pairing can only be with another system. The pairing screen exists only in this mode, and the remote file service only
runs next to an engine.

### Switching

**To Files in Finder** (Settings › General, or the app menu, as the settings don't open while the engine can't start)
changes the mode and restarts. Folders stay where they are and stop synchronizing. Devices paired with this Mac can't
reach it until it switches back, and the alert says so.

**To Synchronize** (the window of the app) has what the system's copy holds to think of. In order, and writing the mode last,
so that stopping anywhere leaves a Mac in Files in Finder mode, which sets up the location again and can repeat this:

1. It is refused while edits are unresolved, and when that can't be told. It asks first if the system has changes the
   extension hasn't accepted, or can't say.
2. What the location can't synchronize is taken out of every folder (below). It stops here if any of it could not be moved,
   or if it can't be told what there is, with every folder still in place.
3. The location goes, with `.preserveDirtyUserData`, as the default removes everything the system holds. This comes before the
   index, as removing a folder discards changes the system hasn't handed to the extension, and this keeps them, in
   `~/Library/CloudStorage/Synctrain-Synctrain (date)`. They are reported. It does not keep what the extension refused, which
   is why that left in step 2, and it names a directory that doesn't exist when it had nothing to keep.
4. Each folder leaves the index. This needs no gateway: one that is gone for good doesn't keep a folder here.
5. The index starts a new generation, and released upload tokens and downloaded content go. Pairings stay.
6. What was moved or kept is reported, and the app waits for the user. Reports are saved until they are seen, as they can be
   the only word of where files went.

**When the index can't be read** none of that can be done, so the window offers the switch without it: the location stays as
it is, and the next paragraph finds it.

**A location left over** in Synchronize mode, as after a failed removal or with restored preferences, is looked for once the
engine has started (`ModeSwitch.removeLeftoverLocation`). Without folders or unresolved edits it is removed. Otherwise the
app says so and removes nothing, as what it holds may be nowhere else.

### What the location can't synchronize

Names macOS keeps for itself (such as `._name`) and paths the folder's ignore patterns exclude stay on this Mac, in the
system's copy alone: the extension refuses them with `excludedFromSync`, so the index never has them. macOS removes its copy
of a folder with everything in it, these files included, and they are nowhere else. So before a folder leaves, whether on
the page of its gateway or by switching modes, `SystemCopy.unknownPaths` walks the system's copy and lists what the index has
no item at, and `FilesLeftovers.moveOut` takes those out of the location, keeping the paths they had in it. They go to a new
directory named after the folder (`ClientLifecycle.directoryForFilesOutsideTheLocation`): in `~/Sync`, or in the home directory
when `~/Sync` is a folder of the engine, which would send them to its devices. Nothing is removed, and the app says how many
there were and where they are now. Unresolved edits are checked first, as they would keep the folder here without these
files.

Rebuilding the location takes them out the same way first. The folder stays, and removing it can be tried again, if any of
this fails: a file that can't be moved, a walk that doesn't finish, or a copy that can't be looked at. Not being able to tell
never counts as there being nothing to save. The only folder that needs no walk is one the system has no copy of, which a
location that exists without its directory shows, or a location that is turned off.

The directory of a file the extension refuses is read with `opendir`, as `FileManager` leaves `._name` files out of a listing,
which are most of what this looks for. Should a directory be left at the top level of the location all the same, it is offered
to the extension as a new item; `excludedFromSync` keeps it on this Mac, where `cannotSynchronize` would have the system ask
again every minute. A folder coming back to a name that directory holds would show nothing of its own, so
`FilesLeftovers.moveAside` renames it to "Name (not synchronized)" first.

### The system's part

macOS asks the user to allow a File Provider, in System Settings under General › Login Items & Extensions › File Providers.
Until then the domain is registered and Finder can't open it. It also doesn't accept an app in a temporary location, such as
a derived data path under `/private/tmp`, nor a stale registration of one: `NSFileProviderManager.add` fails with *The
application cannot be used right now*. A debug run with `SYNCTRAIN_TEST_ROOT` stays out of all this unless
`SYNCTRAIN_TEST_FILE_PROVIDER` is set, because domains, the Keychain and the group container aren't under its scratch
directory (`TestRun.allowsFileProvider`, `ClientLifecycle.allowsSharedState`); it starts in Files in Finder mode if so.

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

There is no separate Stop and Start: the engine runs for as long as the app does.

## Menu commands

The app menu and the menu bar menu have what Syncthing.app's menu has:

- **Pause All Devices** (⌥⌘P) adds every other device to the app's list of paused devices, so it lasts across restarts,
  and the menu bar icon shows a pause symbol. **Resume All Devices** takes its place once the user paused them all, and
  empties the list, so devices paused one by one resume too. The title follows that list, not whether devices are paused
  right now: while connectivity is turned off for the current network or Low Power Mode, devices are paused and the icon
  says so, yet resuming could not bring them back, so the menu still offers Pause All Devices. When a device is added
  while all are paused, the menu offers to pause it too.
- **Rescan All Folders** (⌥⌘R) rescans each folder, as Rescan does for one.
- **Restart Syncthing** has no shortcut, as it interrupts every transfer.
- **Open Web GUI** (⌥⌘O) opens the address in the configuration in the default browser, and is unavailable while the web
  GUI is off or on a Unix socket. On a new configuration it stops at the sign-in page until a user and password are
  chosen ([Web GUI and REST API](#web-gui-and-rest-api)).

## Menu bar status

The menu bar icon says what the engine is doing, and the first line of its menu says it in words, after the engine's
version. The first row that applies wins (`EngineStatus`):

| Icon | First line says | When |
| --- | --- | --- |
| `exclamationmark.triangle.fill` | Error, with the message | The engine failed |
| `hourglass` | Starting… | The engine isn't running yet, or restarts |
| `exclamationmark.triangle.fill` | Error in, with the folder | A folder stopped on an error, such as its path being gone |
| `exclamationmark.triangle.fill` | Failed items in, with the folder | A folder that runs could not scan or synchronize some items |
| `pause.circle.fill` | All devices are paused | Every other device is paused, by the user or by the app while connectivity is turned off |
| `pause.circle.fill` | All folders are paused | Every folder is paused |
| `folder` | Not connected | No other device is connected |
| `arrow.triangle.2.circlepath` | Synchronizing... | Files are going to or from another device. Scanning doesn't count. |
| `folder.fill` | Up to date | None of the above |

The status follows the engine's events, with no polling: each one has `AppState` read the engine again, and the
`MenuBarExtra` follows the result. `MenuBarExtra` has no tooltip, so what Syncthing.app shows in one is on that first line.
Files in Finder mode has no engine, so its icon doesn't change.

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
