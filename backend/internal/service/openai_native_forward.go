package service

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/logger"
	"github.com/gin-gonic/gin"
	"github.com/tidwall/gjson"
	"go.uber.org/zap"
)

func (s *OpenAIGatewayService) ForwardNativeProtocol(
	ctx context.Context,
	c *gin.Context,
	account *Account,
	request *NativeProtocolRequest,
	apiKey *APIKey,
	mapping ChannelMappingResult,
) (*OpenAIForwardResult, error) {
	startTime := time.Now()
	if request == nil || account == nil || NativeProtocolCapability(request.Endpoint) == "" || !account.SupportsOpenAIEndpointCapability(NativeProtocolCapability(request.Endpoint)) {
		return nil, fmt.Errorf("account does not support native endpoint")
	}
	originalModel := request.Model
	forwardModel := mapping.MappedModel
	if forwardModel == "" {
		forwardModel = originalModel
	}
	mappedModel := resolveOpenAIForwardModel(account, forwardModel, "")
	upstreamModel := normalizeOpenAIModelForUpstream(account, mappedModel)
	billingModel, pricing, err := s.nativeProtocolPricing(ctx, apiKey, request, mapping, mappedModel)
	if err != nil {
		nativeProtocolError(c, http.StatusBadRequest, err.Error())
		return nil, err
	}
	upstreamBody, contentType, err := request.withModel(upstreamModel)
	if err != nil {
		return nil, err
	}
	SetOpsUpstreamModel(c, upstreamModel)

	logger.L().Debug("openai native protocol: forwarding",
		zap.Int64("account_id", account.ID),
		zap.String("original_model", originalModel),
		zap.String("billing_model", billingModel),
		zap.String("upstream_model", upstreamModel),
	)

	upstreamKey := strings.TrimSpace(account.GetOpenAIProtocolAPIKey())
	if upstreamKey == "" {
		return nil, fmt.Errorf("account %d missing api_key", account.ID)
	}
	// 协议感知：Anthropic 协议账号的凭证 base_url 指向 /anthropic 端点，
	// 原生 OpenAI 端点使用 OpenAI 格式 base。
	baseURL := account.GetOpenAIFormatBaseURL()
	if baseURL == "" {
		baseURL = "https://api.openai.com"
	}
	validatedURL, err := s.validateUpstreamBaseURL(baseURL)
	if err != nil {
		return nil, fmt.Errorf("invalid base_url: %w", err)
	}
	targetURL := buildOpenAIEndpointURL(validatedURL, request.Endpoint)

	upstreamCtx, releaseUpstreamCtx := detachUpstreamContext(ctx)
	upstreamReq, err := http.NewRequestWithContext(upstreamCtx, http.MethodPost, targetURL, bytes.NewReader(upstreamBody))
	releaseUpstreamCtx()
	if err != nil {
		return nil, fmt.Errorf("build upstream request: %w", err)
	}
	upstreamReq = upstreamReq.WithContext(WithHTTPUpstreamProfile(upstreamReq.Context(), HTTPUpstreamProfileOpenAI))
	upstreamReq.Header.Set("Content-Type", contentType)
	upstreamReq.Header.Set("Authorization", "Bearer "+upstreamKey)
	upstreamReq.Header.Set("Accept", "*/*")
	if request.Stream {
		upstreamReq.Header.Set("Accept", "text/event-stream")
	}
	for key, values := range c.Request.Header {
		lowerKey := strings.ToLower(key)
		if openaiCCRawAllowedHeaders[lowerKey] {
			for _, v := range values {
				upstreamReq.Header.Add(key, v)
			}
		}
	}
	if customUA := account.GetOpenAIUserAgent(); customUA != "" {
		upstreamReq.Header.Set("user-agent", customUA)
	}

	// 账号级请求头覆写（仅 openai api_key 账号启用时生效）
	account.ApplyHeaderOverrides(upstreamReq.Header)

	proxyURL := ""
	if account.ProxyID != nil && account.Proxy != nil {
		proxyURL = account.Proxy.URL()
	}
	resp, err := s.doOpenAIUpstream(upstreamReq, proxyURL, account)
	if err != nil {
		safeErr := sanitizeUpstreamErrorMessage(err.Error())
		setOpsUpstreamError(c, 0, safeErr, "")
		appendOpsUpstreamError(c, OpsUpstreamErrorEvent{
			ProxyID:            opsUpstreamProxyID(account),
			ProxyName:          opsUpstreamProxyName(account),
			Platform:           account.Platform,
			AccountID:          account.ID,
			AccountName:        account.Name,
			UpstreamStatusCode: 0,
			Kind:               "request_error",
			Message:            safeErr,
		})
		writeOpenAIEmbeddingsError(c, http.StatusBadGateway, "upstream_error", "Upstream request failed")
		return nil, fmt.Errorf("upstream request failed: %s", safeErr)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode >= 400 {
		respBody := s.readUpstreamErrorBody(resp)
		_ = resp.Body.Close()
		resp.Body = io.NopCloser(bytes.NewReader(respBody))

		upstreamMsg := strings.TrimSpace(extractUpstreamErrorMessage(respBody))
		upstreamMsg = sanitizeUpstreamErrorMessage(upstreamMsg)
		if s.shouldFailoverOpenAIUpstreamResponse(account, resp.StatusCode, upstreamMsg, respBody) {
			upstreamDetail := ""
			if s.cfg != nil && s.cfg.Gateway.LogUpstreamErrorBody {
				maxBytes := s.cfg.Gateway.LogUpstreamErrorBodyMaxBytes
				if maxBytes <= 0 {
					maxBytes = 2048
				}
				upstreamDetail = truncateString(string(respBody), maxBytes)
			}
			appendOpsUpstreamError(c, OpsUpstreamErrorEvent{
				ProxyID:            opsUpstreamProxyID(account),
				ProxyName:          opsUpstreamProxyName(account),
				Platform:           account.Platform,
				AccountID:          account.ID,
				AccountName:        account.Name,
				UpstreamStatusCode: resp.StatusCode,
				UpstreamRequestID:  resp.Header.Get("x-request-id"),
				Kind:               "failover",
				Message:            upstreamMsg,
				Detail:             upstreamDetail,
			})
			shouldDisable := s.handleOpenAIAccountUpstreamError(ctx, account, resp.StatusCode, resp.Header, respBody, upstreamModel)
			retryableOnSameAccount := !shouldDisable && account.IsPoolMode() && account.IsPoolModeRetryableStatus(resp.StatusCode)
			if isOpenAIHTTPUpstreamAccessStateError(resp.StatusCode, upstreamMsg, respBody) {
				return nil, newOpenAIUpstreamFailoverError(resp.StatusCode, resp.Header, respBody, upstreamMsg, retryableOnSameAccount)
			}
			return nil, &UpstreamFailoverError{StatusCode: resp.StatusCode, ResponseBody: respBody, RetryableOnSameAccount: retryableOnSameAccount}
		}
		writeOpenAIEmbeddingsUpstreamResponse(c, resp, respBody, s.responseHeaderFilter)
		return nil, fmt.Errorf("upstream returned status %d", resp.StatusCode)
	}

	result := &OpenAIForwardResult{RequestID: newNativeBillingRequestID(), UpstreamHeaders: resp.Header, Model: originalModel, BillingModel: billingModel, UpstreamModel: upstreamModel, Stream: request.Stream, NativePricing: pricing, UpstreamEndpoint: request.Endpoint}
	if request.Stream {
		return s.streamNativeCompletions(c, resp, result, startTime)
	}
	respBody, err := ReadUpstreamResponseBody(resp.Body, s.cfg, c, openAITooLargeError)
	if err != nil {
		return nil, fmt.Errorf("read upstream body: %w", err)
	}
	usage, hasUsage := nativeProtocolUsage(respBody)
	if pricing.Mode == BillingModeToken && !hasUsage {
		writeOpenAIEmbeddingsError(c, http.StatusBadGateway, "upstream_error", "Upstream response is missing billable usage")
		return nil, fmt.Errorf("missing native token usage")
	}
	result.Usage = usage
	result.Duration = time.Since(startTime)
	writeOpenAIEmbeddingsUpstreamResponse(c, resp, respBody, s.responseHeaderFilter)
	return result, nil
}

func (s *OpenAIGatewayService) streamNativeCompletions(c *gin.Context, resp *http.Response, result *OpenAIForwardResult, start time.Time) (*OpenAIForwardResult, error) {
	if !strings.Contains(strings.ToLower(resp.Header.Get("Content-Type")), "text/event-stream") {
		return nil, fmt.Errorf("upstream did not return an event stream")
	}
	scanner := s.newUpstreamSSEScanner(resp.Body)
	writeHeaders := s.newStreamHeaderWriter(c, resp.Header)
	writeHeaders()
	hasUsage, done, disconnected := false, false, false
	var upstreamErr error
	for scanner.Scan() {
		line := scanner.Text()
		if payload, ok := extractOpenAISSEDataLine(line); ok {
			if strings.TrimSpace(payload) == "[DONE]" {
				done = true
				continue
			}
			if usage, ok := nativeProtocolUsage([]byte(payload)); ok {
				result.Usage = usage
				hasUsage = true
			}
			if gjson.Get(payload, "error").Exists() {
				upstreamErr = fmt.Errorf("upstream stream reported an error")
			}
			if result.FirstTokenMs == nil {
				elapsed := int(time.Since(start).Milliseconds())
				result.FirstTokenMs = &elapsed
			}
		}
		if !disconnected {
			if _, err := c.Writer.WriteString(line + "\n"); err != nil {
				disconnected = true
			}
			if line == "" {
				c.Writer.Flush()
			}
		}
	}
	result.Duration = time.Since(start)
	failure := upstreamErr
	if failure == nil {
		failure = scanner.Err()
	}
	if failure == nil && !done {
		failure = ErrOpenAIUpstreamStreamTruncated
	}
	if failure == nil && result.NativePricing.Mode == BillingModeToken && !hasUsage {
		failure = fmt.Errorf("upstream stream is missing billable usage")
	}
	if !disconnected {
		if failure != nil {
			writeNativeStreamError(c, failure)
		} else {
			_, _ = c.Writer.WriteString("data: [DONE]\n\n")
			c.Writer.Flush()
		}
	}
	// Meter observed usage even when the client disconnects or the stream truncates.
	if failure != nil && !hasUsage {
		return nil, failure
	}
	return result, failure
}
