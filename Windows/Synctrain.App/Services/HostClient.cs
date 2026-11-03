// Copyright (C) 2026 Gordon
// SPDX-License-Identifier: MPL-2.0
using System.Diagnostics;
using System.IO.Pipes;
using System.Security.Principal;
using System.Runtime.InteropServices;
using Microsoft.Win32.SafeHandles;
using System.Text;
using System.Text.Json;
using System.Text.Json.Nodes;

namespace Synctrain_App.Services;

internal sealed class HostClient
{
    private readonly string pipeName = "Synctrain-" + WindowsIdentity.GetCurrent().User!.Value;

    public async Task<JsonNode?> Call(string method, object? parameters = null, int timeoutSeconds = 60)
    {
        if(method=="quit"){
            try{return await CallPipe(pipeName+"-Supervisor",method,parameters,timeoutSeconds);}
            catch(Exception error)when(error is IOException or TimeoutException or OperationCanceledException){}
        }
        return await CallPipe(pipeName,method,parameters,timeoutSeconds);
    }
    private async Task<JsonNode?> CallPipe(string endpoint,string method,object? parameters,int timeoutSeconds)
    {
        using var timeout = new CancellationTokenSource(TimeSpan.FromSeconds(timeoutSeconds));
        using var pipe = new NamedPipeClientStream(".", endpoint, PipeDirection.InOut, PipeOptions.Asynchronous);
        await pipe.ConnectAsync(5000, timeout.Token);
        AuthenticateServer(pipe);
        var request = JsonSerializer.Serialize(new { method, @params = parameters }) + "\n";
        await pipe.WriteAsync(Encoding.UTF8.GetBytes(request), timeout.Token);
        await pipe.FlushAsync(timeout.Token);
        using var reader = new StreamReader(pipe, Encoding.UTF8, false, 65536, true);
        var line = await reader.ReadLineAsync(timeout.Token) ?? throw new IOException("The background agent stopped responding.");
        var response = JsonNode.Parse(line) ?? throw new IOException("The background agent returned an invalid response.");
        if (response["error"] is JsonNode error) throw new InvalidOperationException(error.GetValue<string>());
        return response["result"]?.DeepClone();
    }

    private static void AuthenticateServer(NamedPipeClientStream pipe)
    {
        if (!GetNamedPipeServerProcessId(pipe.SafePipeHandle, out var id)) throw new System.ComponentModel.Win32Exception(Marshal.GetLastWin32Error());
        using var process = Process.GetProcessById(checked((int)id));
        if (!OpenProcessToken(process.SafeHandle, 8, out var token)) throw new System.ComponentModel.Win32Exception(Marshal.GetLastWin32Error());
        using (token)
        using (var server = new WindowsIdentity(token.DangerousGetHandle()))
        using (var current = WindowsIdentity.GetCurrent())
            if (current.User is null || server.User is null || !server.User.Equals(current.User)) throw new System.Security.SecurityException("The Synctrain pipe belongs to another Windows user.");
    }
    [DllImport("kernel32.dll", SetLastError = true)] [return: MarshalAs(UnmanagedType.Bool)] private static extern bool GetNamedPipeServerProcessId(SafePipeHandle pipe, out uint id);
    [DllImport("advapi32.dll", SetLastError = true)] [return: MarshalAs(UnmanagedType.Bool)] private static extern bool OpenProcessToken(SafeProcessHandle process, uint access, out SafeAccessTokenHandle token);

    public async Task EnsureRunning()
    {
        try { await Call("status", timeoutSeconds: 6); return; }
        catch (Exception error) when (error is IOException or TimeoutException or OperationCanceledException) { }
        Process.Start(new ProcessStartInfo(Path.Combine(AppContext.BaseDirectory, "synctrain-host.exe")) { UseShellExecute = false, CreateNoWindow = true });
        Exception? last = null;
        for (var attempt = 0; attempt < 20; attempt++)
        {
            try { await Call("status", timeoutSeconds: 6); return; }
            catch (Exception error) when (error is IOException or TimeoutException or OperationCanceledException)
            { last = error; await Task.Delay(250); }
        }
        throw new IOException("The background agent could not start. Check the Synctrain host log in Local AppData.", last);
    }
}
