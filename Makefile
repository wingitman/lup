## lup – Makefile
## Targets: build  install  uninstall  clean  release
##
## Usage:
##   make              – build the binary for the current platform
##   make install      – build + install to $(INSTALL_DIR)
##   make uninstall    – remove the installed binary
##   make clean        – remove build artefacts
##   make release      – cross-compile binaries for all supported platforms

BINARY     := lup
CMD        := .
VERSION    := $(shell git describe --tags --always --dirty 2>/dev/null || echo "dev")
BUILD_TIME := $(shell date -u +%Y-%m-%dT%H:%M:%SZ)
LDFLAGS    := -s -w \
              -X github.com/wingitman/lup/internal/version.Commit=$(VERSION) \
              -X github.com/wingitman/lup/internal/version.BuildTime=$(BUILD_TIME)

# Installation directory — defaults to ~/.local/bin (no sudo needed).
# Override: make install INSTALL_DIR=/usr/local/bin
INSTALL_DIR ?= $(HOME)/.local/bin
RELEASES_DIR := releases

# Config template destination
CONFIG_DIR  := $(HOME)/.config/lup

# ──────────────────────────────────────────────────────────
# Default: build for the current platform
# ──────────────────────────────────────────────────────────

.PHONY: all
all: build

.PHONY: build
build:
	@echo "› building $(BINARY) $(VERSION)"
	go build -ldflags="$(LDFLAGS)" -o $(BINARY) $(CMD)
	@echo "  done → ./$(BINARY)"

# ──────────────────────────────────────────────────────────
# Install
# ──────────────────────────────────────────────────────────

.PHONY: build-all
build-all:
	@mkdir -p $(RELEASES_DIR)/linux/amd64 $(RELEASES_DIR)/linux/arm64 $(RELEASES_DIR)/darwin/amd64 $(RELEASES_DIR)/darwin/arm64 $(RELEASES_DIR)/windows
	@echo "  linux/amd64"
	GOOS=linux   GOARCH=amd64 go build -ldflags="$(LDFLAGS)" -o $(RELEASES_DIR)/linux/amd64/$(BINARY) $(CMD)
	@echo "  linux/arm64"
	GOOS=linux   GOARCH=arm64 go build -ldflags="$(LDFLAGS)" -o $(RELEASES_DIR)/linux/arm64/$(BINARY) $(CMD)
	@echo "  darwin/amd64"
	GOOS=darwin  GOARCH=amd64 go build -ldflags="$(LDFLAGS)" -o $(RELEASES_DIR)/darwin/amd64/$(BINARY) $(CMD)
	@echo "  darwin/arm64"
	GOOS=darwin  GOARCH=arm64 go build -ldflags="$(LDFLAGS)" -o $(RELEASES_DIR)/darwin/arm64/$(BINARY) $(CMD)
	@echo "  windows/amd64"
	GOOS=windows GOARCH=amd64 go build -ldflags="$(LDFLAGS)" -o $(RELEASES_DIR)/windows/$(BINARY).exe $(CMD)
	@echo "Pre-built binaries written to $(RELEASES_DIR)/"

.PHONY: install
install:
	@echo "› installing $(BINARY) to $(INSTALL_DIR)"
	@mkdir -p $(INSTALL_DIR)
	@if command -v go >/dev/null 2>&1; then \
		echo "==> Go found - building lup from source..."; \
		go build -ldflags="$(LDFLAGS)" -o $(BINARY) $(CMD) || exit 1; \
		cp $(BINARY) $(INSTALL_DIR)/$(BINARY); \
		echo "    Built and installed from source."; \
	else \
		echo "==> Go not found - installing pre-built binary from releases/..."; \
		OS=$$(uname -s | tr '[:upper:]' '[:lower:]'); \
		ARCH=$$(uname -m); \
		case "$$ARCH" in x86_64|amd64) ARCH=amd64 ;; aarch64|arm64) ARCH=arm64 ;; *) echo "ERROR: Unsupported architecture: $$ARCH"; exit 1 ;; esac; \
		if [ "$$OS" = "darwin" ]; then RELEASE_BIN="$(RELEASES_DIR)/darwin/$$ARCH/$(BINARY)"; elif [ "$$OS" = "linux" ]; then RELEASE_BIN="$(RELEASES_DIR)/linux/$$ARCH/$(BINARY)"; else echo "ERROR: Unsupported OS: $$OS"; exit 1; fi; \
		if [ ! -f "$$RELEASE_BIN" ]; then echo "ERROR: Pre-built binary not found at $$RELEASE_BIN"; echo "       Please install Go (https://go.dev/dl/) and re-run, or ask a developer to run 'make build-all' and commit the releases/ folder."; exit 1; fi; \
		cp "$$RELEASE_BIN" $(INSTALL_DIR)/$(BINARY); \
		echo "    Installed pre-built binary."; \
	fi
	@chmod +x $(INSTALL_DIR)/$(BINARY)
	@echo "  installed → $(INSTALL_DIR)/$(BINARY)"
	@$(MAKE) --no-print-directory install-config

