# Copyright (C) 2026 Gordon
#
# This Source Code Form is subject to the terms of the Mozilla Public
# License, v. 2.0. If a copy of the MPL was not distributed with this file,
# You can obtain one at https://mozilla.org/MPL/2.0/.

"""Synctrain in Nautilus (nautilus-python 4): Keep on This Device, Stop Keeping on This Device and Previous Versions
for items in a Synctrain mount, and a column with their state. The daemon does the work, through the items' extended
attributes; Previous Versions opens Synctrain's window."""

import os
import re
import sys
import time
from urllib.parse import unquote, urlparse

from gi.repository import Gio, GObject, Nautilus

STATE = "user.synctrain.state"
KEEP = "user.synctrain.keep"

LABELS = {
    "remote": "On the gateway",
    "partial": "Partly here",
    "local": "Here",
    "kept": "Kept here",
    "pending": "Sending",
    "failed": "Can't be sent",
    "blocked": "Can't be sent",
    "local-only": "Only here",
}


_mounts = ([], 0.0)


def mounts():
    """The directories Synctrain is mounted at, read again at most every few seconds (Nautilus asks for each file)"""
    global _mounts
    found, read_at = _mounts
    if time.monotonic() - read_at < 5:
        return found
    found = []
    try:
        with open("/proc/self/mounts", encoding="utf-8", errors="surrogateescape") as f:
            for line in f:
                fields = line.split()
                if len(fields) > 2 and fields[2] == "fuse.synctrain":
                    # Spaces and other characters are written as octal escapes; the rest is the path as it is
                    found.append(re.sub(r"\\([0-7]{3})", lambda m: chr(int(m.group(1), 8)), fields[1]))
    except OSError:
        pass
    _mounts = (found, time.monotonic())
    return found


def local_path(file_info):
    """The path of a file in a Synctrain mount, or None"""
    uri = file_info.get_uri()
    if not uri.startswith("file://"):
        return None
    path = unquote(urlparse(uri).path)
    for mount in mounts():
        if path.startswith(mount + "/"):
            return path
    return None


def xattr(path, name):
    try:
        return os.getxattr(path, name).decode()
    except OSError:
        return None


class Synctrain(GObject.GObject, Nautilus.MenuProvider, Nautilus.InfoProvider, Nautilus.ColumnProvider):
    def get_columns(self):
        return [Nautilus.Column(name="SynctrainColumn::state", attribute="synctrain_state", label="Synctrain",
                                description="Whether the item is on this device")]

    def update_file_info(self, file_info):
        path = local_path(file_info)
        if path is None:
            return Nautilus.OperationResult.COMPLETE
        state = xattr(path, STATE)
        file_info.add_string_attribute("synctrain_state", LABELS.get(state, ""))
        return Nautilus.OperationResult.COMPLETE

    def get_file_items(self, files):
        paths = [local_path(f) for f in files]
        if not paths or None in paths:
            return []
        items = []
        kept = [xattr(p, KEEP) == "1" for p in paths]
        if not all(kept):
            keep = Nautilus.MenuItem(name="Synctrain::keep", label="Keep on This Device")
            keep.connect("activate", self.set_keep, files, paths, b"1")
            items.append(keep)
        if any(kept):
            stop = Nautilus.MenuItem(name="Synctrain::stop-keeping", label="Stop Keeping on This Device")
            stop.connect("activate", self.set_keep, files, paths, b"0")
            items.append(stop)
        if len(paths) == 1 and not files[0].is_directory():
            versions = Nautilus.MenuItem(name="Synctrain::versions", label="Previous Versions…")
            versions.connect("activate", self.versions, paths[0])
            items.append(versions)
        return items

    def set_keep(self, menu, files, paths, value):
        failed = []
        for path in paths:
            try:
                os.setxattr(path, KEEP, value)
            except OSError as e:
                failed.append((path, e))
        for f in files:
            f.invalidate_extension_info()
        for path, e in failed:
            # Not kept or kept as asked (an item kept with a folder that contains it can only stop with that folder)
            print(f"Synctrain: {os.path.basename(path)}: {e.strerror}", file=sys.stderr)

    def versions(self, menu, path):
        # Reaped by GLib, unlike a child of subprocess
        Gio.Subprocess.new(["synctrain", "versions", path], Gio.SubprocessFlags.NONE)
