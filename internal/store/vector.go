package store

import (
	"database/sql"
	"encoding/binary"
	"fmt"
	"math"
	"os"
	"path/filepath"

	sqlite_vec "github.com/asg017/sqlite-vec-go-bindings/cgo"
	_ "github.com/mattn/go-sqlite3"
)

func init() {
	sqlite_vec.Auto()
}

// VectorStore wraps the sqlite-vec database for symbol-level embeddings and
// the cross-symbol reference index.
type VectorStore struct {
	db  *sql.DB
	dim int
}

// OpenVectorStore opens (or creates) the sqlite-vec database at
// <projectRoot>/.lup/index.db.
func OpenVectorStore(projectRoot string) (*VectorStore, error) {
	dir := filepath.Join(projectRoot, ".lup")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("vector store: mkdir %s: %w", dir, err)
	}

	path := filepath.Join(dir, "index.db")
	// Single connection: sqlite-vec knn queries require the k= constraint and
	// the embedding MATCH to run on the same connection-level query planner pass.
	db, err := sql.Open("sqlite3", path)
	if err != nil {
		return nil, fmt.Errorf("vector store: open %s: %w", path, err)
	}
	db.SetMaxOpenConns(1)

	vs := &VectorStore{db: db}
	if err := vs.migrate(); err != nil {
		db.Close()
		return nil, err
	}
	return vs, nil
}

// Close releases the database connection.
func (vs *VectorStore) Close() error {
	return vs.db.Close()
}

// ──────────────────────────────────────────────────────────
// Schema migration
// ──────────────────────────────────────────────────────────

func (vs *VectorStore) migrate() error {
	// Metadata: stores embedding dimension.
	if _, err := vs.db.Exec(`CREATE TABLE IF NOT EXISTS lup_meta (
		key   TEXT PRIMARY KEY,
		value TEXT NOT NULL
	)`); err != nil {
		return fmt.Errorf("vector store: migrate meta: %w", err)
	}

	// Symbol registry: one row per indexed symbol (including synthetic
	// "__file__" rows for file-level embeddings).
	if _, err := vs.db.Exec(`CREATE TABLE IF NOT EXISTS lup_symbols (
		id          INTEGER PRIMARY KEY AUTOINCREMENT,
		file_path   TEXT    NOT NULL,
		symbol_name TEXT    NOT NULL,
		symbol_kind TEXT    NOT NULL,
		start_line  INTEGER NOT NULL DEFAULT 0,
		end_line    INTEGER NOT NULL DEFAULT 0,
		UNIQUE(file_path, symbol_name, start_line)
	)`); err != nil {
		return fmt.Errorf("vector store: migrate symbols: %w", err)
	}

	// Reference map: "symbol (from_file, from_symbol) uses symbol named to_name".
	// Indexed on to_name for O(1) reverse lookup.
	if _, err := vs.db.Exec(`CREATE TABLE IF NOT EXISTS lup_references (
		id          INTEGER PRIMARY KEY AUTOINCREMENT,
		from_file   TEXT NOT NULL,
		from_symbol TEXT NOT NULL,
		to_name     TEXT NOT NULL
	)`); err != nil {
		return fmt.Errorf("vector store: migrate references: %w", err)
	}
	if _, err := vs.db.Exec(`CREATE INDEX IF NOT EXISTS idx_lup_references_to
		ON lup_references(to_name)`); err != nil {
		return fmt.Errorf("vector store: migrate references index: %w", err)
	}

	// Load stored embedding dimension.
	var dimStr string
	if err := vs.db.QueryRow(`SELECT value FROM lup_meta WHERE key='dim'`).Scan(&dimStr); err == nil {
		var d int
		fmt.Sscan(dimStr, &d)
		if d > 0 {
			vs.dim = d
			return vs.ensureVecTable()
		}
	}
	return nil
}

func (vs *VectorStore) ensureVecTable() error {
	if vs.dim == 0 {
		return nil
	}
	_, err := vs.db.Exec(fmt.Sprintf(
		`CREATE VIRTUAL TABLE IF NOT EXISTS lup_vec USING vec0(embedding float[%d])`,
		vs.dim,
	))
	if err != nil {
		return fmt.Errorf("vector store: create vec table: %w", err)
	}
	return nil
}

