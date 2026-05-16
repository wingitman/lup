// Package config loads and merges LUP configuration from:
//   - Global:      ~/.config/lup/config.toml
//   - Per-project: <project-root>/.lup/config.toml  (overrides global)
package config

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/BurntSushi/toml"
)

// LLMConfig holds settings for the OpenAI-compatible API endpoint.
type LLMConfig struct {
	BaseURL    string `toml:"base_url"`
	ChatModel  string `toml:"chat_model"`
	EmbedModel string `toml:"embed_model"`
	APIKey     string `toml:"api_key"`
	// TimeoutSecs is the HTTP timeout for a single LLM request.
	TimeoutSecs int `toml:"timeout_secs"`
}

// IndexConfig controls indexing behaviour.
type IndexConfig struct {
	// TopK is the number of results returned by `lup lookup`.
	TopK int `toml:"top_k"`
	// AutoSummarise triggers summarisation whenever a file is opened via the
	// editor plugin. The plugin is responsible for calling `lup summarise`;
	// this flag is read by the plugin, not by the CLI itself.
	AutoSummarise bool `toml:"auto_summarise"`
	// Concurrency controls how many files `lup document` summarises in
	// parallel.  Keep this low (2-3) to avoid hammering the LLM server and
	// the user's machine.
	Concurrency int `toml:"concurrency"`
}

// UpdatesConfig holds update-check and installer preferences.
type UpdatesConfig struct {
	DisableChecks bool   `toml:"disable_checks"`
	CurrentCommit string `toml:"current_commit"`
	RepoPath      string `toml:"repo_path"`
	Terminal      string `toml:"terminal"`
}

// Config is the top-level configuration structure.
type Config struct {
	LLM     LLMConfig     `toml:"llm"`
	Index   IndexConfig   `toml:"index"`
	Updates UpdatesConfig `toml:"updates"`
}

// defaults returns a Config pre-filled with sensible defaults.
func defaults() Config {
	return Config{
		LLM: LLMConfig{
			BaseURL:     "http://localhost:11434/v1",
			ChatModel:   "qwen2.5-coder:7b",
			EmbedModel:  "nomic-embed-text",
			APIKey:      "",
			TimeoutSecs: 120,
		},
		Index: IndexConfig{
			TopK:          5,
			AutoSummarise: true,
			Concurrency:   2,
		},
		Updates: UpdatesConfig{
			DisableChecks: false,
			CurrentCommit: "",
			RepoPath:      "",
			Terminal:      "",
		},
	}
}

// Default returns a Config pre-filled with built-in defaults.
func Default() Config {
	return defaults()
}

// Load returns a Config by merging defaults → global → project-local.
// projectRoot is the directory that contains (or should contain) the .lup/
// folder. Pass an empty string to skip project-local config.
func Load(projectRoot string) (Config, error) {
	cfg := defaults()

	// 1. Global config
	if globalPath, err := globalConfigPath(); err == nil {
		if err := mergeFile(&cfg, globalPath); err != nil && !errors.Is(err, os.ErrNotExist) {
			return cfg, err
		}
	}

	// 2. Per-project config
	if projectRoot != "" {
		localPath := filepath.Join(projectRoot, ".lup", "config.toml")
		if err := mergeFile(&cfg, localPath); err != nil && !errors.Is(err, os.ErrNotExist) {
			return cfg, err
		}
	}

	return cfg, nil
}

// ConfigDir returns the global lup configuration directory.
func ConfigDir() string {
	base, err := os.UserConfigDir()
	if err != nil {
		home, herr := os.UserHomeDir()
		if herr != nil {
			return ""
		}
		return filepath.Join(home, ".config", "lup")
	}
	return filepath.Join(base, "lup")
}

// GlobalConfigPath returns the global lup configuration file path.
func GlobalConfigPath() string {
	return filepath.Join(ConfigDir(), "config.toml")
}