.PHONY: install-config
install-config:
	@if [ ! -f "$(CONFIG_DIR)/config.toml" ]; then \
		echo "› installing default config to $(CONFIG_DIR)/config.toml"; \
		mkdir -p $(CONFIG_DIR); \
		cp lup.toml.example $(CONFIG_DIR)/config.toml; \
		echo "  installed → $(CONFIG_DIR)/config.toml"; \
		echo "  Edit this file to point lup at your LLM endpoint."; \
	else \
		echo "  config already exists at $(CONFIG_DIR)/config.toml — skipping"; \
	fi

# ──────────────────────────────────────────────────────────
# Uninstall
# ──────────────────────────────────────────────────────────

.PHONY: uninstall
uninstall:
	@echo "› removing $(INSTALL_DIR)/$(BINARY)"
	@rm -f $(INSTALL_DIR)/$(BINARY)
	@echo "  done"
	@echo "  Note: config at $(CONFIG_DIR)/config.toml was not removed."

# ──────────────────────────────────────────────────────────
# Clean
# ──────────────────────────────────────────────────────────

.PHONY: clean
clean:
	@rm -f $(BINARY)
	@rm -rf dist/
	@echo "› cleaned"

# ──────────────────────────────────────────────────────────
# Cross-compile release binaries
# ──────────────────────────────────────────────────────────
# CGO is required (tree-sitter + sqlite-vec), so true cross-compilation needs
# the target sysroot or a cross-compiler. The targets below produce native
# binaries via GOARCH overrides for same-OS builds, and document the
# cross-compile flags needed for other platforms.
#
# For CI / GitHub Actions, use a matrix job that runs each target on its
# native OS runner (ubuntu, macos, windows) — this avoids CGO cross-compile
# complexity entirely and is the recommended approach.

DIST := dist

.PHONY: release
release: clean
	@echo "› building release binaries"
	@mkdir -p $(DIST)

	@echo "  linux/amd64"
	GOOS=linux GOARCH=amd64 \
		go build -ldflags="$(LDFLAGS)" -o $(DIST)/$(BINARY)-linux-amd64 $(CMD)

	@echo "  linux/arm64"
	GOOS=linux GOARCH=arm64 \
		go build -ldflags="$(LDFLAGS)" -o $(DIST)/$(BINARY)-linux-arm64 $(CMD)

	@echo "  darwin/amd64"
	GOOS=darwin GOARCH=amd64 \
		go build -ldflags="$(LDFLAGS)" -o $(DIST)/$(BINARY)-darwin-amd64 $(CMD)

	@echo "  darwin/arm64 (Apple Silicon)"
	GOOS=darwin GOARCH=arm64 \
		go build -ldflags="$(LDFLAGS)" -o $(DIST)/$(BINARY)-darwin-arm64 $(CMD)

	@echo "  windows/amd64"
	GOOS=windows GOARCH=amd64 \
		go build -ldflags="$(LDFLAGS)" -o $(DIST)/$(BINARY)-windows-amd64.exe $(CMD)

	@echo "› release binaries written to $(DIST)/"
	@ls -lh $(DIST)/

# ──────────────────────────────────────────────────────────
# Help
# ──────────────────────────────────────────────────────────

.PHONY: help
help:
	@echo ""
	@echo "  lup build system"
	@echo ""
	@echo "  make                   build binary for current platform"
	@echo "  make install           build + install to \$$INSTALL_DIR (default: ~/.local/bin)"
	@echo "  make install           INSTALL_DIR=/usr/local/bin   install to custom path"
	@echo "  make uninstall         remove installed binary"
	@echo "  make clean             remove build artefacts"
	@echo "  make release           cross-compile all platform binaries to dist/"
	@echo "  make help              show this message"
	@echo ""
