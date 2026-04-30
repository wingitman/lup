# lup

AI-powered code documentation tool. Open a file, get a summary. Select a term, find where it lives across your codebase.

lup parses source files with tree-sitter, summarises them with any OpenAI-compatible LLM, and indexes the results locally with sqlite-vec so you can do fast semantic lookups without sending your code anywhere you don't want it to go.

---

## How it works

```
lup summarise src/billing/gross.go
  → tree-sitter extracts all functions and types
  → LLM generates a summary for the file and each symbol
  → stored as JSON in .lup/summaries/
  → embedded and indexed in .lup/index.db

lup lookup "gross revenue"
  → LLM embeds the query
  → sqlite-vec ANN search over .lup/index.db
  → top results hydrated from stored summaries
  → printed to stdout (or --json for editor plugins)
```

Everything runs as a stateless CLI. No daemon, no background service. The editor plugin calls `lup` as a subprocess — it never blocks your editor.

---

## Installation

### Linux / macOS — one-liner

```sh
curl -fsSL https://raw.githubusercontent.com/wingitman/lup/main/install.sh | bash
```

Or download and run manually:

```sh
./install.sh                         # installs to ~/.local/bin
./install.sh /usr/local/bin          # custom directory (may need sudo)
INSTALL_DIR=/usr/local/bin ./install.sh
```

### Windows — PowerShell

```powershell
irm https://raw.githubusercontent.com/wingitman/lup/main/install.ps1 | iex
```

Or download and run:

```powershell
.\install.ps1                        # installs to %USERPROFILE%\.local\bin
.\install.ps1 -InstallDir C:\tools
```

### Build from source with make

Requires Go 1.21+.

```sh
git clone https://github.com/wingitman/lup
cd lup
make install                         # builds + installs to ~/.local/bin
make install INSTALL_DIR=/usr/local/bin
```

| Target | Description |
|---|---|
| `make` | Build binary for current platform |
| `make install` | Build + install to `$INSTALL_DIR` |
| `make uninstall` | Remove installed binary |
| `make clean` | Remove build artefacts |
| `make release` | Cross-compile all platforms to `dist/` |

### Build manually

```sh
git clone https://github.com/wingitman/lup
cd lup
go build -o lup ./cmd/lup
```

---

## Requirements

- Go 1.21+ (to build from source)
- An OpenAI-compatible API server — local or cloud:
  - [Ollama](https://ollama.com) (recommended for local use)
  - [LM Studio](https://lmstudio.ai)
  - [OpenAI](https://platform.openai.com)
  - Any server that speaks the `/v1/chat/completions` and `/v1/embeddings` API

---

## Configuration

The installer creates a default config at `~/.config/lup/config.toml` (Linux/macOS) or `%APPDATA%\lup\config.toml` (Windows). Edit it to point at your LLM server:

```toml
[llm]
base_url    = "http://localhost:11434/v1"  # Ollama default
chat_model  = "qwen2.5-coder:7b"          # model for summarisation
embed_model = "nomic-embed-text"           # model for embeddings
api_key     = ""                           # leave empty for local servers
timeout_secs = 120

[index]
top_k          = 5     # results returned by `lup lookup`
auto_summarise = true  # hint for editor plugins
```

Per-project overrides go in `.lup/config.toml` at your project root. Any value set there takes precedence over the global config.

A fully annotated example is at [`lup.toml.example`](./lup.toml.example).

### Ollama quick setup

```sh
ollama pull qwen2.5-coder:7b
ollama pull nomic-embed-text
```

---

## Commands

```
lup summarise <file> [--force]
```
Parse and summarise a source file. Stores the result in `.lup/`. Skips files already summarised unless `--force` is given.

```
lup lookup <text> [-k N] [--json]
```
Semantic RAG lookup. Embeds the query, searches the local index, returns the most relevant summaries. `--json` emits machine-readable output for editor plugins.

```
lup index
```
Re-embed all stored summaries into the vector index. Useful after changing your embed model.

```
lup config [--json]
```
Show the resolved configuration (global + project-local merged).

```
lup status [--json]
```
List all summarised files in the current project with symbol counts and timestamps.

---

## Project storage

lup stores everything in a `.lup/` directory at your project root (detected by walking up from the current directory, like git):

```
.lup/
├── summaries/      ← JSON summaries, one per file (safe to commit)
├── index.db        ← sqlite-vec embeddings index (exclude from git)
└── config.toml     ← optional per-project config overrides
```

Recommended `.gitignore` entries:

```
.lup/index.db
.lup/index.db-shm
.lup/index.db-wal
```

Committing `.lup/summaries/` is optional but useful if you want to share pre-generated documentation with your team.

---

## Supported languages

| Language | Extensions |
|---|---|
| Go | `.go` |
| Python | `.py` |
| JavaScript | `.js`, `.jsx` |
| TypeScript | `.ts`, `.tsx` |

More languages can be added by extending `internal/parser/parser.go` — tree-sitter grammars are available for 50+ languages.

---

## Editor integrations

| Editor | Repo |
|---|---|
| Neovim | [github.com/wingitman/lup.nvim](https://github.com/wingitman/lup.nvim) |
| VSCode | coming soon |

The CLI's `--json` flag is the stable interface that editor plugins consume. All output schemas are intentionally simple so integrations are easy to write in any language.

---

## License

MIT
