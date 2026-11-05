// Copyright (C) 2026 Gordon
// SPDX-License-Identifier: MPL-2.0
using Microsoft.UI.Xaml;
using Synctrain_App.Services;

namespace Synctrain_App;

public sealed partial class MainWindow : Window
{
    private readonly TrayIcon tray;
    private readonly HostClient host = new();
    private bool quitting;

    public MainWindow()
    {
        InitializeComponent();
        ExtendsContentIntoTitleBar = true;
        SetTitleBar(AppTitleBar);
        AppWindow.SetIcon("Assets/AppIcon.ico");
        AppWindow.Resize(new Windows.Graphics.SizeInt32(1100, 780));
        RootFrame.Navigate(typeof(MainPage));
        tray = new TrayIcon(this, HandleTray);
        if(RootFrame.Content is MainPage page)page.ModeChanged+=mode=>tray.Synchronize=mode=="synchronize";
        AppWindow.Closing += async (_, args) => { if (!quitting) { args.Cancel = true; if (RootFrame.Content is MainPage current && await current.CanClose()) AppWindow.Hide(); } };
        Closed += (_, _) => tray.Dispose();
    }

    public void ShowError(string message) { if (RootFrame.Content is MainPage page) page.ShowError(message); }
    public void Open() { AppWindow.Show(); Activate(); }
    public async Task PairLink(string link)
    {
        Open();
        if (RootFrame.Content is MainPage page) await page.PairLink(link);
    }
    private async void HandleTray(uint command)
    {
        try
        {
            if (command == 1) {await host.EnsureRunning();Open();}
            else if (command == 6) { Open(); if (RootFrame.Content is MainPage current) await current.OpenSettings(); }
            else if (command == 5) { if(RootFrame.Content is MainPage current && !await current.CanClose())return; await host.Call("quit"); quitting = true; Close(); Application.Current.Exit(); }
            else await host.Call("engine.action", new { Action = command switch { 2 => "pauseAll", 3 => "resumeAll", 4 => "rescanAll", _ => "" } });
        }
        catch (Exception error)
        {
            Open();
            if (RootFrame.Content is MainPage page) page.ShowError(error.Message);
        }
    }
}
