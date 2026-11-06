// Copyright (C) 2026 Gordon. SPDX-License-Identifier: MPL-2.0
using Microsoft.UI.Xaml;
using Microsoft.UI.Xaml.Controls;
using System.Text.Json.Nodes;
using Windows.ApplicationModel;

namespace Synctrain_App;

public sealed partial class MainPage
{
    private readonly JsonObject catalog = JsonNode.Parse(File.ReadAllText(Path.Combine(AppContext.BaseDirectory, "catalog.json")))!.AsObject();
    private bool buildingNavigation;
    private bool savingEditor;
    private bool loadingEditor;
    private bool closeRequested;
    private Func<bool>? editorChanged;
    private readonly List<(string ID, string Name)> folderShortcuts = [];

    private string CatalogTitle(string route)
    {
        var settings = route.StartsWith("settings:", StringComparison.Ordinal);
        var id = settings ? route[9..] : route;
        return catalog[settings ? "settings" : "destinations"]!.AsArray().FirstOrDefault(item => S(item, "id") == id)?["title"]?.ToString() ?? "Overview";
    }

    private void BuildNavigation()
    {
        buildingNavigation = true;
        try
        {
            var settings = page.StartsWith("settings:", StringComparison.Ordinal);
            SettingsSearchBox.Visibility = settings ? Visibility.Visible : Visibility.Collapsed;
            var oldFiles = Navigation.MenuItems.OfType<NavigationViewItem>().FirstOrDefault(item => item.Tag?.ToString() == "files");
            var expanded = oldFiles?.IsExpanded ?? true;
            Navigation.MenuItems.Clear();
            Navigation.IsSettingsVisible = !settings;
            if (settings) Navigation.MenuItems.Add(new NavigationViewItem { Content = "Back to App", Tag = appRoute, Icon = new SymbolIcon(Symbol.Back) });
            foreach (var item in catalog[settings ? "settings" : "destinations"]!.AsArray())
            {
                var id = S(item, "id");
                if (id == "settings") continue;
                var route = settings ? "settings:" + id : id;
                var nav = new NavigationViewItem { Content = S(item, "title"), Tag = route };
                if (id == "files")
                {
                    nav.IsExpanded = expanded;
                    foreach (var folder in folderShortcuts) nav.MenuItems.Add(new NavigationViewItem { Content = folder.Name, Tag = "folder:" + folder.ID, Icon = new SymbolIcon(Symbol.Folder) });
                }
                Navigation.MenuItems.Add(nav);
                if (route == page) Navigation.SelectedItem = nav;
            }
        }
        finally { buildingNavigation = false; }
    }

    private async Task LoadFolderShortcuts(string mode)
    {
        var values = await Call(mode == "files" ? "files.children" : "engine.folders", mode == "files" ? new { Parent = 0 } : null);
        folderShortcuts.Clear();
        foreach (var item in values?.AsArray() ?? [])
        {
            var id = mode == "files" ? S(item, "Item") : S(item, "ID");
            var name = mode == "files" ? S(item, "Name") : S(item, "Label");
            folderShortcuts.Add((id, string.IsNullOrEmpty(name) ? id : name));
        }
        folderShortcuts.Sort((first, second) => { var order=StringComparer.OrdinalIgnoreCase.Compare(first.Name,second.Name);return order!=0?order:StringComparer.Ordinal.Compare(first.ID,second.ID); });
    }

    private async Task<bool> MayLeaveEditor()
    {
        if (savingEditor) return false;
        return editorChanged?.Invoke() != true || await Confirm("Discard unsaved changes?", "The changes on this page have not been saved.");
    }

    public async Task<bool> CanClose()
    {
        if (closeRequested) return false;
        closeRequested = true;
        try {
            var accepted = false;
            await Run(async () => { if (!await MayLeaveEditor()) return; if (editorToken is not null) { await CloseEditor(); await Refresh(); } accepted = true; });
            return accepted;
        }
        finally { closeRequested = false; }
    }

    private async Task SettingsCategory(string id, string mode)
    {
        var category = catalog["settings"]!.AsArray().First(item => S(item, "id") == id)!;
        foreach (var section in category["sections"]!.AsArray())
        {
            var title = S(section, "title");
            var capability = S(section, "capability");
            Text(title, true);
            if (capability != "always" && capability != mode)
            {
                Text("Available in " + capability + " mode. Change the operating mode in General.");
                continue;
            }
            try { await CatalogSetting(S(section, "action"), title, mode); }
            catch (OperationCanceledException) { throw; }
            catch (Exception error) { Text("This section is unavailable: " + error.Message); }
        }
    }