// globalConfigPath returns ~/.config/lup/config.toml.
func globalConfigPath() (string, error) {
	path := GlobalConfigPath()
	if path == "" {
		return "", errors.New("could not determine config directory")
	}
	return path, nil
}

// mergeFile decodes a TOML file into dst, overwriting only fields that are
// explicitly present in the file (BurntSushi/toml does this naturally).
func mergeFile(dst *Config, path string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = toml.NewDecoder(f).Decode(dst)
	return err
}

// EnsureProjectDir creates the .lup/ directory tree inside projectRoot if it
// does not already exist.
func EnsureProjectDir(projectRoot string) error {
	dirs := []string{
		filepath.Join(projectRoot, ".lup", "summaries"),
	}
	for _, d := range dirs {
		if err := os.MkdirAll(d, 0o755); err != nil {
			return err
		}
	}
	return nil
}

// RecordUpdateMetadata stores the installed commit and source repo path in the
// global config without changing project-local settings.
func RecordUpdateMetadata(commit, repoPath string) error {
	cfg := defaults()
	path := GlobalConfigPath()
	if err := mergeFile(&cfg, path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if commit != "" {
		cfg.Updates.CurrentCommit = commit
	}
	if repoPath != "" {
		cfg.Updates.RepoPath = repoPath
	}
	if err := os.MkdirAll(ConfigDir(), 0755); err != nil {
		return err
	}
	return os.WriteFile(path, []byte(BuildTOML(cfg)), 0644)
}

// BuildTOML renders a complete config file with all current keys.
func BuildTOML(cfg Config) string {
	return fmt.Sprintf(`# LUP configuration file
#
# Copy to ~/.config/lup/config.toml for global settings, or to
# <project>/.lup/config.toml for per-project overrides.
#
# Any field omitted here falls back to the built-in default.

[llm]
# Base URL of any OpenAI-compatible API server.
# Ollama default:   http://localhost:11434/v1
# LM Studio:        http://localhost:1234/v1
# OpenAI:           https://api.openai.com/v1
base_url = %s

# Model used for file summarisation (chat completions).
chat_model = %s

# Model used for generating embeddings (must support /v1/embeddings).
embed_model = %s

# API key - leave empty for local servers that don't require auth.
api_key = %s

# HTTP timeout in seconds for a single LLM request.
# Increase for slow hardware or large files.
timeout_secs = %d

[index]
# Number of results returned by lup lookup.
top_k = %d

# When true, editor plugins should call lup summarise on file open.
# This flag is read by the editor plugin, not enforced by the CLI.
auto_summarise = %t

# Parallel workers used by lup document.
concurrency = %d

[updates]
# Disable explicit lup updates checks. No automatic startup prompts are shown.
disable_checks = %t

# Installed app commit, maintained by lup updates.
current_commit = %s

# Source checkout used for git-based updates.
repo_path = %s

# Optional terminal command for detached update installs.
terminal = %s
`, quote(cfg.LLM.BaseURL), quote(cfg.LLM.ChatModel), quote(cfg.LLM.EmbedModel), quote(cfg.LLM.APIKey), cfg.LLM.TimeoutSecs, cfg.Index.TopK, cfg.Index.AutoSummarise, cfg.Index.Concurrency, cfg.Updates.DisableChecks, quote(cfg.Updates.CurrentCommit), quote(cfg.Updates.RepoPath), quote(cfg.Updates.Terminal))
}

func quote(s string) string {
	s = strings.ReplaceAll(s, `\`, `\\`)
	s = strings.ReplaceAll(s, `"`, `\"`)
	return `"` + s + `"`
}

// ProjectRoot walks up from startDir looking for a .lup/ directory. If none
// is found it returns startDir itself (treat the working directory as the
// project root).
func ProjectRoot(startDir string) string {
	dir := startDir
	for {
		if _, err := os.Stat(filepath.Join(dir, ".lup")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
		dir = parent
	}
	return startDir
}
