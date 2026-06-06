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

// ActiveConfig selects the generator profile and reasoning/detail level used by
// the TUI and by future commands that support multiple model backends.
type ActiveConfig struct {
	Agent string `toml:"agent"`
	Level string `toml:"level"`
}

// AgentConfig describes one text-generation backend. Providers currently used
// by lup are openai_compatible, openai_responses, anthropic_messages,
// opencode_zen, and cursor_agent.
type AgentConfig struct {
	Provider    string `toml:"provider"`
	BaseURL     string `toml:"base_url"`
	Model       string `toml:"model"`
	EmbedModel  string `toml:"embed_model"`
	APIKey      string `toml:"api_key"`
	APIKeyEnv   string `toml:"api_key_env"`
	TimeoutSecs int    `toml:"timeout_secs"`
	Mode        string `toml:"mode"`
}

// EmbeddingConfig lets summarisation use an agent backend while lookup/indexing
// continue to use a normal embedding endpoint.
type EmbeddingConfig struct {
	Provider    string `toml:"provider"`
	BaseURL     string `toml:"base_url"`
	Model       string `toml:"model"`
	APIKey      string `toml:"api_key"`
	APIKeyEnv   string `toml:"api_key_env"`
	TimeoutSecs int    `toml:"timeout_secs"`
}

// SummarisationConfig controls prompt construction and model sampling.
type SummarisationConfig struct {
	SystemPrompt        string  `toml:"system_prompt"`
	Query               string  `toml:"query"`
	MaxSymbolsPerPrompt int     `toml:"max_symbols_per_prompt"`
	BodyBudget          int     `toml:"body_budget"`
	Temperature         float32 `toml:"temperature"`
}

// AppsConfig holds external application overrides.
type AppsConfig struct {
	Editor string `toml:"editor"`
	Opener string `toml:"opener"`
}

