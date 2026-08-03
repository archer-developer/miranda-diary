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
	rec, err := s.Add(ctx, userAlice, "hello world", []string{"test"}, emb)
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

	rec, err := s.Add(ctx, userAlice, "no tags", nil, []float32{1, 0})
	require.NoError(t, err)
	assert.Equal(t, []string{}, rec.Tags)
}

func TestStore_Remove(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	rec, err := s.Add(ctx, userAlice, "to be removed", nil, []float32{1, 0})
	require.NoError(t, err)

	deleted, err := s.Remove(ctx, userAlice, rec.ID)
	require.NoError(t, err)
	assert.True(t, deleted)

	// Second removal returns false, not an error.
	deleted, err = s.Remove(ctx, userAlice, rec.ID)
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
	aliceRec, err := s.Add(ctx, userAlice, "alice's secret", nil, []float32{1, 0})
	require.NoError(t, err)

	// Bob adds a record.
	_, err = s.Add(ctx, userBob, "bob's entry", nil, []float32{1, 0})
	require.NoError(t, err)

	// Alice's search only returns Alice's record.
	results, err := s.Search(ctx, userAlice, []float32{1, 0}, 10)
	require.NoError(t, err)
	require.Len(t, results, 1)
	assert.Equal(t, "alice's secret", results[0].Content)

	// Bob's search only returns Bob's record.
	results, err = s.Search(ctx, userBob, []float32{1, 0}, 10)
	require.NoError(t, err)
	require.Len(t, results, 1)
	assert.Equal(t, "bob's entry", results[0].Content)

	// Bob cannot delete Alice's record — returns false, not an error.
	deleted, err := s.Remove(ctx, userBob, aliceRec.ID)
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
	_, err := s.Add(ctx, userAlice, "close to query", nil, []float32{1, 0.1, 0})
	require.NoError(t, err)
	_, err = s.Add(ctx, userAlice, "far from query", nil, []float32{0, 1, 0})
	require.NoError(t, err)
	_, err = s.Add(ctx, userAlice, "exact match", nil, []float32{1, 0, 0})
	require.NoError(t, err)

	query := []float32{1, 0, 0}
	results, err := s.Search(ctx, userAlice, query, 10)
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
		_, err := s.Add(ctx, userAlice, "entry", nil, []float32{1, 0})
		require.NoError(t, err)
	}

	results, err := s.Search(ctx, userAlice, []float32{1, 0}, 3)
	require.NoError(t, err)
	assert.Len(t, results, 3)
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
