#!/bin/bash
# Builds SushitrainCore/build/linux-amd64/synctrain (the window) in a container with GTK's development packages, with
# the host's Go and its module and build caches (the first build of gotk4 takes a long time; later ones don't).
# Rootless podman; SELinux labels of the mounted directories are left as they are.
set -eu
root=$(cd "$(dirname "$0")/../.." && pwd)
goroot=$(go env GOROOT)
podman image exists synctrain-window-build ||
	podman build -t synctrain-window-build -f "$root/Linux/build/Containerfile" "$root/Linux/build"
mkdir -p "$(go env GOMODCACHE)" "$(go env GOCACHE)"
version=$(git -C "$root" describe --always --dirty 2>/dev/null || echo dev)
podman run --rm --security-opt label=disable \
	-v "$goroot:/usr/local/go:ro" -v "$(go env GOMODCACHE):/go/pkg/mod" -v "$(go env GOCACHE):/root/.cache/go-build" \
	-v "$root:/src" -w /src/SushitrainCore -e GOMODCACHE=/go/pkg/mod -e GOFLAGS=-buildvcs=false \
	synctrain-window-build /usr/local/go/bin/go build -trimpath -ldflags "-s -w -X main.version=$version" \
	-o build/linux-amd64/synctrain ./cmd/synctrain
