// Package rag ties together the LLM embeddings and the vector store to
// provide semantic lookup over indexed file summaries.
package rag

import (
	"context"
	"fmt"
	"strings"

	"github.com/wingitman/lup/internal/llm"
	"github.com/wingitman/lup/internal/store"
)

// LookupResult is returned by Lookup for each relevant match.
type LookupResult struct {
	File     string  `json:"file"`
	ChunkKey string  `json:"chunk_key"`
	Summary  string  `json:"summary"`
	Distance float64 `json:"distance"`
}

// Engine wraps the LLM client and vector store for RAG queries.
type Engine struct {
	llm *llm.Client
	vs  *store.VectorStore
	// projectRoot is used to load summaries for context hydration.
	projectRoot string
}

// New creates a RAG Engine.
func New(client *llm.Client, vs *store.VectorStore, projectRoot string) *Engine {
	return &Engine{llm: client, vs: vs, projectRoot: projectRoot}
}

// Lookup embeds the query text, searches the vector index, and returns the
// topK most semantically relevant summaries with their context hydrated from
// stored JSON summaries.
func (e *Engine) Lookup(ctx context.Context, queryText string, topK int) ([]LookupResult, error) {
	embedding, err := e.llm.Embed(ctx, queryText)
	if err != nil {
		return nil, fmt.Errorf("rag: embed query: %w", err)
	}

	hits, err := e.vs.Search(embedding, topK)
	if err != nil {
		return nil, fmt.Errorf("rag: vector search: %w", err)
	}

	results := make([]LookupResult, 0, len(hits))
	for _, hit := range hits {
		summary := e.hydrateSummary(hit.FilePath, hit.ChunkKey)
		results = append(results, LookupResult{
			File:     hit.FilePath,
			ChunkKey: hit.ChunkKey,
			Summary:  summary,
			Distance: hit.Distance,
		})
	}
	return results, nil
}

// IndexSummary embeds a FileSummary and upserts it into the vector store.
// It creates one embedding per file (combining the file summary with all
// function summaries) so that any symbol name in the file is represented.
func (e *Engine) IndexSummary(ctx context.Context, fs store.FileSummary) error {
	text := buildIndexText(fs)
	embedding, err := e.llm.Embed(ctx, text)
	if err != nil {
		return fmt.Errorf("rag: embed %s: %w", fs.File, err)
	}
	return e.vs.Upsert(fs.File, "file", embedding)
}

// ──────────────────────────────────────────────────────────
// Helpers
// ──────────────────────────────────────────────────────────

// buildIndexText creates a single text string that captures the full semantic
// content of the file summary — file-level description plus all symbols.
func buildIndexText(fs store.FileSummary) string {
	var b strings.Builder
	b.WriteString(fs.File)
	b.WriteString("\n")
	b.WriteString(fs.FileSummary)
	for _, fn := range fs.Functions {
		b.WriteString("\n")
		b.WriteString(fn.Kind)
		b.WriteString(" ")
		b.WriteString(fn.Name)
		b.WriteString(": ")
		b.WriteString(fn.Summary)
	}
	return b.String()
}

// hydrateSummary looks up the stored JSON summary and formats a human-readable
// context string for the given file/chunk.
func (e *Engine) hydrateSummary(filePath, chunkKey string) string {
	fs, err := store.ReadSummary(e.projectRoot, filePath)
	if err != nil {
		// Fall back to just the file path if the summary is missing.
		return fmt.Sprintf("[%s] (summary not found)", filePath)
	}

	var b strings.Builder
	b.WriteString(fmt.Sprintf("**%s**\n", fs.File))
	b.WriteString(fs.FileSummary)

	if chunkKey == "file" || len(fs.Functions) == 0 {
		return b.String()
	}

	// Append the matching function summaries.
	b.WriteString("\n\nRelated symbols:")
	for _, fn := range fs.Functions {
		b.WriteString(fmt.Sprintf("\n- %s `%s`: %s", fn.Kind, fn.Name, fn.Summary))
	}
	return b.String()
}
