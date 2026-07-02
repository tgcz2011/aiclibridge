# install.ps1 — one-line installer for aiclibridge (Windows).
#
# Usage (PowerShell):
#   irm https://github.com/tgcz2011/aiclibridge/raw/main/scripts/install.ps1 | iex
#   irm https://github.com/tgcz2011/aiclibridge/raw/main/scripts/install.ps1 | iex -ArgsList -Bin "$env:USERPROFILE\bin"
#   irm https://github.com/tgcz2011/aiclibridge/raw/main/scripts/install.ps1 | iex -ArgsList -Version v0.4.1
#
# The script detects ARCH (amd64/arm64), fetches the matching .zip from
# GitHub releases, verifies the sha256 checksum, and installs the binary
# to -Bin (default: $env:USERPROFILE\bin). It never runs the binary
# post-install — the user invokes `aiclibridge version` themselves.
#
# Design notes:
#   - PowerShell 5.1+ (Windows 10+ ships with 5.1; PowerShell 7 also works).
#   - No admin rights required by default: install dir is $env:USERPROFILE\bin.
#     Use -Bin "C:\Program Files\aiclibridge" with an elevated shell for a
#     system-wide install.
#   - Errors throw and the script exits with a non-zero code.

[CmdletBinding()]
param(
    [string]$Bin = "$env:USERPROFILE\bin",
    [string]$Version = "",
    [switch]$Force,
    [switch]$Verbose
)

$ErrorActionPreference = "Stop"
$Owner = "tgcz2011"
$Repo  = "aiclibridge"
$ReleasesLatest = "https://github.com/$Owner/$Repo/releases/latest"
$ApiLatest = "https://api.github.com/repos/$Owner/$Repo/releases/latest"
$DownloadBase = "https://github.com/$Owner/$Repo/releases/download"
# Optional mirror prefix (e.g. https://ghproxy.com) for download URLs.
$Mirror = $env:GITHUB_MIRROR

function Write-Verbose-Log([string]$msg) {
    if ($Verbose) { Write-Host "install.ps1: $msg" -ForegroundColor Cyan }
}
function Write-Err([string]$msg) {
    Write-Host "install.ps1: $msg" -ForegroundColor Red
}

# ── detect ARCH (Windows only ships amd64 release asset today) ──
$arch = $env:PROCESSOR_ARCHITECTURE
switch -Regex ($arch) {
    "AMD64|x86_64" { $goarch = "amd64"; break }
    "ARM64|aarch64" { $goarch = "arm64"; break }
    default {
        Write-Err "unsupported architecture: $arch"
        Write-Err "this installer only supports amd64/arm64; check the release page for available assets."
        exit 1
    }
}
Write-Verbose-Log "detected: goos=windows goarch=$goarch"

# ── resolve version ──
# Primary: follow the releases/latest 302 redirect. github.com/.../releases/latest
# redirects to .../releases/tag/<tag>; we extract <tag> from the final URL.
# This avoids api.github.com which is frequently 403'd by rate limits or
# region-blocking. Fallback to the REST API if the redirect fails.
if ([string]::IsNullOrEmpty($Version)) {
    Write-Verbose-Log "fetching latest release tag ..."
    try {
        $resp = Invoke-WebRequest -Uri $ReleasesLatest -Method Head -UseBasicParsing `
            -Headers @{ "User-Agent" = "aiclibridge-installer/1.0" }
        $finalUrl = $resp.BaseResponse.ResponseUri.AbsoluteUri
        if ($finalUrl -match '/tag/(.+)$') {
            $Version = $matches[1]
        }
    } catch {
        Write-Verbose-Log "redirect method failed; trying GitHub API ..."
    }
    if ([string]::IsNullOrEmpty($Version)) {
        try {
            $apiResp = Invoke-RestMethod -Uri $ApiLatest -UseBasicParsing `
                -Headers @{ "User-Agent" = "aiclibridge-installer/1.0" }
            $Version = $apiResp.tag_name
        } catch {
            Write-Err "could not fetch latest release tag (tried redirect + API): $_"
            Write-Err "check network, or pass -Version v0.5.0 explicitly."
            exit 1
        }
    }
}
Write-Verbose-Log "installing version: $Version"

# ── build asset names ──
$asset = "aiclibridge-windows-$goarch.zip"
$assetSha256 = "$asset.sha256"
$downloadUrl = "$DownloadBase/$Version/$asset"
$sha256Url   = "$DownloadBase/$Version/$assetSha256"

# Apply mirror prefix if GITHUB_MIRROR is set.
if ($Mirror) {
    $downloadUrl = "$($Mirror.TrimEnd('/'))/$downloadUrl"
    $sha256Url   = "$($Mirror.TrimEnd('/'))/$sha256Url"
    Write-Verbose-Log "using mirror: $Mirror"
}

