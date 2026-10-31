<p align="center" width="100%">
<img src="./Docs/logo.png" />
</p>

<p align="center" width="100%">
<h1 align="center">Sushitrain</h1>
</p>

<p align="center" width="100%">
Securely synchronize files with Syncthing on macOS, and open them from iOS through a gateway.
</p>

[Download on the App Store](https://apps.apple.com/nl/app/synctrain/id6553985316) | [Test beta versions through TestFlight](https://testflight.apple.com/join/2f54I4CM)

These are for iOS. The Mac app runs outside the App Sandbox, so it isn't in the App Store: get the notarized disk image from the [releases](https://github.com/gordon8214/sushitrain/releases).

On macOS, Sushitrain runs Syncthing, and has some extra tricks up its sleeve:

- **Selective file synchronization**: only keep specific files or subdirectories from a larger folder on your device.
- **On-demand access to files**: download or stream files that are not available locally from another peer through the Syncthing protocol. This allows access to your files, from your own devices, without setting up a VPN.
- [**Remote thumbnails**](./Docs/thumbnails.md): the app can generate thumbnails for files on other devices (images or videos). You can even synchronize thumbnails between devices, so previews can be generated on a device that has all the files for use on other devices.
- **Photo back-up** (automatically export photos from the system album to a folder) as well as [**photo folders**](./Docs/photo-fs.md) (directly synchronize a system photo album as a send-only folder, without exporting).
- **Tight macOS integration**: Shortcuts, Continuity, a [gateway](./Docs/gateway.md) for your phone, and
  [folders on demand in Finder](./Docs/file-provider.md#on-macos), without a gateway.

On iOS and iPadOS, Synctrain is a [client of gateways](./Docs/file-provider.md): it runs no Syncthing itself. Choose folders from a Mac running Synctrain or a server running `synctrain-gateway`, then browse, search and play them in the app, and open, edit and add files from the Files app.

## Building

The Mac app consists of a framework in Go and front-end code in Swift (the `Synctrain` target). The iOS app is Swift only (the `SynctrainMobile` target, with the two File Provider extensions), and shares the gateway client in `SynctrainFileProviderKit`.

To build the app, you need a macOS computer with the following:

- XCode with the iOS 27 SDK installed
- Developer certificates
- Go (see [go.mod](./SushitrainCore/go.mod) for the version used)

First, verify the Go framework builds correctly. Open a terminal and do the following:

```bash
cd SushitrainCore
make
```

This should not show any errors and lead to a built framework in the `build` directory.

Note that the Makefile assumes you installed Go through Homebrew in `/opt/homebrew`. Change accordingly if your environment
is different.

When the framework is succesfully built, open XCode and follow the following steps:

1. Set up signing (change the 'team ID' in the project settings you a team or developer ID you have access to, then let
   XCode autoamtically provision certificates for you). You may have to change the bundle ID to something else.
2. Finally, pressing Cmd-B should be all that is needed to build the app.

Release builds for the Mac sign with a Developer ID certificate. See [Signing and releasing](./Docs/macos.md#signing-and-releasing)
for what that takes, and for how to notarize a release.

The project doesn't request the `com.apple.developer.device-information.user-assigned-device-name` entitlement, which
is only granted by Apple, for distributed apps. Without it, iOS reports a generic name, so the pairing screen lets you
give the device the name gateways should list it under.

Note that XCode builds the Go framework as part of the `Synctrain` scheme's Run action, but not when archiving: `make mac`
and `make mac-release` run `make core` first. In development it may be easier to build the framework by hand to be able to
easily see any compiler output (a subsequent make invocation from XCode should be rather quick if nothing changed since
the manual invocation). The iOS app doesn't use it.

Then, run `go mod tidy`.

## Architecture

The Mac app is written in Swift and uses SwiftUI. It embeds a framework called SushitrainCore, which is written
in Go. This framework embeds the Syncthing code and provides an interface that is usable from the Swift side.

The Go and Swift sides are bridged by a tool/framework called 'gomobile'. This tool generated bindings for Go code to be
called from Objective-C (and hence from Swift). Gomobile does not support all Go types, however - in particular, slices
(arrays) and therefore almost all of the more complicated data types in Syncthing are not supported. The SushitrainCore
framework papers over this by only exposing the bits necessary for the app to work. To expose arrays, we use a very simple
'list of strings' type that the Swift side can use to iterate items (typically devices or folders).

The primary interface on the Go side is the 'Client' struct. The Swift side constructs one instance of this struct and sets
a Swift class as a delegate, so it can receive events. These events update state on the Swift side, which then leads to
the SwiftUI based UI to be updated automatically. Note that this state may only be changed from the main thread. Therefore
care must be taken to defer work to the main thread upon receiving callbacks from the Go side. Calling the Go side from
Swift typically also happens on the main thread (i.e. in response to a UI action), but this is not typically a requirement,
as most mutating operations are guarded by mutexes in Syncthing (e.g. the mechanism to change and save the configuration,
which is used for a lot of functionality in the app).

Technically the app runs a full-blown Syncthing node in-process. This means it supports all the synchronization features
of Syncthing, as well as features around e.g. discovery, ignore files, et cetera. Not all features are exposed, however, for
ease of use.

The iOS app embeds none of this. It is a SwiftUI client of the gateway API, in `SynctrainMobile`, on top of the index,
outbox and transfers in `SynctrainFileProviderKit`, which the File Provider extension shares with it. What the Mac app also
needs for folders in Finder is in `SynctrainClient`, which both apps compile.

## Features

Sushitrain provides several affordances that regular Syncthing clients typically do not. The most important features are (1) on-demand download and (2) selective synchronisation.

### Selective synchronisation

Typically, mobile devices are constrained in the amount of storage they have. While modern devices are capable enough for
synchronising larger folders using Syncthing, it is often undesirable. Current clients do not offer any other way to access
files through Syncthing without synchronising first.

One solution is _selective synchronisation_. This feature allows the user to select a subset of files and directories in the folder
that should be synchronised. In Sushitrain, this is implemented using the existing 'ignore' capabilities in Syncthing. When
a folder is set into 'selective' mode, an `.stignore` file will be created with a single pattern: `*`. This causes all files
to be ignored for synchronisation. At this point, the folder global index will still be synchronized like normal, but the
client will not 'pull' files from other peers, nor will it 'push' any new files or changes to files. Because the global
index is available and the client does have the ability to pull files, it can still download files 'on demand' (see below).

When a user selects a file to sync to the device, an exception pattern is added to the `.stignore` file before the catch-
all `*` pattern, e.g.:

```
!/some/file/I/want.txt
*
```

This will cause the file to be synchronised (depending on the mode of the folder, changes made locally will also be pushed,
and deleting the file is propagated as well).

There are a few issues with this:

- When the file is renamed locally, Syncthing handles this as a removal and creation of a new file. The removal is pushed
  to other nodes (if the folder is 'send receive'), the new file will be ignored because of the `*` pattern. For this reason,
  the app checks for files that exist in the local directory, but are not 'selected', and offers the user a choice to either
  'select' the file and synchronize it, or delete it locally. Files that exist locally but aren't 'selected' are termed 'extraneous files'.
- When a new file is created locally that has the same name of a file that also exists in the global index, but is not selected,
  the file is not synced until it is 'selected', at which point it might overwrite the existing global file. This situation
  is also handled by providing the user a choice.
- When a file is renamed remotely, it is also a removal and creation. The removal is processed by the client (the selected
  file will disappear), the new file will not appear until 'selected'.

Sushitrain also supports 'selecting' directories. In this case, a pattern is added to `.stignore` that matches any (current
and future) file in the folder:

```
!/some/folder
*
```

Current and future files in the folder will be synced and considered 'implicitly selected'.

Synctrain enables selective synchronisation features in the UI when the `.stignore` file conforms to the following rules:

* The last pattern in the file is `*`
* The file starts with zero or more patterns that start with `(?d)` and that do not contain `/` or `**` (these allow for 
  global ignores of certain files like `.DS_Store` when synchronizing subdirectories)
* The file otherwise contains only patterns that start with `!/` and that do not contain `*`. These indicate paths to be 'selected' (un-ignored)
* Empty lines are allowed

### On-demand downloads

In Sushitrain, files can be accessed on demand. The user can select a file to view, and it will be downloaded from one of
the peers that has the necessary blocks available. The file is selected from the 'global index' (i.e. the set of files that
all nodes participating in a folder synchronisation agree on). Sushitrain always syncs the global index, even when selective
synchronisation is enabled. File downloads use the same mechanism as regular 'pulling' during synchronisation.

To enable streaming playback of media files, Sushitrain runs an HTTP server on localhost on a random port. When a user selects
a video file for streaming, it will point the media player to a special URL (`/file?folder=X&path=Y&signature=Z`) that supports
HTTP range requests. Range requests will cause only the blocks necessary to fulfill the request to be fetched from the Syncthing
remote peer.

### Custom configurations

By default, the Mac app creates a device identity and default configuration on first launch. See
[Docs/macos.md](./Docs/macos.md) for where it keeps them, and how it takes over an existing Syncthing setup.

### Thumbnails

See the [documentation on thumbnails](./Docs/thumbnails.md).

## The name

Sushitrain is the code name for this app. Due to the name being unavailable in the App Store, it is not the final name
of the app.

In Japan, several lower-end sushi restaurants have a system where you order sushi through a tablet, which then is delivered
to your table by a small train that runs between the kitchen and the tables. This is a very efficient system of ordering
small bites. Additionally, 'SushiTrain' shares the 'SyncThing' initials.

## Contributing

Pull requests to this repository are welcomed and will be considered. Contributors to this repository agree to license
their contributions under the license described below (MPLv2). Regardless of the license in effect, you retain the
copyright to your contribution.

If you have found an issue, please notify us through the TestFlight testing program. If this is not possible, you can use
the discussions section on this repository. The developers are not obliged to answer any questions or fix any issues -
remember this is free software. The planning of releases and the roadmap is solely up to the developers. If you have a
specific need, you may contact the developers to discuss a commercial development project.

## License

Sushitrain, Synctrain are (C) Tommy van der Vorst (tommy@t-shaped.nl), 2024-2026.

Except when explicitly noted otherwise, the code in this repository is licensed under the Mozilla Public License 2.0.
Read the license [here](./LICENSE). Contributors to this repository agree to license their contributions under this license.

The initial [3D logo model](./Assets/Sushitrain%20glass%20icon.blend) was made by [Kai Werder](https://werder.fyi) in 2025 with
Blender 4.5.1 LTS, and is distributed under the Mozilla Public License 2.0.

The following items are explicitly _not_ licensed under the abovementioned license. Instead all rights are reserved by the
author / the respective owners:

- The name 'Sushitrain' and the name 'Synctrain'
- The final logo files, contained in:
  - [AppIcon.appiconset](./Sushitrain/Assets.xcassets/AppIcon.appiconset/)
  - [Sushitrain glass.icon](./Assets/Sushitrain%20glass%20icon.icon/)
  - [Sushitrain.icon](./Assets/Sushitrain.icon/)
  - [logo.png](./Docs/logo.png)

Syncthing is a trademark of the Syncthing Foundation. Read more over at [syncthing.net](https://syncthing.net). This project
is not endorsed by, nor affiliated with, neither Syncthing contributors nor the Syncthing Foundation.
