package store

import (
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"sort"
	"sync"
)

const vectorIndexVersion = 1

// VectorStore stores symbol-level embeddings and cross-symbol references in a
// small project-local JSON index.
type VectorStore struct {
	mu sync.Mutex

	path string
	dim  int
	data vectorIndex
}

type vectorIndex struct {
	Version    int                `json:"version"`
	Dim        int                `json:"dim"`
	NextID     int64              `json:"next_id"`
	Symbols    []indexedSymbol    `json:"symbols"`
	References []indexedReference `json:"references"`
}

type indexedSymbol struct {
	ID         int64     `json:"id"`
	FilePath   string    `json:"file_path"`
	SymbolName string    `json:"symbol_name"`
	SymbolKind string    `json:"symbol_kind"`
	StartLine  int       `json:"start_line"`
	EndLine    int       `json:"end_line"`
	Embedding  []float32 `json:"embedding"`
}

type indexedReference struct {
	FromFile   string `json:"from_file"`
	FromSymbol string `json:"from_symbol"`
	ToName     string `json:"to_name"`
}

// OpenVectorStore opens (or creates) the vector index at
// <projectRoot>/.lup/index.json.
func OpenVectorStore(projectRoot string) (*VectorStore, error) {
	dir := filepath.Join(projectRoot, ".lup")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("vector store: mkdir %s: %w", dir, err)
	}

	vs := &VectorStore{
		path: filepath.Join(dir, "index.json"),
		data: vectorIndex{Version: vectorIndexVersion, NextID: 1},
	}
	if err := vs.load(); err != nil {
		return nil, err
	}
	vs.dim = vs.data.Dim
	if vs.data.Version == 0 {
		vs.data.Version = vectorIndexVersion
	}
	if vs.data.NextID < 1 {
		vs.data.NextID = nextSymbolID(vs.data.Symbols)
	}
	return vs, nil
}

// Close releases store resources. The JSON-backed store has no open handles.
func (vs *VectorStore) Close() error {
	return nil
}

func (vs *VectorStore) load() error {
	data, err := os.ReadFile(vs.path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("vector store: read %s: %w", vs.path, err)
	}
	if len(data) == 0 {
		return nil
	}
	if err := json.Unmarshal(data, &vs.data); err != nil {
		return fmt.Errorf("vector store: decode %s: %w", vs.path, err)
	}
	return nil
}

func (vs *VectorStore) saveLocked() error {
	if err := os.MkdirAll(filepath.Dir(vs.path), 0o755); err != nil {
		return fmt.Errorf("vector store: mkdir: %w", err)
	}

	vs.data.Version = vectorIndexVersion
	vs.data.Dim = vs.dim
	if vs.data.NextID < 1 {
		vs.data.NextID = nextSymbolID(vs.data.Symbols)
	}

	data, err := json.MarshalIndent(vs.data, "", "  ")
	if err != nil {
		return fmt.Errorf("vector store: encode: %w", err)
	}
	tmp := vs.path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return fmt.Errorf("vector store: write %s: %w", tmp, err)
	}
	if err := os.Rename(tmp, vs.path); err != nil {
		return fmt.Errorf("vector store: replace %s: %w", vs.path, err)
	}
	return nil
}

func nextSymbolID(symbols []indexedSymbol) int64 {
	var maxID int64
	for _, sym := range symbols {
		if sym.ID > maxID {
			maxID = sym.ID
		}
	}
	return maxID + 1
}

// UpsertSymbol stores or replaces an embedding for a single symbol.
// symbolName "__file__" is the conventional key for a file-level embedding.
func (vs *VectorStore) UpsertSymbol(
	filePath, symbolName, symbolKind string,
	startLine, endLine int,
	embedding []float32,
) error {
	vs.mu.Lock()
	defer vs.mu.Unlock()

	if err := vs.ensureDimLocked(len(embedding)); err != nil {
		return err
	}

	for i := range vs.data.Symbols {
		sym := &vs.data.Symbols[i]
		if sym.FilePath == filePath && sym.SymbolName == symbolName && sym.StartLine == startLine {
			sym.SymbolKind = symbolKind
			sym.EndLine = endLine
			sym.Embedding = cloneFloat32s(embedding)
			return vs.saveLocked()
		}
	}

	vs.data.Symbols = append(vs.data.Symbols, indexedSymbol{
		ID:         vs.data.NextID,
		FilePath:   filePath,
		SymbolName: symbolName,
		SymbolKind: symbolKind,
		StartLine:  startLine,
		EndLine:    endLine,
		Embedding:  cloneFloat32s(embedding),
	})
	vs.data.NextID++
	return vs.saveLocked()
}

// ReplaceReferences replaces all reference rows for a given file.
// Called during IndexSummary with the full set of references extracted by the LLM.
func (vs *VectorStore) ReplaceReferences(filePath string, refs []Reference) error {
	vs.mu.Lock()
	defer vs.mu.Unlock()

	kept := vs.data.References[:0]
	for _, ref := range vs.data.References {
		if ref.FromFile != filePath {
			kept = append(kept, ref)
		}
	}
	vs.data.References = kept

	for _, r := range refs {
		vs.data.References = append(vs.data.References, indexedReference{
			FromFile:   filePath,
			FromSymbol: r.FromSymbol,
			ToName:     r.ToName,
		})
	}

	return vs.saveLocked()
}

