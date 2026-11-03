// Copyright (C) 2026 Gordon
// SPDX-License-Identifier: MPL-2.0
using Microsoft.UI.Xaml;
using System.Runtime.InteropServices;

namespace Synctrain_App.Services;

internal sealed class TrayIcon : IDisposable
{
    private const uint CallbackMessage = 0x8000 + 51;
    private readonly nint window;
    private readonly nint icon;
    private readonly SubclassProc procedure;
    private readonly Action<uint> command;
    private readonly uint taskbarCreated;
    private bool disposed;
    public bool Synchronize {get;set;}

    public TrayIcon(Window owner, Action<uint> command)
    {
        this.command = command;
        window = WinRT.Interop.WindowNative.GetWindowHandle(owner);
        icon = LoadImage(0, Path.Combine(AppContext.BaseDirectory, "Assets", "AppIcon.ico"), 1, 0, 0, 0x10 | 0x40);
        if (icon == 0) throw new System.ComponentModel.Win32Exception(Marshal.GetLastWin32Error());
        taskbarCreated = RegisterWindowMessage("TaskbarCreated");
        procedure = WindowProcedure;
        if (!SetWindowSubclass(window, procedure, 51, 0)) throw new System.ComponentModel.Win32Exception(Marshal.GetLastWin32Error());
        Add();
    }

    private NotifyIconData Data() => new()
    {
        Size = (uint)Marshal.SizeOf<NotifyIconData>(), Window = window, ID = 1,
        Flags = 1 | 2 | 4, Callback = CallbackMessage, Icon = icon, Tip = "Synctrain",
        Info = "", InfoTitle = ""
    };
    private void Add() { var data = Data(); if (!ShellNotifyIcon(0, ref data)) throw new System.ComponentModel.Win32Exception(Marshal.GetLastWin32Error()); }
    private nint WindowProcedure(nint handle, uint message, nuint wParam, nint lParam, nuint subclass, nuint reference)
    {
        if (message == taskbarCreated) { try { Add(); } catch (System.ComponentModel.Win32Exception error) {System.Diagnostics.Debug.WriteLine(error);} return 0; }
        if (message == CallbackMessage)
        {
            var notification = (uint)(long)lParam;
            if (notification is 0x202 or 0x203) command(1);
            if (notification == 0x205) ShowMenu();
            return 0;
        }
        return DefSubclassProc(handle, message, wParam, lParam);
    }
    private void ShowMenu()
    {
        var menu = CreatePopupMenu();
        if (menu == 0) return;
        try
        {
            AppendMenu(menu, 0, 1, "Open Synctrain");
            if(Synchronize){
            AppendMenu(menu, 0, 2, "Pause All Devices");
            AppendMenu(menu, 0, 3, "Resume All Devices");
            AppendMenu(menu, 0, 4, "Rescan All Folders");
            }
            AppendMenu(menu, 0x800, 0, null);
            AppendMenu(menu, 0, 5, "Quit Synctrain");
            GetCursorPos(out var point); SetForegroundWindow(window);
            var selected = TrackPopupMenu(menu, 0x100 | 2, point.X, point.Y, 0, window, 0);
            PostMessage(window, 0, 0, 0);
            if (selected != 0) command(selected);
        }
        finally { DestroyMenu(menu); }
    }
    public void Dispose()
    {
        if (disposed) return; disposed = true;
        var data = Data(); ShellNotifyIcon(2, ref data);
        RemoveWindowSubclass(window, procedure, 51); DestroyIcon(icon);
    }

    [StructLayout(LayoutKind.Sequential, CharSet = CharSet.Unicode)]
    private struct NotifyIconData
    {
        public uint Size; public nint Window; public uint ID, Flags, Callback; public nint Icon;
        [MarshalAs(UnmanagedType.ByValTStr, SizeConst = 128)] public string Tip;
        public uint State, StateMask;
        [MarshalAs(UnmanagedType.ByValTStr, SizeConst = 256)] public string Info;
        public uint Timeout;
        [MarshalAs(UnmanagedType.ByValTStr, SizeConst = 64)] public string InfoTitle;
        public uint InfoFlags; public Guid Guid; public nint BalloonIcon;
    }
    [StructLayout(LayoutKind.Sequential)] private struct Point { public int X, Y; }
    private delegate nint SubclassProc(nint window, uint message, nuint wParam, nint lParam, nuint id, nuint data);
    [DllImport("comctl32.dll")] [return: MarshalAs(UnmanagedType.Bool)] private static extern bool SetWindowSubclass(nint window, SubclassProc procedure, nuint id, nuint data);
    [DllImport("comctl32.dll")] [return: MarshalAs(UnmanagedType.Bool)] private static extern bool RemoveWindowSubclass(nint window, SubclassProc procedure, nuint id);
    [DllImport("comctl32.dll")] private static extern nint DefSubclassProc(nint window, uint message, nuint wParam, nint lParam);
    [DllImport("shell32.dll", EntryPoint = "Shell_NotifyIconW", CharSet = CharSet.Unicode, SetLastError = true)] [return: MarshalAs(UnmanagedType.Bool)] private static extern bool ShellNotifyIcon(uint message, ref NotifyIconData data);
    [DllImport("user32.dll", EntryPoint = "LoadImageW", CharSet = CharSet.Unicode, SetLastError = true)] private static extern nint LoadImage(nint instance, string name, uint type, int width, int height, uint flags);
    [DllImport("user32.dll")] [return: MarshalAs(UnmanagedType.Bool)] private static extern bool DestroyIcon(nint icon);
    [DllImport("user32.dll", EntryPoint = "RegisterWindowMessageW", CharSet = CharSet.Unicode)] private static extern uint RegisterWindowMessage(string message);
    [DllImport("user32.dll")] private static extern nint CreatePopupMenu();
    [DllImport("user32.dll", EntryPoint = "AppendMenuW", CharSet = CharSet.Unicode)] [return: MarshalAs(UnmanagedType.Bool)] private static extern bool AppendMenu(nint menu, uint flags, nuint id, string? text);
    [DllImport("user32.dll")] private static extern uint TrackPopupMenu(nint menu, uint flags, int x, int y, int reserved, nint window, nint rect);
    [DllImport("user32.dll")] [return: MarshalAs(UnmanagedType.Bool)] private static extern bool DestroyMenu(nint menu);
    [DllImport("user32.dll")] [return: MarshalAs(UnmanagedType.Bool)] private static extern bool GetCursorPos(out Point point);
    [DllImport("user32.dll")] [return: MarshalAs(UnmanagedType.Bool)] private static extern bool SetForegroundWindow(nint window);
    [DllImport("user32.dll")] [return: MarshalAs(UnmanagedType.Bool)] private static extern bool PostMessage(nint window, uint message, nuint wParam, nint lParam);
}