    private async Task CatalogSetting(string action, string title, string mode)
    {
        switch (action)
        {
            case "name":
                Body.Children.Add(ActionButton("Change device name…", async () => { var name = await Input("Device Name", "Name shown to other devices"); if (name is not null) { await Call("desktop.engine.action", new { Action = "name", Name = name }); await Refresh(); } }));
                break;
            case "mode":
                Text("Current mode: " + (mode == "files" ? "Files" : mode=="synchronize" ? "Synchronize" : "Unavailable"));
                if(statusFailure is not null) { Text("Reconnect to the background service to change modes."); break; }
                Body.Children.Add(ActionButton(mode == "files" ? "Switch to Synchronize…" : "Switch to Files…", async () => { if (await Confirm("Switch modes?", "Pending changes must reach their gateway first. Existing synchronized folders stay on disk.")) await SetMode(mode == "files" ? "synchronize" : "files"); }));
                break;
            case "startup": await StartupSetting(); break;
            case "presence": Text("Closing the window leaves Synctrain running in the notification area. Use Quit and Stop to stop the background agent."); break;
            case "previews":
                Text("Browse files with native image and media previews. Use the file list’s filter to find items.");
                Body.Children.Add(ActionButton("Browse Files", async () => { page = "files"; await Refresh(); })); break;
            case "storage":
                var storage = await Call("files.storage");
                Text($"Download cache: {N(storage, "used") / 1048576.0:N1} MiB of {N(storage, "budget") / 1048576.0:N0} MiB");
                Text("Kept files and pending edits are retained independently of the disposable cache.");
                Body.Children.Add(ActionButton("Change cache budget…", async () => { var value = await Input("Cache Budget", "MiB of unpinned files to keep", (N(storage, "budget") / 1048576).ToString()); if (value is not null) { await Call("files.budget", new { Budget = checked(long.Parse(value) * 1048576) }); await Refresh(); } })); break;
            case "locations": Text(S(status, "root")); Body.Children.Add(ActionButton("Choose Explorer location…", ChooseLocation)); break;
            case "fileManager": Text("Windows Cloud Files provides on-demand access, offline availability, thumbnails, and Previous Versions in File Explorer."); break;
            case "menuAccess": Text("Open Synctrain from the notification area. Folder actions are available in File Explorer."); break;
            case "mcp": await AssistantSetting(); break;
            case "gui": await EngineAccessSetting(); break;
            case "policy": Text("Automatic pausing is not currently supported by the Windows backend. Pause and Resume are available on Overview."); break;
            case "resources": Body.Children.Add(ActionButton(title, () => EditSettings("engine"))); break;
            case "configuration":
                if(S(status,"configuration").Length>0)Text(S(status,"configuration"));
                Body.Children.Add(ActionButton("Export configuration…", async () => { var path = await PickSave("config.xml"); if (path is not null) await Call("desktop.configuration.export", new { Path = path }); }));
                Body.Children.Add(ActionButton("Import existing configuration…", ImportHome)); break;
            case "advanced":
                Body.Children.Add(ActionButton("Expert engine options", () => EditSettings("advanced/options")));
                Body.Children.Add(ActionButton("LDAP authentication", () => EditSettings("advanced/ldap"))); break;
            case "logs": Body.Children.Add(ActionButton(title, Diagnostics)); break;
            case "versions":
                if (Synctrain_App.Services.HostClient.IsolatedTest) Text("Synctrain for Windows · isolated Debug window");
                else { var version = Package.Current.Id.Version; Text($"Synctrain for Windows {version.Major}.{version.Minor}.{version.Build}.{version.Revision}"); }
                Text("ARM64 and x64 · Windows Cloud Files");
                if(mode=="synchronize")Text("Syncthing "+S(await Call("desktop.engine.status"),"engineVersion"));
                else Text("The engine runs on the gateway in Files mode.");
                break;
            case "support": Body.Children.Add(ActionButton("Support and source", () => OpenPath("https://github.com/gordon8214/syncthing-tools"))); break;
            case "licenses": Body.Children.Add(ActionButton("Open-source notices", () => OpenPath(Path.Combine(AppContext.BaseDirectory, "GoNotices.html")))); break;
            default: Body.Children.Add(ActionButton(title, () => EditSettings(action))); break;
        }
    }

