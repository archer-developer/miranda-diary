package diary

import (
	"context"
	"database/sql"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"sort"
	"strings"
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

// schemaMigrations extends the initial schema with new columns and tables.
// ALTER TABLE ADD COLUMN returns "duplicate column name" if the column already
// exists — that error is tolerated so migrations are safe to replay on
// existing databases. CREATE TABLE IF NOT EXISTS is inherently idempotent.
var schemaMigrations = []string{
	`ALTER TABLE records ADD COLUMN content_nonce BLOB`,
	`ALTER TABLE records ADD COLUMN content_encrypted INTEGER NOT NULL DEFAULT 0`,
	`CREATE TABLE IF NOT EXISTS user_key_checks (
		user_id      TEXT PRIMARY KEY,
		check_nonce  BLOB NOT NULL,
		check_cipher BLOB NOT NULL
	)`,
	// tags_nonce is nil for rows written before tag encryption was added, even
	// if content_encrypted is 1 — those rows' tags column already holds
	// plaintext JSON and stays readable as such. Search tells the two cases
	// apart by checking tags_nonce's length, not just content_encrypted.
	`ALTER TABLE records ADD COLUMN tags_nonce BLOB`,
	// updated_at is NULL for records that were never edited via Update.
	// created_at is deliberately left untouched by Update, so a record that
	// is appended to over weeks keeps sorting and time-filtering by when it
	// was started; updated_at is what says when it last changed.
	`ALTER TABLE records ADD COLUMN updated_at INTEGER`,
}

// ErrEncryptedNoKey is returned by Get (and used internally by Search) when
// a record is stored encrypted but no encryption key was supplied — e.g.
// encryption was enabled for a user and later disabled in config.
var ErrEncryptedNoKey = errors.New("diary: record is encrypted and no encryption key was supplied")

// Store is a SQLite-backed diary store. Records are written with their
// float32 embedding (from Gemini) stored as a raw binary BLOB. Semantic
// search loads all embeddings into memory and ranks by cosine similarity —
// perfectly adequate for a personal diary even at tens of thousands of entries
// (10 000 records × 768 dims × 4 bytes ≈ 30 MB RAM).
//
// When a non-nil encryptionKey is passed to Add/Update/Search, record content
// and tags are protected with AES-256-GCM, each with its own nonce (tags are
// never filtered on in SQL, so encrypting them costs nothing functionally).
// Embeddings are always stored in plaintext so semantic search continues to
// work regardless of encryption state.
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

	for _, m := range schemaMigrations {
		if _, err := db.Exec(m); err != nil && !strings.Contains(err.Error(), "duplicate column name") {
			_ = db.Close()
			return nil, fmt.Errorf("diary: apply migration: %w", err)
		}
	}

	return &Store{db: db}, nil
}

