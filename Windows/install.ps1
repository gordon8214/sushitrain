# Copyright (C) 2026 Gordon
# SPDX-License-Identifier: MPL-2.0
[CmdletBinding()]
param([Parameter(Mandatory=$true)][string]$Package, [string]$DevelopmentCertificate)
$ErrorActionPreference = 'Stop'
if ($DevelopmentCertificate) {
    $certificate = New-Object System.Security.Cryptography.X509Certificates.X509Certificate2((Resolve-Path $DevelopmentCertificate).Path)
    if ($certificate.Subject -ne 'CN=Gordon Byrnes') { throw 'Unexpected development certificate publisher.' }
    if (-not (Test-Path "Cert:\LocalMachine\TrustedPeople\$($certificate.Thumbprint)")) {
        Import-Certificate -FilePath $DevelopmentCertificate -CertStoreLocation Cert:\LocalMachine\TrustedPeople | Out-Null
    }
}
Add-AppxPackage -Path (Resolve-Path $Package).Path
$app = Get-AppxPackage com.gordonbyrnes.Synctrain
if (-not $app) { throw 'Windows did not register Synctrain.' }
Write-Output "Installed $($app.PackageFullName)"
