#!/bin/bash
# Builds the GTK window (default) or a linux-nautilus target with Fedora 44 development packages. Uses the
# host's Go and its module and build caches (the first build of gotk4 takes a long time; later ones don't).
# Rootless podman; SELinux labels of the mounted directories are left as they are.
set -eu
root=$(cd "$(dirname "$0")/../.." && pwd)
target="${1:-linux-window}"
case "$target" in linux-window|linux-nautilus|linux-nautilus-test) ;; *) echo "Unsupported build target: $target" >&2; exit 2 ;; esac
goroot=$(go env GOROOT)
build_arch="${LINUX_ARCH:-$(go env GOARCH)}"
if [[ "$build_arch" != "$(go env GOHOSTARCH)" ]]; then echo "Build native desktop components on a host of the target architecture" >&2; exit 2; fi
# Built every time, which is quick once cached, so that a change to the Containerfile is taken
podman build -q -t synctrain-window-build -f "$root/Linux/build/Containerfile" "$root/Linux/build" >/dev/null
build_cache="$(go env GOCACHE)/synctrain-fedora44-$build_arch"
mkdir -p "$(go env GOMODCACHE)" "$build_cache"
version=$(git -C "$root" describe --always --dirty 2>/dev/null || echo dev)
podman run --rm --security-opt label=disable \
	-v "$goroot:/usr/local/go:ro" -v "$(go env GOMODCACHE):/go/pkg/mod" -v "$build_cache:/root/.cache/go-build" \
	-v "$root:/src" -w /src/SushitrainCore -e GOMODCACHE=/go/pkg/mod -e GOFLAGS=-buildvcs=false \
	-e PATH=/usr/local/go/bin:/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin \
	synctrain-window-build make "$target" "LINUX_ARCH=$build_arch" "LINUX_VERSION=$version"
