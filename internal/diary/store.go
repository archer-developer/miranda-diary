package diary

import (
	"context"
	"database/sql"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"math"
	"sort"
	"time"

	"github.com/google/uuid"
	_ "modernc.org/sqlite" // registers the "sqlite" driver
)

const schema = `
CREATE TABLE IF NOT EXISTS records (
    id         TEXT    PRIMARY KEY,
    user_id    TEXT    NOT NULL,
    content    TEXT    NOT NULL,
    tags       TEXT    NOT NULL DEFAULT '[]',
    created_at INTEGER NOT NULL,
    embedding  BLOB    NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_records_user_id ON records(user_id);
`

// Store is a SQLite-backed diary store. Records are written with their
// float32 embedding (from Gemini) stored as a raw binary BLOB. Semantic
// search loads all embeddings into memory and ranks by cosine similarity —
// perfectly adequate for a personal diary even at tens of thousands of entries
// (10 000 records × 768 dims × 4 bytes ≈ 30 MB RAM).
type Store struct {
	db *sql.DB
}

// New opens (or creates) the SQLite database at path, runs the schema
// migration, and returns a ready Store. The caller must call Close when done.
func New(path string) (*Store, error) {
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, fmt.Errorf("diary: open %s: %w", path, err)
	}

	// Single writer: cap the pool to one connection so concurrent writes queue
	// inside database/sql rather than hitting SQLite's busy_timeout=0 default
	// and returning SQLITE_BUSY immediately.
	db.SetMaxOpenConns(1)

	// WAL mode improves read concurrency without sacrificing durability.
	if _, err := db.Exec("PRAGMA journal_mode=WAL"); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("diary: enable WAL: %w", err)
	}

	if _, err := db.Exec(schema); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("diary: apply schema: %w", err)
	}

	return &Store{db: db}, nil
}

// Add stores a new diary record owned by userID and returns it.
// embedding must be non-empty; tags may be nil (stored as an empty JSON array).
func (s *Store) Add(ctx context.Context, userID, content string, tags []string, embedding []float32) (Record, error) {
	if tags == nil {
		tags = []string{}
	}

	tagsJSON, err := json.Marshal(tags)
	if err != nil {
		return Record{}, fmt.Errorf("diary: marshal tags: %w", err)
	}

	id := uuid.New().String()
	now := time.Now().UTC()

	_, err = s.db.ExecContext(ctx,
		`INSERT INTO records (id, user_id, content, tags, created_at, embedding) VALUES (?, ?, ?, ?, ?, ?)`,
		id, userID, content, string(tagsJSON), now.Unix(), encodeEmbedding(embedding),
	)
	if err != nil {
		return Record{}, fmt.Errorf("diary: insert record: %w", err)
	}

	return Record{ID: id, Content: content, Tags: tags, CreatedAt: now}, nil
}

// Search returns up to limit records owned by userID ranked by cosine similarity
// to queryEmbedding. Only records belonging to userID are considered — other
// users' records are invisible. All matching records are loaded and scored
// in-memory; the caller supplies a pre-validated positive limit.
func (s *Store) Search(ctx context.Context, userID string, queryEmbedding []float32, limit int) ([]SearchResult, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, content, tags, created_at, embedding FROM records WHERE user_id = ?`,
		userID,
	)
	if err != nil {
		return nil, fmt.Errorf("diary: query records: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var results []SearchResult
	for rows.Next() {
		var (
			id        string
			content   string
			tagsJSON  string
			createdAt int64
			embBlob   []byte
		)
		if err := rows.Scan(&id, &content, &tagsJSON, &createdAt, &embBlob); err != nil {
			return nil, fmt.Errorf("diary: scan row: %w", err)
		}

		emb, err := decodeEmbedding(embBlob)
		if err != nil {
			// Skip corrupt rows rather than aborting the whole search.
			continue
		}

		var tags []string
		if err := json.Unmarshal([]byte(tagsJSON), &tags); err != nil {
			tags = []string{}
		}

		score := cosineSimilarity(queryEmbedding, emb)
		results = append(results, SearchResult{
			Record: Record{
				ID:        id,
				Content:   content,
				Tags:      tags,
				CreatedAt: time.Unix(createdAt, 0).UTC(),
			},
			Score: score,
		})
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("diary: iterate rows: %w", err)
	}

	sort.Slice(results, func(i, j int) bool {
		return results[i].Score > results[j].Score
	})

	if limit < len(results) {
		results = results[:limit]
	}
	return results, nil
}

// Remove deletes the record with the given id only if it belongs to userID.
// Returns true if a record was deleted, false if no matching record was found
// (either the id doesn't exist or it belongs to a different user — callers
// cannot distinguish these cases by design, to prevent ID enumeration).
func (s *Store) Remove(ctx context.Context, userID, id string) (bool, error) {
	res, err := s.db.ExecContext(ctx, `DELETE FROM records WHERE id = ? AND user_id = ?`, id, userID)
	if err != nil {
		return false, fmt.Errorf("diary: delete record: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("diary: rows affected: %w", err)
	}
	return n > 0, nil
}

// Count returns the number of records owned by userID.
func (s *Store) Count(ctx context.Context, userID string) (int64, error) {
	var n int64
	err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM records WHERE user_id = ?`, userID).Scan(&n)
	if err != nil {
		return 0, fmt.Errorf("diary: count: %w", err)
	}
	return n, nil
}

// Close closes the underlying database connection.
func (s *Store) Close() error {
	return s.db.Close()
}

// encodeEmbedding packs a float32 slice into a little-endian byte slice for
// storage as a SQLite BLOB.
func encodeEmbedding(v []float32) []byte {
	b := make([]byte, len(v)*4)
	for i, f := range v {
		binary.LittleEndian.PutUint32(b[i*4:], math.Float32bits(f))
	}
	return b
}

// decodeEmbedding unpacks a BLOB produced by encodeEmbedding back into []float32.
func decodeEmbedding(b []byte) ([]float32, error) {
	if len(b)%4 != 0 {
		return nil, fmt.Errorf("invalid embedding blob length %d (not a multiple of 4)", len(b))
	}
	v := make([]float32, len(b)/4)
	for i := range v {
		bits := binary.LittleEndian.Uint32(b[i*4:])
		v[i] = math.Float32frombits(bits)
	}
	return v, nil
}

// cosineSimilarity returns the cosine similarity between two float32 vectors.
// Returns 0 if either vector has zero norm (prevents NaN from division by zero).
func cosineSimilarity(a, b []float32) float64 {
	if len(a) != len(b) || len(a) == 0 {
		return 0
	}
	var dot, normA, normB float64
	for i := range a {
		ai, bi := float64(a[i]), float64(b[i])
		dot += ai * bi
		normA += ai * ai
		normB += bi * bi
	}
	if normA == 0 || normB == 0 {
		return 0
	}
	return dot / (math.Sqrt(normA) * math.Sqrt(normB))
}
