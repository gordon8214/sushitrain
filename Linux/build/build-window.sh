#!/bin/bash
# Builds SushitrainCore/build/linux-$build_arch/synctrain (the window) in a container with GTK's development packages, with
# the host's Go and its module and build caches (the first build of gotk4 takes a long time; later ones don't).
# Rootless podman; SELinux labels of the mounted directories are left as they are.
set -eu
root=$(cd "$(dirname "$0")/../.." && pwd)
goroot=$(go env GOROOT)
build_arch="${LINUX_ARCH:-$(go env GOARCH)}"
if [[ "$build_arch" != "$(go env GOHOSTARCH)" ]]; then echo "Build the GTK window on a host of the target architecture" >&2; exit 2; fi
# Built every time, which is quick once cached, so that a change to the Containerfile is taken
podman build -q -t synctrain-window-build -f "$root/Linux/build/Containerfile" "$root/Linux/build" >/dev/null
build_cache="$(go env GOCACHE)/synctrain-fedora44-$build_arch"
mkdir -p "$(go env GOMODCACHE)" "$build_cache"
version=$(git -C "$root" describe --always --dirty 2>/dev/null || echo dev)
podman run --rm --security-opt label=disable \
	-v "$goroot:/usr/local/go:ro" -v "$(go env GOMODCACHE):/go/pkg/mod" -v "$build_cache:/root/.cache/go-build" \
	-v "$root:/src" -w /src/SushitrainCore -e GOMODCACHE=/go/pkg/mod -e GOFLAGS=-buildvcs=false \
	synctrain-window-build /usr/local/go/bin/go build -trimpath -ldflags "-s -w -X main.version=$version" \
	-o build/linux-$build_arch/synctrain ./cmd/synctrain
