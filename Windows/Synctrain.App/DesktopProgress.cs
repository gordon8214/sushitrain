// Copyright (C) 2026 Gordon. SPDX-License-Identifier: MPL-2.0
using Microsoft.UI.Windowing;
using Microsoft.UI.Xaml;
using Microsoft.UI.Xaml.Automation;
using Microsoft.UI.Xaml.Automation.Peers;
using Microsoft.UI.Xaml.Controls;
using System.Globalization;
using System.Runtime.InteropServices;
using System.Text;
using System.Text.Json.Nodes;

namespace Synctrain_App;

// Live sync progress: pages that show it update their rows in place every second while they are on screen, instead of
// being built again on each engine event (which would lose scrolling, focus and selection).
public sealed partial class MainPage
{
    // Called with the latest status and whether its revision moved; false means the page must be built again
    private readonly List<Func<JsonNode?, bool, Task<bool>>> liveUpdates = [];
    private int liveGeneration = -1;
    private readonly DispatcherTimer liveTimer = new() { Interval = TimeSpan.FromSeconds(1) };
    private bool LiveActive => liveGeneration == generation && liveUpdates.Count > 0;

    // Updates part of the page being built in place while it stays on screen
    private void Live(Func<JsonNode?, bool, Task<bool>> update)
    {
        var owner = renderOperation.Value?.Generation ?? generation;
        if (owner != generation) return;
        if (liveGeneration != owner) { liveUpdates.Clear(); liveGeneration = owner; }
        liveUpdates.Add(update);
        if (IsLoaded && !liveTimer.IsEnabled) liveTimer.Start();
    }

    // Requests made while updating in place give up sooner than a page being built
    private const int LivePollSeconds = 5;
    private readonly AsyncLocal<bool> livePoll = new();
    private bool ViewChanged => renderOperation.Value?.Generation != generation;

    private async Task LiveTick()
    {
        if (!LiveActive) { liveTimer.Stop(); liveUpdates.Clear(); return; }
        // Nothing is asked of the host while the window is hidden or minimized, or while another request runs
        if (activeOperations != 0 || editorToken is not null || status is null || !WindowShown()) return;
        await Run(async () => {
            JsonNode? next = null;
            var current = true;
            livePoll.Value = true;
            try {
                next = await Call("status");
                if (S(next, "mode") != S(status, "mode")) current = false;
                else {
                    var changed = S(next, "revision") != S(status, "revision");
                    if (changed) await UpdateFolderShortcuts(S(next, "mode"));
                    foreach (var update in liveUpdates.ToList()) if (!(current = await update(next, changed))) break;
                }
            }
            catch (OperationCanceledException) when (ViewChanged) { throw; }
            // A failed update builds the page once, with the page's own error handling, instead of failing every second
            catch (Exception) { current = false; }
            finally { livePoll.Value = false; }
            if (!current) { await Refresh(); return; }
            // The page shows this revision now, so the slower timer doesn't build it again
            if (!ViewChanged) status = next;
        }, clearError: false);
    }

    // The folder shortcuts in the navigation follow changes too; the navigation is built again only when they differ
    private async Task UpdateFolderShortcuts(string mode)
    {
        string Shortcuts() => string.Join("\n", folderShortcuts.Select(folder => folder.ID + "\t" + folder.Name));
        var before = Shortcuts();
        await LoadFolderShortcuts(mode);
        if (Shortcuts() != before) BuildNavigation();
    }

    private bool WindowShown()
    {
        try {
            if (XamlRoot is not { IsHostVisible: true } root) return false;
            var window = AppWindow.GetFromWindowId(root.ContentIslandEnvironment.AppWindowId);
            return window is null || window.IsVisible && window.Presenter is not OverlappedPresenter { State: OverlappedPresenterState.Minimized };
        } catch (Exception) { return false; }
    }

    [DllImport("shlwapi.dll", CharSet = CharSet.Unicode)]
    private static extern int StrFormatByteSizeEx(ulong size, int flags, StringBuilder buffer, uint length);

