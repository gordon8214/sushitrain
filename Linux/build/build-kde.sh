#!/bin/bash
# Copyright (C) 2026 Gordon. SPDX-License-Identifier: MPL-2.0
# Build the Qt window and both Dolphin plugins on the target Linux architecture.
set -eu
root=$(cd "$(dirname "$0")/../.." && pwd)
arch=${LINUX_ARCH:-$(go env GOARCH)}
case "$(uname -s):$arch:$(uname -m)" in Linux:amd64:x86_64|Linux:arm64:aarch64) ;; *) echo 'Build KDE components on a Linux host of the target architecture' >&2; exit 2 ;; esac
variant=${SYNCTRAIN_KDE_BUILD_VARIANT:-native}
case "$variant" in native|fedora44) ;; *) echo 'Unsupported KDE build variant' >&2; exit 2 ;; esac
build="$root/SushitrainCore/build/kde-$arch-$variant"
stage="$root/SushitrainCore/build/linux-$arch/kde-stage"
cmake -S "$root/Linux/kde" -B "$build" -G Ninja -DCMAKE_BUILD_TYPE=Release -DCMAKE_INSTALL_PREFIX=/usr -DKDE_INSTALL_USE_QT_SYS_PATHS=ON -DKDE_INSTALL_PLUGINDIR="$(qtpaths6 --query QT_INSTALL_PLUGINS)" -DBUILD_SHARED_LIBS=ON -DBUILD_TESTING=ON
cmake --build "$build" --parallel "${SYNCTRAIN_BUILD_JOBS:-4}"
if [[ "${1:-}" == test ]]; then
 # Native suites normally take around six seconds. CTest also bounds each case.
 timeout 60 ctest --test-dir "$build" --output-on-failure --timeout 30
fi
rm -rf -- "$stage"
DESTDIR="$stage" cmake --install "$build"
