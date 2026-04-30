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
	// Register sqlite-vec extension for all future connections opened via
	// the "sqlite3" driver.
	sqlite_vec.Auto()
}

// VectorStore wraps a sqlite-vec database for storing and querying embeddings.
type VectorStore struct {
	db  *sql.DB
	dim int // embedding dimension (determined on first insert)
}

// OpenVectorStore opens (or creates) the sqlite-vec database at
// <projectRoot>/.lup/index.db.
func OpenVectorStore(projectRoot string) (*VectorStore, error) {
	dir := filepath.Join(projectRoot, ".lup")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("vector store: mkdir %s: %w", dir, err)
	}

	path := filepath.Join(dir, "index.db")
	// Use a single connection (max_open_conns=1) so the COUNT(*) and the knn
	// MATCH query always run on the same SQLite connection.  sqlite-vec's vec0
	// virtual table requires the k= constraint to be visible to the same
	// query planner pass — connection pool switching breaks this.
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
	// Metadata table — stores file path → dimension so we can create the vec
	// table with the right dimension on first use.
	_, err := vs.db.Exec(`CREATE TABLE IF NOT EXISTS lup_meta (
		key   TEXT PRIMARY KEY,
		value TEXT NOT NULL
	)`)
	if err != nil {
		return fmt.Errorf("vector store: migrate meta: %w", err)
	}

	// Index table maps rowid → file path so we can join results.
	_, err = vs.db.Exec(`CREATE TABLE IF NOT EXISTS lup_files (
		id        INTEGER PRIMARY KEY AUTOINCREMENT,
		file_path TEXT NOT NULL UNIQUE,
		chunk_key TEXT NOT NULL
	)`)
	if err != nil {
		return fmt.Errorf("vector store: migrate files: %w", err)
	}

	// Load dimension from meta if available.
	var dimStr string
	row := vs.db.QueryRow(`SELECT value FROM lup_meta WHERE key='dim'`)
	if err := row.Scan(&dimStr); err == nil {
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
	_, err := vs.db.Exec(fmt.Sprintf(`CREATE VIRTUAL TABLE IF NOT EXISTS lup_vec
		USING vec0(embedding float[%d])`, vs.dim))
	if err != nil {
		return fmt.Errorf("vector store: create vec table: %w", err)
	}
	return nil
}

// ──────────────────────────────────────────────────────────
// Upsert
// ──────────────────────────────────────────────────────────

// Upsert stores or replaces an embedding for the given file path.
// chunkKey is a human-readable label (e.g. "file", "function:CalculateGross").
func (vs *VectorStore) Upsert(filePath, chunkKey string, embedding []float32) error {
	if err := vs.ensureDim(len(embedding)); err != nil {
		return err
	}

	tx, err := vs.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	// Insert or replace the file record to get a stable rowid.
	res, err := tx.Exec(`INSERT INTO lup_files(file_path, chunk_key)
		VALUES (?, ?)
		ON CONFLICT(file_path) DO UPDATE SET chunk_key=excluded.chunk_key`,
		filePath, chunkKey)
	if err != nil {
		return fmt.Errorf("vector store upsert files: %w", err)
	}

	rowID, err := res.LastInsertId()
	if err != nil {
		// On UPDATE the LastInsertId may be 0; fetch it explicitly.
		row := tx.QueryRow(`SELECT id FROM lup_files WHERE file_path=?`, filePath)
		if err2 := row.Scan(&rowID); err2 != nil {
			return fmt.Errorf("vector store upsert rowid: %w", err2)
		}
	}

	blob := serializeFloat32(embedding)

	// sqlite-vec: delete existing vector then re-insert (vec0 does not support
	// UPDATE directly).
	tx.Exec(`DELETE FROM lup_vec WHERE rowid=?`, rowID)
	_, err = tx.Exec(`INSERT INTO lup_vec(rowid, embedding) VALUES (?, ?)`, rowID, blob)
	if err != nil {
		return fmt.Errorf("vector store upsert vec: %w", err)
	}

	return tx.Commit()
}

// ──────────────────────────────────────────────────────────
// Search
// ──────────────────────────────────────────────────────────

// Result is a single ANN search result.
type Result struct {
	FilePath  string
	ChunkKey  string
	Distance  float64
}

// Search returns the topK nearest neighbours to query.
func (vs *VectorStore) Search(query []float32, topK int) ([]Result, error) {
	if vs.dim == 0 {
		return nil, nil // nothing indexed yet
	}

	// sqlite-vec requires k ≤ number of indexed rows.  Run the COUNT in a
	// transaction so it shares the same connection as the knn query — this
	// avoids a pool-level race where a second connection opens a conflicting
	// read on the vec0 virtual table.
	tx, err := vs.db.Begin()
	if err != nil {
		return nil, fmt.Errorf("vector store search: begin tx: %w", err)
	}
	defer tx.Rollback()

	var rowCount int
	tx.QueryRow(`SELECT COUNT(*) FROM lup_files`).Scan(&rowCount)
	if rowCount == 0 {
		return nil, nil
	}
	if topK > rowCount {
		topK = rowCount
	}

	blob := serializeFloat32(query)

	// sqlite-vec knn queries require MATCH + k= directly on the virtual table;
	// wrap in a subquery so the JOIN with lup_files doesn't confuse the planner.
	rows, err := tx.Query(`
		SELECT f.file_path, f.chunk_key, v.distance
		FROM (
			SELECT rowid, distance
			FROM lup_vec
			WHERE embedding MATCH ?
			  AND k = ?
			ORDER BY distance
		) v
		JOIN lup_files f ON f.id = v.rowid
	`, blob, topK)
	if err != nil {
		return nil, fmt.Errorf("vector store search: %w", err)
	}
	defer rows.Close()

	var results []Result
	for rows.Next() {
		var r Result
		if err := rows.Scan(&r.FilePath, &r.ChunkKey, &r.Distance); err != nil {
			return nil, err
		}
		results = append(results, r)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	rows.Close()
	tx.Commit()
	return results, nil
}

// DeleteFile removes all vectors associated with filePath.
func (vs *VectorStore) DeleteFile(filePath string) error {
	tx, err := vs.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	var rowID int64
	row := tx.QueryRow(`SELECT id FROM lup_files WHERE file_path=?`, filePath)
	if err := row.Scan(&rowID); err != nil {
		return tx.Commit() // nothing to delete
	}

	tx.Exec(`DELETE FROM lup_vec WHERE rowid=?`, rowID)
	tx.Exec(`DELETE FROM lup_files WHERE id=?`, rowID)
	return tx.Commit()
}

// ──────────────────────────────────────────────────────────
// Helpers
// ──────────────────────────────────────────────────────────

func (vs *VectorStore) ensureDim(dim int) error {
	if vs.dim == 0 {
		vs.dim = dim
		// Persist dimension.
		vs.db.Exec(`INSERT OR REPLACE INTO lup_meta(key, value) VALUES ('dim', ?)`,
			fmt.Sprintf("%d", dim))
		return vs.ensureVecTable()
	}
	if vs.dim != dim {
		return fmt.Errorf("vector store: embedding dimension mismatch: want %d, got %d", vs.dim, dim)
	}
	return nil
}

// serializeFloat32 encodes a []float32 as a little-endian byte slice.
// This is the format sqlite-vec expects for float vectors.
func serializeFloat32(v []float32) []byte {
	b := make([]byte, len(v)*4)
	for i, f := range v {
		binary.LittleEndian.PutUint32(b[i*4:], math.Float32bits(f))
	}
	return b
}
