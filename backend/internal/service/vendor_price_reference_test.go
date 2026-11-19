//go:build unit

package service

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/stretchr/testify/require"
)

func TestVendorReferenceExactSourceAndOptionalRates(t *testing.T) {
	cfg := &config.Config{}
	cfg.Pricing.OverrideFile = filepath.Join(t.TempDir(), "override.json")
	require.NoError(t, os.WriteFile(cfg.Pricing.OverrideFile, []byte(`{"claude-known":{"input_cost_per_token":0.000009},"operator-only":{"input_cost_per_token":0.5,"source":"https://platform.claude.com/pricing","litellm_provider":"anthropic"}}`), 0600))
	s := NewPricingService(cfg, nil)
	cards, _, err := s.buildPricingData([]byte(`{
    "claude-known":{"source":"https://platform.claude.com/docs/pricing","litellm_provider":"anthropic","input_cost_per_token":0.000002,"output_cost_per_token":0.000008},
    "model-free-image":{"source":"https://ai.google.dev/pricing","litellm_provider":"gemini","input_cost_per_token":0,"output_cost_per_image_token":0},
    "model-paid-image":{"source":"https://ai.google.dev/pricing","litellm_provider":"gemini","input_cost_per_token":0.000001,"output_cost_per_image_token":0.000012},
    "no-source":{"input_cost_per_token":1},
    "wrong-vendor":{"source":"https://platform.claude.com/pricing","litellm_provider":"deepseek","input_cost_per_token":1}
  }`))
	require.NoError(t, err)
	s.pricingData = cards
	ref := s.VendorReference("claude-known")
	require.NotNil(t, ref)
	require.Equal(t, 0.000002, *ref.Input)
	require.Equal(t, 0.000009, cards["claude-known"].InputCostPerToken)
	require.Nil(t, ref.ImageOutput)
	require.Equal(t, 0.0, *s.VendorReference("model-free-image").ImageOutput)
	require.Equal(t, 0.000012, *s.VendorReference("model-paid-image").ImageOutput)
	for _, name := range []string{"claude-unknown-model", "operator-only", "no-source", "wrong-vendor"} {
		require.Nil(t, s.VendorReference(name), name)
	}
}

func TestVendorReferenceUsesShippedSourceWithoutChangingLivePrice(t *testing.T) {
	cfg := &config.Config{}
	cfg.Pricing.FallbackFile = filepath.Join("..", "..", "resources", "model-pricing", "model_prices_and_context_window.json")
	s := NewPricingService(cfg, nil)
	cards, _, err := s.buildPricingData([]byte(`{"gpt-4.1":{"litellm_provider":"openai","input_cost_per_token":0.000009}}`))
	require.NoError(t, err)
	s.pricingData = cards
	require.Equal(t, 0.000009, cards["gpt-4.1"].InputCostPerToken)
	ref := s.VendorReference("gpt-4.1")
	require.NotNil(t, ref)
	require.Equal(t, 0.000002, *ref.Input)
	require.Equal(t, 0.000008, *ref.Output)
	require.Nil(t, ref.ImageOutput)
	require.NotNil(t, s.VendorReference("deepseek-v4-pro"))
}
