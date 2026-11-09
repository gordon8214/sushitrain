// Copyright (C) 2026 Gordon. SPDX-License-Identifier: MPL-2.0
using Microsoft.UI.Xaml;
using Microsoft.UI.Xaml.Controls;
using System.Text.Json.Nodes;

namespace Synctrain_App;

public sealed partial class MainPage
{
    private IReadOnlyList<FileAction> OrderedFileActions(IEnumerable<FileAction> actions)
    {
        var order=catalog["detailDescriptors"]!["fileActions"]!.AsArray().Select((item,index)=>(ID:S(item,"id"),Index:index)).ToDictionary(pair=>pair.ID,pair=>pair.Index);
        return actions.OrderBy(action=>order.GetValueOrDefault(action.ID,order["information"])).ToList();
    }
    private static string InformationText(JsonNode? entry) => string.Join("\n",new[]{"Name","Path","Size","Modified","State","Local","Writable"}.Where(key=>entry?[key] is not null).Select(key=>key+": "+S(entry,key)));
    private Task FileInformation(JsonNode? entry) => new ContentDialog {
        XamlRoot=XamlRoot, Title="Information", CloseButtonText="Done",
        Content=new TextBlock { Text=InformationText(entry), TextWrapping=Microsoft.UI.Xaml.TextWrapping.Wrap,IsTextSelectionEnabled=true }
    }.ShowAsync().AsTask();
    // A synchronized file's information, with its download progress while it's open, or that it waits to be synchronized
    // (nothing for a file the ignore patterns leave out)
    private async Task SyncFileInformation(string folder,JsonNode? entry)
    {
        var details=new TextBlock { Text=InformationText(entry), TextWrapping=Microsoft.UI.Xaml.TextWrapping.Wrap,IsTextSelectionEnabled=true };
        var panel=new StackPanel { Spacing=16,MinWidth=380 };panel.Children.Add(details);
        var dialog=new ContentDialog { XamlRoot=XamlRoot,Title="Information",CloseButtonText="Done",Content=panel };
        if(B(entry,"Directory")) { await dialog.ShowAsync();return; }
        var progress=new FileProgress();progress.Hide();panel.Children.Add(progress.Element);
        var path=S(entry,"Path");var local=B(entry,"Local");var ignored=B(entry,"Ignored");var busy=false;var open=true;
        async Task Update() {
            if(busy || !open)return;
            busy=true;
            try {
                var transfers=await host.Call("desktop.engine.transfers",timeoutSeconds:LivePollSeconds);
                var file=(transfers?["Downloads"] as JsonArray)?.FirstOrDefault(item=>S(item,"Folder")==folder && S(item,"Path")==path);
                if(!open)return;
                if(file is not null) { progress.Downloading(file);return; }
                // A file that was missing may have arrived since
                if(!local) {
                    var inspected=await host.Call("desktop.engine.inspect",new { Folder=folder,Path=path },LivePollSeconds);
                    if(!open)return;
                    if(inspected?["entry"] is JsonObject current) { local=B(current,"Local");ignored=B(current,"Ignored");details.Text=InformationText(current); }
                }
                if(local || ignored)progress.Hide(); else progress.Waiting();
            } catch(Exception error) { if(open)progress.Problem(error.Message); }
            finally { busy=false; }
        }
        // The dialog opens at once; its first update follows
        var shown=dialog.ShowAsync().AsTask();
        var timer=new DispatcherTimer { Interval=TimeSpan.FromSeconds(1) };
        timer.Tick+=async(_,_)=>await Update();
        timer.Start();
        _=Update();
        try { await shown; }
        finally { open=false;timer.Stop(); }
    }
    private List<FileAction> RemoteFileActions(JsonNode? entry,long listingParent)
    {
        var item=N(entry,"Item");var node=N(entry,"Node");var name=S(entry,"Name");var directory=N(entry,"Kind")!=0;
        var actions=new List<FileAction> {
            new(B(entry,"Keep")?"Stop keeping":"Keep on this device",async()=>{await Call("files.keep",new { Item=item,Enabled=!B(entry,"Keep") });await Refresh();},"offline"),
            new("Information",()=>FileInformation(entry),"information")
        };
        if(!directory) {
            actions.Add(new("Preview",async()=>{var path=await Call("files.path",new { Item=item });await Preview(Path.Combine(S(status,"root"),path!.GetValue<string>()),name);},"preview"));
            actions.Add(new("Previous Versions",()=>Versions(item),"previousVersions"));
            actions.Add(new("Save a copy…",async()=>{var target=await PickSave(name);if(target is not null)await Call("files.download",new { Item=item,Path=target },150);},"information"));
        }
        if(B(entry,"Writable") && N(entry,"Kind")!=3) {
            actions.Add(new("Rename…",async()=>{var renamed=await Input("Rename","New name",name);if(renamed is not null){await Call("desktop.files.rename",new { Item=item,Node=node,NewName=renamed });await Refresh();}},"renameMove"));
            actions.Add(new("Move…",async()=>{var target=await Input("Move","Destination path within this remote folder, including the file name");if(target is not null){await Call("desktop.files.move",new { Item=item,Node=node,Path=target });await Refresh();}},"renameMove"));
            actions.Add(new("Delete…",async()=>{if(await Confirm("Delete "+name+"?","This also deletes it on the gateway.")){await Call("files.delete",new { Parent=listingParent,Name=name,Directory=directory,Item=item,Node=node,ClientID=S(entry,"ClientID") });await Refresh();}},"delete"));
        }
        return actions;
    }
    private List<FileAction> SyncFileActions(string folder,JsonNode? entry)
    {
        var path=S(entry,"Path");var name=S(entry,"Name");var token=S(entry,"Identity");var directory=B(entry,"Directory");
        var actions=new List<FileAction> { new("Information",()=>SyncFileInformation(folder,entry),"information") };
        if(!directory) {
            actions.Add(new("Preview",()=>Preview(S(entry,"URL"),name),"preview"));
            actions.Add(new("Previous Versions",()=>SyncVersions(folder,path),"previousVersions"));
            actions.Add(new("Save a copy…",async()=>{var target=await PickSave(name);if(target is not null)await Call("desktop.engine.exportCurrent",new { Folder=folder,Path=path,Name=target,Token=token },150);},"information"));
        }
        if(B(entry,"Local") && token.Length>0) {
            actions.Add(new("Rename…",async()=>{var renamed=await Input("Rename","New name",name);if(renamed is not null){await Call("desktop.engine.rename",new { Folder=folder,Path=path,Name=renamed,Token=token });await Refresh();}},"renameMove"));
            actions.Add(new("Move…",async()=>{var target=await Input("Move","Destination relative to this synchronized folder, including the file name",path);if(target is not null){await Call("desktop.engine.move",new { Folder=folder,Path=path,Name=target,Token=token });await Refresh();}},"renameMove"));
            actions.Add(new("Delete…",async()=>{if(await Confirm("Delete "+name+"?","This permanently deletes the local item and synchronizes the deletion to other devices.")){await Call("desktop.engine.action",new { Action="deleteFile",Folder=folder,Path=path,Token=token });await Refresh();}},"delete"));
        }
        return actions;
    }
    private async Task ImportFile(string? folder=null,string path="",long parentItem=0)
    {
        var picker=new Microsoft.Windows.Storage.Pickers.FileOpenPicker(XamlRoot.ContentIslandEnvironment.AppWindowId);
        picker.FileTypeFilter.Add("*");
        var file=await picker.PickSingleFileAsync();if(file is null)return;
        if(folder is null) await Call("desktop.files.import",new { Parent=parentItem,Path=file.Path },150);
        else await Call("desktop.engine.import",new { Folder=folder,Path=path,ID=file.Path,Name=Path.GetFileName(file.Path) },150);
        await Refresh();
    }
}
