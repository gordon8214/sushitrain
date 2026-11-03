# Copyright (C) 2026 Gordon
# SPDX-License-Identifier: MPL-2.0
[CmdletBinding()]
param([ValidateSet('ARM64','x64')][string]$Architecture = 'ARM64')
$ErrorActionPreference = 'Stop'
$root = Split-Path $PSScriptRoot -Parent
$provider = Join-Path $PSScriptRoot "build\$Architecture\Synctrain.Provider.exe"
if (-not (Test-Path $provider)) { throw 'Build the native provider with Windows/build.ps1 first.' }
$nativeArchitecture = [System.Runtime.InteropServices.RuntimeInformation]::ProcessArchitecture.ToString()
if ($Architecture -ne $nativeArchitecture) { throw "Run native tests on $Architecture Windows; this process is $nativeArchitecture." }
$previousProvider = $env:SYNCTRAIN_TEST_PROVIDER
$env:SYNCTRAIN_TEST_PROVIDER = $provider
Push-Location "$root\SushitrainCore"
try {
    # Shared suites normally take about a minute; native Cloud Files takes 40–60
    # seconds. Every Go test process has a hard budget and prints stacks on expiry.
    $ErrorActionPreference = 'Continue'
    & 'C:\Program Files\Go\bin\go.exe' test -count=1 -mod=readonly -tags noupgrade -timeout 180s ./remoteapi ./remoteclient ./adminsock ./files ./gateway ./gatewayadmin ./sthome ./src ./winplatform ./winhost ./cmd/synctrain-host
    $testExit = $LASTEXITCODE
    $ErrorActionPreference = 'Stop'
    if ($testExit -ne 0) { throw 'Windows tests failed.' }
} finally {
    Pop-Location
    $env:SYNCTRAIN_TEST_PROVIDER = $previousProvider
}