    // A size the way File Explorer shows it, such as "1.20 MB"
    private static string Bytes(long value)
    {
        var buffer = new StringBuilder(64);
        // 1 is SFBS_FLAGS_ROUND_TO_NEAREST_DISPLAYED_DIGIT
        return StrFormatByteSizeEx((ulong)Math.Max(0, value), 1, buffer, 64) >= 0 ? buffer.ToString() : value.ToString("N0") + " bytes";
    }
    private static string Speed(long bytesPerSecond) => Bytes(bytesPerSecond) + "/s";
    // A whole percentage that reaches 100% only when it's done
    private static string Percent(double fraction) => (Math.Floor(Math.Clamp(fraction, 0, 1) * 100 + 1e-9) / 100).ToString("P0", CultureInfo.CurrentCulture);
    // h:mm:ss
    private static string TimeLeft(long seconds) => $"{seconds / 3600}:{seconds % 3600 / 60:00}:{seconds % 60:00}";
    private static double Fraction(JsonNode? value, string key) => value?[key]?.GetValue<double>() ?? 0;

    private static string Receiving(long files) => files == 1 ? "Receiving 1 file…" : $"Receiving {files:N0} files…";
    private static string Sending(long files) => files == 1 ? "Sending 1 file…" : $"Sending {files:N0} files…";

    // How far all files going one way have come: "45% · 12.3 MB of 40.0 MB · 1.20 MB/s · 0:00:23 remaining"
    private static string TotalsDetail(JsonNode? totals)
    {
        var parts = new List<string> { Percent(Fraction(totals, "Percentage")), Bytes(N(totals, "BytesDone")) + " of " + Bytes(N(totals, "BytesTotal")) };
        var speed = N(totals, "BytesPerSecond");
        if (speed > 0) {
            parts.Add(Speed(speed));
            var remaining = N(totals, "SecondsRemaining");
            if (remaining >= 0) parts.Add(TimeLeft(remaining) + " remaining");
        } else parts.Add("(Transfer speed unknown)");
        return string.Join(" · ", parts);
    }

    // How far one file has come: "45% · 12.3 MB of 40.0 MB · 1.20 MB/s"
    private static string FileDetail(JsonNode? file, bool remaining = false)
    {
        var detail = Percent(Fraction(file, "Percentage")) + " · " + Bytes(N(file, "BytesDone")) + " of " + Bytes(N(file, "BytesTotal"));
        var speed = N(file, "BytesPerSecond");
        if (speed <= 0) return detail;
        detail += " · " + Speed(speed);
        if (remaining) detail += " · " + TimeLeft((Math.Max(0, N(file, "BytesTotal") - N(file, "BytesDone")) + speed - 1) / speed) + " remaining";
        return detail;
    }

    // The files being received, by folder (the host sorts them by folder label, then path)
    private static List<ProgressGroup> DownloadGroups(JsonNode? transfers) => Groups(transfers?["Downloads"] as JsonArray,
        file => S(file, "Folder"), file => S(file, "FolderLabel"), file => S(file, "Path"), file => S(file, "Path"));

    // The files being sent, by device (sorted by device name, folder label, then path)
    private static List<ProgressGroup> UploadGroups(JsonNode? transfers) => Groups(transfers?["Uploads"] as JsonArray,
        file => S(file, "Device"), file => S(file, "DeviceName"), file => S(file, "Folder") + "\n" + S(file, "Path"), file => S(file, "FolderLabel") + ": " + S(file, "Path"));

    private static List<ProgressGroup> Groups(JsonArray? files, Func<JsonNode?, string> key, Func<JsonNode?, string> title, Func<JsonNode?, string> rowKey, Func<JsonNode?, string> rowTitle)
    {
        var groups = new List<ProgressGroup>();
        var index = new Dictionary<string, ProgressGroup>();
        foreach (var file in files ?? []) {
            if (!index.TryGetValue(key(file), out var group)) { group = new(key(file), title(file), []); index[group.Key] = group; groups.Add(group); }
            group.Items.Add(new(rowKey(file), rowTitle(file), FileDetail(file), Fraction(file, "Percentage")));
        }
        return groups;
    }

