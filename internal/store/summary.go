// Package store manages the .lup/ directory: JSON summary files and the
// sqlite-vec vector index.
package store

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// SymbolSummary is the LLM-generated summary for a single symbol (function,
// method, variable, constant, class, struct, interface, attribute, or chunk).
type SymbolSummary struct {
	Name string `json:"name"`
	Kind string `json:"kind"` // function|method|variable|constant|class|struct|interface|attribute|chunk

	// Signature is the first line (declaration) of the symbol.
	Signature string `json:"signature"`

	// Summary is a plain-English description of what this symbol does or
	// represents.  For variables/constants the LLM is asked to describe the
	// concept the name represents, not just its type.
	Summary string `json:"summary"`

	// StartLine and EndLine are 1-indexed source positions.
	StartLine uint32 `json:"start_line"`
	EndLine   uint32 `json:"end_line"`

	// OccurrenceCount is how many times this name was declared/assigned in the
	// file (useful for short-variable names that appear in many functions).
	OccurrenceCount int `json:"occurrence_count"`

	// References lists other named symbols this symbol uses, calls, or depends
	// on.  Populated by the LLM during summarisation; used to build the
	// cross-file reference index.
	References []string `json:"references,omitempty"`
}

// FileSummary is the stored document for a single source file.
type FileSummary struct {
	// File is the path relative to the project root.
	File         string          `json:"file"`
	SummarisedAt time.Time       `json:"summarised_at"`
	FileSummary  string          `json:"file_summary"`
	Symbols      []SymbolSummary `json:"symbols"`
}

// ──────────────────────────────────────────────────────────
// Summary persistence
// ──────────────────────────────────────────────────────────

// summaryPath returns the path where the summary for relPath is stored.
func summaryPath(projectRoot, relPath string) string {
	safe := strings.ReplaceAll(relPath, string(filepath.Separator), "_")
	return filepath.Join(projectRoot, ".lup", "summaries", safe+".json")
}

// WriteSummary persists a FileSummary as JSON.
func WriteSummary(projectRoot string, fs FileSummary) error {
	path := summaryPath(projectRoot, fs.File)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("store: mkdir: %w", err)
	}
	f, err := os.Create(path)
	if err != nil {
		return fmt.Errorf("store: create summary: %w", err)
	}
	defer f.Close()
	enc := json.NewEncoder(f)
	enc.SetIndent("", "  ")
	return enc.Encode(fs)
}

// ReadSummary loads the summary for relPath. Returns os.ErrNotExist if not
// yet summarised.
func ReadSummary(projectRoot, relPath string) (FileSummary, error) {
	var fs FileSummary
	path := summaryPath(projectRoot, relPath)
	data, err := os.ReadFile(path)
	if err != nil {
		return fs, err
	}
	err = json.Unmarshal(data, &fs)
	return fs, err
}

// ListSummaries returns all FileSummary records stored in projectRoot.
func ListSummaries(projectRoot string) ([]FileSummary, error) {
	dir := filepath.Join(projectRoot, ".lup", "summaries")
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}

	var out []FileSummary
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		data, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			continue
		}
		var fs FileSummary
		if err := json.Unmarshal(data, &fs); err != nil {
			continue
		}
		out = append(out, fs)
	}
	return out, nil
}

// DeleteSummary removes the stored summary for relPath.
func DeleteSummary(projectRoot, relPath string) error {
	return os.Remove(summaryPath(projectRoot, relPath))
}

// SummaryExists reports whether a summary file exists for relPath.
func SummaryExists(projectRoot, relPath string) bool {
	_, err := os.Stat(summaryPath(projectRoot, relPath))
	return err == nil
}
