package service

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// NativeProtocolRequest is a bounded native request. No chat conversion is used.
type NativeProtocolRequest struct {
	Endpoint    string
	Model       string
	Body        []byte
	ContentType string
	Stream      bool
}

func NativeProtocolCapability(endpoint string) OpenAIEndpointCapability {
	switch endpoint {
	case "/v1/completions":
		return OpenAIEndpointCapabilityCompletions
	case "/v1/audio/speech", "/v1/audio/transcriptions", "/v1/audio/translations":
		return OpenAIEndpointCapabilityAudio
	case "/v1/moderations":
		return OpenAIEndpointCapabilityModerations
	case "/v1/rerank":
		return OpenAIEndpointCapabilityRerank
	case "/v1/decisions":
		return OpenAIEndpointCapabilityDecisions
	default:
		return ""
	}
}

func nativeRequiresPerRequestPrice(endpoint string) bool {
	return strings.HasPrefix(endpoint, "/v1/audio/") || endpoint == "/v1/moderations"
}

// ParseNativeProtocolRequest validates the routing fields; the provider validates
// protocol-specific options. Multipart files remain in memory under the route body limit.
func ParseNativeProtocolRequest(endpoint, contentType string, body []byte) (*NativeProtocolRequest, error) {
	if NativeProtocolCapability(endpoint) == "" {
		return nil, fmt.Errorf("unsupported native endpoint")
	}
	req := &NativeProtocolRequest{Endpoint: endpoint, Body: body, ContentType: contentType}
	media, params, err := mime.ParseMediaType(contentType)
	if err != nil {
		return nil, fmt.Errorf("invalid Content-Type")
	}
	if endpoint == "/v1/audio/transcriptions" || endpoint == "/v1/audio/translations" {
		if media != "multipart/form-data" || params["boundary"] == "" {
			return nil, fmt.Errorf("audio upload requires multipart/form-data")
		}
		reader := multipart.NewReader(bytes.NewReader(body), params["boundary"])
		hasFile := false
		for {
			part, err := reader.NextPart()
			if err == io.EOF {
				break
			}
			if err != nil {
				return nil, fmt.Errorf("invalid multipart request: %w", err)
			}
			switch part.FormName() {
			case "model":
				value, err := io.ReadAll(io.LimitReader(part, 1025))
				if err != nil || len(value) > 1024 {
					return nil, fmt.Errorf("invalid multipart model")
				}
				if req.Model != "" {
					return nil, fmt.Errorf("duplicate multipart model")
				}
				req.Model = strings.TrimSpace(string(value))
			case "file":
				if part.FileName() == "" {
					return nil, fmt.Errorf("audio file is required")
				}
				hasFile = true
			}
			if err := part.Close(); err != nil {
				return nil, fmt.Errorf("invalid multipart field: %w", err)
			}
		}
		if !hasFile {
			return nil, fmt.Errorf("audio file is required")
		}
	} else {
		if media != "application/json" || !gjson.ValidBytes(body) || !gjson.ParseBytes(body).IsObject() {
			return nil, fmt.Errorf("a JSON object is required")
		}
		model := gjson.GetBytes(body, "model")
		if model.Type == gjson.String {
			req.Model = strings.TrimSpace(model.String())
		}
		stream := gjson.GetBytes(body, "stream")
		if stream.Exists() && stream.Type != gjson.True && stream.Type != gjson.False {
			return nil, fmt.Errorf("stream must be a boolean")
		}
		req.Stream = stream.Bool()
		if req.Stream && endpoint != "/v1/completions" {
			return nil, fmt.Errorf("stream is only supported by the completions endpoint")
		}
		field := "input"
		switch endpoint {
		case "/v1/completions":
			field = "prompt"
		case "/v1/rerank":
			field = "query"
		}
		if !gjson.GetBytes(body, field).Exists() {
			return nil, fmt.Errorf("%s is required", field)
		}
	}
	if req.Model == "" {
		return nil, fmt.Errorf("model is required")
	}
	return req, nil
}

