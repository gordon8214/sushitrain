// Copyright (C) 2026 Gordon. SPDX-License-Identifier: MPL-2.0
using Microsoft.UI.Xaml.Controls;
using System.Text.Json.Nodes;

namespace Synctrain_App;

public sealed partial class MainPage
{
    // What the Overview shows of the status, other than progress: a change builds the page again
    private static string OverviewShape(JsonNode? value) => string.Join("\n", S(value,"state"), S(value,"error"), S(value,"deviceID"), S(value,"guiURL"), S(value,"root"), value?["projectionIssues"]?.ToJsonString() ?? "");

    private async Task OverviewPage(string mode)
    {
        // In Files mode the page is built again on each change, as before; Synchronize mode shows progress in place
        if(mode=="synchronize") {var shape=OverviewShape(status);Live((next,_)=>Task.FromResult(OverviewShape(next)==shape));}
        foreach(var section in catalog["destinations"]!.AsArray().First(item=>S(item,"id")=="overview")!["sections"]!.AsArray()) {
            try {
                switch(section!.ToString()) {
                    case "status":
                        Text("Status",true);Text(mode=="files"?"Files in Explorer":S(status,"state"));
                        Text(mode=="files"?S(status,"root"):"Device ID: "+S(status,"deviceID"));
                        Text("The background agent continues working when you close this window. It stops when you sign out.");
                        if(mode=="files")Body.Children.Add(ActionButton("Open in File Explorer",()=>OpenPath(S(status,"root"))));
                        else {
                            Command("Pause all",Symbol.Pause,()=>EngineAction("pauseAll"));Command("Resume all",Symbol.Play,()=>EngineAction("resumeAll"));Command("Rescan all",Symbol.Sync,()=>EngineAction("rescanAll"));
                            Body.Children.Add(ActionButton("Open Web GUI",()=>OpenPath(S(status,"guiURL"))));
                        }
                        break;
                    case "attention":
                        Text("Needs Attention",true);
                        var problem=S(status,"error");if(problem.Length>0)Text(problem);
                        foreach(var issue in status?["projectionIssues"]?.AsArray()??[])Text(S(issue,"Name")+": "+S(issue,"Reason"));
                        if(mode=="files") {var pending=await Call("files.pending");Text($"{pending?.AsArray().Count??0} edits waiting for a gateway or review");}
                        else if(problem.Length==0)Text("No engine problems reported. Folder errors are available in Activity.");
                        break;
                    case "summary":
                        Text(mode=="files"?"Gateways":"Synchronization",true);
                        if(mode=="files") {
                            var folders=await Call("files.folders");var gateways=await Call("files.gateways");
                            Text($"{folders?.AsArray().Count??0} folders from {gateways?.AsArray().Count??0} gateways");
                        } else {
                            var counts=LiveText();Body.Children.Add(counts);
                            // Choosing what is received or sent shows its files in Activity
                            var transfers=new TransferSummary(()=>Run(async()=>{page="activity";await Refresh();}));Body.Children.Add(transfers.Element);
                            async Task CountDevices() {
                                var folders=await Call("engine.folders");
                                // The engine lists this device among its devices
                                var devices=(await Call("engine.devices"))?.AsArray().Where(device=>S(device,"ID")!=S(status,"deviceID")).ToList()??[];
                                var folderCount=folders?.AsArray().Count??0;
                                counts.Text=(folderCount==1?"1 folder":$"{folderCount:N0} folders")+" · "+(devices.Count==1?"1 device":$"{devices.Count:N0} devices");
                                transfers.Peers(devices.Count(device=>B(device,"Connected")),devices.Count);
                            }
                            await CountDevices();transfers.Show(await Call("desktop.engine.transfers"));
                            Live(async(_,changed)=>{
                                if(changed)await CountDevices();
                                // Speeds need samples while anything moves, even between engine events
                                if(changed || transfers.Active)transfers.Show(await Call("desktop.engine.transfers"));
                                return true;
                            });
                        }
                        break;
                    case "storage":
                        Text("Storage",true);
                        if(mode=="files") {var storage=await Call("files.storage");Text($"Download cache: {N(storage,"used")/1048576.0:N1} MiB of {N(storage,"budget")/1048576.0:N0} MiB");}
                        else {
                            static string LocalFiles(JsonNode? statistics)=>"Local files: "+N(statistics?["totals"]?["Local"],"Files").ToString("N0")+" · "+Bytes(N(statistics?["totals"]?["Local"],"Bytes"));
                            var local=LiveText();local.IsTextSelectionEnabled=true;Body.Children.Add(local);local.Text=LocalFiles(await Call("engine.stats"));
                            Live(async(_,changed)=>{if(changed)local.Text=LocalFiles(await Call("engine.stats"));return true;});
                        }
                        break;
                }
            } catch(OperationCanceledException) { throw; }
            // A section that failed is built again on the next change, not polled
            catch(Exception error) { Text("This section is unavailable: "+error.Message); Live((_,changed)=>Task.FromResult(!changed)); }
        }
    }
}