// Reference is a directed edge: symbol FromSymbol in file FromFile uses ToName.
type Reference struct {
	FromSymbol string
	ToName     string
}

// SearchResult is a single vector search result.
type SearchResult struct {
	FilePath   string
	SymbolName string
	SymbolKind string
	StartLine  int
	EndLine    int
	Distance   float64
}

// Search returns the topK nearest symbol embeddings to query.
func (vs *VectorStore) Search(query []float32, topK int) ([]SearchResult, error) {
	vs.mu.Lock()
	defer vs.mu.Unlock()

	if vs.dim == 0 || topK <= 0 || len(vs.data.Symbols) == 0 {
		return nil, nil
	}
	if len(query) != vs.dim {
		return nil, fmt.Errorf("vector store: embedding dimension mismatch: want %d, got %d", vs.dim, len(query))
	}

	results := make([]SearchResult, 0, len(vs.data.Symbols))
	for _, sym := range vs.data.Symbols {
		if len(sym.Embedding) != vs.dim {
			continue
		}
		results = append(results, SearchResult{
			FilePath:   sym.FilePath,
			SymbolName: sym.SymbolName,
			SymbolKind: sym.SymbolKind,
			StartLine:  sym.StartLine,
			EndLine:    sym.EndLine,
			Distance:   squaredL2(query, sym.Embedding),
		})
	}

	sort.Slice(results, func(i, j int) bool {
		if results[i].Distance == results[j].Distance {
			return resultLess(results[i], results[j])
		}
		return results[i].Distance < results[j].Distance
	})
	if topK > len(results) {
		topK = len(results)
	}
	return cloneResults(results[:topK]), nil
}

// FindByName returns all indexed symbols with the given name (Type A usages).
// These are files where symbolName is a declared/assigned symbol in its own right.
func (vs *VectorStore) FindByName(name string) ([]SearchResult, error) {
	vs.mu.Lock()
	defer vs.mu.Unlock()

	var results []SearchResult
	for _, sym := range vs.data.Symbols {
		if sym.SymbolName == name && sym.SymbolKind != "file" {
			results = append(results, symbolResult(sym, 0))
		}
	}
	sortResults(results)
	return results, nil
}

// FindReferencers returns all symbols that reference the given name (Type B usages).
// These are symbols where the LLM noted they use/call/depend on name.
func (vs *VectorStore) FindReferencers(name string) ([]SearchResult, error) {
	vs.mu.Lock()
	defer vs.mu.Unlock()

	seen := make(map[string]bool)
	var results []SearchResult
	for _, ref := range vs.data.References {
		if ref.ToName != name {
			continue
		}
		for _, sym := range vs.data.Symbols {
			if sym.FilePath != ref.FromFile || sym.SymbolName != ref.FromSymbol {
				continue
			}
			key := fmt.Sprintf("%s\x00%s\x00%d", sym.FilePath, sym.SymbolName, sym.StartLine)
			if seen[key] {
				continue
			}
			seen[key] = true
			results = append(results, symbolResult(sym, 0))
		}
	}
	sortResults(results)
	return results, nil
}

// DeleteFile removes all vectors, symbol rows, and reference rows for filePath.
func (vs *VectorStore) DeleteFile(filePath string) error {
	vs.mu.Lock()
	defer vs.mu.Unlock()

	symbols := vs.data.Symbols[:0]
	for _, sym := range vs.data.Symbols {
		if sym.FilePath != filePath {
			symbols = append(symbols, sym)
		}
	}
	vs.data.Symbols = symbols

	refs := vs.data.References[:0]
	for _, ref := range vs.data.References {
		if ref.FromFile != filePath {
			refs = append(refs, ref)
		}
	}
	vs.data.References = refs

	return vs.saveLocked()
}

func (vs *VectorStore) ensureDimLocked(dim int) error {
	if dim == 0 {
		return fmt.Errorf("vector store: empty embedding")
	}
	if vs.dim == 0 {
		vs.dim = dim
		return nil
	}
	if vs.dim != dim {
		return fmt.Errorf("vector store: embedding dimension mismatch: want %d, got %d", vs.dim, dim)
	}
	return nil
}

func squaredL2(a, b []float32) float64 {
	var sum float64
	for i := range a {
		d := float64(a[i] - b[i])
		sum += d * d
	}
	if math.IsNaN(sum) || math.IsInf(sum, 0) {
		return math.MaxFloat64
	}
	return sum
}

func cloneFloat32s(in []float32) []float32 {
	out := make([]float32, len(in))
	copy(out, in)
	return out
}

func cloneResults(in []SearchResult) []SearchResult {
	out := make([]SearchResult, len(in))
	copy(out, in)
	return out
}

func symbolResult(sym indexedSymbol, distance float64) SearchResult {
	return SearchResult{
		FilePath:   sym.FilePath,
		SymbolName: sym.SymbolName,
		SymbolKind: sym.SymbolKind,
		StartLine:  sym.StartLine,
		EndLine:    sym.EndLine,
		Distance:   distance,
	}
}

func sortResults(results []SearchResult) {
	sort.Slice(results, func(i, j int) bool {
		return resultLess(results[i], results[j])
	})
}

func resultLess(a, b SearchResult) bool {
	if a.FilePath != b.FilePath {
		return a.FilePath < b.FilePath
	}
	if a.StartLine != b.StartLine {
		return a.StartLine < b.StartLine
	}
	return a.SymbolName < b.SymbolName
}