func (req *NativeProtocolRequest) withModel(model string) ([]byte, string, error) {
	if !strings.HasPrefix(req.ContentType, "multipart/") {
		body, err := sjson.SetBytes(bytes.Clone(req.Body), "model", model)
		if err == nil && req.Stream {
			body, err = ensureOpenAIChatStreamUsage(body)
		}
		return body, req.ContentType, err
	}
	_, params, err := mime.ParseMediaType(req.ContentType)
	if err != nil {
		return nil, "", err
	}
	reader := multipart.NewReader(bytes.NewReader(req.Body), params["boundary"])
	var result bytes.Buffer
	writer := multipart.NewWriter(&result)
	for {
		part, err := reader.NextPart()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, "", err
		}
		target, err := writer.CreatePart(part.Header)
		if err != nil {
			return nil, "", err
		}
		if part.FormName() == "model" {
			_, err = io.WriteString(target, model)
		} else {
			_, err = io.Copy(target, part)
		}
		if err != nil {
			return nil, "", err
		}
		if err = part.Close(); err != nil {
			return nil, "", err
		}
	}
	if err = writer.Close(); err != nil {
		return nil, "", err
	}
	return result.Bytes(), writer.FormDataContentType(), nil
}

func (s *OpenAIGatewayService) nativeProtocolPricing(ctx context.Context, apiKey *APIKey, req *NativeProtocolRequest, mapping ChannelMappingResult, upstreamModel string) (string, *ResolvedPricing, error) {
	if apiKey == nil || apiKey.Group == nil || s.resolver == nil {
		return "", nil, fmt.Errorf("native endpoint pricing is unavailable")
	}
	if mapping.BillingModelSource == BillingModelSourceResponse {
		return "", nil, fmt.Errorf("native endpoints require requested, channel_mapped or upstream billing_model_source")
	}
	model := upstreamModel
	switch mapping.BillingModelSource {
	case BillingModelSourceRequested:
		model = req.Model
	case BillingModelSourceChannelMapped, "":
		model = mapping.MappedModel
		if model == "" {
			model = req.Model
		}
	}
	pricing := s.resolver.Resolve(ctx, PricingInput{Model: model, GroupID: apiKey.GroupID, Group: apiKey.Group})
	if pricing == nil {
		return "", nil, fmt.Errorf("model pricing is required")
	}
	if pricing.Mode == BillingModePerRequest && (pricing.channelPricing == nil || pricing.channelPricing.PerRequestPrice == nil) {
		return "", nil, fmt.Errorf("an explicit per_request_price is required")
	}
	if nativeRequiresPerRequestPrice(req.Endpoint) {
		if pricing.Mode != BillingModePerRequest || pricing.channelPricing == nil || pricing.channelPricing.PerRequestPrice == nil || (pricing.Source != PricingSourceGroup && pricing.Source != PricingSourceChannel) {
			return "", nil, fmt.Errorf("this endpoint requires an explicit per_request model price")
		}
	} else if pricing.Mode != BillingModePerRequest && (pricing.Mode != BillingModeToken || pricing.BasePricing == nil) {
		return "", nil, fmt.Errorf("token or per_request model pricing is required")
	}
	if pricing.Mode == BillingModeToken && pricing.channelPricing != nil && pricing.channelPricing.InputPrice == nil && pricing.channelPricing.OutputPrice == nil {
		if _, err := s.billingService.GetModelPricing(model); err != nil {
			return "", nil, fmt.Errorf("custom token model prices are required")
		}
	}
	return model, pricing, nil
}

// nativeProtocolUsage recognizes token usage without treating total_tokens as
// prompt_tokens when a provider supplied completion tokens as well.
func nativeProtocolUsage(body []byte) (OpenAIUsage, bool) {
	value := gjson.GetBytes(body, "usage")
	found := false
	for _, field := range []string{"input_tokens", "prompt_tokens", "output_tokens", "completion_tokens", "total_tokens"} {
		v := value.Get(field)
		if !v.Exists() {
			continue
		}
		if v.Type != gjson.Number || v.Float() < 0 || v.Float() != float64(v.Int()) {
			return OpenAIUsage{}, false
		}
		found = true
	}
	if !found {
		return OpenAIUsage{}, false
	}
	usage, ok := openAIUsageFromGJSON(value)
	if !value.Get("input_tokens").Exists() && !value.Get("prompt_tokens").Exists() && value.Get("total_tokens").Exists() {
		usage.InputTokens = max(0, int(value.Get("total_tokens").Int())-usage.OutputTokens)
	}
	return usage, ok
}

func newNativeBillingRequestID() string {
	// Client IDs and provider echoes cannot merge two independently executed calls.
	return "native_protocol:" + generateRequestID()
}

func nativeProtocolError(c *gin.Context, status int, message string) {
	c.JSON(status, gin.H{"error": gin.H{"type": "invalid_request_error", "message": message}})
}

func writeNativeStreamError(c *gin.Context, err error) {
	data, _ := json.Marshal(gin.H{"error": gin.H{"type": "upstream_error", "message": err.Error()}})
	_, _ = fmt.Fprintf(c.Writer, "data: %s\n\n", data)
	c.Writer.Flush()
}
