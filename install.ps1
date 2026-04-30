# install.ps1 — lup installer for Windows
#
# Usage (from PowerShell, run as your normal user):
#   .\install.ps1
#   .\install.ps1 -InstallDir "C:\tools"
#   .\install.ps1 -InstallDir "$env:USERPROFILE\.local\bin"
#
# The script will:
#   1. Check for an existing installation and report its version.
#   2. Build lup from source using the Go toolchain if available,
#      or download a pre-built binary from GitHub Releases.
#   3. Copy the binary to InstallDir and add it to the user PATH if needed.
#   4. Install a default config to %APPDATA%\lup\config.toml if none exists.
#   5. Print next steps.

[CmdletBinding()]
param(
    [string]$InstallDir = "$env:USERPROFILE\.local\bin"
)

Set-StrictMode -Version Latest
$ErrorActionPreference = "Stop"

# ──────────────────────────────────────────────────────────
# Config
# ──────────────────────────────────────────────────────────

$Repo      = "wingitman/lup"
$Binary    = "lup.exe"
$ConfigDir = "$env:APPDATA\lup"
$ConfigFile = "$ConfigDir\config.toml"
$TmpDir    = [System.IO.Path]::GetTempPath() + [System.IO.Path]::GetRandomFileName()

# ──────────────────────────────────────────────────────────
# Helpers
# ──────────────────────────────────────────────────────────

function Write-Info    { param($msg) Write-Host "› $msg" -ForegroundColor Green }
function Write-Warn    { param($msg) Write-Host "! $msg" -ForegroundColor Yellow }
function Write-Success { param($msg) Write-Host "✓ $msg" -ForegroundColor Green }
function Write-Fail    { param($msg) Write-Host "✗ $msg" -ForegroundColor Red; exit 1 }

function Ensure-Dir { param($path) if (-not (Test-Path $path)) { New-Item -ItemType Directory -Path $path -Force | Out-Null } }

# ──────────────────────────────────────────────────────────
# Existing install check
# ──────────────────────────────────────────────────────────

function Check-Existing {
    $existing = Get-Command "lup" -ErrorAction SilentlyContinue
    if ($existing) {
        $ver = & lup --version 2>$null
        Write-Warn "lup is already installed: $ver"
        Write-Warn "Continuing will replace it."
        Write-Host ""
    }
}

# ──────────────────────────────────────────────────────────
# Build from source
# ──────────────────────────────────────────────────────────

function Build-FromSource {
    Write-Info "Building lup from source…"

    $scriptDir = Split-Path -Parent $MyInvocation.ScriptName
    $srcDir    = $scriptDir

    if (-not (Test-Path "$srcDir\go.mod")) {
        Write-Info "Cloning repository…"
        Ensure-Dir $TmpDir
        & git clone --depth=1 "https://github.com/$Repo.git" "$TmpDir\lup"
        if ($LASTEXITCODE -ne 0) { Write-Fail "git clone failed." }
        $srcDir = "$TmpDir\lup"
    }

    $version   = (& git -C $srcDir describe --tags --always --dirty 2>$null) ?? "dev"
    $buildTime = (Get-Date -Format "yyyy-MM-ddTHH:mm:ssZ" -AsUTC)
    $outPath   = "$TmpDir\lup.exe"

    Ensure-Dir $TmpDir

    Push-Location $srcDir
    try {
        & go build `
            -ldflags="-s -w -X main.version=$version -X main.buildTime=$buildTime" `
            -o $outPath `
            .\cmd\lup
        if ($LASTEXITCODE -ne 0) { Write-Fail "go build failed." }
    } finally {
        Pop-Location
    }

    return $outPath
}

# ──────────────────────────────────────────────────────────
# Download pre-built binary
# ──────────────────────────────────────────────────────────

function Download-Binary {
    Write-Info "Fetching latest release tag…"

    $apiUrl  = "https://api.github.com/repos/$Repo/releases/latest"
    $release = Invoke-RestMethod -Uri $apiUrl -Headers @{ "User-Agent" = "lup-installer" }
    $tag     = $release.tag_name

    if (-not $tag) { Write-Fail "Could not determine latest release. Check https://github.com/$Repo/releases" }

    $filename = "lup-windows-amd64.exe"
    $url      = "https://github.com/$Repo/releases/download/$tag/$filename"

    Write-Info "Downloading $filename ($tag)…"

    Ensure-Dir $TmpDir
    $outPath = "$TmpDir\lup.exe"

    try {
        Invoke-WebRequest -Uri $url -OutFile $outPath -UseBasicParsing
    } catch {
        Write-Fail "Download failed. Check that release assets exist at:`n  $url"
    }

    return $outPath
}

