"""Exercise native context-menu eligibility without changing user files."""
import importlib.util
import json
from pathlib import Path
from types import SimpleNamespace
import unittest
from unittest.mock import patch

SPEC = importlib.util.spec_from_file_location(
    "synctrain_nautilus", Path(__file__).parents[1] / "nautilus" / "synctrain.py")
EXTENSION = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(EXTENSION)


class NautilusMenuTests(unittest.TestCase):
    """Check the same history action for both synchronized and mounted files."""

    def setUp(self):
        self.provider = EXTENSION.Synctrain()

    def test_synchronized_file_history(self):
        """Local synchronization roots receive a native history action."""
        info = SimpleNamespace(get_uri=lambda: "file:///data/docs/report.txt",
                               is_directory=lambda: False)
        with patch.object(EXTENSION, "mounts", return_value=[]), patch.object(
                EXTENSION, "synchronized_folders", return_value=["/data/docs"]):
            items = self.provider.get_file_items([info])
        self.assertEqual(len(items), 1)
        self.assertEqual(items[0].get_property("label"), "Previous Versions…")

    def test_directory_is_not_a_file_history_target(self):
        """A directory never receives a file-version command."""
        info = SimpleNamespace(get_uri=lambda: "file:///data/docs/sub",
                               is_directory=lambda: True)
        with patch.object(EXTENSION, "mounts", return_value=[]), patch.object(
                EXTENSION, "synchronized_folders", return_value=["/data/docs"]):
            self.assertEqual(self.provider.get_file_items([info]), [])

    def test_remote_uri_cannot_name_a_local_synchronized_file(self):
        """A different file-URI host does not qualify for a local root."""
        info = SimpleNamespace(get_uri=lambda: "file://another/data/docs/report.txt",
                               is_directory=lambda: False)
        with patch.object(EXTENSION, "synchronized_folders", return_value=["/data/docs"]):
            self.assertIsNone(EXTENSION.synchronized_path(info))

    def test_root_prefix_requires_directory_boundary(self):
        """A similarly prefixed neighboring directory stays outside the root."""
        info = SimpleNamespace(get_uri=lambda: "file:///data/docs-other/report.txt",
                               is_directory=lambda: False)
        with patch.object(EXTENSION, "synchronized_folders", return_value=["/data/docs"]):
            self.assertIsNone(EXTENSION.synchronized_path(info))

    def test_mounted_file_keeps_existing_actions(self):
        """FUSE files retain Keep and the unified history action."""
        info = SimpleNamespace(get_uri=lambda: "file:///mount/docs/report.txt",
                               is_directory=lambda: False)
        with patch.object(EXTENSION, "mounts", return_value=["/mount"]), patch.object(
                EXTENSION, "xattr", return_value=None):
            labels = [item.get_property("label")
                      for item in self.provider.get_file_items([info])]
        self.assertEqual(labels, ["Keep on This Device", "Previous Versions…"])

    def test_folder_discovery_uses_private_rpc(self):
        """The background lookup decodes daemon roots rather than reading engine data."""
        home = SimpleNamespace(stdout=json.dumps({"socket": "/private/admin.sock"}))
        with patch.object(EXTENSION.subprocess, "run", return_value=home), patch.object(
                EXTENSION.socket, "socket") as factory:
            connection = factory.return_value.__enter__.return_value
            connection.recv.return_value = b'{"result":[{"Path":"/data/docs"}]}\n'
            EXTENSION.refresh_synchronized_folders()
            connection.connect.assert_called_once_with("/private/admin.sock")
            connection.sendall.assert_called_once_with(b'{"method":"engine.folders"}\n')
        self.assertEqual(EXTENSION.synchronized_folders(), ("/data/docs",))


if __name__ == "__main__":
    unittest.main()
