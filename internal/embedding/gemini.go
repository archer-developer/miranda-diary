package embedding

import (
	"context"
	"fmt"

	"google.golang.org/genai"
)

// GeminiEmbedder implements Embedder using the Google Gemini embedding API.
// text-embedding-004 (the default model) is free-tier with 1 500 req/day —
// generous for a personal diary workload. The client is created once at
// startup and reused across calls; it is safe for concurrent use.
type GeminiEmbedder struct {
	client *genai.Client
	model  string
}

// NewGemini creates a GeminiEmbedder. apiKey is the Google AI API key;
// model is the embedding model name (e.g. "text-embedding-004").
func NewGemini(ctx context.Context, apiKey, model string) (*GeminiEmbedder, error) {
	client, err := genai.NewClient(ctx, &genai.ClientConfig{
		APIKey:  apiKey,
		Backend: genai.BackendGeminiAPI,
	})
	if err != nil {
		return nil, fmt.Errorf("embedding: create gemini client: %w", err)
	}
	return &GeminiEmbedder{client: client, model: model}, nil
}

// Embed returns the embedding vector for text.
func (g *GeminiEmbedder) Embed(ctx context.Context, text string) ([]float32, error) {
	result, err := g.client.Models.EmbedContent(ctx, g.model,
		genai.Text(text), nil,
	)
	if err != nil {
		return nil, fmt.Errorf("embedding: gemini embed: %w", err)
	}
	if len(result.Embeddings) == 0 {
		return nil, fmt.Errorf("embedding: gemini returned no embeddings")
	}
	return result.Embeddings[0].Values, nil
}