// ──────────────────────────────────────────────────────────
// Upsert
// ──────────────────────────────────────────────────────────

// UpsertSymbol stores or replaces an embedding for a single symbol.
// symbolName "__file__" is the conventional key for a file-level embedding.
func (vs *VectorStore) UpsertSymbol(
	filePath, symbolName, symbolKind string,
	startLine, endLine int,
	embedding []float32,
) error {
	if err := vs.ensureDim(len(embedding)); err != nil {
		return err
	}

	tx, err := vs.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	// Upsert the symbol row.
	res, err := tx.Exec(`
		INSERT INTO lup_symbols(file_path, symbol_name, symbol_kind, start_line, end_line)
		VALUES (?, ?, ?, ?, ?)
		ON CONFLICT(file_path, symbol_name, start_line)
		DO UPDATE SET symbol_kind=excluded.symbol_kind, end_line=excluded.end_line`,
		filePath, symbolName, symbolKind, startLine, endLine,
	)
	if err != nil {
		return fmt.Errorf("vector store upsert symbol: %w", err)
	}

	rowID, err := res.LastInsertId()
	if err != nil || rowID == 0 {
		if err2 := tx.QueryRow(
			`SELECT id FROM lup_symbols WHERE file_path=? AND symbol_name=? AND start_line=?`,
			filePath, symbolName, startLine,
		).Scan(&rowID); err2 != nil {
			return fmt.Errorf("vector store upsert rowid: %w", err2)
		}
	}

	blob := serializeFloat32(embedding)
	tx.Exec(`DELETE FROM lup_vec WHERE rowid=?`, rowID)
	if _, err := tx.Exec(`INSERT INTO lup_vec(rowid, embedding) VALUES (?, ?)`, rowID, blob); err != nil {
		return fmt.Errorf("vector store upsert vec: %w", err)
	}

	return tx.Commit()
}

// ReplaceReferences replaces all reference rows for a given file.
// Called during IndexSummary with the full set of references extracted by the LLM.
func (vs *VectorStore) ReplaceReferences(filePath string, refs []Reference) error {
	tx, err := vs.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	if _, err := tx.Exec(`DELETE FROM lup_references WHERE from_file=?`, filePath); err != nil {
		return fmt.Errorf("vector store replace refs delete: %w", err)
	}

	for _, r := range refs {
		if _, err := tx.Exec(
			`INSERT INTO lup_references(from_file, from_symbol, to_name) VALUES (?, ?, ?)`,
			filePath, r.FromSymbol, r.ToName,
		); err != nil {
			return fmt.Errorf("vector store insert ref: %w", err)
		}
	}

	return tx.Commit()
}

// Reference is a directed edge: symbol FromSymbol in file FromFile uses ToName.
type Reference struct {
	FromSymbol string
	ToName     string
}

// ──────────────────────────────────────────────────────────
// Search
// ──────────────────────────────────────────────────────────

// SearchResult is a single ANN search result.
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
	if vs.dim == 0 {
		return nil, nil
	}

	tx, err := vs.db.Begin()
	if err != nil {
		return nil, fmt.Errorf("vector store search begin: %w", err)
	}
	defer tx.Rollback()

	var rowCount int
	tx.QueryRow(`SELECT COUNT(*) FROM lup_symbols`).Scan(&rowCount)
	if rowCount == 0 {
		return nil, nil
	}
	if topK > rowCount {
		topK = rowCount
	}

	blob := serializeFloat32(query)

	rows, err := tx.Query(`
		SELECT s.file_path, s.symbol_name, s.symbol_kind, s.start_line, s.end_line, v.distance
		FROM (
			SELECT rowid, distance
			FROM lup_vec
			WHERE embedding MATCH ?
			  AND k = ?
			ORDER BY distance
		) v
		JOIN lup_symbols s ON s.id = v.rowid
	`, blob, topK)
	if err != nil {
		return nil, fmt.Errorf("vector store search: %w", err)
	}

	var results []SearchResult
	for rows.Next() {
		var r SearchResult
		if err := rows.Scan(&r.FilePath, &r.SymbolName, &r.SymbolKind, &r.StartLine, &r.EndLine, &r.Distance); err != nil {
			rows.Close()
			return nil, err
		}
		results = append(results, r)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, err
	}
	rows.Close()
	tx.Commit()
	return results, nil
}

