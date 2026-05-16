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

.PHONY: install
install: build
	@echo "› installing $(BINARY) to $(INSTALL_DIR)"
	@mkdir -p $(INSTALL_DIR)
	cp $(BINARY) $(INSTALL_DIR)/$(BINARY)
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
