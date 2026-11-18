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
		{"/v1/decisions", `{"model":"alias","input":"test","questions":[{"type":"choice","name":"route","instructions":"Choose a route","choices":[{"value":"yes"},{"value":"no"}]}]}`, `{"answers":[{"type":"choice","name":"route","choice":"yes"}],"usage":{"input_tokens":4}}`, "application/json"},
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
		req, err := ParseNativeProtocolRequest(endpoint, strings.Replace(writer.FormDataContentType(), "multipart/form-data", "Multipart/Form-Data", 1), body.Bytes())
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
		_, ok := nativeProtocolUsage("/v1/rerank", []byte(body))
		require.False(t, ok)
	}
	usage, ok := nativeProtocolUsage("/v1/rerank", []byte(`{"usage":{"total_tokens":7,"completion_tokens":2}}`))
	require.True(t, ok)
	require.Equal(t, 7, usage.InputTokens)
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

func TestNativeProtocolModerationDefaultModel(t *testing.T) {
	for _, body := range []string{`{"input":"hello"}`, `{"model":null,"input":"hello"}`, `{"model":" ","input":"hello"}`} {
		req, err := ParseNativeProtocolRequest("/v1/moderations", "application/json", []byte(body))
		require.NoError(t, err)
		require.Equal(t, "omni-moderation-latest", req.Model)
	}
}

func TestNativeProtocolRejectsAmbiguousModel(t *testing.T) {
	for _, body := range []string{
		`{"model":"cheap","model":"expensive","prompt":"hello"}`,
		`{"model":"cheap","Model":"expensive","prompt":"hello"}`,
		`{"model":"cheap","model":null,"prompt":"hello"}`,
		`{"model":"m","prompt":"hello","stream":false,"stream":true}`,
		`{"model":"m","prompt":"hello","stream":false,"Stream":true}`,
		`{"model":"m","prompt":"hello","stream":false,"ſtream":true}`,
		`{"model":"m","prompt":"hello","stream_options":{},"stream_options":{"include_usage":false}}`,
		`{"model":"m","prompt":"hello","stream":true,"stream_options":{"include_usage":true,"include_usage":false}}`,
		`{"model":"m","prompt":"hello","stream":true,"stream_options":{"include_usage":true,"Include_Usage":false}}`,
	} {
		_, err := ParseNativeProtocolRequest("/v1/completions", "application/json", []byte(body))
		require.Error(t, err, "ambiguous routing model must fail before forwarding: %s", body)
	}
	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	require.NoError(t, writer.WriteField("model", ""))
	require.NoError(t, writer.WriteField("model", "second-model"))
	file, err := writer.CreateFormFile("file", "sample.wav")
	require.NoError(t, err)
	_, err = file.Write([]byte("audio"))
	require.NoError(t, err)
	require.NoError(t, writer.Close())
	_, err = ParseNativeProtocolRequest("/v1/audio/transcriptions", writer.FormDataContentType(), body.Bytes())
	require.Error(t, err, "blank first model cannot hide a duplicate routing field")
}

func TestNativeProtocolAudioStreamControl(t *testing.T) {
	speech, err := ParseNativeProtocolRequest("/v1/audio/speech", "application/json", []byte(`{"model":"m","input":"hello","stream_format":"sse"}`))
	require.NoError(t, err)
	require.True(t, speech.Stream)
	require.False(t, speech.BinaryAudio)
	mapped, _, err := speech.withModel("provider-voice")
	require.NoError(t, err)
	require.JSONEq(t, `{"model":"provider-voice","input":"hello","stream_format":"sse"}`, string(mapped))
	_, err = ParseNativeProtocolRequest("/v1/audio/speech", "application/json", []byte(`{"model":"m","input":"hello","stream_format":"invalid"}`))
	require.Error(t, err)
	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	require.NoError(t, writer.WriteField("model", "m"))
	require.NoError(t, writer.WriteField("stream", "true"))
	file, err := writer.CreateFormFile("file", "sample.wav")
	require.NoError(t, err)
	_, err = file.Write([]byte("audio"))
	require.NoError(t, err)
	require.NoError(t, writer.Close())
	upload, err := ParseNativeProtocolRequest("/v1/audio/transcriptions", writer.FormDataContentType(), body.Bytes())
	require.NoError(t, err)
	require.True(t, upload.Stream)
	require.False(t, upload.BinaryAudio)
}

func TestNativeProtocolCompletionPartialUsageIsUnknown(t *testing.T) {
	for _, usage := range []string{`{"prompt_tokens":3}`, `{"completion_tokens":2}`, `{"total_tokens":5}`} {
		svc, account, key, req, c, rec, _ := nativeFixture(t, "/v1/completions", `{"model":"alias","prompt":"hello"}`, "application/json", `{"choices":[{"text":"hello","index":0}],"usage":`+usage+`}`, false)
		input, output := 2e-6, 4e-6
		key.Group.ModelPricing[0] = ChannelModelPricing{Models: []string{"alias"}, BillingMode: BillingModeToken, InputPrice: &input, OutputPrice: &output}
		result, err := svc.ForwardNativeProtocol(t.Context(), c, account, req, key, ChannelMappingResult{MappedModel: "alias"})
		require.Error(t, err, "partial completion usage cannot become a zero-priced output")
		require.Nil(t, result)
		require.Equal(t, 502, rec.Code)
	}
}