// Add stores a new diary record owned by userID and returns it. embedding must
// be non-empty; tags may be nil (stored as an empty JSON array).
//
// When encryptionKey is non-nil, content and tags are each encrypted with
// AES-256-GCM (separate nonces) before storage. On first use for a given
// user, the key is validated and a sentinel is stored; on subsequent calls
// the sentinel is used to detect wrong keys before any record is written.
// Pass nil to store content and tags in plaintext.
func (s *Store) Add(ctx context.Context, userID, content string, tags []string, embedding []float32, encryptionKey []byte) (Record, error) {
	if tags == nil {
		tags = []string{}
	}

	tagsJSON, err := json.Marshal(tags)
	if err != nil {
		return Record{}, fmt.Errorf("diary: marshal tags: %w", err)
	}

	id := uuid.New().String()
	now := time.Now().UTC()

	if encryptionKey != nil {
		if err := verifyOrInitKeyCheck(ctx, s.db, userID, encryptionKey); err != nil {
			return Record{}, err
		}
		contentCiphertext, contentNonce, err := encryptContent(encryptionKey, content)
		if err != nil {
			return Record{}, fmt.Errorf("diary: encrypt content: %w", err)
		}
		tagsCiphertext, tagsNonce, err := encryptContent(encryptionKey, string(tagsJSON))
		if err != nil {
			return Record{}, fmt.Errorf("diary: encrypt tags: %w", err)
		}
		_, err = s.db.ExecContext(ctx,
			`INSERT INTO records (id, user_id, content, tags, created_at, embedding, content_nonce, content_encrypted, tags_nonce) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			id, userID, contentCiphertext, tagsCiphertext, now.Unix(), encodeEmbedding(embedding), contentNonce, 1, tagsNonce,
		)
		if err != nil {
			return Record{}, fmt.Errorf("diary: insert record: %w", err)
		}
	} else {
		_, err = s.db.ExecContext(ctx,
			`INSERT INTO records (id, user_id, content, tags, created_at, embedding) VALUES (?, ?, ?, ?, ?, ?)`,
			id, userID, content, string(tagsJSON), now.Unix(), encodeEmbedding(embedding),
		)
		if err != nil {
			return Record{}, fmt.Errorf("diary: insert record: %w", err)
		}
	}

	// Return plaintext content regardless of encryption — the caller passed it and
	// doesn't need it back as a ciphertext blob.
	return Record{ID: id, Content: content, Tags: tags, CreatedAt: now}, nil
}

// VerifyEncryptionKey checks encryptionKey against userID's stored sentinel
// (creating it on first use), without touching any record. Callers use this
// to reject a wrong key before doing expensive work (e.g. a Gemini embedding
// call) ahead of Add/Search, which each also run this same check internally
// before actually reading or writing a record.
func (s *Store) VerifyEncryptionKey(ctx context.Context, userID string, encryptionKey []byte) error {
	return verifyOrInitKeyCheck(ctx, s.db, userID, encryptionKey)
}

// Search returns up to limit records owned by userID ranked by cosine
// similarity to queryEmbedding. Only records belonging to userID are
// considered. All matching records are loaded and scored in-memory; the caller
// supplies a pre-validated positive limit.
//
// When encryptionKey is non-nil, encrypted rows are decrypted before being
// returned. A wrong key is detected early via the key sentinel and returns an
// error. Encrypted rows encountered when encryptionKey is nil are skipped
// rather than returned (e.g. when encryption was enabled and then later
// disabled for a user); skippedEncrypted counts how many, so the caller can
// surface that to whoever asked instead of it looking like "no matches."
//
// timeRange, if non-nil, additionally bounds results to created_at values in
// [From, To] (either bound may be nil). This is applied as a SQL WHERE clause
// rather than an in-memory filter after scoring, since it's the cheaper place
// to exclude rows — no point decrypting or embedding-scoring a record outside
// the requested window. The caller (mcpserver) is responsible for converting
// a user-facing local time range into the UTC bounds stored here; created_at
// itself is always UTC, see Add.
func (s *Store) Search(ctx context.Context, userID string, queryEmbedding []float32, limit int, encryptionKey []byte, timeRange *TimeRange) (results []SearchResult, skippedEncrypted int, err error) {
	if encryptionKey != nil {
		if err := verifyOrInitKeyCheck(ctx, s.db, userID, encryptionKey); err != nil {
			return nil, 0, err
		}
	}

	query := `SELECT id, content, tags, created_at, updated_at, embedding, content_nonce, content_encrypted, tags_nonce FROM records WHERE user_id = ?`
	args := []any{userID}
	if timeRange != nil {
		if timeRange.From != nil {
			query += ` AND created_at >= ?`
			args = append(args, timeRange.From.Unix())
		}
		if timeRange.To != nil {
			query += ` AND created_at <= ?`
			args = append(args, timeRange.To.Unix())
		}
	}

	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, 0, fmt.Errorf("diary: query records: %w", err)
	}
	defer func() { _ = rows.Close() }()

	for rows.Next() {
		var (
			r       storedRow
			embBlob []byte
		)
		if err := rows.Scan(&r.id, &r.content, &r.tags, &r.createdAt, &r.updatedAt, &embBlob, &r.contentNonce, &r.contentEncrypted, &r.tagsNonce); err != nil {
			return nil, 0, fmt.Errorf("diary: scan row: %w", err)
		}

		emb, err := decodeEmbedding(embBlob)
		if err != nil {
			// Skip rows with corrupt embeddings rather than aborting the search.
			continue
		}

		rec, err := r.decode(encryptionKey)
		if errors.Is(err, ErrEncryptedNoKey) {
			// Encrypted row with no key — encryption was on, then disabled.
			// Skip it, but count it so the caller can tell the difference
			// from "no matches" instead of it looking like data loss.
			skippedEncrypted++
			continue
		}
		if err != nil {
			// Corrupt row or (should not happen after the key check above)
			// a decryption failure — skip rather than fail the whole search.
			continue
		}

		results = append(results, SearchResult{
			Record: rec,
			Score:  cosineSimilarity(queryEmbedding, emb),
		})
	}
	if err := rows.Err(); err != nil {
		return nil, 0, fmt.Errorf("diary: iterate rows: %w", err)
	}

	sort.Slice(results, func(i, j int) bool {
		return results[i].Score > results[j].Score
	})

	if limit < len(results) {
		results = results[:limit]
	}
	return results, skippedEncrypted, nil
}

// Get returns the record with the given id if it belongs to userID. found is
// false (with a nil error) when no such record exists for userID — as with
// Remove, a record owned by someone else is indistinguishable from a missing
// one, to prevent ID enumeration.
//
// When encryptionKey is non-nil it is validated first (see
// verifyOrInitKeyCheck) and used to decrypt the record. An encrypted record
// requested with a nil key returns ErrEncryptedNoKey rather than ciphertext.
func (s *Store) Get(ctx context.Context, userID, id string, encryptionKey []byte) (rec Record, found bool, err error) {
	if encryptionKey != nil {
		if err := verifyOrInitKeyCheck(ctx, s.db, userID, encryptionKey); err != nil {
			return Record{}, false, err
		}
	}

	var r storedRow
	err = s.db.QueryRowContext(ctx,
		`SELECT id, content, tags, created_at, updated_at, content_nonce, content_encrypted, tags_nonce FROM records WHERE id = ? AND user_id = ?`,
		id, userID,
	).Scan(&r.id, &r.content, &r.tags, &r.createdAt, &r.updatedAt, &r.contentNonce, &r.contentEncrypted, &r.tagsNonce)
	if errors.Is(err, sql.ErrNoRows) {
		return Record{}, false, nil
	}
	if err != nil {
		return Record{}, false, fmt.Errorf("diary: get record: %w", err)
	}

	rec, err = r.decode(encryptionKey)
	if err != nil {
		return Record{}, false, err
	}
	return rec, true, nil
}

// Update overwrites the content, tags and embedding of the record with the
// given id, but only if it belongs to userID (found is false otherwise, same
// enumeration-safe contract as Remove). created_at is preserved; updated_at
// is set to now. tags may be nil (stored as an empty JSON array) — callers
// that want to keep the existing tags must pass them through explicitly.
//
// The record is re-written in whatever encryption mode encryptionKey implies
// right now, regardless of how it was stored before: a non-nil key encrypts
// content and tags with fresh nonces, a nil key stores plaintext. This mirrors
// Add, so an edited record always ends up the same as a freshly added one.
//
// Update is a plain overwrite, not a read-modify-write: appending to a record
// is done by the caller (mcpserver) as Get → concatenate → embed → Update.
// Two concurrent appends to the same record could therefore lose one of
// them. That's accepted for a single-caller personal diary — holding a
// transaction open across the Gemini embedding round-trip would block every
// other request on the store's single connection instead.
func (s *Store) Update(ctx context.Context, userID, id, content string, tags []string, embedding []float32, encryptionKey []byte) (rec Record, found bool, err error) {
	if tags == nil {
		tags = []string{}
	}
	tagsJSON, err := json.Marshal(tags)
	if err != nil {
		return Record{}, false, fmt.Errorf("diary: marshal tags: %w", err)
	}

	// Defaults are the plaintext representation; overwritten below when encrypting.
	var (
		contentValue     any = content
		tagsValue        any = string(tagsJSON)
		contentNonce     []byte
		tagsNonce        []byte
		contentEncrypted = 0
	)
	if encryptionKey != nil {
		if err := verifyOrInitKeyCheck(ctx, s.db, userID, encryptionKey); err != nil {
			return Record{}, false, err
		}
		contentCiphertext, cn, err := encryptContent(encryptionKey, content)
		if err != nil {
			return Record{}, false, fmt.Errorf("diary: encrypt content: %w", err)
		}
		tagsCiphertext, tn, err := encryptContent(encryptionKey, string(tagsJSON))
		if err != nil {
			return Record{}, false, fmt.Errorf("diary: encrypt tags: %w", err)
		}
		contentValue, tagsValue = contentCiphertext, tagsCiphertext
		contentNonce, tagsNonce = cn, tn
		contentEncrypted = 1
	}

	// RETURNING created_at both detects "no such record for this user" (no
	// row) and gives back the original creation time without a second query.
	// now is truncated to whole seconds so the returned UpdatedAt matches what
	// a later Get/Search reads back from the INTEGER column.
	now := time.Now().UTC().Truncate(time.Second)
	var createdAt int64
	err = s.db.QueryRowContext(ctx,
		`UPDATE records
		    SET content = ?, tags = ?, embedding = ?, content_nonce = ?, content_encrypted = ?, tags_nonce = ?, updated_at = ?
		  WHERE id = ? AND user_id = ?
		RETURNING created_at`,
		contentValue, tagsValue, encodeEmbedding(embedding), contentNonce, contentEncrypted, tagsNonce, now.Unix(),
		id, userID,
	).Scan(&createdAt)
	if errors.Is(err, sql.ErrNoRows) {
		return Record{}, false, nil
	}
	if err != nil {
		return Record{}, false, fmt.Errorf("diary: update record: %w", err)
	}

	return Record{
		ID:        id,
		Content:   content,
		Tags:      tags,
		CreatedAt: time.Unix(createdAt, 0).UTC(),
		UpdatedAt: now,
	}, true, nil
}

// Remove deletes the record with the given id only if it belongs to userID.
// Returns true if a record was deleted, false if no matching record was found
// (either the id doesn't exist or it belongs to a different user — callers
// cannot distinguish these cases by design, to prevent ID enumeration).
//
// When encryptionKey is non-nil, the key is validated before the delete so
// that Remove cannot be used to probe key correctness without authorization.
func (s *Store) Remove(ctx context.Context, userID, id string, encryptionKey []byte) (bool, error) {
	if encryptionKey != nil {
		if err := verifyOrInitKeyCheck(ctx, s.db, userID, encryptionKey); err != nil {
			return false, err
		}
	}

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

// storedRow is one records row as scanned from SQLite, before decryption —
// shared by Search and Get so both decode encrypted/plaintext rows the same way.
type storedRow struct {
	id               string
	content          []byte
	tags             []byte
	createdAt        int64
	updatedAt        sql.NullInt64 // NULL until the record is first edited via Update
	contentNonce     []byte        // nil for non-encrypted rows
	contentEncrypted int
	tagsNonce        []byte // nil for rows written before tag encryption existed
}

// decode turns r into a plaintext Record, decrypting with encryptionKey where
// needed. Returns ErrEncryptedNoKey for an encrypted row when encryptionKey
// is nil, and a plain error for a corrupt row or failed decryption.
func (r storedRow) decode(encryptionKey []byte) (Record, error) {
	var content string
	if r.contentEncrypted == 1 {
		if encryptionKey == nil {
			return Record{}, ErrEncryptedNoKey
		}
		if len(r.contentNonce) != gcmNonceSize {
			// content_nonce has no NOT NULL constraint, so a corrupt or
			// out-of-band-written row can carry a nil or wrong-length nonce.
			// gcm.Open panics (rather than erroring) on a wrong-length nonce,
			// so this must be checked before calling decryptContent.
			return Record{}, fmt.Errorf("diary: record %s has an invalid content nonce", r.id)
		}
		var err error
		content, err = decryptContent(encryptionKey, r.contentNonce, r.content)
		if err != nil {
			return Record{}, err
		}
	} else {
		content = string(r.content)
	}

	// tags_nonce set means this row was written after tag encryption was
	// added and r.tags is ciphertext; otherwise (including rows with
	// content_encrypted=1 written before this change) r.tags is already
	// plaintext JSON.
	tagsJSON := r.tags
	if len(r.tagsNonce) == gcmNonceSize {
		if encryptionKey == nil {
			return Record{}, ErrEncryptedNoKey
		}
		tagsPlain, err := decryptContent(encryptionKey, r.tagsNonce, r.tags)
		if err != nil {
			return Record{}, err
		}
		tagsJSON = []byte(tagsPlain)
	}

	var tags []string
	if err := json.Unmarshal(tagsJSON, &tags); err != nil {
		tags = []string{}
	}

	rec := Record{
		ID:        r.id,
		Content:   content,
		Tags:      tags,
		CreatedAt: time.Unix(r.createdAt, 0).UTC(),
	}
	if r.updatedAt.Valid {
		rec.UpdatedAt = time.Unix(r.updatedAt.Int64, 0).UTC()
	}
	return rec, nil
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
