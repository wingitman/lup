// Package rag ties together LLM embeddings, the vector store, and the summary
// store to provide symbol-level semantic lookup with cross-file usage tracking.
package rag

import (
	"context"
	"fmt"
	"strings"

	"github.com/wingitman/lup/internal/llm"
	"github.com/wingitman/lup/internal/store"
)

// ──────────────────────────────────────────────────────────
// Result types
// ──────────────────────────────────────────────────────────

// LookupResult is a single rich result returned by Lookup.
type LookupResult struct {
	File       string `json:"file"`
	SymbolName string `json:"symbol_name"`
	SymbolKind string `json:"symbol_kind"`
	StartLine  int    `json:"start_line"`

	// Summary is the LLM-generated description of this symbol.
	Summary string `json:"summary"`

	// UsageCount is the total number of places this name is used across the
	// indexed project (declared + referenced).
	UsageCount int `json:"usage_count"`

	// Usages lists individual usage locations (capped at 10 unless --show-all).
	Usages []Usage `json:"usages"`

	// Similar lists other symbols with semantically close embeddings.
	Similar []SimilarSymbol `json:"similar,omitempty"`

	Distance float64 `json:"distance"`
}

// Usage is a single usage location for a symbol.
type Usage struct {
	File       string `json:"file"`
	SymbolName string `json:"symbol_name"`
	SymbolKind string `json:"symbol_kind"`
	StartLine  int    `json:"start_line"`
	// Context is the one-line summary of the symbol that uses this one.
	Context string `json:"context"`
}

// SimilarSymbol is a semantically related symbol surfaced alongside a result.
type SimilarSymbol struct {
	File       string  `json:"file"`
	SymbolName string  `json:"symbol_name"`
	SymbolKind string  `json:"symbol_kind"`
	Summary    string  `json:"summary"`
	Distance   float64 `json:"distance"`
}

// ──────────────────────────────────────────────────────────
// Engine
// ──────────────────────────────────────────────────────────

// Engine wraps the LLM client and vector store for RAG operations.
type Engine struct {
	llm         *llm.Client
	vs          *store.VectorStore
	projectRoot string
}

// New creates a RAG Engine.
func New(client *llm.Client, vs *store.VectorStore, projectRoot string) *Engine {
	return &Engine{llm: client, vs: vs, projectRoot: projectRoot}
}

// ──────────────────────────────────────────────────────────
// Indexing
// ──────────────────────────────────────────────────────────

// IndexSummary embeds every symbol in fs individually and writes the reference
// map to the SQLite index.  One embedding per symbol + one file-level embedding.
func (e *Engine) IndexSummary(ctx context.Context, fs store.FileSummary) error {
	// 1. File-level embedding — captures the broad topic of the file.
	fileText := "file " + fs.File + ": " + fs.FileSummary
	fileEmbed, err := e.llm.Embed(ctx, fileText)
	if err != nil {
		return fmt.Errorf("rag: embed file %s: %w", fs.File, err)
	}
	if err := e.vs.UpsertSymbol(fs.File, "__file__", "file", 0, 0, fileEmbed); err != nil {
		return fmt.Errorf("rag: upsert file embedding %s: %w", fs.File, err)
	}

	// 2. Per-symbol embeddings.
	var refs []store.Reference
	for _, sym := range fs.Symbols {
		text := buildSymbolText(sym)
		symEmbed, err := e.llm.Embed(ctx, text)
		if err != nil {
			// Non-fatal: log and continue — one bad symbol shouldn't abort
			// the whole file index.
			continue
		}
		if err := e.vs.UpsertSymbol(
			fs.File,
			sym.Name,
			sym.Kind,
			int(sym.StartLine),
			int(sym.EndLine),
			symEmbed,
		); err != nil {
			continue
		}

		// Collect reference edges for this symbol.
		for _, ref := range sym.References {
			if ref != "" && ref != sym.Name {
				refs = append(refs, store.Reference{
					FromSymbol: sym.Name,
					ToName:     ref,
				})
			}
		}
	}

	// 3. Write reference map (replaces any previous entries for this file).
	if err := e.vs.ReplaceReferences(fs.File, refs); err != nil {
		return fmt.Errorf("rag: write references %s: %w", fs.File, err)
	}

	return nil
}

// ──────────────────────────────────────────────────────────
// Lookup
// ──────────────────────────────────────────────────────────

