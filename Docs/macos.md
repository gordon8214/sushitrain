# Synctrain on macOS

On a Mac, Synctrain is meant to replace Syncthing.app: one engine, configured from the app, and from its web GUI or its
REST API where a configuration has the GUI enabled. This page records where the Mac app differs from iOS and why.

The Mac app runs outside the App Sandbox (with the hardened runtime), so that it reaches folders anywhere, as Syncthing
does; that also means it can't come from the Mac App Store, and comes as a notarized disk image instead (see
[Signing and releasing](#signing-and-releasing)). Its share extension stays sandboxed, as macOS requires of
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
`return-to-syncthing-app` checks for both.

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

The Release configuration signs the app and its share extension with Developer ID Application, with the hardened runtime
and without the `get-task-allow` entitlement. It needs no provisioning profile: neither has an entitlement that takes
one, and macOS accepts an app group that starts with the team's ID without one, and without asking the user. Debug and
iOS builds sign as they did. Both schemes run the Release configuration, so running the app on a Mac from Xcode takes
the certificate too, or a scheme set to Debug; a Release run has no debugger, as it lacks `get-task-allow`. `make mac` and
`make build` don't sign, and take no certificate.

`make mac-release` archives the app and hands the archive to [`Scripts/notarize-mac.sh`](../Scripts/notarize-mac.sh),
which leaves the stapled app and `Synctrain-<version>.<build>.dmg` in `Build/release`:

1. It checks that all code in the app is signed by one team with Developer ID, with a secure timestamp and the hardened
   runtime and without `get-task-allow`; that the app is not sandboxed and its extensions are; and that they all use one
   app group with the team's prefix, which neither notarization nor Gatekeeper looks at.
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

3. **CI.** Turn workflows on in the repository's Actions tab: GitHub doesn't run them in a fork until then. Create an
   environment named `mac-release` in the repository's settings, allow it only tags matching `v*`, and add these secrets
   to that environment rather than to the repository, so that no branch can read them:

   | Secret | Value |
   | --- | --- |
   | `DEVELOPER_ID_P12` | The `.p12`, as base64 (`base64 -i developer-id.p12`) |
   | `DEVELOPER_ID_P12_PASSWORD` | Its password |
   | `NOTARY_KEY_P8` | The `.p8` key, as base64 |
   | `NOTARY_KEY_ID` | The key's ID |
   | `NOTARY_ISSUER_ID` | The issuer ID shown above the keys |

Neither file belongs in the repository, and Git ignores both types. The workflow keeps them in a temporary keychain and
folder, which it removes after the build.

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
