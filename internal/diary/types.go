// Package diary implements diary record storage backed by SQLite with
// vector embeddings for semantic search.
package diary

import "time"

// Record is a single diary entry as returned from the store.
type Record struct {
	ID        string
	Content   string
	Tags      []string
	CreatedAt time.Time
}

// SearchResult pairs a Record with its cosine similarity score against a
// search query. Score is in [0, 1]: 1 means identical direction, 0 means
// orthogonal. Values are sorted descending by score before being returned
// to the caller.
type SearchResult struct {
	Record
	Score float64
}
