package service

import (
	"bytes"
	"context"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/pkg/ctxkey"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

func nativeFixture(t *testing.T, endpoint, body, responseType, response string, stream bool) (*OpenAIGatewayService, *Account, *APIKey, *NativeProtocolRequest, *gin.Context, *httptest.ResponseRecorder, *httpUpstreamRecorder) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	request, err := ParseNativeProtocolRequest(endpoint, "application/json", []byte(body))
	require.NoError(t, err)
	request.Stream = stream
	price := 0.25
	group := &Group{ID: 1, ModelPricing: []ChannelModelPricing{{Models: []string{"alias"}, BillingMode: BillingModePerRequest, PerRequestPrice: &price}}}
	key := &APIKey{GroupID: &group.ID, Group: group}
	upstream := &httpUpstreamRecorder{resp: &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {responseType}}, Body: io.NopCloser(strings.NewReader(response))}}
	billing := NewBillingService(nil, nil)
	svc := &OpenAIGatewayService{cfg: &config.Config{}, httpUpstream: upstream, billingService: billing, resolver: NewModelPricingResolver(nil, billing)}
	account := &Account{ID: 42, Platform: PlatformOpenAI, Type: AccountTypeAPIKey, Credentials: map[string]any{"api_key": "synthetic", "base_url": "https://provider.example/v1", "model_mapping": map[string]any{"alias": "native-model"}}}
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, endpoint, strings.NewReader(body))
	c.Request.Header.Set("Content-Type", "application/json")
	return svc, account, key, request, c, rec, upstream
}

func TestNativeProtocolForwardAndPrice(t *testing.T) {
	for _, tc := range []struct{ endpoint, body, response, content string }{
		{"/v1/completions", `{"model":"alias","prompt":["Hello","World"],"suffix":"!","n":2}`, `{"choices":[{"text":"Hello!","index":0}],"usage":{"prompt_tokens":4,"completion_tokens":2,"total_tokens":6}}`, "application/json"},
		{"/v1/rerank", `{"model":"alias","query":"hello","documents":["a","b"]}`, `{"results":[{"index":0,"relevance_score":0.9}],"usage":{"total_tokens":6}}`, "application/json"},
		{"/v1/decisions", `{"model":"alias","input":"test","choices":["a","b"]}`, `{"decision":"a","usage":{"input_tokens":4,"output_tokens":2}}`, "application/json"},
		{"/v1/moderations", `{"model":"alias","input":"hello"}`, `{"results":[{"flagged":false}]}`, "application/json"},
		{"/v1/audio/speech", `{"model":"alias","input":"hello","voice":"alloy","response_format":"mp3"}`, "ID3synthetic-audio", "audio/mpeg"},
	} {
		t.Run(tc.endpoint, func(t *testing.T) {
			svc, account, key, req, c, rec, up := nativeFixture(t, tc.endpoint, tc.body, tc.content, tc.response, false)
			result, err := svc.ForwardNativeProtocol(t.Context(), c, account, req, key, ChannelMappingResult{MappedModel: "alias"})
			require.NoError(t, err)
			require.Equal(t, tc.response, rec.Body.String())
			require.Equal(t, tc.content, rec.Header().Get("Content-Type"))
			require.Equal(t, "https://provider.example"+tc.endpoint, up.lastReq.URL.String())
			require.Equal(t, "native-model", gjson.GetBytes(up.lastBody, "model").String())
			require.Equal(t, "Bearer synthetic", up.lastReq.Header.Get("Authorization"))
			require.True(t, strings.HasPrefix(result.RequestID, "native_protocol:"))
			cost, err := svc.calculateOpenAIRecordUsageCost(t.Context(), result, key, []string{"alias"}, 1, 1, 1, 1, UsageTokens{}, "", nil, false, time.Now())
			require.NoError(t, err)
			require.InDelta(t, 0.25, cost.ActualCost, 0.000001)
		})
	}
}

