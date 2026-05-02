#!/usr/bin/env bash
# install.sh — lup installer for Linux and macOS
#
# Usage:
#   ./install.sh                  install to ~/.local/bin  (no sudo)
#   ./install.sh /usr/local/bin   install to a custom directory
#   INSTALL_DIR=/usr/local/bin ./install.sh
#
# The script will:
#   1. Check for an existing installation and report its version.
#   2. Build lup from source using the Go toolchain already on PATH,
#      or fall back to downloading a pre-built binary from GitHub Releases.
#   3. Copy the binary to INSTALL_DIR.
#   4. Install a default config to ~/.config/lup/config.toml if none exists.
#   5. Print next steps.

set -euo pipefail

# ──────────────────────────────────────────────────────────
# Config
# ──────────────────────────────────────────────────────────

REPO="wingitman/lup"
BINARY="lup"
INSTALL_DIR="${1:-${INSTALL_DIR:-$HOME/.local/bin}}"
CONFIG_DIR="$HOME/.config/lup"
CONFIG_FILE="$CONFIG_DIR/config.toml"
TMP_DIR="$(mktemp -d)"

# Colours (suppressed if not a TTY)
if [ -t 1 ]; then
  BOLD="\033[1m"; GREEN="\033[0;32m"; YELLOW="\033[0;33m"
  RED="\033[0;31m"; RESET="\033[0m"
else
  BOLD=""; GREEN=""; YELLOW=""; RED=""; RESET=""
fi

info()    { echo -e "${GREEN}›${RESET} $*"; }
warn()    { echo -e "${YELLOW}!${RESET} $*"; }
error()   { echo -e "${RED}✗${RESET} $*" >&2; exit 1; }
success() { echo -e "${GREEN}✓${RESET} $*"; }

cleanup() { rm -rf "$TMP_DIR"; }
trap cleanup EXIT

# ──────────────────────────────────────────────────────────
# Platform detection
# ──────────────────────────────────────────────────────────

detect_platform() {
  local os arch

  case "$(uname -s)" in
    Linux)  os="linux"  ;;
    Darwin) os="darwin" ;;
    *)      error "Unsupported OS: $(uname -s). Use install.ps1 on Windows." ;;
  esac

  case "$(uname -m)" in
    x86_64 | amd64)  arch="amd64" ;;
    arm64  | aarch64) arch="arm64" ;;
    *)      error "Unsupported architecture: $(uname -m)" ;;
  esac

  echo "${os}-${arch}"
}

# ──────────────────────────────────────────────────────────
# Existing install check
# ──────────────────────────────────────────────────────────

check_existing() {
  if command -v "$BINARY" &>/dev/null; then
    local existing
    existing="$("$BINARY" --version 2>/dev/null || echo "unknown version")"
    warn "lup is already installed: $existing"
    warn "Continuing will replace it."
    echo ""
  fi
}

# ──────────────────────────────────────────────────────────
# Build from source (preferred — avoids CGO cross-compile issues)
# ──────────────────────────────────────────────────────────

build_from_source() {
  info "Building lup from source…"

  # Determine source root: if this script is inside the repo, use it directly.
  local src_dir
  src_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

  if [ ! -f "$src_dir/go.mod" ]; then
    # Script was downloaded standalone — clone the repo.
    info "Cloning repository…"
    git clone --depth=1 "https://github.com/$REPO.git" "$TMP_DIR/lup"
    src_dir="$TMP_DIR/lup"
  fi

  local version buildtime
  version="$(git -C "$src_dir" describe --tags --always --dirty 2>/dev/null || echo "dev")"
  buildtime="$(date -u +%Y-%m-%dT%H:%M:%SZ)"

  (
    cd "$src_dir"
    go build \
      -ldflags="-s -w -X main.version=$version -X main.buildTime=$buildtime" \
      -o "$TMP_DIR/$BINARY" \
      .
  )

  echo "$TMP_DIR/$BINARY"
}

# ──────────────────────────────────────────────────────────
# Download pre-built binary from GitHub Releases
# ──────────────────────────────────────────────────────────

