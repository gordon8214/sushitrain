// Copyright (C) 2026 Gordon
// SPDX-License-Identifier: MPL-2.0
using Microsoft.UI.Xaml;
using Microsoft.UI.Xaml.Controls;
using Synctrain_App.Services;
using System.Diagnostics;
using System.Text.Json.Nodes;
using Windows.ApplicationModel;
using Windows.Media.Core;

namespace Synctrain_App;

public sealed partial class MainPage : Page
{
    public event Action<string>? ModeChanged;
    private readonly HostClient host = new();
    private string page = "overview";
    private JsonNode? status;
    private int generation;
    private readonly Stack<long> parents = new();
    private long parent;
    private string? editorToken;
    private List<FileRow> fileRows = [];
    private string? syncFolder;
    private string syncPath="";
    private int activeOperations;
    private Func<Task>? detailRoute;
    private string detailMode="";
    private readonly AsyncLocal<int> renderGeneration=new();
    private string fileFilterScope="";
    private readonly DispatcherTimer updates=new() { Interval=TimeSpan.FromSeconds(2) };

    public MainPage()
    {
        InitializeComponent();
        updates.Tick+=async(_,_)=>{
            if(activeOperations!=0 || editorToken is not null || status is null) return;
            await Run(async()=>{ var next=await Call("status"); if(S(next,"revision")!=S(status,"revision") || S(next,"mode")!=S(status,"mode")) await Refresh(); },clearError:false);
        };
        Loaded+=(_,_)=>updates.Start(); Unloaded+=(_,_)=>updates.Stop();
        Loaded += async (_, _) => await Run(async () => { await host.EnsureRunning(); Navigation.SelectedItem = Navigation.MenuItems[0]; });
    }

