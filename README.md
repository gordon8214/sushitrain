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

On macOS, Sushitrain does one of two things, chosen per Mac:

- **Synchronize**: it runs Syncthing, and every folder syncs all of its contents.
- [**Files in Finder**](./Docs/file-provider.md#on-macos): it runs no Syncthing. Finder shows the folders of a gateway on another system, which has all their files, and macOS fetches what you open.

In Synchronize mode it has some extra tricks up its sleeve:

- [**Remote thumbnails**](./Docs/thumbnails.md): the app can generate thumbnails for files on other devices (images or videos). You can even synchronize thumbnails between devices, so previews can be generated on a device that has all the files for use on other devices.
- **Tight macOS integration**: Shortcuts, Continuity, and a [gateway](./Docs/gateway.md) for your phone.

On iOS and iPadOS, Synctrain is a [client of gateways](./Docs/file-provider.md): it runs no Syncthing itself. Choose folders from a Mac running Synctrain or a server running `synctrain-gateway`, then browse, search and play them in the app, and open, edit and add files from the Files app.

On Linux (Fedora with GNOME or KDE Plasma), [Synctrain for Linux](./Docs/linux.md) has the same two modes: **Synchronize** runs Syncthing, with the remote file service for phones, and **Files** shows the folders of a gateway in `~/Synctrain`, fetching files as they are opened. Build its package with `Linux/build/build-rpm.sh`.

On Windows 11, [Synctrain for Windows](./Docs/windows.md) uses native WinUI controls and supports **Synchronize** and **Files in Explorer** modes. The Files mode integrates with Windows Cloud Files for on-demand access, editing, pinning, thumbnails, and version history. Build ARM64 and x64 MSIX packages with `Windows/build.ps1`.

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

### Moving to another commit of the fork

The engine is [gordon8214/syncthing](https://github.com/gordon8214/syncthing), branch `synctrain/v2.1.7`, which
`SushitrainCore/go.mod` pins with a `replace`. To move the app to another commit, push that commit to the fork, keep the fork
checked out next to this repository as `syncthing`, and pass the script a commit or a branch:

```bash
Scripts/update-fork.sh synctrain/v2.1.7
```

The script:

1. Fetches the fork and checks that the commit is pushed. A branch means its tip on `origin`, and a local branch with commits
   of its own is refused.
2. Points the `replace` at the commit (`go mod edit`) and runs `go mod tidy`.
3. Checks that the web GUI built into the commit matches its `gui` folder, which fails if the fork changed `gui` without
   regenerating `lib/api/auto/gui.files.go`. There are no files to copy: the pages are in that generated file.
4. Regenerates `notices.html` with the `go-licenses` version in `go.mod`.
5. Raises N in `ENGINE_VERSION` (`v2.1.7-synctrain.N`) in `SushitrainCore/Makefile`, the version the engine reports.
6. Rebuilds the framework with `make`.

It changes only `go.mod`, `go.sum`, `notices.html` and the `Makefile` in `SushitrainCore`, stops if they already have
uncommitted changes, and doesn't commit: review the diff, and commit it with whatever code the move needed. A failure
before the build puts the four files back. If the framework doesn't build, they stay, so that the code can be fixed for the
new commit; `make -C SushitrainCore` then finishes the job.

The script keeps the `v2.1.7` in `ENGINE_VERSION`, so change that by hand when the fork moves to another Syncthing release.
It builds only the Mac framework: the `synctrain-gateway` executables have versions of their own, see
[Docs/gateway.md](./Docs/gateway.md).

### Moving a build tool to a newer version

`gomobile`, `gobind` and `go-licenses` are pinned in the `tool` block of `SushitrainCore/go.mod`. `make deps`, `make core` and
the rest of the build use those versions and leave `go.mod` alone, so a newer version is always a deliberate step:

```bash
cd SushitrainCore
go get -tool golang.org/x/mobile/cmd/gomobile@latest
go get -tool github.com/google/go-licenses@latest
go mod tidy
make deps
make
```

The first command moves `gobind` too, because both tools are in the `golang.org/x/mobile` module. `make deps` installs the
`gobind` from `go.mod` in `~/go/bin`, where `gomobile bind` finds it, and `make` regenerates `notices.html` and rebuilds the
framework. Review the changes to `go.mod`, `go.sum` and `notices.html` before committing them, because `go get` can also
raise modules the engine uses.

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

### Streaming

A file that hasn't arrived on this device yet, or that a folder's ignore patterns exclude, can still be viewed. It is selected
from the 'global index' (i.e. the set of files that all nodes participating in a folder synchronisation agree on), and
downloaded from one of the peers that has the necessary blocks available. File downloads use the same mechanism as regular
'pulling' during synchronisation.

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

## Assistant access

The bundled [MCP server](./Docs/mcp.md) provides structured inspection and management
through local stdio. Start Synctrain and explicitly enable MCP in Settings;
enabling it grants full management access. Headless gateways use the documented
local enable/disable command and can connect remote clients through SSH.