func TestNativeProtocolAudioMultipartPreservesFile(t *testing.T) {
	for _, endpoint := range []string{"/v1/audio/transcriptions", "/v1/audio/translations"} {
		var body bytes.Buffer
		writer := multipart.NewWriter(&body)
		require.NoError(t, writer.WriteField("model", "alias"))
		require.NoError(t, writer.WriteField("response_format", "text"))
		file, err := writer.CreateFormFile("file", "sample.wav")
		require.NoError(t, err)
		audio := []byte{0, 1, 2, 255, 13, 10}
		_, err = file.Write(audio)
		require.NoError(t, err)
		require.NoError(t, writer.Close())
		req, err := ParseNativeProtocolRequest(endpoint, writer.FormDataContentType(), body.Bytes())
		require.NoError(t, err)
		mapped, ct, err := req.withModel("native-model")
		require.NoError(t, err)
		_, params, err := mime.ParseMediaType(ct)
		require.NoError(t, err)
		reader := multipart.NewReader(bytes.NewReader(mapped), params["boundary"])
		fields := map[string][]byte{}
		for {
			part, err := reader.NextPart()
			if err == io.EOF {
				break
			}
			require.NoError(t, err)
			data, err := io.ReadAll(part)
			require.NoError(t, err)
			fields[part.FormName()] = data
		}
		require.Equal(t, audio, fields["file"])
		require.Equal(t, "native-model", string(fields["model"]))
		require.Equal(t, "text", string(fields["response_format"]))
		svc, account, key, _, c, rec, up := nativeFixture(t, "/v1/audio/speech", `{"model":"alias","input":"hi"}`, "text/plain", "recognized text", false)
		c.Request = httptest.NewRequest(http.MethodPost, endpoint, bytes.NewReader(body.Bytes()))
		c.Request.Header.Set("Content-Type", writer.FormDataContentType())
		result, err := svc.ForwardNativeProtocol(t.Context(), c, account, req, key, ChannelMappingResult{MappedModel: "alias"})
		require.NoError(t, err)
		require.NotNil(t, result)
		require.Equal(t, "https://provider.example"+endpoint, up.lastReq.URL.String())
		require.Equal(t, "recognized text", rec.Body.String())
		require.Equal(t, "text/plain", rec.Header().Get("Content-Type"))
		parsed, err := ParseNativeProtocolRequest(endpoint, up.lastReq.Header.Get("Content-Type"), up.lastBody)
		require.NoError(t, err)
		require.Equal(t, "native-model", parsed.Model)
		require.True(t, bytes.Contains(up.lastBody, audio))

	}
}

func TestNativeProtocolCompletionsStream(t *testing.T) {
	response := "data: {\"choices\":[{\"text\":\"hello\",\"index\":0}]}\n\ndata: {\"choices\":[],\"usage\":{\"prompt_tokens\":3,\"completion_tokens\":2}}\n\ndata: [DONE]\n\n"
	svc, account, key, req, c, rec, up := nativeFixture(t, "/v1/completions", `{"model":"alias","prompt":"hello","stream":true}`, "text/event-stream", response, true)
	result, err := svc.ForwardNativeProtocol(t.Context(), c, account, req, key, ChannelMappingResult{MappedModel: "alias"})
	require.NoError(t, err)
	require.Equal(t, 3, result.Usage.InputTokens)
	require.Equal(t, 2, result.Usage.OutputTokens)
	require.Contains(t, rec.Body.String(), `"text":"hello"`)
	require.Equal(t, 1, strings.Count(rec.Body.String(), "[DONE]"))
	require.True(t, gjson.GetBytes(up.lastBody, "stream_options.include_usage").Bool())
}

func TestNativeProtocolFailsWithoutPricingAndUsage(t *testing.T) {
	svc, account, key, req, c, rec, up := nativeFixture(t, "/v1/audio/speech", `{"model":"alias","input":"hi"}`, "audio/mpeg", "audio", false)
	key.Group.ModelPricing[0].BillingMode = BillingModeToken
	_, err := svc.ForwardNativeProtocol(t.Context(), c, account, req, key, ChannelMappingResult{MappedModel: "alias"})
	require.Error(t, err)
	require.Nil(t, up.lastReq)
	require.Equal(t, 400, rec.Code)
	for _, body := range []string{`{}`, `{"usage":{}}`, `{"usage":{"input_tokens":-1}}`, `{"usage":{"input_tokens":1.5}}`} {
		_, ok := nativeProtocolUsage([]byte(body))
		require.False(t, ok)
	}
	usage, ok := nativeProtocolUsage([]byte(`{"usage":{"total_tokens":7,"completion_tokens":2}}`))
	require.True(t, ok)
	require.Equal(t, 5, usage.InputTokens)
}

func TestNativeProtocolCapabilitiesAndValidation(t *testing.T) {
	oauth := &Account{Platform: PlatformOpenAI, Type: AccountTypeOAuth}
	require.False(t, oauth.SupportsOpenAIEndpointCapability(OpenAIEndpointCapabilityCompletions))
	account := &Account{Platform: PlatformOpenAI, Type: AccountTypeAPIKey, Credentials: map[string]any{openAIEndpointCapabilitiesCredentialKey: []string{"chat_completions"}}}
	require.False(t, account.SupportsOpenAIEndpointCapability(OpenAIEndpointCapabilityAudio))
	account.Credentials[openAIEndpointCapabilitiesCredentialKey] = []string{"audio"}
	require.True(t, account.SupportsOpenAIEndpointCapability(OpenAIEndpointCapabilityAudio))
	for _, body := range []string{`{}`, `{"model":"m"}`, `{"model":"m","prompt":"hi","stream":"true"}`} {
		_, err := ParseNativeProtocolRequest("/v1/completions", "application/json", []byte(body))
		require.Error(t, err)
	}
	_, err := ParseNativeProtocolRequest("/arbitrary/proxy", "application/json", []byte(`{"model":"m"}`))
	require.Error(t, err)
	ctx := context.WithValue(t.Context(), ctxkey.ClientRequestID, "same-client-id")
	a := newNativeBillingRequestID()
	b := newNativeBillingRequestID()
	require.NotEqual(t, a, b)
	require.Equal(t, a, resolveUsageBillingRequestID(ctx, a))
	require.Equal(t, b, resolveUsageBillingRequestID(ctx, b))
}

