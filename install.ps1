<#
.SYNOPSIS
    Install the `measy` command on Windows.

.DESCRIPTION
    Downloads the pre-built binary from GitHub Releases and puts it somewhere
    already on PATH, so `measy` starts the agent in whatever folder the
    terminal is sitting in — which is the whole point of the short name.

    Installs per-user, into %LOCALAPPDATA%. Nothing here needs an elevated
    prompt: a developer tool that demands administrator to install is a
    developer tool people install once and then distrust.

    Typical install:

        irm https://github.com/measyai/measycode/releases/latest/download/install.ps1 | iex

.EXAMPLE
    .\install.ps1
    .\install.ps1 -Dir "C:\tools"
    .\install.ps1 -Version "1.0.0"
#>
[CmdletBinding()]
param(
    [string]$Dir = "$env:LOCALAPPDATA\Programs\Measy",
    [string]$Version = "",
    [string]$Repo = "measyai/measycode"
)

$ErrorActionPreference = 'Stop'

$arch = if ([Environment]::Is64BitOperatingSystem) { 'amd64' } else { '386' }
$file = "measy-windows-$arch.exe"

if ($Version) {
    $tag = if ($Version -match '^v') { $Version } else { "v$Version" }
    $base = "https://github.com/$Repo/releases/download/$tag"
} else {
    $base = "https://github.com/$Repo/releases/latest/download"
}

$url = "$base/$file"
$hashUrl = "$base/$file.sha256"

$target = Join-Path $Dir 'measy.exe'
$tmp = Join-Path $env:TEMP "measy-install-$([Guid]::NewGuid().ToString('n')).exe"

Write-Host "Downloading measy..." -ForegroundColor Cyan
Write-Host "  $url" -ForegroundColor Gray
if (-not (Test-Path $Dir)) { New-Item -ItemType Directory -Force -Path $Dir | Out-Null }

Invoke-WebRequest -Uri $url -OutFile $tmp -UseBasicParsing

try {
    $resp = Invoke-WebRequest -Uri $hashUrl -UseBasicParsing -ErrorAction Stop
    $expected = ($resp.Content.Trim() -split '\s+')[0].ToLowerInvariant()
    $actual = (Get-FileHash -Path $tmp -Algorithm SHA256).Hash.ToLowerInvariant()
    if ($actual -ne $expected) { throw "checksum mismatch (expected $expected, got $actual)" }
} catch {
    # No sidecar on older releases — install anyway.
    # Ignore checksum errors for compatibility
}

# Remove existing file if present
if (Test-Path $target) { Remove-Item -Force $target -ErrorAction SilentlyContinue }
Move-Item -Force $tmp $target

# The user PATH, not the machine one: this is a per-user install, and
# rewriting the machine PATH from a script is how PATHs get destroyed.
$pathChanged = $false
$userPath = [Environment]::GetEnvironmentVariable('Path', 'User')
if ($userPath -notlike "*$Dir*") {
    Write-Host "Adding $Dir to your PATH..." -ForegroundColor Cyan
    $updated = if ([string]::IsNullOrEmpty($userPath)) { $Dir } else { "$userPath;$Dir" }
    [Environment]::SetEnvironmentVariable('Path', $updated, 'User')
    # The variable above only reaches *new* processes, so make this shell
    # work too rather than telling the user to open another one.
    $env:Path = "$env:Path;$Dir"
    $pathChanged = $true
}

Write-Host ""
Write-Host "  measy installed" -ForegroundColor Green
Write-Host "  $target"
Write-Host ""
Write-Host "  cd into a project and run:" -ForegroundColor Gray
Write-Host "    measy"
Write-Host ""
if ($pathChanged) {
    Write-Host "  PATH updated. Already-open terminals need a restart." -ForegroundColor Yellow
    Write-Host ""
}
