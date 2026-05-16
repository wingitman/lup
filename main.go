package main

import (
	gocontext "context"
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/spf13/cobra"

	"github.com/wingitman/lup/internal/config"
	"github.com/wingitman/lup/internal/llm"
	"github.com/wingitman/lup/internal/parser"
	"github.com/wingitman/lup/internal/rag"
	"github.com/wingitman/lup/internal/store"
	appupdate "github.com/wingitman/lup/internal/update"
	appversion "github.com/wingitman/lup/internal/version"
)

var outputJSON bool

func main() {
	var recordUpdate bool
	var updateCommit string
	var updateRepo string

	root := &cobra.Command{
		Use:     "lup",
		Short:   "LUP — look up your codebase with AI-powered summaries",
		Version: appversion.Commit + " (built " + appversion.BuildTime + ")",
		Long: `LUP parses source files, summarises every symbol with an LLM, and indexes
them so you can look up any variable, function, or concept and understand what
it does, where it's used, and what similar symbols exist.

All commands write to stdout and exit cleanly — safe to call from editor plugins.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			if recordUpdate {
				return config.RecordUpdateMetadata(updateCommit, updateRepo)
			}
			return cmd.Help()
		},
	}

	root.PersistentFlags().BoolVar(&outputJSON, "json", false, "emit JSON output")
	root.PersistentFlags().BoolVar(&recordUpdate, "record-update", false, "record installed update metadata and exit")
	root.PersistentFlags().StringVar(&updateCommit, "update-commit", "", "commit to record with --record-update")
	root.PersistentFlags().StringVar(&updateRepo, "update-repo", "", "repo path to record with --record-update")
	_ = root.PersistentFlags().MarkHidden("record-update")
	_ = root.PersistentFlags().MarkHidden("update-commit")
	_ = root.PersistentFlags().MarkHidden("update-repo")

	root.AddCommand(
		summariseCmd(),
		lookupCmd(),
		documentCmd(),
		indexCmd(),
		configCmd(),
		statusCmd(),
		updatesCmd(),
		recordUpdateCmd(),
	)

	if err := root.Execute(); err != nil {
		os.Exit(1)
	}
}

// ──────────────────────────────────────────────────────────
// lup summarise <file>
// ──────────────────────────────────────────────────────────

func summariseCmd() *cobra.Command {
	var force bool

	cmd := &cobra.Command{
		Use:   "summarise <file>",
		Short: "Parse and summarise a source file, storing the result in .lup/",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			absPath, err := filepath.Abs(args[0])
			if err != nil {
				return err
			}

			projectRoot := config.ProjectRoot(filepath.Dir(absPath))
			if err := config.EnsureProjectDir(projectRoot); err != nil {
				return err
			}

			cfg, err := config.Load(projectRoot)
			if err != nil {
				return err
			}

			relPath, err := filepath.Rel(projectRoot, absPath)
			if err != nil {
				relPath = args[0]
			}

			if !force && store.SummaryExists(projectRoot, relPath) {
				printInfo(fmt.Sprintf("already summarised: %s (use --force to re-run)", relPath))
				return nil
			}

			parsed, err := parser.ParseFile(absPath)
			if err != nil {
				return fmt.Errorf("parse: %w", err)
			}

			client := newLLMClient(cfg)
			ctx := gocontext.Background()

			fs, err := summariseFile(ctx, client, relPath, parsed)
			if err != nil {
				return err
			}

			if err := store.WriteSummary(projectRoot, fs); err != nil {
				return err
			}

			vs, err := store.OpenVectorStore(projectRoot)
			if err != nil {
				return fmt.Errorf("vector store: %w", err)
			}
			defer vs.Close()

			engine := rag.New(client, vs, projectRoot)
			if err := engine.IndexSummary(ctx, fs); err != nil {
				printWarning(fmt.Sprintf("indexing failed (summary still saved): %v", err))
			}

			if outputJSON {
				return printJSON(fs)
			}
			printInfo(fmt.Sprintf("summarised %s — %d symbol(s) indexed", relPath, len(fs.Symbols)))
			return nil
		},
	}

	cmd.Flags().BoolVarP(&force, "force", "f", false, "re-summarise even if already done")
	return cmd
}

// ──────────────────────────────────────────────────────────
// lup lookup <text>
// ──────────────────────────────────────────────────────────

func lookupCmd() *cobra.Command {
	var topK int
	var context string
	var showAll bool

	cmd := &cobra.Command{
		Use:   "lookup <text>",
		Short: "Semantic RAG lookup: find symbols relevant to the given text",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			queryText := args[0]

			embeddingQuery := queryText
			if context != "" {
				embeddingQuery = context + "\n\n" + queryText
			}

			cwd, _ := os.Getwd()
			projectRoot := config.ProjectRoot(cwd)

			cfg, err := config.Load(projectRoot)
			if err != nil {
				return err
			}
			if topK == 0 {
				topK = cfg.Index.TopK
			}

			vs, err := store.OpenVectorStore(projectRoot)
			if err != nil {
				return fmt.Errorf("vector store: %w", err)
			}
			defer vs.Close()

			client := newLLMClient(cfg)
			engine := rag.New(client, vs, projectRoot)

			ctx := gocontext.Background()
			results, err := engine.Lookup(ctx, embeddingQuery, topK, showAll)
			if err != nil {
				return err
			}

			if outputJSON {
				return printJSON(results)
			}

			if len(results) == 0 {
				printInfo("no relevant symbols found — try running `lup document .` first")
				return nil
			}

			for i, r := range results {
				printLookupResult(i+1, r, showAll)
			}
			return nil
		},
	}

	cmd.Flags().IntVarP(&topK, "top", "k", 0, "number of results (default from config)")
	cmd.Flags().StringVar(&context, "context", "", "surrounding lines to enrich the embedding query")
	cmd.Flags().BoolVar(&showAll, "show-all", false, "show all usage locations (default: cap at 10)")
	return cmd
}

// printLookupResult renders a single LookupResult to stdout.
func printLookupResult(n int, r rag.LookupResult, showAll bool) {
	// Header line.
	loc := r.File
	if r.StartLine > 0 {
		loc = fmt.Sprintf("%s:%d", r.File, r.StartLine)
	}
	sep := strings.Repeat("─", 60)
	fmt.Printf("\n%s\n", sep)
	fmt.Printf("── %s  (%s)  %s\n", r.SymbolName, r.SymbolKind, loc)
	fmt.Printf("%s\n", sep)

	// Summary.
	if r.Summary != "" {
		fmt.Printf("\n%s\n", wordWrap(r.Summary, 72))
	}

	// Usages.
	if r.UsageCount > 0 {
		showCount := len(r.Usages)
		if !showAll && r.UsageCount > 10 {
			fmt.Printf("\nUsed in %d places (showing 10, use --show-all for all):\n", r.UsageCount)
		} else {
			fmt.Printf("\nUsed in %d place%s:\n", r.UsageCount, pluralS(r.UsageCount))
		}
		for i, u := range r.Usages {
			_ = i
			loc := u.File
			if u.StartLine > 0 {
				loc = fmt.Sprintf("%s:%d", u.File, u.StartLine)
			}
			fmt.Printf("  %-45s  %s  (%s)\n", loc, u.SymbolName, u.SymbolKind)
			if u.Context != "" {
				fmt.Printf("    %s\n", truncate(u.Context, 70))
			}
		}
		if !showAll && r.UsageCount > showCount {
			fmt.Printf("  ... and %d more (use --show-all)\n", r.UsageCount-showCount)
		}
	}

	// Similar symbols.
	if len(r.Similar) > 0 {
		fmt.Printf("\nSimilar symbols:\n")
		for _, s := range r.Similar {
			fmt.Printf("  %-20s  (%s)  %s\n", s.SymbolName, s.SymbolKind, s.File)
			if s.Summary != "" {
				fmt.Printf("    %s\n", truncate(s.Summary, 70))
			}
		}
	}

	fmt.Printf("\n  distance: %.4f\n", r.Distance)
}

// ──────────────────────────────────────────────────────────
// lup document [dir]
// ──────────────────────────────────────────────────────────

// dirSkipList is the set of directory names that lup never descends into.
var dirSkipList = map[string]bool{
	".git": true, "node_modules": true, "vendor": true,
	"dist": true, "build": true, ".lup": true,
	"__pycache__": true, ".cache": true, ".next": true,
	"coverage": true, "target": true, ".venv": true,
	"venv": true, "env": true,
}

func documentCmd() *cobra.Command {
	var force bool
	var concurrency int

	cmd := &cobra.Command{
		Use:   "document [dir]",
		Short: "Summarise every supported file in a directory tree",
		Long: `document walks the directory tree from [dir] (default: current directory),
summarises every supported source file it finds, and indexes all symbols into
the vector store.

Files already summarised are skipped unless --force is given.
Directories named .git, node_modules, vendor, dist, etc. are always skipped.
Lines from .gitignore are respected as path prefix/glob filters.`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			root := "."
			if len(args) == 1 {
				root = args[0]
			}

			absRoot, err := filepath.Abs(root)
			if err != nil {
				return err
			}

			projectRoot := config.ProjectRoot(absRoot)
			if err := config.EnsureProjectDir(projectRoot); err != nil {
				return err
			}

			cfg, err := config.Load(projectRoot)
			if err != nil {
				return err
			}

			if concurrency == 0 {
				concurrency = cfg.Index.Concurrency
				if concurrency <= 0 {
					concurrency = 2
				}
			}

			// Load .gitignore patterns (best-effort).
			ignorePatterns := loadGitignore(absRoot)

			// Collect all files to process.
			var files []string
			err = filepath.WalkDir(absRoot, func(path string, d fs.DirEntry, err error) error {
				if err != nil {
					return nil // skip unreadable entries
				}
				if d.IsDir() {
					name := d.Name()
					if dirSkipList[name] || strings.HasPrefix(name, ".") && name != "." {
						return filepath.SkipDir
					}
					return nil
				}

				// Check gitignore patterns.
				rel, _ := filepath.Rel(absRoot, path)
				if matchesIgnore(rel, ignorePatterns) {
					return nil
				}

				if isSupportedFile(path) {
					files = append(files, path)
				}
				return nil
			})
			if err != nil {
				return fmt.Errorf("document: walk %s: %w", absRoot, err)
			}

			total := len(files)
			if total == 0 {
				printInfo("no supported files found")
				return nil
			}

			// Open shared resources.
			client := newLLMClient(cfg)
			vs, err := store.OpenVectorStore(projectRoot)
			if err != nil {
				return fmt.Errorf("vector store: %w", err)
			}
			defer vs.Close()

			engine := rag.New(client, vs, projectRoot)

			// Worker pool.
			fileCh := make(chan string, total)
			for _, f := range files {
				fileCh <- f
			}
			close(fileCh)

			var (
				newCount     int64
				skippedCount int64
				failedCount  int64
				processed    int64
				mu           sync.Mutex
			)

			worker := func() {
				for absPath := range fileCh {
					relPath, _ := filepath.Rel(projectRoot, absPath)
					if relPath == "" {
						relPath = absPath
					}

					n := atomic.AddInt64(&processed, 1)

					if !force && store.SummaryExists(projectRoot, relPath) {
						atomic.AddInt64(&skippedCount, 1)
						continue
					}

					parsed, err := parser.ParseFile(absPath)
					if err != nil {
						atomic.AddInt64(&failedCount, 1)
						printWarning(fmt.Sprintf("parse failed %s: %v", relPath, err))
						continue
					}

					ctx := gocontext.Background()
					fs, err := summariseFile(ctx, client, relPath, parsed)
					if err != nil {
						atomic.AddInt64(&failedCount, 1)
						printWarning(fmt.Sprintf("summarise failed %s: %v", relPath, err))
						continue
					}

					if err := store.WriteSummary(projectRoot, fs); err != nil {
						atomic.AddInt64(&failedCount, 1)
						printWarning(fmt.Sprintf("write failed %s: %v", relPath, err))
						continue
					}

					if err := engine.IndexSummary(ctx, fs); err != nil {
						printWarning(fmt.Sprintf("index failed %s: %v", relPath, err))
					}

					atomic.AddInt64(&newCount, 1)

					mu.Lock()
					fmt.Printf("[%d/%d] summarised %s — %d symbol(s)\n", n, total, relPath, len(fs.Symbols))
					mu.Unlock()
				}
			}

			var wg sync.WaitGroup
			for i := 0; i < concurrency; i++ {
				wg.Add(1)
				go func() {
					defer wg.Done()
					worker()
				}()
			}
			wg.Wait()

			printInfo(fmt.Sprintf(
				"document complete: %d new, %d skipped, %d failed",
				newCount, skippedCount, failedCount,
			))
			return nil
		},
	}

	cmd.Flags().BoolVarP(&force, "force", "f", false, "re-summarise files already done")
	cmd.Flags().IntVar(&concurrency, "concurrency", 0, "parallel workers (default from config)")
	return cmd
}

// isSupportedFile reports whether path has an extension or base name lup can parse.
func isSupportedFile(path string) bool {
	supported := map[string]bool{
		".go": true, ".py": true, ".js": true, ".jsx": true, ".ts": true, ".tsx": true,
		".sh": true, ".bash": true, ".zsh": true,
		".toml": true, ".yaml": true, ".yml": true,
		".json": true, ".md": true, ".markdown": true,
	}
	supportedNames := map[string]bool{
		"makefile": true, "dockerfile": true, "vagrantfile": true, "justfile": true,
	}
	ext := strings.ToLower(filepath.Ext(path))
	base := strings.ToLower(filepath.Base(path))
	return supported[ext] || supportedNames[base]
}

// loadGitignore reads .gitignore from dir and returns non-comment, non-empty lines.
func loadGitignore(dir string) []string {
	data, err := os.ReadFile(filepath.Join(dir, ".gitignore"))
	if err != nil {
		return nil
	}
	var patterns []string
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		patterns = append(patterns, line)
	}
	return patterns
}

// matchesIgnore reports whether relPath matches any gitignore pattern.
func matchesIgnore(relPath string, patterns []string) bool {
	relPath = filepath.ToSlash(relPath)
	for _, pat := range patterns {
		pat = filepath.ToSlash(pat)
		// Exact or prefix match.
		if relPath == pat || strings.HasPrefix(relPath, pat+"/") {
			return true
		}
		// Glob match against the base name.
		if matched, _ := filepath.Match(pat, filepath.Base(relPath)); matched {
			return true
		}
		// Glob match against the full path.
		if matched, _ := filepath.Match(pat, relPath); matched {
			return true
		}
	}
	return false
}

// ──────────────────────────────────────────────────────────
// lup index
// ──────────────────────────────────────────────────────────

func indexCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "index",
		Short: "Re-embed all stored summaries into the vector index",
		RunE: func(cmd *cobra.Command, args []string) error {
			cwd, _ := os.Getwd()
			projectRoot := config.ProjectRoot(cwd)

			cfg, err := config.Load(projectRoot)
			if err != nil {
				return err
			}

			summaries, err := store.ListSummaries(projectRoot)
			if err != nil {
				return err
			}
			if len(summaries) == 0 {
				printInfo("no summaries found — run `lup document .` first")
				return nil
			}

			vs, err := store.OpenVectorStore(projectRoot)
			if err != nil {
				return err
			}
			defer vs.Close()

			client := newLLMClient(cfg)
			engine := rag.New(client, vs, projectRoot)
			ctx := gocontext.Background()

			var indexed, failed int
			for _, fs := range summaries {
				if err := engine.IndexSummary(ctx, fs); err != nil {
					printWarning(fmt.Sprintf("failed to index %s: %v", fs.File, err))
					failed++
				} else {
					indexed++
					printInfo(fmt.Sprintf("indexed %s", fs.File))
				}
			}

			printInfo(fmt.Sprintf("done: %d indexed, %d failed", indexed, failed))
			return nil
		},
	}
}

// ──────────────────────────────────────────────────────────
// lup config
// ──────────────────────────────────────────────────────────

func configCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "config",
		Short: "Show the resolved configuration for the current project",
		RunE: func(cmd *cobra.Command, args []string) error {
			cwd, _ := os.Getwd()
			projectRoot := config.ProjectRoot(cwd)

			cfg, err := config.Load(projectRoot)
			if err != nil {
				return err
			}

			if outputJSON {
				return printJSON(cfg)
			}

			fmt.Printf("[llm]\n")
			fmt.Printf("  base_url     = %s\n", cfg.LLM.BaseURL)
			fmt.Printf("  chat_model   = %s\n", cfg.LLM.ChatModel)
			fmt.Printf("  embed_model  = %s\n", cfg.LLM.EmbedModel)
			fmt.Printf("  api_key      = %s\n", maskAPIKey(cfg.LLM.APIKey))
			fmt.Printf("  timeout      = %ds\n", cfg.LLM.TimeoutSecs)
			fmt.Printf("\n[index]\n")
			fmt.Printf("  top_k          = %d\n", cfg.Index.TopK)
			fmt.Printf("  auto_summarise = %v\n", cfg.Index.AutoSummarise)
			fmt.Printf("  concurrency    = %d\n", cfg.Index.Concurrency)
			fmt.Printf("\n[updates]\n")
			fmt.Printf("  disable_checks = %v\n", cfg.Updates.DisableChecks)
			fmt.Printf("  current_commit = %s\n", cfg.Updates.CurrentCommit)
			fmt.Printf("  repo_path      = %s\n", cfg.Updates.RepoPath)
			fmt.Printf("  terminal       = %s\n", cfg.Updates.Terminal)
			fmt.Printf("\n[project]\n")
			fmt.Printf("  root = %s\n", projectRoot)
			return nil
		},
	}
}

// ──────────────────────────────────────────────────────────
// lup status
// ──────────────────────────────────────────────────────────

func statusCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "status",
		Short: "Show what files are summarised in the current project",
		RunE: func(cmd *cobra.Command, args []string) error {
			cwd, _ := os.Getwd()
			projectRoot := config.ProjectRoot(cwd)

			summaries, err := store.ListSummaries(projectRoot)
			if err != nil {
				return err
			}

			type statusEntry struct {
				File         string    `json:"file"`
				SummarisedAt time.Time `json:"summarised_at"`
				Symbols      int       `json:"symbols"`
			}

			entries := make([]statusEntry, 0, len(summaries))
			for _, fs := range summaries {
				entries = append(entries, statusEntry{
					File:         fs.File,
					SummarisedAt: fs.SummarisedAt,
					Symbols:      len(fs.Symbols),
				})
			}

			if outputJSON {
				return printJSON(entries)
			}

			if len(entries) == 0 {
				printInfo(fmt.Sprintf("no summaries in %s — run `lup document .` to get started", projectRoot))
				return nil
			}

			fmt.Printf("project root: %s\n\n", projectRoot)
			fmt.Printf("%-50s %-8s %s\n", "file", "symbols", "summarised at")
			fmt.Println(strings.Repeat("─", 80))
			for _, e := range entries {
				fmt.Printf("%-50s %-8d %s\n", e.File, e.Symbols, e.SummarisedAt.Format("2006-01-02 15:04"))
			}
			return nil
		},
	}
}

// ──────────────────────────────────────────────────────────
// lup updates
// ──────────────────────────────────────────────────────────

func updatesCmd() *cobra.Command {
	var check bool
	var install bool
	var commit string
	var historyLimit int

	cmd := &cobra.Command{
		Use:   "updates",
		Short: "Show update status or launch a detached git-based installer",
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := config.Load("")
			if err != nil {
				return err
			}

			if commit != "" {
				install = true
			}
			checkCfg := cfg
			if install {
				checkCfg.Updates.DisableChecks = false
			}

			info := appupdate.Check(&checkCfg, appversion.Commit, historyLimit)
			if !install {
				if outputJSON {
					return printJSON(info)
				}
				if check {
					printUpdateCheck(info)
				} else {
					printUpdateStatus(info)
				}
				return nil
			}

			if info.RepoPath == "" {
				return fmt.Errorf("update repo unavailable: %s", info.CheckError)
			}
			target := commit
			latest := target == ""
			if latest {
				target = info.LatestCommit
			}
			if target == "" {
				return fmt.Errorf("could not resolve update target")
			}
			recorder, _ := os.Executable()
			req := appupdate.InstallRequest{
				RepoPath:       info.RepoPath,
				TargetCommit:   target,
				Latest:         latest,
				Terminal:       cfg.Updates.Terminal,
				RecorderBinary: recorder,
			}
			if err := appupdate.LaunchDetached(req); err != nil {
				return err
			}
			if outputJSON {
				return printJSON(map[string]interface{}{
					"launched":      true,
					"repo_path":     info.RepoPath,
					"target_commit": target,
					"latest":        latest,
				})
			}
			printInfo(fmt.Sprintf("update installer launched for %s", shortCommit(target)))
			return nil
		},
	}

	cmd.Flags().BoolVar(&check, "check", false, "check for available updates and exit")
	cmd.Flags().BoolVar(&install, "install", false, "launch a detached terminal to install the latest update")
	cmd.Flags().StringVar(&commit, "commit", "", "install a specific commit, including older versions")
	cmd.Flags().IntVar(&historyLimit, "history", 12, "number of commits to show")
	return cmd
}

func recordUpdateCmd() *cobra.Command {
	var commit string
	var repo string
	cmd := &cobra.Command{
		Use:    "record-update",
		Hidden: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			return config.RecordUpdateMetadata(commit, repo)
		},
	}
	cmd.Flags().StringVar(&commit, "commit", "", "commit to record")
	cmd.Flags().StringVar(&repo, "repo", "", "repo path to record")
	return cmd
}

func printUpdateCheck(info appupdate.Info) {
	if !info.UpdatesEnabled {
		printInfo("update checks disabled")
		return
	}
	if info.CheckError != "" {
		printInfo("update check failed: " + info.CheckError)
		return
	}
	if len(info.Available) > 0 {
		printInfo(fmt.Sprintf("updates available: %d commit%s (current %s, latest %s)", len(info.Available), pluralS(len(info.Available)), shortCommit(info.CurrentCommit), shortCommit(info.LatestCommit)))
		return
	}
	printInfo("up to date: " + shortCommit(info.CurrentCommit))
}

func printUpdateStatus(info appupdate.Info) {
	if !info.UpdatesEnabled {
		printInfo("update checks disabled")
		return
	}
	if info.CheckError != "" {
		printInfo("update check failed: " + info.CheckError)
		return
	}
	fmt.Printf("repo:     %s\n", info.RepoPath)
	fmt.Printf("branch:   %s\n", info.Branch)
	fmt.Printf("upstream: %s\n", info.Upstream)
	fmt.Printf("current:  %s\n", shortCommit(info.CurrentCommit))
	fmt.Printf("latest:   %s\n", shortCommit(info.LatestCommit))
	if len(info.Available) > 0 {
		fmt.Printf("status:   %d update%s available\n", len(info.Available), pluralS(len(info.Available)))
		fmt.Println("\navailable:")
		for _, c := range info.Available {
			fmt.Printf("  %s  %s  %s\n", c.Short, c.Date, c.Subject)
		}
	} else {
		fmt.Println("status:   up to date")
	}
	if len(info.History) > 0 {
		fmt.Println("\nhistory:")
		for _, c := range info.History {
			fmt.Printf("  %s  %s  %s\n", c.Short, c.Date, c.Subject)
		}
	}
}

func shortCommit(hash string) string {
	if len(hash) > 12 {
		return hash[:12]
	}
	if hash == "" {
		return "unknown"
	}
	return hash
}

// ──────────────────────────────────────────────────────────
// LLM summarisation
// ──────────────────────────────────────────────────────────

const systemPrompt = `You are a code documentation assistant. Given a source file and its extracted
symbols, produce concise, accurate summaries.

Rules:
- FUNCTIONS/METHODS: describe what it does and its side effects in one sentence.
- VARIABLES/CONSTANTS: describe the CONCEPT the name represents and HOW it is
  used. If occurrence_count > 1, note the pattern of usage across scopes.
  Example: "outputJSON controls whether CLI output is emitted as JSON or
  human-readable text; enabled by the --json global flag."
- CLASSES/STRUCTS/INTERFACES: describe the entity and its responsibilities.
- CHUNKS (plain-text sections): summarise what this section configures or does.
- For ALL symbols: populate "references" with names of other symbols, variables,
  or functions this symbol uses, calls, reads, or depends on. Be specific —
  list actual names visible in the code, not generic descriptions.

Respond ONLY with valid JSON matching this schema exactly:
{
  "file_summary": "One or two sentences describing the overall purpose of the file.",
  "symbols": [
    {
      "name": "<symbol name>",
      "kind": "<function|method|variable|constant|class|struct|interface|attribute|chunk>",
      "signature": "<first line of the symbol declaration>",
      "summary": "<description>",
      "references": ["name1", "name2"]
    }
  ]
}
Do not add any text outside the JSON object.`

type llmSummaryResponse struct {
	FileSummary string                `json:"file_summary"`
	Symbols     []store.SymbolSummary `json:"symbols"`
}

func summariseFile(ctx gocontext.Context, client *llm.Client, relPath string, parsed *parser.File) (store.FileSummary, error) {
	var b strings.Builder
	b.WriteString(fmt.Sprintf("File: %s\nLanguage: %s\n\n", relPath, parsed.Lang))

	if len(parsed.Symbols) == 0 {
		b.WriteString("No symbols were extracted. Summarise the file based on its path alone.\n")
	} else {
		// Cap symbols sent to the LLM to prevent overly long responses that
		// models truncate mid-JSON.  Prioritise: vars/consts → structs/classes
		// → functions/methods (first N of each bucket).
		const maxSymbolsPerPrompt = 25
		symsToSend := parsed.Symbols
		if len(symsToSend) > maxSymbolsPerPrompt {
			var priority0, priority1, priority2 []parser.Symbol
			for _, s := range symsToSend {
				switch s.Kind {
				case "variable", "constant", "attribute":
					priority0 = append(priority0, s)
				case "struct", "interface", "class":
					priority1 = append(priority1, s)
				default:
					priority2 = append(priority2, s)
				}
			}
			symsToSend = nil
			for _, bucket := range [][]parser.Symbol{priority0, priority1, priority2} {
				for _, s := range bucket {
					symsToSend = append(symsToSend, s)
					if len(symsToSend) >= maxSymbolsPerPrompt {
						break
					}
				}
				if len(symsToSend) >= maxSymbolsPerPrompt {
					break
				}
			}
		}

		b.WriteString("Symbols:\n")
		for _, sym := range symsToSend {
			occ := ""
			if sym.OccurrenceCount > 1 {
				occ = fmt.Sprintf(" [occurrence_count=%d]", sym.OccurrenceCount)
			}
			b.WriteString(fmt.Sprintf("  [%s] %s%s\n    signature: %s\n",
				sym.Kind, sym.Name, occ, sym.Signature))
		}

		// Include symbol bodies up to a total character budget. Prioritise
		// variables and constants first (they're small and need body context
		// to be summarised well), then functions/methods/chunks.
		// Replace literal tabs so the LLM's JSON output stays valid.
		const bodyBudget = 8000
		used := 0
		var bodyLines []string

		// Priority order: variables/constants first, then structural, then functions.
		priority := func(kind string) int {
			switch kind {
			case "variable", "constant", "attribute":
				return 0
			case "struct", "interface", "class":
				return 1
			default:
				return 2
			}
		}
		// Re-sort symsToSend by priority for body inclusion.
		sortedForBody := make([]parser.Symbol, len(symsToSend))
		copy(sortedForBody, symsToSend)
		for i := 1; i < len(sortedForBody); i++ {
			for j := i; j > 0 && priority(sortedForBody[j].Kind) < priority(sortedForBody[j-1].Kind); j-- {
				sortedForBody[j], sortedForBody[j-1] = sortedForBody[j-1], sortedForBody[j]
			}
		}

		for _, sym := range sortedForBody {
			lines := strings.Split(sym.Body, "\n")
			maxLines := 30
			if sym.Kind == "variable" || sym.Kind == "constant" || sym.Kind == "attribute" {
				maxLines = 5 // vars are small, don't need many lines
			}
			if len(lines) > maxLines {
				lines = lines[:maxLines]
			}
			clean := strings.ReplaceAll(strings.Join(lines, "\n"), "\t", "    ")
			chunk := fmt.Sprintf("\n--- %s %s ---\n%s\n", sym.Kind, sym.Name, clean)
			if used+len(chunk) > bodyBudget {
				break
			}
			bodyLines = append(bodyLines, chunk)
			used += len(chunk)
		}
		if len(bodyLines) > 0 {
			b.WriteString("\nSymbol bodies (truncated):\n")
			for _, l := range bodyLines {
				b.WriteString(l)
			}
		}
	}

	raw, err := client.Complete(ctx, systemPrompt, b.String())
	if err != nil {
		return store.FileSummary{}, fmt.Errorf("llm summarise: %w", err)
	}

	raw = strings.TrimSpace(raw)
	raw = strings.TrimPrefix(raw, "```json")
	raw = strings.TrimPrefix(raw, "```")
	raw = strings.TrimSuffix(raw, "```")
	raw = strings.TrimSpace(raw)

	var resp llmSummaryResponse
	if err := json.Unmarshal([]byte(raw), &resp); err != nil {
		return store.FileSummary{}, fmt.Errorf("llm response parse: %w\nraw: %s", err, raw)
	}

	// Copy StartLine/EndLine from parser symbols into the LLM response,
	// since the LLM doesn't know line numbers.
	lineMap := map[string]parser.Symbol{}
	for _, s := range parsed.Symbols {
		lineMap[s.Name] = s
	}
	for i := range resp.Symbols {
		if ps, ok := lineMap[resp.Symbols[i].Name]; ok {
			resp.Symbols[i].StartLine = ps.StartLine
			resp.Symbols[i].EndLine = ps.EndLine
			resp.Symbols[i].OccurrenceCount = ps.OccurrenceCount
		}
	}

	return store.FileSummary{
		File:         relPath,
		SummarisedAt: time.Now().UTC(),
		FileSummary:  resp.FileSummary,
		Symbols:      resp.Symbols,
	}, nil
}

// ──────────────────────────────────────────────────────────
// Shared helpers
// ──────────────────────────────────────────────────────────

func newLLMClient(cfg config.Config) *llm.Client {
	return llm.New(cfg.LLM.BaseURL, cfg.LLM.APIKey, cfg.LLM.ChatModel, cfg.LLM.EmbedModel, cfg.LLM.TimeoutSecs)
}

func printJSON(v interface{}) error {
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	return enc.Encode(v)
}

func printInfo(msg string)    { fmt.Fprintln(os.Stdout, msg) }
func printWarning(msg string) { fmt.Fprintln(os.Stderr, "warning: "+msg) }

func maskAPIKey(key string) string {
	if key == "" {
		return "(not set)"
	}
	if len(key) <= 8 {
		return "****"
	}
	return key[:4] + strings.Repeat("*", len(key)-8) + key[len(key)-4:]
}

func pluralS(n int) string {
	if n == 1 {
		return ""
	}
	return "s"
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n-1] + "…"
}

func wordWrap(s string, width int) string {
	words := strings.Fields(s)
	if len(words) == 0 {
		return s
	}
	var lines []string
	current := ""
	for _, w := range words {
		if current == "" {
			current = w
		} else if len(current)+1+len(w) <= width {
			current += " " + w
		} else {
			lines = append(lines, current)
			current = w
		}
	}
	if current != "" {
		lines = append(lines, current)
	}
	return strings.Join(lines, "\n")
}