// KeybindsConfig holds TUI key mappings.
type KeybindsConfig struct {
	Up          string `toml:"up"`
	Down        string `toml:"down"`
	Left        string `toml:"left"`
	Right       string `toml:"right"`
	Confirm     string `toml:"confirm"`
	PageUp      string `toml:"page_up"`
	PageDown    string `toml:"page_down"`
	JumpTop     string `toml:"jump_top"`
	JumpBottom  string `toml:"jump_bottom"`
	Quit        string `toml:"quit"`
	Edit        string `toml:"edit"`
	Options     string `toml:"options"`
	SwitchAgent string `toml:"switch_agent"`
	SwitchLevel string `toml:"switch_level"`
	Regenerate  string `toml:"regenerate"`
	ShowHints   string `toml:"show_hints"`
	ShowUpdates string `toml:"show_updates"`
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
	LLM           LLMConfig              `toml:"llm"`
	Active        ActiveConfig           `toml:"active"`
	Agents        map[string]AgentConfig `toml:"agents"`
	Embedding     EmbeddingConfig        `toml:"embedding"`
	Summarisation SummarisationConfig    `toml:"summarisation"`
	Apps          AppsConfig             `toml:"apps"`
	Keybinds      KeybindsConfig         `toml:"keybinds"`
	Index         IndexConfig            `toml:"index"`
	Updates       UpdatesConfig          `toml:"updates"`
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
		Active: ActiveConfig{
			Agent: "local",
			Level: "medium",
		},
		Agents: map[string]AgentConfig{
			"local": {
				Provider:    "openai_compatible",
				BaseURL:     "http://localhost:11434/v1",
				Model:       "qwen2.5-coder:7b",
				EmbedModel:  "nomic-embed-text",
				TimeoutSecs: 120,
			},
			"opencode_zen": {
				Provider:    "opencode_zen",
				Model:       "opencode/gpt-5.5",
				APIKeyEnv:   "OPENCODE_API_KEY",
				TimeoutSecs: 120,
			},
			"cursor": {
				Provider:    "cursor_agent",
				BaseURL:     "https://api.cursor.com/v1",
				Model:       "composer-2",
				APIKeyEnv:   "CURSOR_API_KEY",
				TimeoutSecs: 600,
				Mode:        "agent",
			},
		},
		Embedding: EmbeddingConfig{
			Provider:    "openai_compatible",
			BaseURL:     "http://localhost:11434/v1",
			Model:       "nomic-embed-text",
			TimeoutSecs: 120,
		},
		Summarisation: SummarisationConfig{
			SystemPrompt:        "",
			Query:               "Summarise this document for lup. Return only the JSON object matching the schema.",
			MaxSymbolsPerPrompt: 25,
			BodyBudget:          8000,
			Temperature:         0.2,
		},
		Apps: AppsConfig{
			Editor: "",
			Opener: "",
		},
		Keybinds: KeybindsConfig{
			Up:          "up",
			Down:        "down",
			Left:        "left",
			Right:       "right",
			Confirm:     "enter",
			PageUp:      "pgup",
			PageDown:    "pgdown",
			JumpTop:     "home",
			JumpBottom:  "end",
			Quit:        "q",
			Edit:        "e",
			Options:     "o",
			SwitchAgent: "m",
			SwitchLevel: "l",
			Regenerate:  "r",
			ShowHints:   "?",
			ShowUpdates: "U",
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

	normalize(&cfg)
	return cfg, nil
}

func normalize(cfg *Config) {
	if cfg.Agents == nil {
		cfg.Agents = map[string]AgentConfig{}
	}
	if _, ok := cfg.Agents["local"]; !ok {
		cfg.Agents["local"] = AgentConfig{
			Provider:    "openai_compatible",
			BaseURL:     cfg.LLM.BaseURL,
			Model:       cfg.LLM.ChatModel,
			EmbedModel:  cfg.LLM.EmbedModel,
			APIKey:      cfg.LLM.APIKey,
			TimeoutSecs: cfg.LLM.TimeoutSecs,
		}
	} else {
		local := cfg.Agents["local"]
		defaultLocal := defaults().Agents["local"]
		if local.Provider == defaultLocal.Provider && local.BaseURL == defaultLocal.BaseURL && local.Model == defaultLocal.Model && local.EmbedModel == defaultLocal.EmbedModel && local.APIKey == defaultLocal.APIKey {
			local.BaseURL = cfg.LLM.BaseURL
			local.Model = cfg.LLM.ChatModel
			local.EmbedModel = cfg.LLM.EmbedModel
			local.APIKey = cfg.LLM.APIKey
			local.TimeoutSecs = cfg.LLM.TimeoutSecs
			cfg.Agents["local"] = local
		}
	}
	if cfg.Active.Agent == "" {
		cfg.Active.Agent = "local"
	}
	if cfg.Active.Level == "" {
		cfg.Active.Level = "medium"
	}
	if cfg.Embedding.Provider == "" {
		cfg.Embedding.Provider = "openai_compatible"
	}
	if cfg.Embedding.BaseURL == "" {
		cfg.Embedding.BaseURL = cfg.LLM.BaseURL
	}
	if cfg.Embedding.Model == "" {
		cfg.Embedding.Model = cfg.LLM.EmbedModel
	}
	if cfg.Embedding.TimeoutSecs <= 0 {
		cfg.Embedding.TimeoutSecs = cfg.LLM.TimeoutSecs
	}
	if cfg.Summarisation.MaxSymbolsPerPrompt <= 0 {
		cfg.Summarisation.MaxSymbolsPerPrompt = 25
	}
	if cfg.Summarisation.BodyBudget <= 0 {
		cfg.Summarisation.BodyBudget = 8000
	}
	if cfg.Summarisation.Temperature == 0 {
		cfg.Summarisation.Temperature = 0.2
	}
	if cfg.Summarisation.Query == "" {
		cfg.Summarisation.Query = "Summarise this document for lup. Return only the JSON object matching the schema."
	}
}

// ActiveAgent returns the selected generation backend.
func (cfg Config) ActiveAgent() AgentConfig {
	if a, ok := cfg.Agents[cfg.Active.Agent]; ok {
		return a
	}
	return cfg.Agents["local"]
}

// APIKey resolves an inline API key or an api_key_env reference.
func (a AgentConfig) ResolvedAPIKey() string {
	if a.APIKey != "" {
		return a.APIKey
	}
	if a.APIKeyEnv != "" {
		return os.Getenv(a.APIKeyEnv)
	}
	return ""
}

// ResolvedAPIKey resolves an inline embedding API key or an api_key_env reference.
func (e EmbeddingConfig) ResolvedAPIKey() string {
	if e.APIKey != "" {
		return e.APIKey
	}
	if e.APIKeyEnv != "" {
		return os.Getenv(e.APIKeyEnv)
	}
	return ""
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
	normalize(&cfg)
	local := cfg.Agents["local"]
	opencode := cfg.Agents["opencode_zen"]
	cursor := cfg.Agents["cursor"]
	return fmt.Sprintf(`# LUP configuration file
#
# Copy to ~/.config/lup/config.toml for global settings, or to
# <project>/.lup/config.toml for per-project overrides.
#
# Any field omitted here falls back to the built-in default.

[active]
# Selected generator profile and effort level used by lup tui and generation commands.
agent = %s
level = %s  # low, medium, high, xhigh

[llm]
# Backward-compatible OpenAI-compatible settings used by existing CLI commands.
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

[agents.local]
# Direct OpenAI-compatible generation for Ollama, vLLM, LM Studio, OpenAI-compatible gateways.
provider = %s
base_url = %s
model = %s
embed_model = %s
api_key = %s
api_key_env = %s
timeout_secs = %d

[agents.opencode_zen]
# OpenCode Zen pay-as-you-go gateway. Set OPENCODE_API_KEY or api_key.
provider = %s
model = %s
api_key = %s
api_key_env = %s
timeout_secs = %d

[agents.cursor]
# Cursor Cloud Agent backend. Set CURSOR_API_KEY or api_key.
provider = %s
base_url = %s
model = %s
api_key = %s
api_key_env = %s
timeout_secs = %d
mode = %s

[embedding]
# Embeddings remain separate so agent-backed summarisation can still use local lookup/indexing.
provider = %s
base_url = %s
model = %s
api_key = %s
api_key_env = %s
timeout_secs = %d

[summarisation]
query = %s
system_prompt = %s
max_symbols_per_prompt = %d
body_budget = %d
temperature = %.2f

[apps]
editor = %s
opener = %s

[keybinds]
up = %s
down = %s
left = %s
right = %s
confirm = %s
page_up = %s
page_down = %s
jump_top = %s
jump_bottom = %s
quit = %s
edit = %s
options = %s
switch_agent = %s
switch_level = %s
regenerate = %s
show_hints = %s
show_updates = %s

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
`, quote(cfg.Active.Agent), quote(cfg.Active.Level), quote(cfg.LLM.BaseURL), quote(cfg.LLM.ChatModel), quote(cfg.LLM.EmbedModel), quote(cfg.LLM.APIKey), cfg.LLM.TimeoutSecs,
		quote(local.Provider), quote(local.BaseURL), quote(local.Model), quote(local.EmbedModel), quote(local.APIKey), quote(local.APIKeyEnv), local.TimeoutSecs,
		quote(opencode.Provider), quote(opencode.Model), quote(opencode.APIKey), quote(opencode.APIKeyEnv), opencode.TimeoutSecs,
		quote(cursor.Provider), quote(cursor.BaseURL), quote(cursor.Model), quote(cursor.APIKey), quote(cursor.APIKeyEnv), cursor.TimeoutSecs, quote(cursor.Mode),
		quote(cfg.Embedding.Provider), quote(cfg.Embedding.BaseURL), quote(cfg.Embedding.Model), quote(cfg.Embedding.APIKey), quote(cfg.Embedding.APIKeyEnv), cfg.Embedding.TimeoutSecs,
		quote(cfg.Summarisation.Query), quote(cfg.Summarisation.SystemPrompt), cfg.Summarisation.MaxSymbolsPerPrompt, cfg.Summarisation.BodyBudget, cfg.Summarisation.Temperature,
		quote(cfg.Apps.Editor), quote(cfg.Apps.Opener),
		quote(cfg.Keybinds.Up), quote(cfg.Keybinds.Down), quote(cfg.Keybinds.Left), quote(cfg.Keybinds.Right), quote(cfg.Keybinds.Confirm), quote(cfg.Keybinds.PageUp), quote(cfg.Keybinds.PageDown), quote(cfg.Keybinds.JumpTop), quote(cfg.Keybinds.JumpBottom), quote(cfg.Keybinds.Quit), quote(cfg.Keybinds.Edit), quote(cfg.Keybinds.Options), quote(cfg.Keybinds.SwitchAgent), quote(cfg.Keybinds.SwitchLevel), quote(cfg.Keybinds.Regenerate), quote(cfg.Keybinds.ShowHints), quote(cfg.Keybinds.ShowUpdates),
		cfg.Index.TopK, cfg.Index.AutoSummarise, cfg.Index.Concurrency, cfg.Updates.DisableChecks, quote(cfg.Updates.CurrentCommit), quote(cfg.Updates.RepoPath), quote(cfg.Updates.Terminal))
}

func quote(s string) string {
	s = strings.ReplaceAll(s, `\`, `\\`)
	s = strings.ReplaceAll(s, `"`, `\"`)
	s = strings.ReplaceAll(s, "\n", `\n`)
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
