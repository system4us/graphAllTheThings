# gatt installer for Windows.
#   irm https://raw.githubusercontent.com/system4us/graphAllTheThings/main/scripts/install.ps1 | iex
# Env overrides: GATT_VERSION (tag, default latest), GATT_INSTALL_DIR (default %LOCALAPPDATA%\gatt\bin)
$ErrorActionPreference = "Stop"

$repo = "system4us/graphAllTheThings"

# Only amd64 is published for Windows; ARM64 Windows runs it via emulation.
$arch = "amd64"

$version = $env:GATT_VERSION
if (-not $version) {
    $version = (Invoke-RestMethod "https://api.github.com/repos/$repo/releases/latest").tag_name
}

$asset = "gatt-windows-$arch.exe"
$url = "https://github.com/$repo/releases/download/$version/$asset"

$dir = $env:GATT_INSTALL_DIR
if (-not $dir) { $dir = Join-Path $env:LOCALAPPDATA "gatt\bin" }
New-Item -ItemType Directory -Force -Path $dir | Out-Null
$exe = Join-Path $dir "gatt.exe"

Write-Host "downloading gatt $version (windows/$arch)..."
Invoke-WebRequest -Uri $url -OutFile $exe

try {
    $sums = (Invoke-WebRequest "https://github.com/$repo/releases/download/$version/checksums.txt").Content
    $expected = ($sums -split "`n" | Where-Object { $_ -match " $asset$" }) -split "\s+" | Select-Object -First 1
    if ($expected) {
        $actual = (Get-FileHash $exe -Algorithm SHA256).Hash.ToLower()
        if ($expected -ne $actual) { throw "checksum mismatch" }
    }
} catch [System.Net.WebException] {
    Write-Warning "checksums.txt not found; skipping verification"
}

$userPath = [Environment]::GetEnvironmentVariable("Path", "User")
if ($userPath -notlike "*$dir*") {
    [Environment]::SetEnvironmentVariable("Path", "$userPath;$dir", "User")
    Write-Host "added $dir to user PATH (restart your terminal)"
}

Write-Host "installed $exe"
& $exe version