func TestNativeProtocolAudioStreamTerminal(t *testing.T) {
	const request = `{"model":"alias","input":"test","stream_format":"sse"}`
	for _, tc := range []struct {
		name, wire string
		success    bool
	}{
		{"complete", "event: speech.audio.delta\ndata: {\"type\":\"speech.audio.delta\",\"audio\":\"YQ==\"}\n\nevent: speech.audio.done\ndata: {}\n\n", true},
		{"partial terminal", "data: {\"type\":\"speech.audio.done\"}\n", false},
		{"wrong terminal", "data: {\"type\":\"transcript.text.done\"}\n\n", false},
		{"error", "event: error\ndata: {\"type\":\"error\"}\n\n", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			svc, account, key, req, c, rec, _ := nativeFixture(t, "/v1/audio/speech", request, "text/event-stream", tc.wire, true)
			result, err := svc.ForwardNativeProtocol(t.Context(), c, account, req, key, ChannelMappingResult{})
			if !tc.success {
				require.Error(t, err)
				require.Nil(t, result)
			} else {
				require.NoError(t, err)
				require.Equal(t, BillingModePerRequest, result.NativePricing.Mode)
				require.Equal(t, tc.wire, rec.Body.String())
			}
			require.NotContains(t, rec.Body.String(), "[DONE]")
		})
	}
}

func TestNativeProtocolAudioRejectsPartialSuccessStatus(t *testing.T) {
	for _, status := range []int{http.StatusPartialContent, http.StatusFound} {
		svc, account, key, req, c, _, transport := nativeFixture(t, "/v1/audio/speech", `{"model":"alias","input":"hello","stream_format":"audio"}`, "audio/mpeg", "abc", true)
		transport.resp.StatusCode = status
		result, err := svc.ForwardNativeProtocol(t.Context(), c, account, req, key, ChannelMappingResult{})
		require.Error(t, err)
		require.Nil(t, result)
	}
}

func TestNativeProtocolAudioReadIdle(t *testing.T) {
	svc, account, key, req, c, rec, transport := nativeFixture(t, "/v1/audio/speech", `{"model":"alias","input":"hi","stream_format":"sse"}`, "text/event-stream", "", true)
	reader, writer := io.Pipe()
	t.Cleanup(func() { require.NoError(t, writer.Close()) })
	transport.resp.Body = reader
	svc.cfg.Gateway.StreamDataIntervalTimeout = 1
	delta := "data: {\"type\":\"speech.audio.delta\",\"audio\":\"YQ==\"}\n\n"
	go func() { _, _ = io.WriteString(writer, delta) }()
	result, err := svc.ForwardNativeProtocol(t.Context(), c, account, req, key, ChannelMappingResult{})
	require.Error(t, err)
	require.Nil(t, result)
	require.Contains(t, rec.Body.String(), delta)
	require.Contains(t, rec.Body.String(), "event: error")
	require.NotContains(t, rec.Body.String(), "speech.audio.done")
}

func TestNativeProtocolCustomPriceRequiresMeteredDirections(t *testing.T) {
	svc, _, key, req, _, _, _ := nativeFixture(t, "/v1/completions", `{"model":"alias","prompt":"hello"}`, "application/json", `{}`, false)
	price := 0.0
	for _, card := range []ChannelModelPricing{
		{Models: []string{"alias"}, BillingMode: BillingModeToken, InputPrice: &price},
		{Models: []string{"alias"}, BillingMode: BillingModeToken, OutputPrice: &price},
	} {
		key.Group.ModelPricing[0] = card
		_, _, err := svc.nativeProtocolPricing(t.Context(), key, req, ChannelMappingResult{MappedModel: "alias"}, "native-model")
		require.Error(t, err, "a missing price is not explicit free pricing")
	}
	key.Group.ModelPricing[0] = ChannelModelPricing{Models: []string{"alias"}, BillingMode: BillingModeToken, InputPrice: &price, OutputPrice: &price}
	_, _, err := svc.nativeProtocolPricing(t.Context(), key, req, ChannelMappingResult{MappedModel: "alias"}, "native-model")
	require.NoError(t, err)
	key.Group.ModelPricing[0].OutputPrice = nil
	for _, endpoint := range []string{"/v1/rerank", "/v1/decisions"} {
		req.Endpoint = endpoint
		_, _, err := svc.nativeProtocolPricing(t.Context(), key, req, ChannelMappingResult{MappedModel: "alias"}, "native-model")
		require.NoError(t, err)
	}
}

func TestNativeProtocolInputOnlyUsageIgnoresOutputMetadata(t *testing.T) {
	for _, endpoint := range []string{"/v1/rerank", "/v1/decisions"} {
		usage, ok := nativeProtocolUsage(endpoint, []byte(`{"usage":{"input_tokens":120,"output_tokens":999,"total_tokens":1119}}`))
		require.True(t, ok)
		require.Equal(t, 120, usage.InputTokens)
		require.Zero(t, usage.OutputTokens, "this endpoint only meters input tokens")
	}
	_, ok := nativeProtocolUsage("/v1/decisions", []byte(`{"usage":{"output_tokens":999}}`))
	require.False(t, ok)
}