    // Files-mode downloads from the gateway, at most this many rows, in one group without a title
    private const int MaxFileTransfers = 100;
    private static List<ProgressGroup> FileTransferGroups(JsonNode? transfers)
    {
        var items = new List<ProgressItem>();
        foreach (var transfer in (transfers as JsonArray)?.Take(MaxFileTransfers) ?? []) {
            var id = S(transfer, "id"); var path = S(transfer, "path"); var state = S(transfer, "state");
            var blocks = N(transfer, "totalBlocks"); var size = N(transfer, "totalBytes");
            var detail = "Download · " + state;
            if (blocks > 0 && size > 0) detail += " · " + Bytes(N(transfer, "verifiedBytes")) + " of " + Bytes(size);
            else if (size > 0) detail += " · " + Bytes(size);
            // The share of blocks is known once the gateway's manifest of the content is in
            double? fraction = blocks > 0 ? (double)N(transfer, "verifiedBlocks") / blocks : null;
            items.Add(new(id, path.Length > 0 ? path.Replace('/', Path.DirectorySeparatorChar) : id, detail, fraction, fraction is null && state == "downloading", S(transfer, "error")));
        }
        return items.Count == 0 ? [] : [new("", "", items)];
    }
    private static string MoreTransfers(JsonNode? transfers)
    {
        var more = ((transfers as JsonArray)?.Count ?? 0) - MaxFileTransfers;
        return more <= 0 ? "" : more == 1 ? "1 more transfer" : $"{more:N0} more transfers";
    }

    // Styles from App.xaml whose colors are theme resources, so they follow light, dark and high-contrast changes
    private static Style? TextStyle(string key)
    {
        try { return Application.Current.Resources[key] as Style; }
        catch (Exception) { return null; }
    }
    private static TextBlock LiveText(bool secondary = false)
    {
        var text = new TextBlock { TextWrapping = TextWrapping.Wrap, FontSize = 14 };
        if (secondary && TextStyle("SecondaryProgressText") is { } style) text.Style = style;
        return text;
    }
    private static TextBlock ErrorText()
    {
        var text = LiveText();
        if (TextStyle("CriticalProgressText") is { } style) text.Style = style;
        return text;
    }

    private sealed record ProgressItem(string Key, string Title, string Detail, double? Fraction, bool Indeterminate = false, string Error = "");
    private sealed record ProgressGroup(string Key, string Title, List<ProgressItem> Items);

    // A label, a progress bar and a line of detail, all changed in place
    private sealed class ProgressRow
    {
        public readonly StackPanel Element = new() { Spacing = 4 };
        private readonly TextBlock title = LiveText();
        private readonly ProgressBar bar = new() { Minimum = 0, Maximum = 100 };
        private readonly TextBlock detail = LiveText(secondary: true);
        private readonly TextBlock problem = ErrorText();

        public ProgressRow()
        {
            foreach (var child in new UIElement[] { title, bar, detail, problem }) Element.Children.Add(child);
        }

        public void Show(string label, string details, double? fraction, bool indeterminate = false, string error = "")
        {
            if (title.Text != label) { title.Text = label; AutomationProperties.SetName(bar, label); }
            title.Visibility = label.Length > 0 ? Visibility.Visible : Visibility.Collapsed;
            bar.Visibility = fraction is not null || indeterminate ? Visibility.Visible : Visibility.Collapsed;
            bar.IsIndeterminate = indeterminate;
            if (fraction is double value) bar.Value = Math.Clamp(value, 0, 1) * 100;
            detail.Text = details;
            problem.Text = error;
            problem.Visibility = error.Length > 0 ? Visibility.Visible : Visibility.Collapsed;
        }
    }

