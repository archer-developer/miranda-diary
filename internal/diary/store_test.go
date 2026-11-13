package diary

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func newTestStore(t *testing.T) *Store {
	t.Helper()
	s, err := New(":memory:")
	require.NoError(t, err)
	t.Cleanup(func() { _ = s.Close() })
	return s
}

const (
	userAlice = "alice"
	userBob   = "bob"
)

func TestStore_AddAndCount(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	emb := []float32{1, 0, 0}
	rec, err := s.Add(ctx, userAlice, "hello world", []string{"test"}, emb, nil)
	require.NoError(t, err)
	assert.NotEmpty(t, rec.ID)
	assert.Equal(t, "hello world", rec.Content)
	assert.Equal(t, []string{"test"}, rec.Tags)
	assert.False(t, rec.CreatedAt.IsZero())

	n, err := s.Count(ctx, userAlice)
	require.NoError(t, err)
	assert.Equal(t, int64(1), n)
}

func TestStore_AddNilTags(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	rec, err := s.Add(ctx, userAlice, "no tags", nil, []float32{1, 0}, nil)
	require.NoError(t, err)
	assert.Equal(t, []string{}, rec.Tags)
}

func TestStore_Remove(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	rec, err := s.Add(ctx, userAlice, "to be removed", nil, []float32{1, 0}, nil)
	require.NoError(t, err)

	deleted, err := s.Remove(ctx, userAlice, rec.ID, nil)
	require.NoError(t, err)
	assert.True(t, deleted)

	// Second removal returns false, not an error.
	deleted, err = s.Remove(ctx, userAlice, rec.ID, nil)
	require.NoError(t, err)
	assert.False(t, deleted)

	n, err := s.Count(ctx, userAlice)
	require.NoError(t, err)
	assert.Equal(t, int64(0), n)
}

func TestStore_UserIsolation(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	// Alice adds a record.
	aliceRec, err := s.Add(ctx, userAlice, "alice's secret", nil, []float32{1, 0}, nil)
	require.NoError(t, err)

	// Bob adds a record.
	_, err = s.Add(ctx, userBob, "bob's entry", nil, []float32{1, 0}, nil)
	require.NoError(t, err)

	// Alice's search only returns Alice's record.
	results, _, err := s.Search(ctx, userAlice, []float32{1, 0}, 10, nil, nil)
	require.NoError(t, err)
	require.Len(t, results, 1)
	assert.Equal(t, "alice's secret", results[0].Content)

	// Bob's search only returns Bob's record.
	results, _, err = s.Search(ctx, userBob, []float32{1, 0}, 10, nil, nil)
	require.NoError(t, err)
	require.Len(t, results, 1)
	assert.Equal(t, "bob's entry", results[0].Content)

	// Bob cannot delete Alice's record — returns false, not an error.
	deleted, err := s.Remove(ctx, userBob, aliceRec.ID, nil)
	require.NoError(t, err)
	assert.False(t, deleted)

	// Alice's record is still there.
	n, err := s.Count(ctx, userAlice)
	require.NoError(t, err)
	assert.Equal(t, int64(1), n)
}

