// Copyright (C) 2026 Gordon. SPDX-License-Identifier: MPL-2.0
using Microsoft.UI.Xaml.Controls;
using System.Text.Json.Nodes;

namespace Synctrain_App;

public sealed partial class MainPage
{
    private async Task FolderDetails(string id)
    {
        Detail(() => FolderDetails(id));
        var folders = await Call("desktop.engine.folders");
        var folder = folders?.AsArray().FirstOrDefault(item => S(item, "ID") == id) ?? throw new InvalidOperationException("The folder no longer exists.");
        ShowBody(); Body.Children.Clear(); Commands.PrimaryCommands.Clear(); Heading.Text = S(folder, "Label");
        Command("Back", Symbol.Back, LeaveDetails);
        Command(B(folder, "Paused") ? "Resume" : "Pause", B(folder, "Paused") ? Symbol.Play : Symbol.Pause, () => EngineAction(B(folder, "Paused") ? "resume" : "pause", id));
        Command("Rescan", Symbol.Sync, () => EngineAction("rescan", id));
        var state = new FolderStatus(); state.Show(folder);
        // The state and synchronization progress change in place; anything the commands and sections depend on builds the page again
        static string Shape(JsonNode? value) => string.Join("\t", S(value, "Label"), S(value, "Path"), S(value, "Type"), B(value, "Paused"));
        var shape = Shape(folder);
        Live(async (_, changed) => {
            if (!changed && !state.Syncing) return true;
            var current = (await Call("desktop.engine.folders"))?.AsArray().FirstOrDefault(item => S(item, "ID") == id);
            if (current is null || Shape(current) != shape) return false;
            state.Show(current); return true;
        });
        foreach (var section in catalog["detailDescriptors"]!["folder"]!.AsArray())
        {
            var title = S(section, "title"); Text(title, true);
            switch (S(section, "id"))
            {
                case "overview":
                    Text(S(folder, "Path")); Body.Children.Add(state.Element);
                    Body.Children.Add(ActionButton("Folder settings…", () => EditSettings("basic/folder/" + id)));
                    Body.Children.Add(ActionButton("Browse", () => BrowseSync(id, ""))); break;
                case "synchronization":
                    Body.Children.Add(ActionButton("Synchronization options…", () => EditSettings("folder/" + id)));
                    Body.Children.Add(ActionButton("Reset Synchronization Settings…", () => EditSettings("reset/folder/" + id))); break;
                case "sharing": Body.Children.Add(ActionButton("Manage sharing…", () => ShareFolder(id))); break;
                case "versioning":
                    Body.Children.Add(ActionButton("Versioning settings…", () => EditSettings("versioning/" + id)));
                    Body.Children.Add(ActionButton("Deleted files", () => DeletedFiles(id))); break;
                case "ignorePatterns": Body.Children.Add(ActionButton("Edit ignore patterns…", () => Ignores(id))); break;
                case "advanced":
                    Body.Children.Add(ActionButton("Expert folder settings…", () => EditSettings("advanced/folder/" + id)));
                    Body.Children.Add(ActionButton("Folder errors", () => FolderErrors(id)));
                    if (S(folder, "Type") == "sendonly") Body.Children.Add(ActionButton("Override Remote Changes…", async () => { if (await Confirm("Override remote changes?", "Other devices will match this folder, including deletions.")) await EngineAction("override", id); }));
                    if (S(folder, "Type") == "receiveencrypted") Body.Children.Add(ActionButton("Delete Unexpected Items…", async () => { if (await Confirm("Delete unexpected items?", "This encrypted receive-only folder permanently deletes unexpected local items, including unencrypted files. They cannot be recovered through Synctrain.")) await EngineAction("revert", id); }));
                    else if (S(folder, "Type") == "receiveonly") Body.Children.Add(ActionButton("Revert Local Changes…", async () => { if (await Confirm("Revert local changes?", "Local changes are replaced with synchronized versions.")) await EngineAction("revert", id); }));
                    break;
            }
        }
    }

    private async Task DeviceDetails(string id)
    {
        Detail(() => DeviceDetails(id));
        var devices = await Call("desktop.engine.devices");
        var device = devices?.AsArray().FirstOrDefault(item => S(item, "ID") == id) ?? throw new InvalidOperationException("The device no longer exists.");
        ShowBody(); Body.Children.Clear(); Commands.PrimaryCommands.Clear(); Heading.Text = S(device, "Name");
        Command("Back", Symbol.Back, LeaveDetails);
        foreach (var section in catalog["detailDescriptors"]!["device"]!.AsArray())
        {
            var title = S(section, "title"); Text(title, true);
            switch (S(section, "id"))
            {
                case "overview": Text(id + "\n" + (B(device, "Connected") ? "Connected" : "Disconnected")); Body.Children.Add(ActionButton("Device settings…", () => EditSettings("basic/device/" + id))); break;
                case "sharedFolders":
                    foreach (var folder in (await Call("desktop.engine.folders"))?.AsArray() ?? [])
                    {
                        if (folder?["Devices"]?.AsArray().Any(value => value?.ToString() == id) == true)
                        {
                            var folderID = S(folder, "ID");
                            Body.Children.Add(ActionButton(S(folder, "Label"), () => FolderDetails(folderID)));
                        }
                    }
                    break;
                case "connections": Body.Children.Add(ActionButton("Connection settings…", () => EditSettings("device/" + id))); break;
                case "advanced": Body.Children.Add(ActionButton("Expert device settings…", () => EditSettings("advanced/device/" + id))); break;
            }
        }
    }
}
