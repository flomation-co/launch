package embedding

import "context"

// Provider generates text embeddings for semantic search.
type Provider interface {
	Embed(ctx context.Context, text string) ([]float32, error)
}
