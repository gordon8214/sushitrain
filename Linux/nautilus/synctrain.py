# Copyright (C) 2026 Gordon
#
# This Source Code Form is subject to the terms of the Mozilla Public
# License, v. 2.0. If a copy of the MPL was not distributed with this file,
# You can obtain one at https://mozilla.org/MPL/2.0/.

"""Native Nautilus actions for Synctrain files and their previous versions.

Mount actions use extended attributes; synchronized folders use the private admin socket.
"""

import json
import os
import socket
import subprocess
import threading
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


_MOUNTS = {"paths": [], "at": 0.0}
_SYNC = {"paths": [], "at": 0.0, "loading": False}
_SYNC_LOCK = threading.Lock()


def mounts():
    """Cache mount locations for a few seconds across Nautilus file queries."""
    found, read_at = _MOUNTS["paths"], _MOUNTS["at"]
    if time.monotonic() - read_at < 5:
        return found
    found = []
    try:
        with open("/proc/self/mounts", encoding="utf-8", errors="surrogateescape") as f:
            for line in f:
                fields = line.split()
                if len(fields) > 2 and fields[2] == "fuse.synctrain":
                    # Mount-table paths use octal escapes.
                    found.append(re.sub(r"\\([0-7]{3})",
                                        lambda match: chr(int(match.group(1), 8)), fields[1]))
    except OSError:
        pass
    _MOUNTS.update(paths=found, at=time.monotonic())
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


def synchronized_folders():
    """Return cached roots and refresh without blocking Nautilus's UI thread."""
    with _SYNC_LOCK:
        if not _SYNC["loading"] and time.monotonic() - _SYNC["at"] > 5:
            _SYNC["loading"] = True
            threading.Thread(target=refresh_synchronized_folders, daemon=True).start()
        return tuple(_SYNC["paths"])


def refresh_synchronized_folders():
    """Read the current daemon's folder roots through its private admin socket."""
    paths = []
    try:
        result = subprocess.run(["synctrain-gateway", "home", "--json"],
                                capture_output=True, check=True, timeout=2)
        endpoint = json.loads(result.stdout)["socket"]
        with socket.socket(socket.AF_UNIX, socket.SOCK_STREAM) as connection:
            connection.settimeout(0.2)
            connection.connect(endpoint)
            connection.sendall(b'{"method":"engine.folders"}\n')
            response = bytearray()
            while b"\n" not in response and len(response) < 1024 * 1024:
                part = connection.recv(65536)
                if not part:
                    break
                response.extend(part)
        folders = json.loads(response.split(b"\n", 1)[0]).get("result", [])
        paths = [os.path.abspath(folder["Path"]) for folder in folders]
    except (OSError, ValueError, KeyError, subprocess.SubprocessError):
        pass
    finally:
        with _SYNC_LOCK:
            _SYNC.update(paths=paths, at=time.monotonic(), loading=False)


def synchronized_path(file_info):
    """Return a regular file's path only inside a currently synchronized root."""
    uri = urlparse(file_info.get_uri())
    if uri.scheme != "file" or uri.netloc not in ("", "localhost"):
        return None
    path = os.path.abspath(unquote(uri.path))
    for root in synchronized_folders():
        if path.startswith(root + os.sep) and not file_info.is_directory():
            return path
    return None


def xattr(path, name):
    """Read a state attribute, returning None when it is unavailable."""
    try:
        return os.getxattr(path, name).decode()
    except OSError:
        return None


class Synctrain(GObject.GObject, Nautilus.MenuProvider,
                Nautilus.InfoProvider, Nautilus.ColumnProvider):
    """Expose native context actions and placeholder state in Nautilus."""
    @staticmethod
    def get_columns(*args, **kwargs):
        del args, kwargs
        return [Nautilus.Column(name="SynctrainColumn::state",
                                attribute="synctrain_state", label="Synctrain",
                                description="Whether the item is on this device")]

    def update_file_info(self, *args, **kwargs):
        file_info = args[0] if args else kwargs["file"]
        path = local_path(file_info)
        if path is None:
            return Nautilus.OperationResult.COMPLETE
        state = xattr(path, STATE)
        file_info.add_string_attribute("synctrain_state", LABELS.get(state, ""))
        return Nautilus.OperationResult.COMPLETE

    def get_file_items(self, *args, **kwargs):
        files = args[0] if args else kwargs["files"]
        paths = [local_path(f) for f in files]
        if not paths or None in paths:
            if len(files) == 1:
                path = synchronized_path(files[0])
                if path:
                    return [self.versions_item(path)]
            return []
        items = []
        kept = [xattr(p, KEEP) == "1" for p in paths]
        if not all(kept):
            keep = Nautilus.MenuItem(name="Synctrain::keep", label="Keep on This Device")
            keep.connect("activate", self.set_keep, files, paths, b"1")
            items.append(keep)
        if any(kept):
            stop = Nautilus.MenuItem(name="Synctrain::stop-keeping",
                                     label="Stop Keeping on This Device")
            stop.connect("activate", self.set_keep, files, paths, b"0")
            items.append(stop)
        if len(paths) == 1 and not files[0].is_directory():
            items.append(self.versions_item(paths[0]))
        return items

    def set_keep(self, _menu, files, paths, value):
        """Apply offline retention and refresh visible state after each operation."""
        failed = []
        for path in paths:
            try:
                os.setxattr(path, KEEP, value)
            except OSError as e:
                failed.append((path, e))
        for f in files:
            f.invalidate_extension_info()
        for path, e in failed:
            # Items retained by their parent must stop being kept with that parent.
            print(f"Synctrain: {os.path.basename(path)}: {e.strerror}", file=sys.stderr)

    def versions_item(self, path):
        """Build the shared native history command for either mode."""
        item = Nautilus.MenuItem(name="Synctrain::versions", label="Previous Versions…")
        item.connect("activate", self.versions, path)
        return item

    def versions(self, _menu, path):
        """Open the native version browser without leaving a child to reap."""
        # Reaped by GLib, unlike a child of subprocess
        Gio.Subprocess.new(["synctrain", "versions", path], Gio.SubprocessFlags.NONE)


synchronized_folders()
