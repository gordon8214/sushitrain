# Copyright (C) 2026 Gordon
# SPDX-License-Identifier: MPL-2.0
# Generate Windows packaging sizes from the existing, unchanged Synctrain logo.
[CmdletBinding()]
param([string]$LogoPath = (Join-Path $PSScriptRoot '..\..\Docs\logo.png'))
$ErrorActionPreference='Stop'
Add-Type -AssemblyName System.Drawing
$assets=Join-Path $PSScriptRoot '..\Synctrain.App\Assets'
$source=[System.Drawing.Image]::FromFile((Resolve-Path $LogoPath).ProviderPath)
try {
    $sizes=@{
        'StoreLogo.png'=@(50,50)
        'Square44x44Logo.scale-200.png'=@(88,88)
        'Square150x150Logo.scale-200.png'=@(300,300)
        'Wide310x150Logo.scale-200.png'=@(620,300)
        'SplashScreen.scale-200.png'=@(1240,600)
        'LockScreenLogo.scale-200.png'=@(48,48)
        'Square44x44Logo.targetsize-24_altform-unplated.png'=@(24,24)
        'Square44x44Logo.targetsize-48_altform-lightunplated.png'=@(48,48)
    }
    foreach($name in $sizes.Keys) {
        $width,$height=$sizes[$name]
        $bitmap=New-Object System.Drawing.Bitmap $width,$height
        $graphics=[System.Drawing.Graphics]::FromImage($bitmap)
        try {
            $graphics.Clear([System.Drawing.Color]::Transparent)
            $graphics.InterpolationMode=[System.Drawing.Drawing2D.InterpolationMode]::HighQualityBicubic
            $side=[Math]::Min($width,$height)
            $graphics.DrawImage($source,[int](($width-$side)/2),[int](($height-$side)/2),$side,$side)
            $bitmap.Save((Join-Path $assets $name),[System.Drawing.Imaging.ImageFormat]::Png)
        } finally {$graphics.Dispose();$bitmap.Dispose()}
    }
    # Windows supports a PNG-backed ICO. Embed the original 256px image without
    # modifying its pixels; shell rendering chooses the display size.
    $png=[System.IO.File]::ReadAllBytes((Resolve-Path $LogoPath).ProviderPath)
    $stream=[System.IO.File]::Create((Join-Path $assets 'AppIcon.ico'))
    $writer=New-Object System.IO.BinaryWriter $stream
    try {
        $writer.Write([UInt16]0);$writer.Write([UInt16]1);$writer.Write([UInt16]1)
        $writer.Write([Byte]0);$writer.Write([Byte]0);$writer.Write([Byte]0);$writer.Write([Byte]0)
        $writer.Write([UInt16]1);$writer.Write([UInt16]32);$writer.Write([UInt32]$png.Length);$writer.Write([UInt32]22);$writer.Write($png)
    } finally {$writer.Dispose();$stream.Dispose()}
} finally {$source.Dispose()}
