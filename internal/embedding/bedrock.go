package embedding

import (
	"context"
	"encoding/json"
	"fmt"

	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/bedrockruntime"
)

// BedrockProvider generates embeddings using AWS Bedrock Titan Embeddings v2.
type BedrockProvider struct {
	client     *bedrockruntime.Client
	modelID    string
	dimensions int
}

// NewBedrockProvider creates a provider using the default AWS credential chain.
func NewBedrockProvider(region, modelID string, dimensions int) (*BedrockProvider, error) {
	if modelID == "" {
		modelID = "amazon.titan-embed-text-v2:0"
	}
	if dimensions <= 0 {
		dimensions = 1024
	}

	cfg, err := awsconfig.LoadDefaultConfig(context.Background(),
		awsconfig.WithRegion(region),
	)
	if err != nil {
		return nil, fmt.Errorf("load AWS config: %w", err)
	}

	return &BedrockProvider{
		client:     bedrockruntime.NewFromConfig(cfg),
		modelID:    modelID,
		dimensions: dimensions,
	}, nil
}

type titanRequest struct {
	InputText  string `json:"inputText"`
	Dimensions int    `json:"dimensions"`
	Normalize  bool   `json:"normalize"`
}

type titanResponse struct {
	Embedding []float32 `json:"embedding"`
}

// Embed generates a vector embedding for the given text.
func (p *BedrockProvider) Embed(ctx context.Context, text string) ([]float32, error) {
	payload, err := json.Marshal(titanRequest{
		InputText:  text,
		Dimensions: p.dimensions,
		Normalize:  true,
	})
	if err != nil {
		return nil, fmt.Errorf("marshal request: %w", err)
	}

	resp, err := p.client.InvokeModel(ctx, &bedrockruntime.InvokeModelInput{
		ModelId:     &p.modelID,
		Body:        payload,
		ContentType: strPtr("application/json"),
		Accept:      strPtr("application/json"),
	})
	if err != nil {
		return nil, fmt.Errorf("invoke Bedrock model %s: %w", p.modelID, err)
	}

	var result titanResponse
	if err := json.Unmarshal(resp.Body, &result); err != nil {
		return nil, fmt.Errorf("unmarshal response: %w", err)
	}

	if len(result.Embedding) == 0 {
		return nil, fmt.Errorf("empty embedding returned from Bedrock")
	}

	return result.Embedding, nil
}

func strPtr(s string) *string { return &s }