download_binary() {
  local platform="$1"
  local tag

  info "Fetching latest release tag…"
  tag="$(curl -fsSL "https://api.github.com/repos/$REPO/releases/latest" \
        | grep '"tag_name"' | head -1 | sed 's/.*"tag_name": "\(.*\)".*/\1/')"

  if [ -z "$tag" ]; then
    error "Could not determine latest release. Check https://github.com/$REPO/releases"
  fi

  local filename="${BINARY}-${platform}"
  local url="https://github.com/$REPO/releases/download/$tag/$filename"

  info "Downloading $filename ($tag)…"
  curl -fsSL --progress-bar -o "$TMP_DIR/$BINARY" "$url" \
    || error "Download failed. Check that release assets exist for $platform at:\n  $url"

  echo "$TMP_DIR/$BINARY"
}

# ──────────────────────────────────────────────────────────
# Install config
# ──────────────────────────────────────────────────────────

install_config() {
  if [ -f "$CONFIG_FILE" ]; then
    info "Config already exists at $CONFIG_FILE — skipping."
    return
  fi

  mkdir -p "$CONFIG_DIR"

  # Try to find the example config relative to this script or the temp clone.
  local example=""
  local script_dir
  script_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

  if [ -f "$script_dir/lup.toml.example" ]; then
    example="$script_dir/lup.toml.example"
  elif [ -f "$TMP_DIR/lup/lup.toml.example" ]; then
    example="$TMP_DIR/lup/lup.toml.example"
  fi

  if [ -n "$example" ]; then
    cp "$example" "$CONFIG_FILE"
    success "Default config installed → $CONFIG_FILE"
  else
    # Write a minimal inline config so the user can get started immediately.
    cat > "$CONFIG_FILE" <<'EOF'
[llm]
# Base URL of any OpenAI-compatible API server.
# Ollama default: http://localhost:11434/v1
# OpenAI:         https://api.openai.com/v1
base_url    = "http://localhost:11434/v1"
chat_model  = "qwen2.5-coder:7b"
embed_model = "nomic-embed-text"
api_key     = ""
timeout_secs = 120

[index]
top_k          = 5
auto_summarise = true
EOF
    success "Minimal config installed → $CONFIG_FILE"
  fi
}

# ──────────────────────────────────────────────────────────
# Main
# ──────────────────────────────────────────────────────────

main() {
  echo ""
  echo -e "${BOLD}lup installer${RESET}"
  echo "────────────────────────────────"
  echo ""

  check_existing

  local platform
  platform="$(detect_platform)"
  info "Platform: $platform"
  info "Install dir: $INSTALL_DIR"
  echo ""

  local binary_path

  # Prefer building from source if Go is available.
  if command -v go &>/dev/null; then
    info "Go toolchain found: $(go version)"
    binary_path="$(build_from_source)"
  else
    warn "Go not found — downloading pre-built binary."
    warn "To build from source, install Go: https://go.dev/dl/"
    echo ""
    binary_path="$(download_binary "$platform")"
  fi

  # Install binary.
  mkdir -p "$INSTALL_DIR"
  cp "$binary_path" "$INSTALL_DIR/$BINARY"
  chmod +x "$INSTALL_DIR/$BINARY"
  success "lup installed → $INSTALL_DIR/$BINARY"

  # Install config.
  install_config

  # PATH check.
  echo ""
  if ! echo "$PATH" | grep -q "$INSTALL_DIR"; then
    warn "$INSTALL_DIR is not in your \$PATH."
    warn "Add this to your shell profile (~/.bashrc, ~/.zshrc, etc.):"
    echo ""
    echo "    export PATH=\"\$HOME/.local/bin:\$PATH\""
    echo ""
  fi

  # Version check.
  if command -v "$BINARY" &>/dev/null; then
    local installed_ver
    installed_ver="$("$BINARY" --version 2>/dev/null || echo "")"
    success "$installed_ver"
  fi

  echo ""
  echo -e "${BOLD}Next steps:${RESET}"
  echo "  1. Edit $CONFIG_FILE"
  echo "     → set base_url to your LLM server (Ollama, OpenAI, etc.)"
  echo "     → set chat_model and embed_model to models you have pulled"
  echo ""
  echo "  2. Open a project and run:"
  echo "     lup summarise path/to/file.go"
  echo ""
  echo "  3. Look up a term:"
  echo "     lup lookup \"gross revenue\""
  echo ""
  echo "  Neovim plugin: https://github.com/wingitman/lup.nvim"
  echo ""
}

main "$@"
