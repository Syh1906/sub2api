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

	"github.com/Wei-Shaw/sub2api/internal/pkg/requestmodel"
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
	BinaryAudio bool
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
	req.ContentType = mime.FormatMediaType(media, params)
	if endpoint == "/v1/audio/transcriptions" || endpoint == "/v1/audio/translations" {
		if media != "multipart/form-data" || params["boundary"] == "" {
			return nil, fmt.Errorf("audio upload requires multipart/form-data")
		}
		reader := multipart.NewReader(bytes.NewReader(body), params["boundary"])
		hasFile := false
		modelSeen, streamSeen := false, false
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
				if part.FileName() != "" {
					return nil, fmt.Errorf("model must be a text field")
				}
				value, err := io.ReadAll(io.LimitReader(part, 1025))
				if err != nil || len(value) > 1024 {
					return nil, fmt.Errorf("invalid multipart model")
				}
				if modelSeen {
					return nil, fmt.Errorf("duplicate multipart model")
				}
				modelSeen = true
				req.Model = strings.TrimSpace(string(value))
			case "stream":
				if streamSeen || part.FileName() != "" {
					return nil, fmt.Errorf("stream must be a single text field")
				}
				streamSeen = true
				value, err := io.ReadAll(io.LimitReader(part, 1025))
				if err != nil || len(value) > 1024 {
					return nil, fmt.Errorf("invalid multipart stream")
				}
				switch strings.ToLower(strings.TrimSpace(string(value))) {
				case "true", "1", "yes":
					req.Stream = true
				case "", "false", "0", "no":
				default:
					return nil, fmt.Errorf("stream must be a boolean")
				}
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
		if err := validateNativeRoutingFields(body); err != nil {
			return nil, err
		}
		model := gjson.GetBytes(body, "model")
		if model.Type == gjson.String {
			req.Model = strings.TrimSpace(model.String())
		} else if model.Type != gjson.Null {
			return nil, fmt.Errorf("model must be a string")
		}
		if endpoint == "/v1/moderations" && req.Model == "" {
			req.Model = requestmodel.DefaultModerationModel
		}
		stream := gjson.GetBytes(body, "stream")
		if stream.Exists() && stream.Type != gjson.True && stream.Type != gjson.False {
			return nil, fmt.Errorf("stream must be a boolean")
		}
		req.Stream = stream.Bool()
		if req.Stream && endpoint != "/v1/completions" {
			return nil, fmt.Errorf("stream is only supported by the completions endpoint")
		}
		if endpoint == "/v1/audio/speech" {
			format := gjson.GetBytes(body, "stream_format")
			if format.Exists() && format.Type != gjson.String {
				return nil, fmt.Errorf("stream_format must be a string")
			}
			switch strings.ToLower(strings.TrimSpace(format.String())) {
			case "":
			case "audio":
				req.Stream, req.BinaryAudio = true, true
			case "sse":
				req.Stream = true
			default:
				return nil, fmt.Errorf("stream_format must be audio or sse")
			}
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

func validateNativeRoutingFields(body []byte) error {
	seen := make(map[string]bool, 4)
	var err error
	gjson.ParseBytes(body).ForEach(func(key, value gjson.Result) bool {
		for _, name := range []string{"model", "stream", "stream_options", "stream_format"} {
			if !strings.EqualFold(key.String(), name) {
				continue
			}
			if key.String() != name || seen[name] {
				err = fmt.Errorf("ambiguous %s field: use one lowercase field", name)
				return false
			}
			seen[name] = true
			if name == "stream_options" && value.IsObject() {
				seenUsage := false
				value.ForEach(func(option, _ gjson.Result) bool {
					if !strings.EqualFold(option.String(), "include_usage") {
						return true
					}
					if option.String() != "include_usage" || seenUsage {
						err = fmt.Errorf("ambiguous include_usage field: use one lowercase field")
						return false
					}
					seenUsage = true
					return true
				})
				if err != nil {
					return false
				}
			}
			break
		}
		return true
	})
	return err
}

func (req *NativeProtocolRequest) withModel(model string) ([]byte, string, error) {
	if !strings.HasPrefix(req.ContentType, "multipart/") {
		body, err := sjson.SetBytes(bytes.Clone(req.Body), "model", model)
		if err == nil && req.Stream && req.Endpoint == "/v1/completions" {
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
	if pricing.Mode == BillingModeToken && pricing.channelPricing != nil {
		if _, err := s.billingService.GetModelPricing(model); err != nil {
			needsOutput := req.Endpoint == "/v1/completions"
			if pricing.channelPricing.InputPrice == nil || (needsOutput && pricing.channelPricing.OutputPrice == nil) {
				return "", nil, fmt.Errorf("explicit prices for every metered token direction are required")
			}
		}
	}

	return model, pricing, nil
}

// nativeProtocolUsage recognizes token usage without treating total_tokens as
// prompt_tokens when a provider supplied completion tokens as well.
func nativeProtocolUsage(endpoint string, body []byte) (OpenAIUsage, bool) {
	value := gjson.GetBytes(body, "usage")
	if endpoint == "/v1/decisions" || endpoint == "/v1/rerank" {
		fields := []string{"input_tokens"}
		if endpoint == "/v1/rerank" {
			fields = append(fields, "prompt_tokens", "total_tokens")
		}
		for _, field := range fields {
			v := value.Get(field)
			if !v.Exists() {
				continue
			}
			if v.Type != gjson.Number || v.Float() < 0 || v.Float() != float64(v.Int()) {
				return OpenAIUsage{}, false
			}
			return OpenAIUsage{InputTokens: int(v.Int())}, true
		}
		return OpenAIUsage{}, false
	}

	if endpoint == "/v1/completions" && (!(value.Get("prompt_tokens").Exists() || value.Get("input_tokens").Exists()) || !(value.Get("completion_tokens").Exists() || value.Get("output_tokens").Exists())) {
		return OpenAIUsage{}, false
	}

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
