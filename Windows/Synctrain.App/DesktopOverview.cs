// Copyright (C) 2026 Gordon. SPDX-License-Identifier: MPL-2.0
using Microsoft.UI.Xaml.Controls;
using System.Text.Json.Nodes;

namespace Synctrain_App;

public sealed partial class MainPage
{
    private async Task OverviewPage(string mode)
    {
        JsonNode? statistics=null;
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
                            var folders=await Call("engine.folders");var devices=await Call("engine.devices");Text($"{folders?.AsArray().Count??0} folders · {devices?.AsArray().Count??0} devices");
                            statistics=await Call("engine.stats");
                            foreach(var direction in new[]{"download","upload"})if(statistics?[direction] is JsonNode progress) {
                                Text((direction=="download"?"Downloading":"Uploading")+": "+S(progress,"FilesTotal")+" files");
                                Body.Children.Add(new ProgressBar { Minimum=0,Maximum=100,Value=(progress["Percentage"]?.GetValue<double>()??0)*100 });
                            }
                        }
                        break;
                    case "storage":
                        Text("Storage",true);
                        if(mode=="files") {var storage=await Call("files.storage");Text($"Download cache: {N(storage,"used")/1048576.0:N1} MiB of {N(storage,"budget")/1048576.0:N0} MiB");}
                        else {statistics??=await Call("engine.stats");Text("Local files: "+S(statistics?["totals"]?["Local"],"Files")+" · "+S(statistics?["totals"]?["Local"],"Bytes")+" bytes");}
                        break;
                }
            } catch(OperationCanceledException) { throw; }
            catch(Exception error) { Text("This section is unavailable: "+error.Message); }
        }
    }
}