# ── temp work dir ──
$tmp = New-Item -ItemType Directory -Path ([System.IO.Path]::GetTempPath() + "aiclibridge-install-" + [System.Guid]::NewGuid().ToString("N")) -Force
try {
    Write-Verbose-Log "temp dir: $($tmp.FullName)"

    # ── download ──
    Write-Verbose-Log "downloading $downloadUrl"
    try {
        Invoke-WebRequest -Uri $downloadUrl -OutFile "$($tmp.FullName)\$asset" -UseBasicParsing
    } catch {
        Write-Err "download failed: $downloadUrl"
        Write-Err "verify the version ($Version) has a $asset asset on the release page."
        Write-Err "$_"
        exit 1
    }

    Write-Verbose-Log "downloading $sha256Url"
    try {
        Invoke-WebRequest -Uri $sha256Url -OutFile "$($tmp.FullName)\$assetSha256" -UseBasicParsing
    } catch {
        Write-Err "sha256 file download failed: $sha256Url"
        Write-Err "refusing to install without integrity verification."
        exit 1
    }

    # ── verify sha256 ──
    # The .sha256 file format is "<hex>  <filename>" (sha256sum output).
    $expected = (Get-Content "$($tmp.FullName)\$assetSha256" -Raw).Trim().Split(" ")[0]
    if ([string]::IsNullOrEmpty($expected)) {
        Write-Err "could not parse expected sha256 from $assetSha256"
        exit 1
    }
    $actual = (Get-FileHash "$($tmp.FullName)\$asset" -Algorithm SHA256).Hash.ToLower()
    if ($actual -ne $expected.ToLower()) {
        Write-Err "checksum mismatch!"
        Write-Err "  expected: $expected"
        Write-Err "  actual:   $actual"
        Write-Err "the download may be corrupted or tampered with. Aborting."
        exit 1
    }
    Write-Verbose-Log "checksum OK: $actual"

    # ── extract ──
    Write-Verbose-Log "extracting $asset"
    Expand-Archive -Path "$($tmp.FullName)\$asset" -DestinationPath $tmp.FullName -Force
    # v0.5.2+ ships 'aiclibridge.exe'; older releases shipped
    # 'aiclibridge-windows-{goarch}.exe'. Try canonical first.
    $extractedBin = Join-Path $tmp.FullName "aiclibridge.exe"
    if (-not (Test-Path $extractedBin)) {
        $extractedBin = Join-Path $tmp.FullName "aiclibridge-windows-$goarch.exe"
        if (-not (Test-Path $extractedBin)) {
            Write-Err "extracted archive did not contain an aiclibridge binary"
            Write-Err "expected 'aiclibridge.exe' or 'aiclibridge-windows-$goarch.exe' in $asset"
            exit 1
        }
    }

    # ── ensure install dir ──
    if (-not (Test-Path $Bin)) {
        New-Item -ItemType Directory -Path $Bin -Force | Out-Null
    }
    $target = Join-Path $Bin "aiclibridge.exe"

    # ── refuse overwrite unless -Force ──
    if ((Test-Path $target) -and -not $Force) {
        $answer = Read-Host "aiclibridge: $target already exists. Overwrite? [y/N]"
        if ($answer -notmatch "^[yY]") {
            Write-Err "aborted; rerun with -Force to skip this prompt."
            exit 1
        }
    }

    # ── install ──
    Write-Verbose-Log "installing to $target"
    Copy-Item $extractedBin $target -Force

    # ── verify ──
    Write-Verbose-Log "verifying install"
    $installedVersion = (& $target version 2>$null | Select-Object -First 1) -replace '^aiclibridge\s+', ''
    if ([string]::IsNullOrEmpty($installedVersion)) {
        Write-Err "installed binary at $target did not respond to 'version'; check it is executable."
        exit 1
    }

    # ── done ──
    Write-Host ""
    Write-Host "aiclibridge $installedVersion installed to $target" -ForegroundColor Green
    Write-Host ""
    Write-Host "Next steps:"
    Write-Host "  aiclibridge version"
    Write-Host "  aiclibridge --help"
    Write-Host "  aiclibridge start                  # background daemon on 127.0.0.1:8787"
    Write-Host "  aiclibridge run --model claude/anthropic/claude-sonnet-4.5 `"hello`""
    Write-Host ""
    Write-Host "Make sure $Bin is on your PATH:"
    Write-Host "  [Environment]::SetEnvironmentVariable('PATH', `"$Bin;`" + [Environment]::GetEnvironmentVariable('PATH', 'User'), 'User')"
    Write-Host ""
    Write-Host "Docs: https://github.com/$Owner/$Repo#readme"
}
finally {
    if ($tmp -and (Test-Path $tmp.FullName)) {
        Remove-Item -Recurse -Force $tmp.FullName -ErrorAction SilentlyContinue
    }
}
