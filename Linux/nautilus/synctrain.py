# Copyright (C) 2026 Gordon
#
# This Source Code Form is subject to the terms of the Mozilla Public
# License, v. 2.0. If a copy of the MPL was not distributed with this file,
# You can obtain one at https://mozilla.org/MPL/2.0/.

"""Synctrain in Nautilus (nautilus-python 4): Keep on This Device, Stop Keeping on This Device and Previous Versions
for items in a Synctrain mount, and a column with their state. The daemon does the work, through the items' extended
attributes; Previous Versions opens Synctrain's window."""

import os
import subprocess
from urllib.parse import unquote, urlparse

from gi.repository import GObject, Nautilus

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


def mounts():
    """The directories Synctrain is mounted at"""
    found = []
    try:
        with open("/proc/self/mounts") as f:
            for line in f:
                fields = line.split()
                if len(fields) > 2 and fields[2] == "fuse.synctrain":
                    found.append(fields[1].encode().decode("unicode_escape"))
    except OSError:
        pass
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
            keep.connect("activate", self.set_keep, paths, b"1")
            items.append(keep)
        if any(kept):
            stop = Nautilus.MenuItem(name="Synctrain::stop-keeping", label="Stop Keeping on This Device")
            stop.connect("activate", self.set_keep, paths, b"0")
            items.append(stop)
        if len(paths) == 1 and not files[0].is_directory():
            versions = Nautilus.MenuItem(name="Synctrain::versions", label="Previous Versions…")
            versions.connect("activate", self.versions, paths[0])
            items.append(versions)
        return items

    def set_keep(self, menu, paths, value):
        for path in paths:
            try:
                os.setxattr(path, KEEP, value)
            except OSError:
                pass

    def versions(self, menu, path):
        subprocess.Popen(["synctrain", "versions", path], start_new_session=True)
