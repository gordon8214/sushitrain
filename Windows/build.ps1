# Copyright (C) 2026 Gordon
# SPDX-License-Identifier: MPL-2.0
[CmdletBinding()]
param(
    [ValidateSet('ARM64','x64')][string[]]$Architecture = @('ARM64','x64'),
    [switch]$DevelopmentSign,
    [switch]$SkipPackage
)
$ErrorActionPreference = 'Stop'
$ProgressPreference = 'SilentlyContinue'
$root = Split-Path $PSScriptRoot -Parent
if ($root.StartsWith('\\')) { throw 'Build from a checkout on local NTFS, not a Parallels shared folder.' }
$go = Get-Command go -ErrorAction SilentlyContinue
if (-not $go) { $go = Get-Command 'C:\Program Files\Go\bin\go.exe' -ErrorAction Stop }
$vswhere = "${env:ProgramFiles(x86)}\Microsoft Visual Studio\Installer\vswhere.exe"
$vs = & $vswhere -latest -products '*' -requires Microsoft.VisualStudio.Component.VC.Tools.x86.x64 -property installationPath
if (-not $vs) { throw 'Visual Studio with C++ desktop tools is required.' }
$msbuild = Join-Path $vs 'MSBuild\Current\Bin\MSBuild.exe'
$compiler = Get-ChildItem "$vs\VC\Tools\MSVC" -Directory | Sort-Object { [version]$_.Name } -Descending | Select-Object -First 1
$hostArchitecture = [System.Runtime.InteropServices.RuntimeInformation]::ProcessArchitecture.ToString().ToLowerInvariant()
$engineVersion = [regex]::Match((Get-Content "$root\SushitrainCore\Makefile" -Raw), '(?m)^ENGINE_VERSION\s*=\s*(\S+)').Groups[1].Value
if (-not $engineVersion) { throw 'Cannot read the engine version from SushitrainCore/Makefile.' }
$certificate = $null
if ($DevelopmentSign) {
    $certificate = Get-ChildItem Cert:\CurrentUser\My | Where-Object { $_.Subject -eq 'CN=Gordon Byrnes' -and $_.FriendlyName -eq 'Synctrain development signing' -and $_.NotAfter -gt (Get-Date).AddDays(7) } | Select-Object -First 1
    if (-not $certificate) {
        $certificate = New-SelfSignedCertificate -Type CodeSigningCert -Subject 'CN=Gordon Byrnes' -FriendlyName 'Synctrain development signing' -CertStoreLocation Cert:\CurrentUser\My -HashAlgorithm SHA256
    }
}
$oldCgo = $env:CGO_ENABLED
$oldArch = $env:GOARCH
$oldOS = $env:GOOS
try {
    $env:CGO_ENABLED = '0'
    $env:GOOS = 'windows'
    foreach ($arch in $Architecture) {
        $goArch = if ($arch -eq 'ARM64') { 'arm64' } else { 'amd64' }
        $env:GOARCH = $goArch
        $output = Join-Path $PSScriptRoot "build\$arch"
        New-Item -ItemType Directory -Force $output | Out-Null
        Push-Location "$root\SushitrainCore"
        try {
            $flags = "-X github.com/syncthing/syncthing/lib/build.Version=$engineVersion -X github.com/syncthing/syncthing/lib/build.Tags=noupgrade,synctrain -X github.com/syncthing/syncthing/lib/build.User=synctrain -X github.com/syncthing/syncthing/lib/build.Host=gordon8214.github.io"
            $ErrorActionPreference = 'Continue'
            $notices = & $go.Source tool go-licenses report ./cmd/synctrain-host --template=./notices.template.html 2> "$output\notices.log"
            $noticeExit = $LASTEXITCODE
            $ErrorActionPreference = 'Stop'
            if ($noticeExit -ne 0) { throw 'Open-source notices could not be generated.' }
            $notices | Set-Content -Encoding utf8 "$output\GoNotices.html"
            $ErrorActionPreference = 'Continue'
            & $go.Source build -mod=readonly -trimpath -tags noupgrade -ldflags $flags -o "$output\synctrain-host.exe" ./cmd/synctrain-host
            $hostExit = $LASTEXITCODE
            $ErrorActionPreference = 'Stop'
            if ($hostExit -ne 0) { throw 'The Go host did not build.' }
        } finally { Pop-Location }
        & $msbuild "$PSScriptRoot\Synctrain.Provider\Synctrain.Provider.vcxproj" /nologo /v:minimal /p:Configuration=Release "/p:Platform=$arch"
        if ($LASTEXITCODE -ne 0) { throw 'The native provider did not build.' }
        $symbolTool = Join-Path $compiler.FullName "bin\Host$hostArchitecture\$($arch.ToLowerInvariant())\mspdbcmf.exe"
        $arguments = @('build', "$PSScriptRoot\Synctrain.App\Synctrain.App.csproj", '-c', 'Release', "-p:Platform=$arch", "-p:RuntimeIdentifier=win-$($arch.ToLowerInvariant())", '-p:SelfContained=true', '-p:WindowsAppSDKSelfContained=true')
        if (Test-Path $symbolTool) { $arguments += "-p:MsPdbCmfExeFullpath=$symbolTool" }
        if (-not $SkipPackage) {
            $arguments += @('-p:GenerateAppxPackageOnBuild=true','-p:AppxBundle=Never','-p:AppxSymbolPackageEnabled=false','-p:UapAppxPackageBuildMode=SideloadOnly',"-p:AppxPackageDir=$output\Packages\")
            if ($certificate) { $arguments += @('-p:AppxPackageSigningEnabled=true',"-p:PackageCertificateThumbprint=$($certificate.Thumbprint)") }
            else { $arguments += '-p:AppxPackageSigningEnabled=false' }
        }
        & dotnet @arguments
        if ($LASTEXITCODE -ne 0) { throw "The $arch app did not build." }
        if ($certificate -and -not $SkipPackage) { Export-Certificate -Cert $certificate -FilePath "$output\Synctrain.Development.cer" | Out-Null }
    }
} finally { $env:CGO_ENABLED = $oldCgo; $env:GOARCH = $oldArch; $env:GOOS = $oldOS }
