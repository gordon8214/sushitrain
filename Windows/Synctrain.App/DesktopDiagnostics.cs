// Copyright (C) 2026 Gordon. SPDX-License-Identifier: MPL-2.0
using Microsoft.UI.Xaml;
using Microsoft.UI.Xaml.Controls;

namespace Synctrain_App;

public sealed partial class MainPage
{
    private async Task Diagnostics()
    {
        Detail(Diagnostics);
        ShowBody(); Body.Children.Clear(); Commands.PrimaryCommands.Clear(); Heading.Text="Diagnostic Logging and Maintenance";
        Command("Back",Symbol.Back,LeaveDetails);
        Command("Activity Log",Symbol.List,ShowLogs);
        Body.Children.Add(ActionButton("Run maintenance…",async()=>{if(await Confirm("Run maintenance?","The engine performs its existing database maintenance. This may temporarily pause synchronization.")){await Call("desktop.engine.action",new { Action="maintenance" },150);await Refresh();}}));
        Body.Children.Add(ActionButton("Reset log levels",async()=>{await Call("desktop.engine.action",new { Action="resetLogLevels" });await Refresh();}));
        Text("Connection Diagnostics",true);
        var diagnostics=await Call("desktop.engine.diagnostics");
        foreach(var key in new[]{"engineVersion","listening","configuredListeners","globalDiscovery","localDiscovery","discoveryServers","relays","nat","connectedDevices"})
            if(diagnostics?[key] is not null) Text(key+": "+S(diagnostics,key));
        Text("Logging",true);
        foreach(var facility in (await Call("desktop.engine.action",new { Action="logFacilities" }))?.AsArray()??[]) {
            var name=S(facility,"name");
            Row(name,S(facility,"description"),ActionButton("Change log level…",async()=>{
                var choices=new ComboBox { Header="Log level",ItemsSource=new[]{"debug","info","warn","error"},SelectedIndex=1,MinWidth=240 };
                if(await new ContentDialog { XamlRoot=XamlRoot,Title=name,Content=choices,PrimaryButtonText="Save",CloseButtonText="Cancel" }.ShowAsync()==ContentDialogResult.Primary) {
                    await Call("desktop.engine.action",new { Action="logLevel",Name=name,ID=choices.SelectedItem.ToString() });await Refresh();
                }
            }));
        }
    }
}