    // Sections of progress rows. A row keeps its element for as long as its file stays, so the rest of the page doesn't move.
    private sealed class ProgressList
    {
        public readonly StackPanel Element = new() { Spacing = 12 };
        private readonly StackPanel empty = new() { Spacing = 4 };
        private readonly Dictionary<string, TextBlock> headers = [];
        private readonly Dictionary<string, ProgressRow> rows = [];
        private readonly TextBlock more = LiveText(secondary: true);

        public ProgressList(string emptyTitle, string emptyDetail = "")
        {
            empty.Children.Add(new TextBlock { Text = emptyTitle, FontSize = 14, TextWrapping = TextWrapping.Wrap });
            if (emptyDetail.Length > 0) { var detail = LiveText(secondary: true); detail.Text = emptyDetail; empty.Children.Add(detail); }
        }

        public int Count => rows.Count;

        // moreText follows the rows when some are left out
        public void Show(List<ProgressGroup> groups, string moreText = "")
        {
            var wanted = new List<UIElement>();
            var usedHeaders = new HashSet<string>(); var usedRows = new HashSet<string>();
            foreach (var group in groups) {
                if (group.Title.Length > 0) {
                    if (!headers.TryGetValue(group.Key, out var header)) {
                        header = new TextBlock { FontSize = 16, FontWeight = Microsoft.UI.Text.FontWeights.SemiBold, TextWrapping = TextWrapping.Wrap, Margin = new Thickness(0, 4, 0, 0) };
                        AutomationProperties.SetHeadingLevel(header, AutomationHeadingLevel.Level3);
                        headers[group.Key] = header;
                    }
                    header.Text = group.Title; wanted.Add(header); usedHeaders.Add(group.Key);
                }
                foreach (var item in group.Items) {
                    var key = group.Key + "\0" + item.Key;
                    if (!rows.TryGetValue(key, out var row)) rows[key] = row = new ProgressRow();
                    row.Show(item.Title, item.Detail, item.Fraction, item.Indeterminate, item.Error);
                    wanted.Add(row.Element); usedRows.Add(key);
                }
            }
            if (wanted.Count == 0) wanted.Add(empty);
            more.Text = moreText;
            if (moreText.Length > 0) wanted.Add(more);
            foreach (var key in headers.Keys.Except(usedHeaders).ToList()) headers.Remove(key);
            foreach (var key in rows.Keys.Except(usedRows).ToList()) rows.Remove(key);
            // Only a file that started or finished changes the children
            if (!Element.Children.SequenceEqual(wanted)) { Element.Children.Clear(); foreach (var element in wanted) Element.Children.Add(element); }
        }
    }

    // What the engine receives and sends overall (Overview and Activity), or whether it's connected when it does neither
    private sealed class TransferSummary
    {
        public readonly StackPanel Element = new() { Spacing = 8 };
        private readonly ProgressRow receiving = new(), sending = new();
        private readonly FrameworkElement receivingBlock, sendingBlock;
        private readonly TextBlock connection = LiveText();
        private JsonNode? transfers;
        private int connected = -1, devices;

        // open shows the files in Activity; without it there are no buttons and no connection line
        public TransferSummary(Func<Task>? open)
        {
            receivingBlock = Block(receiving, open, "Show the files being received");
            sendingBlock = Block(sending, open, "Show the files being sent");
            Element.Children.Add(receivingBlock); Element.Children.Add(sendingBlock);
            if (open is not null) Element.Children.Add(connection);
            Render();
        }

        // Whether anything was moving at the last update
        public bool Active => transfers?["Download"] is JsonObject || transfers?["Upload"] is JsonObject;
        public void Show(JsonNode? value) { transfers = value; Render(); }
        public void Peers(int connectedDevices, int allDevices) { connected = connectedDevices; devices = allDevices; Render(); }

