// Package embedding provides the Embedder interface and its Gemini
// implementation. The interface is kept narrow — a single Embed method —
// so tests can substitute a fake without pulling in HTTP or the genai SDK.
package embedding

import "context"

// Embedder converts text into a float32 vector suitable for cosine similarity
// comparison.
type Embedder interface {
	// Embed returns the embedding for text. The returned slice length is
	// determined by the underlying model (gemini-embedding-2) and is
	// consistent across calls with the same model.
	Embed(ctx context.Context, text string) ([]float32, error)
}
