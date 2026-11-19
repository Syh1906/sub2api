package service

import (
	"encoding/json"
	"net/url"
	"strings"
)

// VendorPriceReference contains the unmodified catalog entry, in USD per token.
// A nil field is absent from the source; an explicit zero is a published zero.
type VendorPriceReference struct {
	Model        string   `json:"model"`
	SourceURL    string   `json:"source_url"`
	Input        *float64 `json:"input_price,omitempty"`
	Output       *float64 `json:"output_price,omitempty"`
	CacheRead    *float64 `json:"cache_read_price,omitempty"`
	CacheWrite   *float64 `json:"cache_write_price,omitempty"`
	CacheWrite1h *float64 `json:"cache_write_1h_price,omitempty"`
	ImageInput   *float64 `json:"image_input_price,omitempty"`
	ImageOutput  *float64 `json:"image_output_price,omitempty"`
}

// Only accept catalog attribution to the model provider's own documentation.
var vendorSources = map[string][]string{
	"openai":                     {"openai.com", "platform.openai.com", "developers.openai.com"},
	"anthropic":                  {"anthropic.com", "www.anthropic.com", "docs.anthropic.com", "platform.claude.com"},
	"gemini":                     {"ai.google.dev", "cloud.google.com", "aistudio.google.com", "developers.googleblog.com"},
	"vertex_ai-language-models":  {"ai.google.dev", "cloud.google.com", "developers.googleblog.com"},
	"vertex_ai-embedding-models": {"ai.google.dev", "cloud.google.com"},
	"deepseek":                   {"api-docs.deepseek.com"},
}

func parseVendorReference(name string, raw json.RawMessage) *VendorPriceReference {
	var entry struct {
		LiteLLMRawEntry
		Source json.RawMessage `json:"source"`
	}
	if err := json.Unmarshal(raw, &entry); err != nil || entry.InputCostPerToken == nil && entry.OutputCostPerToken == nil {
		return nil
	}
	var source string
	if json.Unmarshal(entry.Source, &source) != nil {
		return nil
	}
	u, err := url.Parse(source)
	if err != nil || u.Scheme != "https" || u.User != nil || u.Port() != "" {
		return nil
	}
	for _, host := range vendorSources[entry.LiteLLMProvider] {
		if !strings.EqualFold(u.Hostname(), host) {
			continue
		}
		for _, price := range []*float64{entry.InputCostPerToken, entry.OutputCostPerToken, entry.CacheReadInputTokenCost, entry.CacheCreationInputTokenCost, entry.CacheCreationInputTokenCostAbove1hr, entry.InputCostPerImageToken, entry.OutputCostPerImageToken} {
			if price != nil && (*price < 0) {
				return nil
			}
		}
		return &VendorPriceReference{
			Model: name, SourceURL: source,
			Input: entry.InputCostPerToken, Output: entry.OutputCostPerToken,
			CacheRead: entry.CacheReadInputTokenCost, CacheWrite: entry.CacheCreationInputTokenCost,
			CacheWrite1h: entry.CacheCreationInputTokenCostAbove1hr,
			ImageInput:   entry.InputCostPerImageToken, ImageOutput: entry.OutputCostPerImageToken,
		}
	}
	return nil
}

// VendorReference deliberately does not reuse family matching, aliases or operator rates.
func (s *PricingService) VendorReference(model string) *VendorPriceReference {
	if s == nil {
		return nil
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	if card := s.pricingData[strings.TrimSpace(model)]; card != nil {
		return card.vendorReference
	}
	return nil
}