func TestNativeProtocolTokenBillingAndMissingUsage(t *testing.T) {
	for _, hasUsage := range []bool{true, false} {
		response := `{"choices":[{"text":"hello"}]}`
		if hasUsage {
			response = `{"choices":[{"text":"hello"}],"usage":{"prompt_tokens":10,"completion_tokens":2}}`
		}
		svc, account, key, req, c, rec, _ := nativeFixture(t, "/v1/completions", `{"model":"alias","prompt":"hi"}`, "application/json", response, false)
		input, output := 2e-6, 4e-6
		key.Group.ModelPricing[0] = ChannelModelPricing{Models: []string{"alias"}, BillingMode: BillingModeToken, InputPrice: &input, OutputPrice: &output}
		result, err := svc.ForwardNativeProtocol(t.Context(), c, account, req, key, ChannelMappingResult{MappedModel: "alias"})
		if !hasUsage {
			require.Error(t, err)
			require.Nil(t, result)
			require.Equal(t, 502, rec.Code)
			continue
		}
		require.NoError(t, err)
		cost, err := svc.calculateOpenAIRecordUsageCost(t.Context(), result, key, []string{"alias"}, 1, 1, 1, 1, UsageTokens{InputTokens: result.Usage.InputTokens, OutputTokens: result.Usage.OutputTokens}, "", nil, false, time.Now())
		require.NoError(t, err)
		require.InDelta(t, 0.000028, cost.ActualCost, 0.000000001)
	}
}

func TestNativeProtocolTruncatedStreamRetainsObservedUsage(t *testing.T) {
	response := "data: {\"choices\":[{\"text\":\"partial\"}],\"usage\":{\"prompt_tokens\":3,\"completion_tokens\":1}}\n\n"
	svc, account, key, req, c, rec, _ := nativeFixture(t, "/v1/completions", `{"model":"alias","prompt":"hi","stream":true}`, "text/event-stream", response, true)
	result, err := svc.ForwardNativeProtocol(t.Context(), c, account, req, key, ChannelMappingResult{MappedModel: "alias"})
	require.ErrorIs(t, err, ErrOpenAIUpstreamStreamTruncated)
	require.NotNil(t, result)
	require.Equal(t, 3, result.Usage.InputTokens)
	require.NotContains(t, rec.Body.String(), "[DONE]")
	require.Contains(t, rec.Body.String(), "upstream_error")
}

func TestNativeProtocolPricingSources(t *testing.T) {
	svc, _, key, req, _, _, _ := nativeFixture(t, "/v1/completions", `{"model":"alias","prompt":"hello"}`, "application/json", `{}`, false)
	price := 0.5
	key.Group.ModelPricing = append(key.Group.ModelPricing, ChannelModelPricing{Models: []string{"channel-model", "upstream-model"}, BillingMode: BillingModePerRequest, PerRequestPrice: &price})
	for _, tc := range []struct{ source, model string }{{BillingModelSourceRequested, "alias"}, {BillingModelSourceChannelMapped, "channel-model"}, {BillingModelSourceUpstream, "upstream-model"}} {
		model, _, err := svc.nativeProtocolPricing(t.Context(), key, req, ChannelMappingResult{BillingModelSource: tc.source, MappedModel: "channel-model"}, "upstream-model")
		require.NoError(t, err)
		require.Equal(t, tc.model, model)
	}
	_, _, err := svc.nativeProtocolPricing(t.Context(), key, req, ChannelMappingResult{BillingModelSource: BillingModelSourceResponse}, "upstream-model")
	require.Error(t, err)
	key.Group.ModelPricing[0] = ChannelModelPricing{Models: []string{"alias"}, BillingMode: BillingModeToken}
	_, _, err = svc.nativeProtocolPricing(t.Context(), key, req, ChannelMappingResult{MappedModel: "alias"}, "upstream-model")
	require.Error(t, err)
}

func TestNativeProtocolPreservesUpstreamClientError(t *testing.T) {
	body := `{"error":{"type":"invalid_request_error","message":"invalid prompt"}}`
	svc, account, key, req, c, rec, up := nativeFixture(t, "/v1/completions", `{"model":"alias","prompt":"hello"}`, "application/json", body, false)
	up.resp.StatusCode = http.StatusBadRequest
	result, err := svc.ForwardNativeProtocol(t.Context(), c, account, req, key, ChannelMappingResult{MappedModel: "alias"})
	require.Error(t, err)
	require.Nil(t, result)
	require.Equal(t, 400, rec.Code)
	require.Equal(t, body, rec.Body.String())
}