# ──────────────────────────────────────────────────────────
# Install config
# ──────────────────────────────────────────────────────────

function Install-Config {
    if (Test-Path $ConfigFile) {
        Write-Info "Config already exists at $ConfigFile — skipping."
        return
    }

    Ensure-Dir $ConfigDir

    # Try to find the example config next to this script or in the temp clone.
    $scriptDir  = Split-Path -Parent $MyInvocation.ScriptName
    $exampleSrc = ""

    if (Test-Path "$scriptDir\lup.toml.example") {
        $exampleSrc = "$scriptDir\lup.toml.example"
    } elseif (Test-Path "$TmpDir\lup\lup.toml.example") {
        $exampleSrc = "$TmpDir\lup\lup.toml.example"
    }

    if ($exampleSrc) {
        Copy-Item $exampleSrc $ConfigFile
    } else {
        # Inline minimal config.
        @'
[llm]
# Base URL of any OpenAI-compatible API server.
# Ollama default: http://localhost:11434/v1
# OpenAI:         https://api.openai.com/v1
base_url     = "http://localhost:11434/v1"
chat_model   = "qwen2.5-coder:7b"
embed_model  = "nomic-embed-text"
api_key      = ""
timeout_secs = 120

[index]
top_k          = 5
auto_summarise = true
'@ | Set-Content -Path $ConfigFile -Encoding UTF8
    }

    Write-Success "Default config installed → $ConfigFile"
}

# ──────────────────────────────────────────────────────────
# PATH helper
# ──────────────────────────────────────────────────────────

function Ensure-InPath {
    param([string]$dir)

    $userPath = [System.Environment]::GetEnvironmentVariable("PATH", "User")
    if ($userPath -split ";" | Where-Object { $_ -eq $dir }) {
        return  # already present
    }

    Write-Info "Adding $dir to user PATH…"
    $newPath = "$userPath;$dir"
    [System.Environment]::SetEnvironmentVariable("PATH", $newPath, "User")
    # Update the current session too.
    $env:PATH = "$env:PATH;$dir"
    Write-Success "$dir added to PATH (takes effect in new terminals)."
}

# ──────────────────────────────────────────────────────────
# Main
# ──────────────────────────────────────────────────────────

Write-Host ""
Write-Host "lup installer" -ForegroundColor White
Write-Host "────────────────────────────────"
Write-Host ""

Check-Existing

Write-Info "Platform: windows/amd64"
Write-Info "Install dir: $InstallDir"
Write-Host ""

$binaryPath = ""

$goCmd = Get-Command "go" -ErrorAction SilentlyContinue
if ($goCmd) {
    Write-Info "Go toolchain found: $(& go version)"
    $binaryPath = Build-FromSource
} else {
    Write-Warn "Go not found — downloading pre-built binary."
    Write-Warn "To build from source, install Go: https://go.dev/dl/"
    Write-Host ""
    $binaryPath = Download-Binary
}

# Install binary.
Ensure-Dir $InstallDir
$dest = "$InstallDir\$Binary"
Copy-Item $binaryPath $dest -Force
Write-Success "lup installed → $dest"

# Install config.
Install-Config

# Ensure InstallDir is on PATH.
Ensure-InPath $InstallDir

# Confirm version.
Write-Host ""
try {
    $ver = & "$dest" --version 2>$null
    Write-Success $ver
} catch { }

# Cleanup temp dir.
if (Test-Path $TmpDir) { Remove-Item $TmpDir -Recurse -Force -ErrorAction SilentlyContinue }

Write-Host ""
Write-Host "Next steps:" -ForegroundColor White
Write-Host "  1. Edit $ConfigFile"
Write-Host "     -> set base_url to your LLM server (Ollama, OpenAI, etc.)"
Write-Host "     -> set chat_model and embed_model to models you have pulled"
Write-Host ""
Write-Host "  2. Open a project and run:"
Write-Host "     lup summarise path\to\file.go"
Write-Host ""
Write-Host "  3. Look up a term:"
Write-Host '     lup lookup "gross revenue"'
Write-Host ""
Write-Host "  Neovim plugin: https://github.com/wingitman/lup.nvim"
Write-Host ""