func TestStore_GetAndUpdate(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	rec, err := s.Add(ctx, userAlice, "day 1: started", []string{"trip"}, []float32{1, 0}, nil)
	require.NoError(t, err)

	got, found, err := s.Get(ctx, userAlice, rec.ID, nil)
	require.NoError(t, err)
	require.True(t, found)
	assert.Equal(t, "day 1: started", got.Content)
	assert.Equal(t, []string{"trip"}, got.Tags)
	assert.True(t, got.UpdatedAt.IsZero(), "never-edited record has no updated_at")

	updated, found, err := s.Update(ctx, userAlice, rec.ID, "day 1: started\n\nday 2: continued", []string{"trip", "log"}, []float32{0, 1}, nil)
	require.NoError(t, err)
	require.True(t, found)
	assert.Equal(t, rec.CreatedAt.Unix(), updated.CreatedAt.Unix(), "created_at is preserved")
	assert.False(t, updated.UpdatedAt.IsZero())

	// The edit is visible through both Get and Search, including the new embedding.
	got, _, err = s.Get(ctx, userAlice, rec.ID, nil)
	require.NoError(t, err)
	assert.Equal(t, "day 1: started\n\nday 2: continued", got.Content)
	assert.Equal(t, []string{"trip", "log"}, got.Tags)
	assert.Equal(t, updated.UpdatedAt, got.UpdatedAt)

	results, _, err := s.Search(ctx, userAlice, []float32{0, 1}, 10, nil, nil)
	require.NoError(t, err)
	require.Len(t, results, 1)
	assert.InDelta(t, 1.0, results[0].Score, 1e-6)
	assert.False(t, results[0].UpdatedAt.IsZero())

	n, err := s.Count(ctx, userAlice)
	require.NoError(t, err)
	assert.Equal(t, int64(1), n, "update must not create a new record")
}

// TestStore_GetAndUpdate_UserIsolation mirrors TestStore_UserIsolation for
// the edit path: another user's record is indistinguishable from a missing
// one, and can't be overwritten.
func TestStore_GetAndUpdate_UserIsolation(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	aliceRec, err := s.Add(ctx, userAlice, "alice's secret", nil, []float32{1, 0}, nil)
	require.NoError(t, err)

	_, found, err := s.Get(ctx, userBob, aliceRec.ID, nil)
	require.NoError(t, err)
	assert.False(t, found)

	_, found, err = s.Update(ctx, userBob, aliceRec.ID, "hijacked", nil, []float32{1, 0}, nil)
	require.NoError(t, err)
	assert.False(t, found)

	got, _, err := s.Get(ctx, userAlice, aliceRec.ID, nil)
	require.NoError(t, err)
	assert.Equal(t, "alice's secret", got.Content)

	_, found, err = s.Update(ctx, userAlice, "no-such-id", "x", nil, []float32{1, 0}, nil)
	require.NoError(t, err)
	assert.False(t, found)
}

func TestStore_Search_RankedByScore(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	// Three records with known embeddings.
	_, err := s.Add(ctx, userAlice, "close to query", nil, []float32{1, 0.1, 0}, nil)
	require.NoError(t, err)
	_, err = s.Add(ctx, userAlice, "far from query", nil, []float32{0, 1, 0}, nil)
	require.NoError(t, err)
	_, err = s.Add(ctx, userAlice, "exact match", nil, []float32{1, 0, 0}, nil)
	require.NoError(t, err)

	query := []float32{1, 0, 0}
	results, _, err := s.Search(ctx, userAlice, query, 10, nil, nil)
	require.NoError(t, err)
	require.Len(t, results, 3)

	// Results must be sorted descending by score.
	assert.Equal(t, "exact match", results[0].Content)
	assert.InDelta(t, 1.0, results[0].Score, 0.001)
	assert.Greater(t, results[1].Score, results[2].Score)
}

func TestStore_Search_LimitRespected(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	for range 5 {
		_, err := s.Add(ctx, userAlice, "entry", nil, []float32{1, 0}, nil)
		require.NoError(t, err)
	}

	results, _, err := s.Search(ctx, userAlice, []float32{1, 0}, 3, nil, nil)
	require.NoError(t, err)
	assert.Len(t, results, 3)
}