    private async Task EngineAccessSetting()
    {
        var engine = await Call("desktop.engine.status");
        var confirmed = B(engine, "guiEnabled");
        var toggle = new ToggleSwitch { Header = "Enable Web GUI and REST API", IsOn = confirmed };
        toggle.Toggled += async (_, _) => {
            var desired = toggle.IsOn;
            if (desired == confirmed) return;
            toggle.IsOn = confirmed; toggle.IsEnabled = false;
            try { await Run(async () => {
                await Call("desktop.engine.action", new { Action = "gui", Enabled = desired });
                confirmed = desired; toggle.IsOn = confirmed;
                if (await Confirm("Restart required", "Restart the engine to apply the access change?")) await EngineAction("restart");
            }); } finally { toggle.IsEnabled = true; }
        };
        Body.Children.Add(toggle);
        Body.Children.Add(ActionButton("Authentication and connection settings…", () => EditSettings("gui")));
    }

    private async Task StartupSetting()
    {
        if (Synctrain_App.Services.HostClient.IsolatedTest) { Text("Login startup is disabled for this isolated test window."); return; }
        var startup = await StartupTask.GetAsync("SynctrainStartup");
        var confirmed = startup.State == StartupTaskState.Enabled;
        var toggle = new ToggleSwitch { Header = "Launch at login", IsOn = confirmed };
        toggle.Toggled += async (_, _) => {
            var desired = toggle.IsOn;
            if (desired == confirmed) return;
            toggle.IsOn = confirmed; toggle.IsEnabled = false;
            try { await Run(async () => {
                if (desired) { if (await startup.RequestEnableAsync() != StartupTaskState.Enabled) throw new InvalidOperationException("Enable Synctrain in Windows Settings → Apps → Startup."); }
                else startup.Disable();
                confirmed = desired; toggle.IsOn = confirmed;
            }); } finally { toggle.IsEnabled = true; }
        };
        Body.Children.Add(toggle);
    }

    private async Task AssistantSetting()
    {
        var settings = await Call("mcp.settings");
        var confirmed = B(settings, "enabled");
        var toggle = new ToggleSwitch { Header = "Enable MCP", IsOn = confirmed };
        toggle.Toggled += async (_, _) => {
            var desired = toggle.IsOn;
            if (desired == confirmed) return;
            toggle.IsOn = confirmed; toggle.IsEnabled = false;
            try { await Run(async () => { var updated = await Call("mcp.settings", new { enabled = desired }); confirmed = B(updated, "enabled"); toggle.IsOn = confirmed; }); }
            finally { toggle.IsEnabled = true; }
        };
        Body.Children.Add(toggle);
        Text("Connected assistants have full management access, including changing settings and deleting accessible files. Changes can propagate to other devices.");
        Body.Children.Add(new TextBox { Text = "{\"mcpServers\":{\"synctrain\":{\"command\":\"synctrain-mcp.exe\"}}}", IsReadOnly = true, TextWrapping = TextWrapping.Wrap });
    }

    private async Task HistoryPage()
    {
        Text("Previous Versions", true);
        Body.Children.Add(ActionButton("Choose a File", async () => { page = "files"; await Refresh(); }));
        Text("Choose Previous Versions from a file’s actions. Deleted files are available from their synchronized folder.");
        Body.Children.Add(ActionButton("Manage History Sources", HistorySources));
    }

    private async Task ActivityPage(string mode)
    {
        Text("Transfers", true);
        if (mode == "files")
        {
            Text("Pending Changes", true); await Pending();
            Text("Errors", true);
            foreach (var issue in status?["projectionIssues"]?.AsArray() ?? []) Text(S(issue, "Name") + ": " + S(issue, "Reason"));

        }
        else
        {
            var stats = await Call("engine.stats");
            foreach (var direction in new[] { "download", "upload" }) if (stats?[direction] is JsonNode progress) Text(direction + ": " + S(progress, "FilesTotal") + " files");
            Body.Children.Add(ActionButton("Folder Errors", async () => { page = "folders"; await Refresh(); }));
            Text("Activity Log", true); Body.Children.Add(ActionButton("Activity Log", ShowLogs));
        }
        Text("Recovery", true); await RecoveredCaptures();
    }
}