// Lookup embeds the query, searches the symbol index, and returns rich results
// with cross-file usage counts and similar symbols.
//
// showAll: when false, usages are capped at 10 per result.
func (e *Engine) Lookup(ctx context.Context, queryText string, topK int, showAll bool) ([]LookupResult, error) {
	embedding, err := e.llm.Embed(ctx, queryText)
	if err != nil {
		return nil, fmt.Errorf("rag: embed query: %w", err)
	}

	hits, err := e.vs.Search(embedding, topK)
	if err != nil {
		return nil, fmt.Errorf("rag: vector search: %w", err)
	}

	results := make([]LookupResult, 0, len(hits))
	for i, hit := range hits {
		// Skip synthetic file-level embeddings in results — they're only
		// useful as a fallback if no symbol matches are found.
		if hit.SymbolName == "__file__" && len(hits) > 1 {
			continue
		}

		// Build similar symbols from the other hits (not the same symbol).
		var similar []SimilarSymbol
		for j, other := range hits {
			if j == i || other.SymbolName == "__file__" {
				continue
			}
			if other.SymbolName == hit.SymbolName && other.FilePath == hit.FilePath {
				continue
			}
			sim := e.buildSimilar(other)
			similar = append(similar, sim)
			if len(similar) >= 3 {
				break
			}
		}

		result, err := e.hydrateLookup(hit, similar, showAll)
		if err != nil {
			// Fall back to a minimal result rather than failing the whole lookup.
			result = LookupResult{
				File:       hit.FilePath,
				SymbolName: hit.SymbolName,
				SymbolKind: hit.SymbolKind,
				StartLine:  hit.StartLine,
				Summary:    fmt.Sprintf("[summary unavailable: %v]", err),
				Distance:   hit.Distance,
			}
		}
		results = append(results, result)
	}

	// If all hits were __file__ embeddings (no symbols indexed yet), surface
	// them so the user gets something.
	if len(results) == 0 && len(hits) > 0 {
		for _, hit := range hits {
			result, _ := e.hydrateLookup(hit, nil, showAll)
			results = append(results, result)
		}
	}

	return results, nil
}

// ──────────────────────────────────────────────────────────
// Internal helpers
// ──────────────────────────────────────────────────────────

// buildSymbolText constructs the text string that will be embedded for a symbol.
func buildSymbolText(sym store.SymbolSummary) string {
	var b strings.Builder
	b.WriteString(sym.Kind)
	b.WriteString(" ")
	b.WriteString(sym.Name)
	b.WriteString(": ")
	b.WriteString(sym.Summary)
	if sym.OccurrenceCount > 1 {
		b.WriteString(fmt.Sprintf(" (used %d times in this file)", sym.OccurrenceCount))
	}
	if len(sym.References) > 0 {
		b.WriteString("\nreferences: ")
		b.WriteString(strings.Join(sym.References, ", "))
	}
	return b.String()
}

// hydrateLookup assembles a rich LookupResult for a vector search hit.
func (e *Engine) hydrateLookup(hit store.SearchResult, similar []SimilarSymbol, showAll bool) (LookupResult, error) {
	result := LookupResult{
		File:       hit.FilePath,
		SymbolName: hit.SymbolName,
		SymbolKind: hit.SymbolKind,
		StartLine:  hit.StartLine,
		Distance:   hit.Distance,
		Similar:    similar,
	}

	// Load the stored summary for the symbol's file.
	fs, err := store.ReadSummary(e.projectRoot, hit.FilePath)
	if err == nil {
		// Find the matching SymbolSummary.
		for _, sym := range fs.Symbols {
			if sym.Name == hit.SymbolName &&
				(hit.SymbolName == "__file__" || int(sym.StartLine) == hit.StartLine || sym.Name == hit.SymbolName) {
				result.Summary = sym.Summary
				break
			}
		}
		// For file-level hits use the file summary.
		if result.Summary == "" && hit.SymbolName == "__file__" {
			result.Summary = fs.FileSummary
		}
		// If still empty, use the file summary as fallback.
		if result.Summary == "" {
			result.Summary = fs.FileSummary
		}
	}

	// Build usage list: Type A (declared) + Type B (referenced).
	typeA, err := e.vs.FindByName(hit.SymbolName)
	if err != nil {
		typeA = nil
	}
	typeB, err := e.vs.FindReferencers(hit.SymbolName)
	if err != nil {
		typeB = nil
	}

	seen := map[string]bool{}
	var allUsages []Usage

	addUsage := func(sr store.SearchResult) {
		key := fmt.Sprintf("%s:%s:%d", sr.FilePath, sr.SymbolName, sr.StartLine)
		if seen[key] {
			return
		}
		// Don't list the symbol itself as a usage of itself.
		if sr.FilePath == hit.FilePath && sr.SymbolName == hit.SymbolName && sr.StartLine == hit.StartLine {
			return
		}
		seen[key] = true

		usage := Usage{
			File:       sr.FilePath,
			SymbolName: sr.SymbolName,
			SymbolKind: sr.SymbolKind,
			StartLine:  sr.StartLine,
		}
		// Try to get the one-line context for this using symbol.
		if ufs, err := store.ReadSummary(e.projectRoot, sr.FilePath); err == nil {
			for _, sym := range ufs.Symbols {
				if sym.Name == sr.SymbolName {
					usage.Context = sym.Summary
					break
				}
			}
		}
		allUsages = append(allUsages, usage)
	}

	for _, sr := range typeA {
		addUsage(sr)
	}
	for _, sr := range typeB {
		addUsage(sr)
	}

	result.UsageCount = len(allUsages)

	if !showAll && len(allUsages) > 10 {
		result.Usages = allUsages[:10]
	} else {
		result.Usages = allUsages
	}

	return result, nil
}

// buildSimilar creates a SimilarSymbol from a search hit.
func (e *Engine) buildSimilar(hit store.SearchResult) SimilarSymbol {
	sim := SimilarSymbol{
		File:       hit.FilePath,
		SymbolName: hit.SymbolName,
		SymbolKind: hit.SymbolKind,
		Distance:   hit.Distance,
	}
	if fs, err := store.ReadSummary(e.projectRoot, hit.FilePath); err == nil {
		for _, sym := range fs.Symbols {
			if sym.Name == hit.SymbolName {
				sim.Summary = sym.Summary
				break
			}
		}
	}
	return sim
}