func TestStore_Search_TimeRange(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	base := time.Date(2026, 9, 26, 0, 0, 0, 0, time.UTC)
	insertAt := func(id string, at time.Time) {
		_, err := s.db.ExecContext(ctx,
			`INSERT INTO records (id, user_id, content, tags, created_at, embedding) VALUES (?, ?, ?, ?, ?, ?)`,
			id, userAlice, "entry "+id, "[]", at.Unix(), encodeEmbedding([]float32{1, 0}),
		)
		require.NoError(t, err)
	}
	insertAt("early", base.Add(10*time.Hour))
	insertAt("mid", base.Add(14*time.Hour))
	insertAt("late", base.Add(18*time.Hour))

	t.Run("both bounds set", func(t *testing.T) {
		from := base.Add(12 * time.Hour)
		to := base.Add(16 * time.Hour)
		results, _, err := s.Search(ctx, userAlice, []float32{1, 0}, 10, nil, &TimeRange{From: &from, To: &to})
		require.NoError(t, err)
		require.Len(t, results, 1)
		assert.Equal(t, "entry mid", results[0].Content)
	})

	t.Run("from only", func(t *testing.T) {
		from := base.Add(12 * time.Hour)
		results, _, err := s.Search(ctx, userAlice, []float32{1, 0}, 10, nil, &TimeRange{From: &from})
		require.NoError(t, err)
		require.Len(t, results, 2)
	})

	t.Run("to only", func(t *testing.T) {
		to := base.Add(12 * time.Hour)
		results, _, err := s.Search(ctx, userAlice, []float32{1, 0}, 10, nil, &TimeRange{To: &to})
		require.NoError(t, err)
		require.Len(t, results, 1)
		assert.Equal(t, "entry early", results[0].Content)
	})

	t.Run("nil TimeRange means unbounded", func(t *testing.T) {
		results, _, err := s.Search(ctx, userAlice, []float32{1, 0}, 10, nil, nil)
		require.NoError(t, err)
		assert.Len(t, results, 3)
	})
}

