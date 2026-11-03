# Copyright (C) 2026 Gordon
# SPDX-License-Identifier: MPL-2.0
[CmdletBinding()]
param([Parameter(Mandatory=$true)][string]$Path)
$ErrorActionPreference='Stop'
Add-Type -TypeDefinition @'
using System;
using System.Runtime.InteropServices;
public static class SynctrainShellAcceptance {
 [ComImport,Guid("43826D1E-E718-42EE-BC55-A1E261C37BFE"),InterfaceType(ComInterfaceType.InterfaceIsIUnknown)] interface IShellItem {}
 [ComImport,Guid("7F73BE3F-FB79-493C-A6C7-7EE14E245841"),InterfaceType(ComInterfaceType.InterfaceIsIUnknown)] interface IInitializeWithItem {void Initialize(IShellItem item,uint mode);}
 [ComImport,Guid("E357FCCD-A995-4576-B01F-234630154E96"),InterfaceType(ComInterfaceType.InterfaceIsIUnknown)] interface IThumbnailProvider {void GetThumbnail(uint size,out IntPtr bitmap,out uint alpha);}
 [DllImport("ole32.dll")] static extern int CoCreateInstance(ref Guid clsid,IntPtr outer,uint context,ref Guid iid,out IntPtr result);
 [DllImport("shell32.dll",CharSet=CharSet.Unicode,PreserveSig=false)] static extern void SHCreateItemFromParsingName(string path,IntPtr context,ref Guid iid,out IShellItem item);
 [DllImport("kernel32.dll",CharSet=CharSet.Unicode,SetLastError=true)] static extern IntPtr CreateFile(string path,uint access,uint share,IntPtr security,uint disposition,uint flags,IntPtr template);
 [DllImport("kernel32.dll")] static extern bool CloseHandle(IntPtr handle);
 [DllImport("CldApi.dll")] static extern int CfGetPlaceholderInfo(IntPtr file,int type,IntPtr info,uint size,out uint returned);
 [DllImport("gdi32.dll")] static extern bool DeleteObject(IntPtr bitmap);
 static long DiskBytes(string path) {
  var file=CreateFile(path,0x80,7,IntPtr.Zero,3,0x200000,IntPtr.Zero);
  if(file==new IntPtr(-1))throw new System.ComponentModel.Win32Exception();
  var buffer=Marshal.AllocHGlobal(8192);
  try{uint returned;Marshal.ThrowExceptionForHR(CfGetPlaceholderInfo(file,1,buffer,8192,out returned));return Marshal.ReadInt64(buffer);}
  finally{Marshal.FreeHGlobal(buffer);CloseHandle(file);}
 }
 public static void Probe(string path) {
  // Activation goes through the installed package's COM registration.
  foreach(var suffix in new[]{"29CE","29CF","29D0"}) {
   var clsid=new Guid("36F7C1C1-6702-4E7E-9FF8-04970455"+suffix);var iid=new Guid("00000000-0000-0000-C000-000000000046");IntPtr instance;
   Marshal.ThrowExceptionForHR(CoCreateInstance(ref clsid,IntPtr.Zero,4,ref iid,out instance));Marshal.Release(instance);
  }
  var before=DiskBytes(path);var thumbnailClass=new Guid("36F7C1C1-6702-4E7E-9FF8-0497045529CE");var thumbnailID=typeof(IThumbnailProvider).GUID;IntPtr raw;
  Marshal.ThrowExceptionForHR(CoCreateInstance(ref thumbnailClass,IntPtr.Zero,4,ref thumbnailID,out raw));
  object provider=Marshal.GetObjectForIUnknown(raw);Marshal.Release(raw);
  IShellItem item;var shellID=typeof(IShellItem).GUID;SHCreateItemFromParsingName(path,IntPtr.Zero,ref shellID,out item);
  try{((IInitializeWithItem)provider).Initialize(item,0);IntPtr bitmap;uint alpha;((IThumbnailProvider)provider).GetThumbnail(256,out bitmap,out alpha);if(bitmap==IntPtr.Zero)throw new Exception("Empty thumbnail");DeleteObject(bitmap);}
  finally{Marshal.ReleaseComObject(item);Marshal.ReleaseComObject(provider);}
  var after=DiskBytes(path);if(before!=after)throw new Exception("Thumbnail extraction hydrated the placeholder");
  Console.WriteLine("Packaged COM classes activated; thumbnail succeeded; on-disk bytes {0} -> {1}",before,after);
 }
}
'@
[SynctrainShellAcceptance]::Probe($Path)