// FindByName returns all indexed symbols with the given name (Type A usages).
// These are files where symbolName is a declared/assigned symbol in its own right.
func (vs *VectorStore) FindByName(name string) ([]SearchResult, error) {
	rows, err := vs.db.Query(`
		SELECT file_path, symbol_name, symbol_kind, start_line, end_line
		FROM lup_symbols
		WHERE symbol_name = ? AND symbol_kind != 'file'
		ORDER BY file_path, start_line
	`, name)
	if err != nil {
		return nil, fmt.Errorf("vector store find by name: %w", err)
	}
	defer rows.Close()

	var results []SearchResult
	for rows.Next() {
		var r SearchResult
		if err := rows.Scan(&r.FilePath, &r.SymbolName, &r.SymbolKind, &r.StartLine, &r.EndLine); err != nil {
			return nil, err
		}
		results = append(results, r)
	}
	return results, rows.Err()
}

// FindReferencers returns all symbols that reference the given name (Type B usages).
// These are symbols where the LLM noted they use/call/depend on name.
// O(1) via the index on lup_references.to_name.
func (vs *VectorStore) FindReferencers(name string) ([]SearchResult, error) {
	rows, err := vs.db.Query(`
		SELECT DISTINCT s.file_path, s.symbol_name, s.symbol_kind, s.start_line, s.end_line
		FROM lup_references r
		JOIN lup_symbols s
		  ON s.file_path = r.from_file AND s.symbol_name = r.from_symbol
		WHERE r.to_name = ?
		ORDER BY s.file_path, s.start_line
	`, name)
	if err != nil {
		return nil, fmt.Errorf("vector store find referencers: %w", err)
	}
	defer rows.Close()

	var results []SearchResult
	for rows.Next() {
		var r SearchResult
		if err := rows.Scan(&r.FilePath, &r.SymbolName, &r.SymbolKind, &r.StartLine, &r.EndLine); err != nil {
			return nil, err
		}
		results = append(results, r)
	}
	return results, rows.Err()
}

// DeleteFile removes all vectors, symbol rows, and reference rows for filePath.
func (vs *VectorStore) DeleteFile(filePath string) error {
	tx, err := vs.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	// Collect rowids to delete from lup_vec.
	rows, err := tx.Query(`SELECT id FROM lup_symbols WHERE file_path=?`, filePath)
	if err != nil {
		return err
	}
	var ids []int64
	for rows.Next() {
		var id int64
		rows.Scan(&id)
		ids = append(ids, id)
	}
	rows.Close()

	for _, id := range ids {
		tx.Exec(`DELETE FROM lup_vec WHERE rowid=?`, id)
	}
	tx.Exec(`DELETE FROM lup_symbols WHERE file_path=?`, filePath)
	tx.Exec(`DELETE FROM lup_references WHERE from_file=?`, filePath)

	return tx.Commit()
}

// ──────────────────────────────────────────────────────────
// Helpers
// ──────────────────────────────────────────────────────────

func (vs *VectorStore) ensureDim(dim int) error {
	if vs.dim == 0 {
		vs.dim = dim
		vs.db.Exec(`INSERT OR REPLACE INTO lup_meta(key, value) VALUES ('dim', ?)`,
			fmt.Sprintf("%d", dim))
		return vs.ensureVecTable()
	}
	if vs.dim != dim {
		return fmt.Errorf("vector store: embedding dimension mismatch: want %d, got %d", vs.dim, dim)
	}
	return nil
}

func serializeFloat32(v []float32) []byte {
	b := make([]byte, len(v)*4)
	for i, f := range v {
		binary.LittleEndian.PutUint32(b[i*4:], math.Float32bits(f))
	}
	return b
}