func TestStore_Encryption(t *testing.T) {
	ctx := context.Background()

	// Deterministic non-zero key and a clearly different wrong key.
	key := make([]byte, 32)
	for i := range key {
		key[i] = byte(i + 1)
	}
	wrongKey := make([]byte, 32)
	for i := range wrongKey {
		wrongKey[i] = byte(i + 100)
	}

	t.Run("add and search with correct key", func(t *testing.T) {
		s := newTestStore(t)

		rec, err := s.Add(ctx, userAlice, "secret diary entry", nil, []float32{1, 0}, key)
		require.NoError(t, err)
		assert.NotEmpty(t, rec.ID)
		// Add returns plaintext content — the caller passed it and doesn't need it back encrypted.
		assert.Equal(t, "secret diary entry", rec.Content)

		results, _, err := s.Search(ctx, userAlice, []float32{1, 0}, 10, key, nil)
		require.NoError(t, err)
		require.Len(t, results, 1)
		assert.Equal(t, "secret diary entry", results[0].Content)
	})

	t.Run("search with wrong key returns error", func(t *testing.T) {
		s := newTestStore(t)

		_, err := s.Add(ctx, userAlice, "secret", nil, []float32{1, 0}, key)
		require.NoError(t, err)

		// verifyOrInitKeyCheck detects the mismatch before any row is scanned.
		_, _, err = s.Search(ctx, userAlice, []float32{1, 0}, 10, wrongKey, nil)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "wrong encryption key")
	})

	t.Run("add with wrong key returns error", func(t *testing.T) {
		s := newTestStore(t)

		// Initialize the sentinel with the correct key first.
		_, err := s.Add(ctx, userAlice, "first entry", nil, []float32{1, 0}, key)
		require.NoError(t, err)

		// A subsequent add with the wrong key is rejected before any write.
		_, err = s.Add(ctx, userAlice, "should fail", nil, []float32{1, 0}, wrongKey)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "wrong encryption key")
	})

	t.Run("encrypted rows skipped but counted when no key provided", func(t *testing.T) {
		s := newTestStore(t)

		_, err := s.Add(ctx, userAlice, "encrypted entry", nil, []float32{1, 0}, key)
		require.NoError(t, err)

		// Searching without a key: encrypted rows are skipped from results but
		// counted (migration scenario — encryption was on, then turned off in
		// config), so the caller can tell this apart from "no matches."
		results, skipped, err := s.Search(ctx, userAlice, []float32{1, 0}, 10, nil, nil)
		require.NoError(t, err)
		assert.Len(t, results, 0)
		assert.Equal(t, 1, skipped)
	})

	t.Run("remove with wrong key returns error", func(t *testing.T) {
		s := newTestStore(t)

		rec, err := s.Add(ctx, userAlice, "to remove", nil, []float32{1, 0}, key)
		require.NoError(t, err)

		_, err = s.Remove(ctx, userAlice, rec.ID, wrongKey)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "wrong encryption key")

		// Record must still exist after the failed remove.
		n, err := s.Count(ctx, userAlice)
		require.NoError(t, err)
		assert.Equal(t, int64(1), n)
	})

	t.Run("remove with correct key succeeds", func(t *testing.T) {
		s := newTestStore(t)

		rec, err := s.Add(ctx, userAlice, "to remove", nil, []float32{1, 0}, key)
		require.NoError(t, err)

		deleted, err := s.Remove(ctx, userAlice, rec.ID, key)
		require.NoError(t, err)
		assert.True(t, deleted)
	})

	t.Run("get and update round-trip encrypted content and tags", func(t *testing.T) {
		s := newTestStore(t)

		rec, err := s.Add(ctx, userAlice, "secret v1", []string{"private"}, []float32{1, 0}, key)
		require.NoError(t, err)

		_, found, err := s.Update(ctx, userAlice, rec.ID, "secret v2", []string{"private", "edited"}, []float32{1, 0}, key)
		require.NoError(t, err)
		require.True(t, found)

		// Stored at rest as ciphertext, not plaintext.
		var rawContent, rawTags []byte
		require.NoError(t, s.db.QueryRowContext(ctx, `SELECT content, tags FROM records WHERE id = ?`, rec.ID).Scan(&rawContent, &rawTags))
		assert.NotContains(t, string(rawContent), "secret v2")
		assert.NotContains(t, string(rawTags), "edited")

		got, found, err := s.Get(ctx, userAlice, rec.ID, key)
		require.NoError(t, err)
		require.True(t, found)
		assert.Equal(t, "secret v2", got.Content)
		assert.Equal(t, []string{"private", "edited"}, got.Tags)
	})

	t.Run("get and update with wrong key return error", func(t *testing.T) {
		s := newTestStore(t)

		rec, err := s.Add(ctx, userAlice, "secret", nil, []float32{1, 0}, key)
		require.NoError(t, err)

		_, _, err = s.Get(ctx, userAlice, rec.ID, wrongKey)
		require.Error(t, err)
		_, _, err = s.Update(ctx, userAlice, rec.ID, "overwrite", nil, []float32{1, 0}, wrongKey)
		require.Error(t, err)

		got, _, err := s.Get(ctx, userAlice, rec.ID, key)
		require.NoError(t, err)
		assert.Equal(t, "secret", got.Content)
	})

	t.Run("get of encrypted record without key returns ErrEncryptedNoKey", func(t *testing.T) {
		s := newTestStore(t)

		rec, err := s.Add(ctx, userAlice, "secret", nil, []float32{1, 0}, key)
		require.NoError(t, err)

		_, _, err = s.Get(ctx, userAlice, rec.ID, nil)
		require.ErrorIs(t, err, ErrEncryptedNoKey)
	})

	t.Run("unencrypted records still searchable without key", func(t *testing.T) {
		s := newTestStore(t)

		rec, err := s.Add(ctx, userAlice, "plaintext entry", nil, []float32{1, 0}, nil)
		require.NoError(t, err)
		assert.Equal(t, "plaintext entry", rec.Content)

		results, _, err := s.Search(ctx, userAlice, []float32{1, 0}, 10, nil, nil)
		require.NoError(t, err)
		require.Len(t, results, 1)
		assert.Equal(t, "plaintext entry", results[0].Content)
	})

	t.Run("tags are encrypted at rest and decrypted on search", func(t *testing.T) {
		s := newTestStore(t)

		rec, err := s.Add(ctx, userAlice, "secret entry", []string{"therapy", "argument"}, []float32{1, 0}, key)
		require.NoError(t, err)
		assert.Equal(t, []string{"therapy", "argument"}, rec.Tags)

		// The tags column must not contain the plaintext tag values — only
		// ciphertext bytes decryptable with the key.
		var rawTags []byte
		err = s.db.QueryRowContext(ctx, `SELECT tags FROM records WHERE id = ?`, rec.ID).Scan(&rawTags)
		require.NoError(t, err)
		assert.NotContains(t, string(rawTags), "therapy")
		assert.NotContains(t, string(rawTags), "argument")

		results, _, err := s.Search(ctx, userAlice, []float32{1, 0}, 10, key, nil)
		require.NoError(t, err)
		require.Len(t, results, 1)
		assert.Equal(t, []string{"therapy", "argument"}, results[0].Tags)
	})

	t.Run("pre-existing encrypted row with plaintext tags and no tags_nonce still reads tags", func(t *testing.T) {
		s := newTestStore(t)

		// Simulates a row written before tag encryption existed: content is
		// encrypted, tags is plaintext JSON, tags_nonce is NULL.
		ciphertext, nonce, err := encryptContent(key, "old secret entry")
		require.NoError(t, err)
		_, err = s.db.ExecContext(ctx,
			`INSERT INTO records (id, user_id, content, tags, created_at, embedding, content_nonce, content_encrypted) VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
			"legacy-row", userAlice, ciphertext, `["old-tag"]`, 0, encodeEmbedding([]float32{1, 0}), nonce, 1,
		)
		require.NoError(t, err)

		results, _, err := s.Search(ctx, userAlice, []float32{1, 0}, 10, key, nil)
		require.NoError(t, err)
		require.Len(t, results, 1)
		assert.Equal(t, "old secret entry", results[0].Content)
		assert.Equal(t, []string{"old-tag"}, results[0].Tags)
	})

	t.Run("row with nil content_nonce is skipped instead of panicking", func(t *testing.T) {
		s := newTestStore(t)

		// content_nonce has no NOT NULL constraint at the schema level, so a
		// corrupt or out-of-band-written row can carry a NULL nonce. Simulate
		// that directly — gcm.Open panics on a wrong-length nonce rather than
		// erroring, so Search must guard against this before decrypting.
		_, err := s.db.ExecContext(ctx,
			`INSERT INTO records (id, user_id, content, tags, created_at, embedding, content_nonce, content_encrypted) VALUES (?, ?, ?, ?, ?, ?, NULL, 1)`,
			"corrupt-nonce-row", userAlice, []byte("garbage-ciphertext"), "[]", 0, encodeEmbedding([]float32{1, 0}),
		)
		require.NoError(t, err)

		require.NotPanics(t, func() {
			results, _, err := s.Search(ctx, userAlice, []float32{1, 0}, 10, key, nil)
			require.NoError(t, err)
			assert.Len(t, results, 0)
		})
	})
}

func TestCosineSimilarity(t *testing.T) {
	assert.InDelta(t, 1.0, cosineSimilarity([]float32{1, 0}, []float32{1, 0}), 0.0001)
	assert.InDelta(t, 0.0, cosineSimilarity([]float32{1, 0}, []float32{0, 1}), 0.0001)
	assert.Equal(t, 0.0, cosineSimilarity([]float32{0, 0}, []float32{1, 0}))
	assert.Equal(t, 0.0, cosineSimilarity([]float32{1, 0}, []float32{1, 0, 0})) // mismatched dims
}

func TestEncodeDecodeEmbedding(t *testing.T) {
	original := []float32{1.5, -2.7, 0.0, 3.14}
	encoded := encodeEmbedding(original)
	decoded, err := decodeEmbedding(encoded)
	require.NoError(t, err)
	assert.Equal(t, original, decoded)
}
