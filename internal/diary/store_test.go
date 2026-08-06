package diary

import (
	"context"
	"testing"

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
	results, err := s.Search(ctx, userAlice, []float32{1, 0}, 10, nil)
	require.NoError(t, err)
	require.Len(t, results, 1)
	assert.Equal(t, "alice's secret", results[0].Content)

	// Bob's search only returns Bob's record.
	results, err = s.Search(ctx, userBob, []float32{1, 0}, 10, nil)
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
	results, err := s.Search(ctx, userAlice, query, 10, nil)
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

	results, err := s.Search(ctx, userAlice, []float32{1, 0}, 3, nil)
	require.NoError(t, err)
	assert.Len(t, results, 3)
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

		results, err := s.Search(ctx, userAlice, []float32{1, 0}, 10, key)
		require.NoError(t, err)
		require.Len(t, results, 1)
		assert.Equal(t, "secret diary entry", results[0].Content)
	})

	t.Run("search with wrong key returns error", func(t *testing.T) {
		s := newTestStore(t)

		_, err := s.Add(ctx, userAlice, "secret", nil, []float32{1, 0}, key)
		require.NoError(t, err)

		// verifyOrInitKeyCheck detects the mismatch before any row is scanned.
		_, err = s.Search(ctx, userAlice, []float32{1, 0}, 10, wrongKey)
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

	t.Run("encrypted rows skipped silently when no key provided", func(t *testing.T) {
		s := newTestStore(t)

		_, err := s.Add(ctx, userAlice, "encrypted entry", nil, []float32{1, 0}, key)
		require.NoError(t, err)

		// Searching without a key: encrypted rows are silently skipped
		// (migration scenario — encryption was on, then turned off in config).
		results, err := s.Search(ctx, userAlice, []float32{1, 0}, 10, nil)
		require.NoError(t, err)
		assert.Len(t, results, 0)
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

	t.Run("unencrypted records still searchable without key", func(t *testing.T) {
		s := newTestStore(t)

		rec, err := s.Add(ctx, userAlice, "plaintext entry", nil, []float32{1, 0}, nil)
		require.NoError(t, err)
		assert.Equal(t, "plaintext entry", rec.Content)

		results, err := s.Search(ctx, userAlice, []float32{1, 0}, 10, nil)
		require.NoError(t, err)
		require.Len(t, results, 1)
		assert.Equal(t, "plaintext entry", results[0].Content)
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
			results, err := s.Search(ctx, userAlice, []float32{1, 0}, 10, key)
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