        private static FrameworkElement Block(ProgressRow row, Func<Task>? open, string help)
        {
            if (open is null) return row.Element;
            var button = new Button { Content = row.Element, HorizontalAlignment = HorizontalAlignment.Stretch, HorizontalContentAlignment = HorizontalAlignment.Stretch, Padding = new Thickness(12) };
            AutomationProperties.SetHelpText(button, help);
            button.Click += async (_, _) => await open();
            return button;
        }

        private void Render()
        {
            // Like the Mac: with no device connected there is nothing to transfer
            var online = connected != 0;
            var download = online ? transfers?["Download"] as JsonObject : null;
            var upload = online ? transfers?["Upload"] as JsonObject : null;
            Show(receivingBlock, receiving, download, Receiving(N(download, "FilesTotal")));
            Show(sendingBlock, sending, upload, Sending(N(upload, "FilesTotal")));
            connection.Visibility = download is null && upload is null && connected >= 0 ? Visibility.Visible : Visibility.Collapsed;
            connection.Text = (connected > 0 ? "Connected" : "Not connected") + $" · {connected}/{devices} " + (devices == 1 ? "device" : "devices");
        }

        private static void Show(FrameworkElement block, ProgressRow row, JsonObject? totals, string label)
        {
            block.Visibility = totals is null ? Visibility.Collapsed : Visibility.Visible;
            if (totals is null) return;
            row.Show(label, TotalsDetail(totals), Fraction(totals, "Percentage"));
            if (block is Button && AutomationProperties.GetName(block) != label) AutomationProperties.SetName(block, label);
        }
    }

    // A folder's state, or while it's syncing, how much of what the other devices have it has here
    private sealed class FolderStatus
    {
        public readonly StackPanel Element = new() { Spacing = 4 };
        private readonly TextBlock state = LiveText();
        private readonly ProgressBar bar = new() { Minimum = 0, Maximum = 100 };
        private readonly TextBlock remaining = LiveText(secondary: true);
        private readonly TextBlock problem = ErrorText();

        public FolderStatus()
        {
            foreach (var child in new UIElement[] { state, bar, remaining, problem }) Element.Children.Add(child);
        }

        public bool Syncing { get; private set; }

        public void Show(JsonNode? folder)
        {
            var sync = folder?["Sync"] as JsonObject;
            var global = N(sync, "GlobalBytes");
            Syncing = S(folder, "State") == "syncing" && global > 0;
            state.Text = Syncing ? "Synchronizing…" : S(folder, "State");
            bar.Visibility = remaining.Visibility = Syncing ? Visibility.Visible : Visibility.Collapsed;
            if (Syncing) {
                bar.Value = Math.Clamp(Fraction(sync, "Fraction"), 0, 1) * 100;
                AutomationProperties.SetName(bar, "Synchronizing " + (S(folder, "Label") is { Length: > 0 } label ? label : S(folder, "ID")));
                remaining.Text = Percent(Fraction(sync, "Fraction")) + " · " + Bytes(Math.Max(0, global - N(sync, "LocalBytes"))) + " remaining";
            }
            problem.Text = S(folder, "Error");
            problem.Visibility = problem.Text.Length > 0 ? Visibility.Visible : Visibility.Collapsed;
        }
    }

    // A file being received, or one that isn't here yet (Synchronize-mode file information)
    private sealed class FileProgress
    {
        public readonly StackPanel Element = new() { Spacing = 4 };
        private readonly ProgressRow row = new();

        public FileProgress() { Element.Children.Add(row.Element); }

        public void Downloading(JsonNode? file) { Element.Visibility = Visibility.Visible; row.Show("Downloading file…", FileDetail(file, remaining: true), Fraction(file, "Percentage")); }
        public void Waiting() { Element.Visibility = Visibility.Visible; row.Show("Waiting to synchronize…", "", null); }
        public void Problem(string message) { Element.Visibility = Visibility.Visible; row.Show("", "", null, error: message); }
        public void Hide() => Element.Visibility = Visibility.Collapsed;
    }
}