    public void ShowError(string message) { Error.Message = message; Error.IsOpen = true; }
    public async Task PairLink(string link)
    {
        await Run(async () => {
            await host.EnsureRunning();
            var state = await Call("status");
            var uri=new Uri(link);
            if(uri.Scheme=="synctrain" && uri.Host=="versions") {
                var query=new Windows.Foundation.WwwFormUrlDecoder(uri.Query);
                if(S(state,"mode")!="files" || query.GetFirstValueByName("client")!=S(state,"clientID")) throw new InvalidOperationException("This file belongs to a different Files location.");
                var item=long.Parse(query.GetFirstValueByName("item"));if(item<=0) throw new InvalidOperationException("Invalid file identity.");
                status=state;await Versions(item);return;
            }
            if (S(state,"mode") != "files") { ShowError("Switch to Files mode to pair with a gateway."); return; }
            var confirmed = await Input("Pair with a gateway", "Pairing link", link);
            if (confirmed is not null) await Call("files.pair", new { Link=confirmed },600);
            page="gateway";await Refresh();
        });
    }
    private static string S(JsonNode? value, string key) => value?[key]?.ToString() ?? "";
    private static long N(JsonNode? value, string key) => value?[key]?.GetValue<long>() ?? 0;
    private static bool B(JsonNode? value, string key) => value?[key]?.GetValue<bool>() ?? false;
    private async Task<JsonNode?> Call(string method,object? parameters=null,int seconds=60)
    {
        var result=await host.Call(method,parameters,seconds);
        if(renderGeneration.Value!=generation) throw new OperationCanceledException("The view changed while its request was running.");
        return result;
    }
    private async Task Run(Func<Task> action,bool clearError=true)
    {
        renderGeneration.Value=generation;
        activeOperations++;
        try { if(clearError)Error.IsOpen = false; await action(); }
        catch (Exception error) { if(renderGeneration.Value==generation){Error.Message = error.Message; Error.IsOpen = true;} }
        finally { activeOperations--; }
    }
    private async void Navigate(NavigationView sender, NavigationViewSelectionChangedEventArgs args)
    {
        ++generation;
        await Run(async () => { await CloseEditor(); detailRoute=null; page = args.IsSettingsSelected ? "settings" : (args.SelectedItem as NavigationViewItem)?.Tag?.ToString() ?? "overview"; await Refresh(); });
    }
    private Button ActionButton(string label, Func<Task> action)
    {
        var button = new Button { Content = label };
        button.Click += async (_, _) => await Run(action); return button;
    }
    private void Command(string label, Symbol symbol, Func<Task> action)
    {
        var button = new AppBarButton { Label = label, Icon = new SymbolIcon(symbol) };
        button.Click += async (_, _) => await Run(action); Commands.PrimaryCommands.Add(button);
    }
    private void Text(string text, bool title = false) => Body.Children.Add(new TextBlock { Text = text, TextWrapping = TextWrapping.Wrap, FontSize = title ? 20 : 14, IsTextSelectionEnabled = true });
    private void Row(string title, string detail, params Button[] buttons)
    {
        var row = new StackPanel { Spacing = 8, Margin = new Thickness(0, 4, 0, 12) };
        row.Children.Add(new TextBlock { Text = title, FontSize = 18, TextWrapping = TextWrapping.Wrap });
        row.Children.Add(new TextBlock { Text = detail, TextWrapping = TextWrapping.Wrap, IsTextSelectionEnabled = true });
        var actions = new StackPanel { Orientation = Orientation.Horizontal, Spacing = 8 };
        foreach (var button in buttons.Take(2)) actions.Children.Add(button);
        if(buttons.Length>2) {
            var more=new Button {Content="More"};var flyout=new Flyout();var remaining=new StackPanel {Spacing=8};foreach(var button in buttons.Skip(2))remaining.Children.Add(button);flyout.Content=remaining;more.Flyout=flyout;actions.Children.Add(more);
        }
        row.Children.Add(actions); Body.Children.Add(row);
    }
    private async Task<bool> Confirm(string title, string message) => await new ContentDialog { XamlRoot = XamlRoot, Title = title, Content = message, PrimaryButtonText = "Continue", CloseButtonText = "Cancel", DefaultButton = ContentDialogButton.Close }.ShowAsync() == ContentDialogResult.Primary;
    private async Task<string?> Input(string title, string label, string value = "")
    {
        var box = new TextBox { Header = label, Text = value, MinWidth = 380 };
        var result = await new ContentDialog { XamlRoot = XamlRoot, Title = title, Content = box, PrimaryButtonText = "Continue", CloseButtonText = "Cancel" }.ShowAsync();
        return result == ContentDialogResult.Primary ? box.Text : null;
    }
    private async Task Refresh()
    {
        var current = ++generation;renderGeneration.Value=current;
        status = await Call("status"); if (current != generation) return;
        ShowBody(); Body.Children.Clear(); Commands.PrimaryCommands.Clear(); Heading.Text = page switch { "gateway" => "Remote Files", "pending" => "Pending Changes", "recovery" => "Recovered Captures", _ => char.ToUpperInvariant(page[0]) + page[1..] };
        Command("Refresh", Symbol.Refresh, Refresh);
        var mode = S(status, "mode");
        ModeChanged?.Invoke(mode);
        if(detailRoute is not null && detailMode==mode) {await detailRoute();return;}
        detailRoute=null;
        if (mode.Length == 0)
        {
            Text("Choose what this PC does", true);
            Text("Synchronize keeps full copies of your folders here. Files shows a gateway’s folders in File Explorer and fetches files when you open them.");
            Body.Children.Add(ActionButton("Synchronize", () => SetMode("synchronize")));
            Body.Children.Add(ActionButton("Files in Explorer", () => SetMode("files")));
            Text("Explorer location: "+S(status,"root"));Body.Children.Add(ActionButton("Choose Explorer location…",ChooseLocation));
            Body.Children.Add(ActionButton("Import existing Syncthing configuration…",ImportHome));return;
        }
        switch (page)
        {
            case "overview":
                Text(mode == "files" ? "Files in Explorer" : S(status, "state"), true);
                if (S(status, "error").Length > 0) Text(S(status, "error"));
                Text(mode == "files" ? S(status, "root") : "Device ID: " + S(status, "deviceID"));
                if(mode=="files")foreach(var issue in status?["projectionIssues"]?.AsArray()??[])Text(S(issue,"Name")+": "+S(issue,"Reason"));
                Text("The background agent continues working when you close this window. It stops when you sign out.");
                if (mode == "files") Body.Children.Add(ActionButton("Open in File Explorer", () => OpenPath(S(status, "root"))));
                else
                {
                    Command("Pause all", Symbol.Pause, () => EngineAction("pauseAll")); Command("Resume all", Symbol.Play, () => EngineAction("resumeAll"));
                    Command("Rescan all", Symbol.Sync, () => EngineAction("rescanAll")); Command("Restart engine", Symbol.RepeatAll, () => EngineAction("restart"));
                    Body.Children.Add(ActionButton("Open Web GUI", () => OpenPath(S(status, "guiURL"))));
                    var stats=await Call("engine.stats");Text("Local files: "+S(stats?["totals"]?["Local"],"Files")+" · "+S(stats?["totals"]?["Local"],"Bytes")+" bytes");
                    foreach(var direction in new[]{"download","upload"}) if(stats?[direction] is JsonNode progress) {
                        Text((direction=="download"?"Downloading":"Uploading")+": "+S(progress,"FilesTotal")+" files");
                        Body.Children.Add(new ProgressBar { Minimum=0,Maximum=100,Value=(progress["Percentage"]?.GetValue<double>()??0)*100 });
                    }
                    Body.Children.Add(ActionButton("Activity log",ShowLogs));
                }
                Body.Children.Add(ActionButton("Switch to " + (mode == "files" ? "Synchronize" : "Files"), async () => { if (await Confirm("Switch modes?", "Pending changes must reach their gateway first. Existing synchronized folders stay on disk.")) await SetMode(mode == "files" ? "synchronize" : "files"); })); break;
            case "folders": if (mode == "files") await RemoteFolders(); else await SyncFolders(); break;
            case "devices": if (mode == "synchronize") await Devices(); else Text("Devices are managed by the gateway in Files mode."); break;
            case "files": if (mode == "files") await Browse(); else if(syncFolder is not null) await BrowseSync(syncFolder,syncPath); else await SyncFolders(); break;
            case "gateway": if (mode == "files") await Gateways(); else await Gateway(); break;
            case "recovery":await RecoveredCaptures();break;
            case "pending": if (mode == "files") await Pending(); else Text("Pending file changes are shown in Files mode."); break;
            case "settings": await Settings(mode); break;
        }
    }
    private async Task SetMode(string mode) { await Call("mode.set", new { Mode = mode }, 150); parent = 0; parents.Clear(); await Refresh(); }
    private async Task EngineAction(string action, string? folder = null, string? device = null, string? id=null) { await Call("engine.action", new { Action = action, Folder = folder, Device = device,ID=id }); await Refresh(); }
    private static Task OpenPath(string path) { if (path.Length > 0) Process.Start(new ProcessStartInfo(path) { UseShellExecute = true }); return Task.CompletedTask; }
    private async Task SyncFolders()
    {
        Command("Add folder", Symbol.Add, async () =>
        {
            var path = await PickFolder(); if (path is null) return;
            var id = await Input("Folder ID", "Use the same folder ID on devices sharing this folder", Guid.NewGuid().ToString("N")[..12]); if (id is null) return;
            await Call("engine.addFolder", new { ID = id, Path = path }); await Refresh();
        });
        foreach (var folder in (await Call("engine.folders"))?.AsArray() ?? [])
        {
            var id = S(folder, "ID");
            Row(S(folder, "Label") is { Length: > 0 } label ? label : id, S(folder, "Path") + "\n" + S(folder, "State") + " " + S(folder, "Error"),
                ActionButton("Browse", () => BrowseSync(id, "")), ActionButton("Settings", () => EditSettings("folder/" + id)), ActionButton("Share…",()=>ShareFolder(id)), ActionButton("Ignore patterns",()=>Ignores(id)), ActionButton("Deleted files",()=>DeletedFiles(id)), ActionButton("Errors",()=>FolderErrors(id)),
                ActionButton(B(folder, "Paused") ? "Resume" : "Pause", () => EngineAction(B(folder, "Paused") ? "resume" : "pause", id)),
                ActionButton("Rescan", () => EngineAction("rescan", id)),
                ActionButton("Open folder",()=>OpenPath(S(folder,"Path"))),
                ActionButton("Move…",async()=>{var target=await PickFolder();if(target is not null){await Call("engine.action",new { Action="relocate",Folder=id,Path=target },150);await Refresh();}}),
                ActionButton(B(folder,"Trash")?"Stop using Recycle Bin":"Use Recycle Bin",async()=>{await Call("engine.action",new { Action="trash",Folder=id,Enabled=!B(folder,"Trash") });await Refresh();}), ActionButton("Remove", async () => { if (await Confirm("Remove folder?", "The folder stays on this PC. Syncthing stops synchronizing it.")) await EngineAction("remove", id); }));
        }
    }
    private async Task Devices()
    {
        Command("Add device", Symbol.Add, async () => { var id = await Input("Add device", "Syncthing device ID"); if (id is null) return; await Call("engine.addDevice", new { ID = id }); await Refresh(); });
        Text("Connection requests",true);
        foreach(var pending in (await Call("engine.pending"))?.AsArray()??[]) {
            var id=S(pending,"DeviceID");Row(S(pending,"Name"),id+"\n"+S(pending,"Address"),
                ActionButton("Add device",async()=>{await Call("engine.addDevice",new { ID=id });await Refresh();}),
                ActionButton("Ignore",()=>EngineAction("ignore",device:null,folder:null,id:id)));
        }
        Text("Devices",true);
        foreach (var device in (await Call("engine.devices"))?.AsArray() ?? [])
        {
            var id = S(device, "ID"); Row(S(device, "Name"), id + "\n" + (B(device, "Connected") ? "Connected" : "Disconnected"),
                ActionButton("Settings", () => EditSettings("device/" + id)),
                ActionButton(B(device, "Paused") ? "Resume" : "Pause", () => EngineAction(B(device, "Paused") ? "resume" : "pause", device: id)),
                ActionButton("Remove", async () => { if (await Confirm("Remove device?", "This PC stops sharing folders with this device.")) await EngineAction("remove", device: id); }));
        }
    }
    private async Task CloseEditor() { if (editorToken is string token) { editorToken = null; await Call("settings.close",new { Token=token }); } }
    private async Task EditSettings(string section)
    {
        await CloseEditor();
        var editor = await Call("settings.get", new { Section = section }); ShowBody(); Body.Children.Clear(); Commands.PrimaryCommands.Clear(); Heading.Text = "Settings";
        editorToken=S(editor,"token");
        var values = new Dictionary<string, Func<string>>();
        foreach (var field in editor?["fields"]?.AsArray() ?? [])
        {
            var key = S(field, "Path"); var label = S(field, "Label"); var value = S(field, "Value"); var enabled = !B(field, "IsReadOnly");
            if (S(field, "Kind") == "bool") { var toggle = new ToggleSwitch { Header = label, IsOn = value == "true", IsEnabled = enabled }; Body.Children.Add(toggle); values[key] = () => toggle.IsOn ? "true" : "false"; }
            else if (B(field, "IsSecret")) { var password = new PasswordBox { Header = label, IsEnabled = enabled }; Body.Children.Add(password); values[key] = () => password.Password; }
            else if (S(field, "Kind") == "enum") { var options = field?["Options"]?.AsArray().Select(option => option!.ToString()).ToArray() ?? []; var combo = new ComboBox { Header = label, ItemsSource = options, SelectedItem = value, IsEnabled = enabled }; Body.Children.Add(combo); values[key] = () => combo.SelectedItem?.ToString() ?? value; }
            else { var box = new TextBox { Header = label, Text = value, IsReadOnly = !enabled, AcceptsReturn = S(field, "Kind") is "list" or "json", TextWrapping = TextWrapping.Wrap }; Body.Children.Add(box); values[key] = () => box.Text; }
        }
        Command("Save", Symbol.Save, async () =>
        {
            var result = await Call("settings.save", new { Token = S(editor, "token"), Values = values.ToDictionary(pair => pair.Key, pair => pair.Value()) }); editorToken=null; await Refresh();
            if (B(result, "restartRequired") && await Confirm("Restart required", "Restart Syncthing to apply these settings?")) await EngineAction("restart");
        });
        Command("Cancel", Symbol.Cancel, async () => { await CloseEditor(); await Refresh(); });
    }
    private void ShowBody() { BodyScroll.Visibility=Visibility.Visible; FileBrowser.Visibility=Visibility.Collapsed; }
    private void ShowFiles(List<FileRow> rows,string scope)
    {
        var selected=FileList.SelectedItem as FileRow;var scroll=FindScroll(FileList);var offset=scroll?.VerticalOffset??0;var sameScope=fileFilterScope==scope;
        fileRows=rows; BodyScroll.Visibility=Visibility.Collapsed; FileBrowser.Visibility=Visibility.Visible;
        if(fileFilterScope!=scope){fileFilterScope=scope;FileFilter.Text="";}
        FileList.ItemsSource=fileRows.Where(row=>row.Title.Contains(FileFilter.Text,StringComparison.CurrentCultureIgnoreCase)).ToList();
        if(sameScope && selected is not null) FileList.SelectedItem=fileRows.FirstOrDefault(row=>row.Title==selected.Title && row.Detail==selected.Detail);
        var current=generation;if(sameScope)DispatcherQueue.TryEnqueue(()=>{if(current==generation)FindScroll(FileList)?.ChangeView(null,offset,null,true);});
    }
    private static ScrollViewer? FindScroll(DependencyObject root)
    {
        if(root is ScrollViewer scroll)return scroll;
        for(int i=0;i<Microsoft.UI.Xaml.Media.VisualTreeHelper.GetChildrenCount(root);i++) if(FindScroll(Microsoft.UI.Xaml.Media.VisualTreeHelper.GetChild(root,i)) is ScrollViewer child)return child;
        return null;
    }
    private void FilterFiles(object sender, TextChangedEventArgs args) => FileList.ItemsSource=fileRows.Where(row=>row.Title.Contains(FileFilter.Text,StringComparison.CurrentCultureIgnoreCase)).ToList();
    private async void OpenFileRow(object sender, ItemClickEventArgs args) { if(args.ClickedItem is FileRow row) await Run(row.Open); }
    private async void OpenFileButton(object sender, RoutedEventArgs args) { if(sender is FrameworkElement { DataContext:FileRow row }) await Run(row.Open); }
    private void FileActions(object sender, RoutedEventArgs args)
    {
        if(sender is not Button { DataContext:FileRow row } button) return;
        var menu=new MenuFlyout();
        foreach(var action in row.Actions) { var item=new MenuFlyoutItem { Text=action.Label }; item.Click+=async(_,_)=>await Run(action.Run);menu.Items.Add(item); }
        menu.ShowAt(button);
    }
    private async Task Preview(string location,string name)
    {
        var extension=Path.GetExtension(name).ToLowerInvariant();
        FrameworkElement? preview=null;
        if(new[]{".png",".jpg",".jpeg",".gif",".bmp",".webp"}.Contains(extension)) preview=new Image { Source=new Microsoft.UI.Xaml.Media.Imaging.BitmapImage(new Uri(location)), Stretch=Microsoft.UI.Xaml.Media.Stretch.Uniform,MaxWidth=900,MaxHeight=600 };
        else if(new[]{".mp4",".m4v",".mov",".mp3",".m4a",".aac",".wav",".webm"}.Contains(extension)) preview=new MediaPlayerElement { Source=MediaSource.CreateFromUri(new Uri(location)), AreTransportControlsEnabled=true, AutoPlay=true,MinWidth=500,MinHeight=280,MaxHeight=600 };
        if(preview is null) {await OpenPath(location);return;}
        try {
            var result=await new ContentDialog { XamlRoot=XamlRoot,Title=name,Content=preview,PrimaryButtonText="Open in default app",CloseButtonText="Close" }.ShowAsync();
            if(result==ContentDialogResult.Primary) await OpenPath(location);
        } finally { if(preview is MediaPlayerElement player) { player.MediaPlayer?.Pause();player.Source=null; } }
    }
    private async Task Browse()
    {
        if(parent!=0){
            try {await Call("files.entry",new {Item=parent});}
            catch(InvalidOperationException error)when(error.Message=="file does not exist"){parent=0;parents.Clear();}
        }
        var listingParent=parent;
        if (parents.Count > 0) Command("Back", Symbol.Back, async () => { parent = parents.Pop(); await Refresh(); });
        if(parent!=0) Command("New folder",Symbol.NewFolder,async()=>{var name=await Input("New folder","Folder name");if(name is not null){await Call("files.create",new { Parent=listingParent,Name=name,Directory=true });await Refresh();}});
        var rows=new List<FileRow>();
        foreach (var entry in (await Call("files.children", new { Parent = parent }))?.AsArray() ?? [])
        {
            var item = N(entry, "Item"); var name = S(entry, "Name"); var directory = N(entry, "Kind") != 0;
            var actions=new List<FileAction> {
                new(B(entry,"Keep")?"Stop keeping":"Keep on this device",async()=>{await Call("files.keep",new { Item=item,Enabled=!B(entry,"Keep") });await Refresh();}),
                new("Previous versions",()=>Versions(item))
            };
            if(!directory)actions.Add(new("Save a copy…",async()=>{var target=await PickSave(name);if(target is not null)await Call("files.download",new { Item=item,Path=target },150);}));
            if(B(entry,"Writable") && N(entry,"Kind")!=3) {
                actions.Add(new("Rename",async()=>{var renamed=await Input("Rename","New name",name);if(renamed is not null){await Call("files.rename",new { Parent=listingParent,Name=name,NewParent=listingParent,Path=renamed,Item=item,Node=N(entry,"Node"),ClientID=S(entry,"ClientID") });await Refresh();}}));
                actions.Add(new("Delete",async()=>{if(await Confirm("Delete "+name+"?","This also deletes it on the gateway.")){await Call("files.delete",new { Parent=listingParent,Name=name,Directory=directory,Item=item,Node=N(entry,"Node"),ClientID=S(entry,"ClientID") });await Refresh();}}));
            }
            rows.Add(new(name,directory?"Folder":N(entry,"Size").ToString("N0")+" bytes",async()=>{
                if(directory){parents.Push(parent);parent=item;await Refresh();}
                else{var path=await Call("files.path",new { Item=item });await Preview(Path.Combine(S(status,"root"),path!.GetValue<string>()),name);}
            },actions));
        }
        ShowFiles(rows,"files:"+listingParent);
    }
    private async Task BrowseSync(string folder, string path)
    {
        detailRoute=null;page="files";syncFolder=folder;syncPath=path;
        var entries=await Call("engine.browse",new { Folder=folder,Path=path }); ShowBody();Body.Children.Clear();Commands.PrimaryCommands.Clear();Heading.Text="Files";
        Command("Back",Symbol.Back,()=>path.Length==0?LeaveSyncFolder():BrowseSync(folder,path.Contains('/')?path[..path.LastIndexOf('/')]:""));
        Command("Search",Symbol.Find,async()=>{var query=await Input("Search synchronized files","Name contains");if(query is not null) await SearchSync(folder,path,query);});
        var rows=new List<FileRow>();
        foreach(var entry in entries?.AsArray()??[]) {
            var name=S(entry,"Name");var isDirectory=B(entry,"Directory");
            rows.Add(new(name,isDirectory?"Folder":N(entry,"Size").ToString("N0")+" bytes",()=>isDirectory?BrowseSync(folder,S(entry,"Path")):Preview(S(entry,"URL"),name),isDirectory?[]:[new("Previous versions",()=>SyncVersions(folder,S(entry,"Path")))]));
        }
        ShowFiles(rows,"sync:"+folder+"/"+path);
    }
    private void Detail(Func<Task> route) {detailRoute=route;detailMode=S(status,"mode");}
    private async Task LeaveDetails() {detailRoute=null;await Refresh();}
    private async Task LeaveSyncFolder() {syncFolder=null;syncPath="";await Refresh();}
    private async Task<string?> PickFolder() => (await new Microsoft.Windows.Storage.Pickers.FolderPicker(XamlRoot.ContentIslandEnvironment.AppWindowId).PickSingleFolderAsync())?.Path;
    private static string SafeSuggestedName(string name) {
        var invalid=Path.GetInvalidFileNameChars();name=new string(name.Select(c=>invalid.Contains(c)?'_':c).ToArray()).TrimEnd(' ','.');
        var period=name.IndexOf('.');
        var stem=(period<0?name:name[..period]).ToUpperInvariant();
        if(stem is "CON" or "PRN" or "AUX" or "NUL" or "CONIN$" or "CONOUT$" || (stem.Length==4 && (stem.StartsWith("COM") || stem.StartsWith("LPT")) && "123456789¹²³".Contains(stem[3])))name="_"+name;
        return name.Length==0?"Saved file":name[..Math.Min(name.Length,240)];
    }
    private async Task<string?> PickSave(string name) => (await new Microsoft.Windows.Storage.Pickers.FileSavePicker(XamlRoot.ContentIslandEnvironment.AppWindowId) { SuggestedFileName=SafeSuggestedName(name) }.PickSaveFileAsync())?.Path;
    private async Task ShareFolder(string folder)
    {
        Detail(()=>ShareFolder(folder));
        ShowBody();Body.Children.Clear();Commands.PrimaryCommands.Clear();Heading.Text="Share folder";Command("Back",Symbol.Back,LeaveDetails);
        foreach(var device in (await Call("engine.devices"))?.AsArray()??[]) {
            var id=S(device,"ID");Row(S(device,"Name"),id,
                ActionButton("Share",async()=>{await Call("engine.share",new { Folder=folder,Device=id,Enabled=true });await Refresh();}),
                ActionButton("Share encrypted…",async()=>{var password=new PasswordBox { Header="Encryption password",MinWidth=350 };if(await new ContentDialog{ XamlRoot=XamlRoot,Title="Share encrypted with "+S(device,"Name"),Content=password,PrimaryButtonText="Share",CloseButtonText="Cancel" }.ShowAsync()==ContentDialogResult.Primary){await Call("engine.share",new { Folder=folder,Device=id,Enabled=true,Password=password.Password });await Refresh();}}),
                ActionButton("Stop sharing",async()=>{await Call("engine.share",new { Folder=folder,Device=id,Enabled=false });await Refresh();}));
        }
    }
    private async Task Ignores(string folder)
    {
        var lines=await Call("engine.action",new { Folder=folder,Action="ignores" });
        var box=new TextBox { Header="One pattern per line",AcceptsReturn=true,TextWrapping=TextWrapping.NoWrap,MinWidth=450,MinHeight=260,Text=string.Join("\n",lines?.AsArray().Select(line=>line!.ToString())??[]) };
        if(await new ContentDialog{ XamlRoot=XamlRoot,Title="Ignore patterns",Content=box,PrimaryButtonText="Save",CloseButtonText="Cancel" }.ShowAsync()==ContentDialogResult.Primary) {await Call("engine.action",new { Folder=folder,Action="saveIgnores",Path=box.Text });await Refresh();}
    }
    private async Task FolderErrors(string folder)
    {
        Detail(()=>FolderErrors(folder));
        var errors=await Call("engine.action",new { Folder=folder,Action="failed" });ShowBody();Body.Children.Clear();Commands.PrimaryCommands.Clear();Heading.Text="Folder errors";Command("Back",Symbol.Back,LeaveDetails);
        foreach(var error in errors?.AsArray()??[]) Row(S(error,"Path"),S(error,"Error"));
    }
    private async Task DeletedFiles(string folder)
    {
        Detail(()=>DeletedFiles(folder));
        var entries=await Call("engine.action",new { Folder=folder,Action="deleted" });ShowBody();Body.Children.Clear();Commands.PrimaryCommands.Clear();Heading.Text="Deleted files";Command("Back",Symbol.Back,LeaveDetails);
        foreach(var entry in entries?.AsArray()??[]) {var path=S(entry,"Path");Row(path,S(entry,"Versions")+" versions",ActionButton("Previous versions",()=>SyncVersions(folder,path)));}
    }
    private async Task SyncVersions(string folder,string path)
    {
        Detail(()=>SyncVersions(folder,path));
        var versions=await Call("engine.versions",new { Folder=folder,Path=path });ShowBody();Body.Children.Clear();Commands.PrimaryCommands.Clear();Heading.Text="Previous Versions";Command("Back",Symbol.Back,LeaveDetails);
        foreach(var version in versions?.AsArray()??[]) {
            var id=S(version,"ID");Row(DateTimeOffset.FromUnixTimeMilliseconds(N(version,"VersionTime")).LocalDateTime.ToString("g"),N(version,"Size").ToString("N0")+" bytes",
                ActionButton("Save a copy…",async()=>{var destination=await PickSave(Path.GetFileName(path));if(destination is not null) await Call("engine.exportVersion",new { Folder=folder,Path=path,ID=id,Name=destination });}),
                ActionButton("Restore",async()=>{if(await Confirm("Restore this version?","This writes an older version as a new synchronized change.")) await Call("engine.restoreVersion",new { Folder=folder,Path=path,ID=id });}));
        }
    }
    private async Task SearchSync(string folder,string prefix,string query)
    {
        Detail(()=>SearchSync(folder,prefix,query));
        var results=await Call("engine.search",new { Folder=folder,Path=prefix,Name=query });Commands.PrimaryCommands.Clear();Heading.Text="Search results";Command("Back",Symbol.Back,()=>BrowseSync(folder,prefix));
        var rows=new List<FileRow>();foreach(var entry in results?.AsArray()??[]){var targetFolder=S(entry,"Folder");var targetPath=S(entry,"Path");rows.Add(new(S(entry,"Name"),targetPath,()=>B(entry,"Directory")?BrowseSync(targetFolder,targetPath):Preview(S(entry,"URL"),S(entry,"Name")),[new("Previous versions",()=>SyncVersions(targetFolder,targetPath))]));}ShowFiles(rows,"search:"+folder+"/"+prefix+"/"+query);
    }
    private async Task ShowLogs()
    {
        Detail(()=>ShowLogs());
        var logs=await Call("engine.logs");ShowBody();Body.Children.Clear();Commands.PrimaryCommands.Clear();Heading.Text="Activity log";Command("Back",Symbol.Back,LeaveDetails);
        Body.Children.Add(new TextBox { Text=string.Join("\n",(logs?.AsArray()??[]).TakeLast(2000).Select(line=>S(line,"facility")+": "+S(line,"message"))),IsReadOnly=true,AcceptsReturn=true,TextWrapping=TextWrapping.Wrap,MinHeight=400 });
    }
    private async Task Gateways()
    {
        Command("Pair", Symbol.Add, async () => { var link = await Input("Pair with a gateway", "Paste the synctrain+pair:// link from the gateway"); if (link is null) return; Text("Waiting for approval on the gateway…"); await Call("files.pair", new { Link = link }, 600); await Refresh(); });
        foreach (var gateway in (await Call("files.gateways"))?.AsArray() ?? [])
        {
            var id = S(gateway, "serviceID"); Row(S(gateway, "name"), S(gateway, "host") + " · " + S(gateway, "state"), ActionButton("Choose folders", () => ChooseRemoteFolders(id)), ActionButton("Forget", async () => { if (await Confirm("Forget gateway?", "Remove its folders from this PC first.")) { await Call("files.forget", new { ServiceID = id }); await Refresh(); } }));
        }
    }
    private async Task ChooseRemoteFolders(string service)
    {
        Detail(()=>ChooseRemoteFolders(service));var folders=await Call("files.available",new { ServiceID=service });ShowBody();Body.Children.Clear();Commands.PrimaryCommands.Clear();Heading.Text="Choose remote folders";Command("Back",Symbol.Back,LeaveDetails);
        foreach(var folder in folders?.AsArray()??[]){var id=S(folder,"id");Row(S(folder,"label"),id,ActionButton("Add to this PC",async()=>{await Call("files.add",new { ServiceID=service,Folder=id });await Refresh();}));}
    }
    private async Task RemoteFolders()
    {
        foreach (var folder in (await Call("files.folders"))?.AsArray() ?? [])
        {
            var id = N(folder, "id"); Row(S(folder, "label"), S(folder, "gateway") + "\n" + S(folder, "state"), ActionButton("Remove from this PC", async () =>
            {
                if (await Confirm("Remove folder from this PC?", "The folder stays on its gateway. Changes must be synchronized first, and local-only files will be preserved.")) { await Call("files.remove", new { Item = id }); await Refresh(); }
            }));
        }
        Body.Children.Add(ActionButton("Choose folders from a gateway", async () => { page = "gateway"; await Refresh(); }));
    }
    private async Task Pending()
    {
        foreach (var edit in (await Call("files.pending"))?.AsArray() ?? [])
        {
            var id = N(edit, "id"); Row(S(edit, "path"), S(edit, "state") + "\n" + S(edit, "error"),
                ActionButton("Export", async () => { var destination = await PickSave(Path.GetFileName(S(edit,"path"))); if (destination is not null) await Call("files.export", new { Item = id, Path = destination }); }),
                ActionButton("Discard", async () => { if (await Confirm("Discard pending change?", "This gives up the local change and restores what the gateway has.")) { await Call("files.discard", new { Item = id }); await Refresh(); } }));
        }
    }
    private async Task Versions(long item)
    {
        Detail(()=>Versions(item));
        var versions=await Call("files.versions",new { Item=item });ShowBody();Body.Children.Clear();Commands.PrimaryCommands.Clear();Heading.Text="Previous Versions";
        Command("Back",Symbol.Back,LeaveDetails);
        foreach(var version in versions?["versions"]?.AsArray()??[]) {
            var id=S(version,"id");Row(S(version,"archived"),N(version,"size").ToString("N0")+" bytes",
                ActionButton("Preview",async()=>{var path=await Call("files.versionPreview",new { Item=item,ID=id });if(path is not null) await Preview(path.GetValue<string>(),Path.GetFileName(path.GetValue<string>()));}),
                ActionButton("Restore as a copy",async()=>{await Call("files.copyVersion",new { Item=item,ID=id });await Refresh();}),
                ActionButton("Restore",async()=>{if(await Confirm("Restore this version?","The gateway writes this version as a new change.")) await Call("files.restore",new { Item=item,ID=id });}));
        }
    }
    private async Task Gateway()
    {
        var enabled = B(status, "gatewayEnabled"); Body.Children.Add(ActionButton(enabled ? "Turn off remote file service" : "Turn on remote file service", async () => { await Call("gateway.enable", new { Enabled = !enabled }); await Refresh(); })); if (S(status,"gatewayError").Length>0) Text(S(status,"gatewayError")); if (!enabled || !B(status,"gatewayRunning")) return;
        Command("Pair a client", Symbol.Add, async () => { var pairing = await Call("gateway.pair"); var box = new TextBox { Text = S(pairing, "url"), IsReadOnly = true, TextWrapping = TextWrapping.Wrap, MinWidth = 420 }; await new ContentDialog { XamlRoot = XamlRoot, Title = "Pair with this PC", Content = box, CloseButtonText = "Done" }.ShowAsync(); });
        Text("Shared folders", true);
        foreach (var folder in (await Call("engine.folders"))?.AsArray() ?? [])
        {
            var id = S(folder, "ID"); Row(S(folder, "Label"), id,
                ActionButton("Read only", async () => { await Call("gateway.export", new { Folder = id, Enabled = false }); await Refresh(); }),
                ActionButton("Read and write", async () => { await Call("gateway.export", new { Folder = id, Enabled = true }); await Refresh(); }),
                ActionButton("Stop sharing", async () => { await Call("gateway.unexport", new { Folder = id }); await Refresh(); }));
        }
        Text("Waiting for approval", true);
        foreach (var pending in (await Call("gateway.pending"))?.AsArray() ?? [])
        {
            var id=S(pending,"requestID");
            Row(S(pending,"clientName"), S(pending,"platform")+" · "+S(pending,"clientID"),
                ActionButton("Approve…", async () => {
                    var choices=new StackPanel { Spacing=12 }; var grants=new Dictionary<string,ComboBox>();
                    foreach (var export in (await Call("gateway.exports"))?.AsArray() ?? []) {
                        var folderID=S(export,"folderID"); var options=B(export,"allowWrite") ? new[]{"No access","Read only","Read and write"} : new[]{"No access","Read only"};
                        var choice=new ComboBox { Header=folderID, ItemsSource=options, SelectedIndex=0 }; choices.Children.Add(choice); grants[folderID]=choice;
                    }
                    if (await new ContentDialog { XamlRoot=XamlRoot, Title="Approve "+S(pending,"clientName"), Content=choices, PrimaryButtonText="Approve", CloseButtonText="Cancel", DefaultButton=ContentDialogButton.Close }.ShowAsync()==ContentDialogResult.Primary) {
                        var selected=grants.Where(pair=>pair.Value.SelectedIndex>0).ToDictionary(pair=>pair.Key,pair=>pair.Value.SelectedIndex==2?"write":"read");
                        await Call("gateway.approve",new { ID=id, Grants=selected }); await Refresh();
                    }
                }), ActionButton("Deny",async()=>{ await Call("gateway.deny",new { ID=id }); await Refresh(); }));
        }
        Text("Paired clients", true);
        foreach (var client in (await Call("gateway.clients"))?.AsArray() ?? []) { var id = S(client, "clientID"); Row(S(client, "name"), S(client, "platform"), ActionButton("Permissions…",()=>ClientPermissions(client!)), ActionButton("Revoke", async () => { if (await Confirm("Revoke this client?", "Its gateway access stops immediately.")) { await Call("gateway.revoke", new { ID = id }); await Refresh(); } })); }
    }
    private async Task ClientPermissions(JsonNode client)
    {
        var controls=new StackPanel {Spacing=12};var grants=new Dictionary<string,ComboBox>();
        foreach(var export in (await Call("gateway.exports"))?.AsArray()??[]) {
            var id=S(export,"folderID");var options=B(export,"allowWrite")?new[]{"No access","Read only","Read and write"}:new[]{"No access","Read only"};
            var current=client["permissions"]?[id]?.ToString();var selection=current=="write" && options.Length>2?2:current is "write" or "read"?1:0;
            var choice=new ComboBox {Header=id,ItemsSource=options,SelectedIndex=selection};grants[id]=choice;controls.Children.Add(choice);
        }
        if(await new ContentDialog {XamlRoot=XamlRoot,Title="Permissions for "+S(client,"name"),Content=controls,PrimaryButtonText="Save",CloseButtonText="Cancel"}.ShowAsync()!=ContentDialogResult.Primary)return;
        foreach(var pair in grants) await Call("gateway.permission",new {ID=S(client,"clientID"),Folder=pair.Key,Action=pair.Value.SelectedIndex==2?"write":pair.Value.SelectedIndex==1?"read":""});
        await Refresh();
    }
    private async Task ImportHome(){var source=await PickFolder();if(source is not null && await Confirm("Import Syncthing configuration?","Syncthing must be stopped. Its configuration and identity are copied; synchronized folders stay where they are.")){await Call("home.import",new {Path=source});await SetMode("synchronize");}}
    private async Task RecoveredCaptures()
    {
        Text("These complete snapshots were retained before a save could finish. Export keeps the original capture for recovery.");
        foreach(var capture in (await Call("recovery.captures"))?.AsArray()??[]) {
            var name=S(capture,"Name");var path=S(capture,"Path");Row(path.Length>0?path:"Unassigned captured file",N(capture,"Size").ToString("N0")+" bytes · "+S(capture,"Modified"),ActionButton("Export…",async()=>{var destination=await PickSave(path.Length>0?Path.GetFileName(path):"Recovered file");if(destination is not null)await Call("recovery.export",new {Captured=name,Path=destination});}));
        }
    }
    private async Task ChooseLocation(){var path=await PickFolder();if(path is not null){await Call("root.set",new {Path=path});await Refresh();}}
    private async Task Settings(string mode)
    {
        var startup = await StartupTask.GetAsync("SynctrainStartup");
        var toggle = new ToggleSwitch { Header = "Start at login", IsOn = startup.State == StartupTaskState.Enabled };
        toggle.Toggled += async (_, _) => await Run(async () => { if (toggle.IsOn) { var result = await startup.RequestEnableAsync(); if (result != StartupTaskState.Enabled) throw new InvalidOperationException("Windows has disabled startup for Synctrain. Enable it in Settings → Apps → Startup."); } else startup.Disable(); }); Body.Children.Add(toggle);
        if(mode=="synchronize")Body.Children.Add(ActionButton("Choose Explorer location…",ChooseLocation));
        if (mode == "synchronize") foreach (var section in new[] { "options", "gui", "ldap", "defaults/folder", "defaults/device", "defaults/ignores" }) Body.Children.Add(ActionButton(section switch { "options" => "Engine options", "gui" => "Web GUI and REST API", "ldap" => "LDAP", "defaults/folder" => "New folder defaults", "defaults/device" => "New device defaults", _ => "Default ignore patterns" }, () => EditSettings(section)));
        else
        {
            var storage = await Call("files.storage"); Text($"Download cache: {N(storage,"used") / 1048576.0:N1} MiB of {N(storage,"budget") / 1048576.0:N0} MiB");
            Text("Kept files use additional storage. The cache budget also limits unpinned files in Explorer.");
            Body.Children.Add(ActionButton("Change cache budget", async () => { var budget = await Input("Cache budget", "MiB of unpinned files to keep", "5120"); if (budget is not null) { await Call("files.budget", new { Budget = checked(long.Parse(budget) * 1024 * 1024) }); await Refresh(); } }));
        }
        var version=Package.Current.Id.Version;Text($"Synctrain for Windows {version.Major}.{version.Minor}.{version.Build}.{version.Revision}", true); Text("ARM64 and x64 · Windows Cloud Files · Mozilla Public License 2.0");
        Body.Children.Add(ActionButton("Open-source notices",()=>OpenPath(Path.Combine(AppContext.BaseDirectory,"GoNotices.html"))));
    }
}
