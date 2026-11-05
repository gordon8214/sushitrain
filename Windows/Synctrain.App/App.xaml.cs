// Copyright (C) 2026 Gordon
// SPDX-License-Identifier: MPL-2.0
using Microsoft.UI.Xaml;
using Microsoft.Windows.AppLifecycle;
using Synctrain_App.Services;
using Windows.ApplicationModel.Activation;

namespace Synctrain_App;

public partial class App : Application
{
    private MainWindow? window;
    private AppInstance? instance;
    public App() { InitializeComponent(); }

    protected override async void OnLaunched(Microsoft.UI.Xaml.LaunchActivatedEventArgs args)
    {
        try {
        var activation = AppInstance.GetCurrent().GetActivatedEventArgs();
        instance = AppInstance.FindOrRegisterForKey(HostClient.IsolatedTest ? "Synctrain.UIFixture" : "Synctrain");
        if (!instance.IsCurrent) { await instance.RedirectActivationToAsync(activation); Exit(); return; }
        window = new MainWindow();
        instance.Activated += (_, incoming) => window.DispatcherQueue.TryEnqueue(async () =>
        {
            try {
                await new HostClient().EnsureRunning();window.Open();
                if (incoming.Data is IProtocolActivatedEventArgs protocol) await window.PairLink(protocol.Uri.AbsoluteUri);
            } catch(Exception error) {window.Open();window.ShowError(error.Message);}
        });
        if (activation.Kind == ExtendedActivationKind.StartupTask) await new HostClient().EnsureRunning();
        else window.Open();
        if (activation.Data is IProtocolActivatedEventArgs pairing) await window.PairLink(pairing.Uri.AbsoluteUri);
        } catch (Exception error) {
            window ??= new MainWindow(); window.Open(); window.ShowError(error.Message);
        }
    }
}
