// Package config loads and merges LUP configuration from:
//   - Global:      ~/.config/lup/config.toml
//   - Per-project: <project-root>/.lup/config.toml  (overrides global)
package config

import (
	"errors"
	"os"
	"path/filepath"

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

// Config is the top-level configuration structure.
type Config struct {
	LLM   LLMConfig   `toml:"llm"`
	Index IndexConfig `toml:"index"`
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
	}
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

// globalConfigPath returns ~/.config/lup/config.toml.
func globalConfigPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".config", "lup", "config.toml"), nil
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
