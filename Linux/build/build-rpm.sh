#!/bin/bash
# Builds an RPM of Synctrain for Linux on a Fedora host (Linux/synctrain.spec): the daemons with the host's Go (the
# release go.mod names), the window with GTK's development packages when they are installed or in a container
# otherwise, then the package, in SushitrainCore/build/rpm.
set -eu
root=$(cd "$(dirname "$0")/../.." && pwd)
core="$root/SushitrainCore"
make -C "$core" linux
if pkg-config --exists gtk4 libadwaita-1 2>/dev/null; then
	make -C "$core" linux-window
else
	"$root/Linux/build/build-window.sh"
fi

# v2.8.85-137-g8a968ad: version 2.8.85, release 137.g8a968ad
describe=$(git -C "$root" describe --tags --long --match 'v*' 2>/dev/null || echo v0.0.0-0-gunknown)
describe=${describe#v}
version=${describe%%-*}
rest=${describe#*-}
release="${rest%%-*}.${rest#*-}"

stage=$(mktemp -d)
trap 'rm -rf "$stage"' EXIT
cp "$core"/build/linux-amd64/{synctrain,synctrain-files,synctrain-gateway} "$stage/"
cp "$root"/Linux/systemd/*.service "$root"/Linux/com.gordonbyrnes.Synctrain.{desktop,metainfo.xml} \
	"$root/Linux/nautilus/synctrain.py" "$root/LICENSE" "$root/Docs/linux.md" "$stage/"
cp "$root/Docs/logo.png" "$stage/com.gordonbyrnes.Synctrain.png"
# The spec keeps no changelog: the git history is it
rpmbuild -bb --define "_sourcedir $stage" --define "_rpmdir $core/build/rpm" --define "source_date_epoch_from_changelog 0" \
	--define "synctrain_version $version" --define "synctrain_release $release" "$root/Linux/synctrain.spec"
ls "$core"/build/rpm/*/synctrain-*.rpm
