#!/bin/bash
# Builds an RPM of Synctrain for Linux on a Fedora host (Linux/synctrain.spec): the daemons with the host's Go (the
# release go.mod names), the window with GTK's development packages when they are installed or in a container
# otherwise, then the package, in SushitrainCore/build/rpm.
set -eu
root=$(cd "$(dirname "$0")/../.." && pwd)
core="$root/SushitrainCore"
export LINUX_ARCH="${LINUX_ARCH:-$(go env GOARCH)}"
case "$LINUX_ARCH" in amd64|arm64) ;; *) echo "Unsupported Linux architecture: $LINUX_ARCH" >&2; exit 2 ;; esac
make -C "$core" linux
desktops="${SYNCTRAIN_DESKTOPS:-gnome,kde}"
case "$desktops" in gnome|kde|gnome,kde) ;; *) echo 'SYNCTRAIN_DESKTOPS must be gnome, kde, or gnome,kde' >&2; exit 2 ;; esac
rpm_options=()
if [[ "$desktops" == *gnome* ]]; then
 if [[ "${SYNCTRAIN_CONTAINER_BUILD:-0}" != 1 ]] && pkg-config --exists gtk4 libadwaita-1; then make -C "$core" linux-window; else "$root/Linux/build/build-window.sh"; fi
 if [[ "${SYNCTRAIN_CONTAINER_BUILD:-0}" != 1 ]] && pkg-config --exists libnautilus-extension-4 json-glib-1.0 gio-unix-2.0; then make -C "$core" linux-nautilus; else "$root/Linux/build/build-window.sh" linux-nautilus; fi
else
 rpm_options+=(--without gnome)
fi
if [[ "$desktops" == *kde* ]]; then
 # Configure the complete project: Qt Quick alone does not establish that KDE,
 # Multimedia, PDF, ECM, and all other required development files are present.
 probe=$(mktemp -d)
 if [[ "${SYNCTRAIN_CONTAINER_BUILD:-0}" != 1 ]] && \
    cmake -S "$root/Linux/kde" -B "$probe" -DBUILD_TESTING=ON >"$probe/configure.log" 2>&1; then
  rm -rf -- "$probe"
  "$root/Linux/build/build-kde.sh"
 else
  rm -rf -- "$probe"
  podman build -q -t synctrain-kde-build -f "$root/Linux/build/Containerfile.kde" "$root/Linux/build" >/dev/null
  podman run --rm --security-opt label=disable -v "$root:/src" -w /src -e LINUX_ARCH="$LINUX_ARCH" -e SYNCTRAIN_KDE_BUILD_VARIANT=fedora44 synctrain-kde-build Linux/build/build-kde.sh
 fi
else
 rpm_options+=(--without kde)
fi

# v2.8.85-137-g8a968ad: version 2.8.85, release 137.g8a968ad
describe=${SYNCTRAIN_PACKAGE_DESCRIBE:-$(git -C "$root" describe --tags --long --match 'v*' 2>/dev/null || echo v0.0.0-0-gunknown)}
describe=${describe#v}
version=${describe%%-*}
rest=${describe#*-}
release="${rest%%-*}.${rest#*-}"
# A tree with changes isn't the commit: its package mustn't pass for that commit's
git -C "$root" diff --quiet HEAD 2>/dev/null || release="$release.dirty"

stage=$(mktemp -d)
trap 'rm -rf "$stage"' EXIT
cp "$core"/build/linux-"$LINUX_ARCH"/{synctrain-mcp,synctrain-files,synctrain-gateway,synctrain-desktop} "$stage/"
cp "$root/Linux/synctrain" "$stage/synctrain"
if [[ "$desktops" == *gnome* ]]; then cp "$core"/build/linux-"$LINUX_ARCH"/{synctrain-gtk,libsynctrain-nautilus.so} "$stage/"; fi
if [[ "$desktops" == *kde* ]]; then cp -a "$core/build/linux-$LINUX_ARCH/kde-stage" "$stage/kde-stage"; fi
cp "$root"/Linux/systemd/*.service "$root"/Linux/com.gordonbyrnes.Synctrain.{desktop,metainfo.xml} \
	"$root/LICENSE" "$root/Docs/linux.md" "$root/Docs/mcp.md" "$root/Docs/mcp-acceptance.md" "$stage/"
cp "$root/Docs/logo.png" "$stage/com.gordonbyrnes.Synctrain.png"
cp "$core/notices.html" "$stage/GoNotices.html"
# The spec keeps no changelog: the git history is it
rpmbuild -bb "${rpm_options[@]}" --define "_sourcedir $stage" --define "_rpmdir $core/build/rpm" --define "source_date_epoch_from_changelog 0" \
	--define "synctrain_version $version" --define "synctrain_release $release" "$root/Linux/synctrain.spec"
ls "$core"/build/rpm/*/synctrain-*.rpm
