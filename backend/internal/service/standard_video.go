package service

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

const (
	standardVideoRetention                                          = 7 * 24 * time.Hour
	StandardVideoCreate                    GrokMediaEndpoint        = "standard_video_create"
	StandardVideoStatus                    GrokMediaEndpoint        = "standard_video_status"
	StandardVideoContent                   GrokMediaEndpoint        = "standard_video_content"
	StandardVideoDelete                    GrokMediaEndpoint        = "standard_video_delete"
	OpenAIEndpointCapabilityStandardVideos OpenAIEndpointCapability = "standard_videos"
)

func (e GrokMediaEndpoint) IsStandardVideo() bool {
	switch e {
	case StandardVideoCreate, StandardVideoStatus, StandardVideoContent, StandardVideoDelete:
		return true
	}
	return false
}

// NormalizeStandardVideoCreate supports text-to-video. Forward a single JSON representation
// so permissions, moderation and upstream routing all see the same prompt and model.
func NormalizeStandardVideoCreate(contentType string, body []byte) ([]byte, GrokMediaRequestInfo, error) {
	var info GrokMediaRequestInfo
	media, params, err := mime.ParseMediaType(contentType)
	if err != nil {
		return nil, info, fmt.Errorf("invalid video Content-Type")
	}
	fields := map[string]string{}
	add := func(k, v string) error {
		switch k {
		case "model", "prompt", "seconds", "size":
		default:
			return fmt.Errorf("unsupported text-to-video field %q", k)
		}
		if _, ok := fields[k]; ok {
			return fmt.Errorf("duplicate video field %q", k)
		}
		fields[k] = v
		return nil
	}
	switch media {
	case "application/json":
		if !gjson.ValidBytes(body) || !gjson.ParseBytes(body).IsObject() {
			return nil, info, errors.New("video request must be a JSON object")
		}
		gjson.ParseBytes(body).ForEach(func(k, v gjson.Result) bool {
			if v.Type != gjson.String {
				err = fmt.Errorf("video field %q must be a string", k.String())
				return false
			}
			err = add(k.String(), v.String())
			return err == nil
		})
	case "multipart/form-data":
		if params["boundary"] == "" {
			return nil, info, errors.New("missing form boundary")
		}
		reader := multipart.NewReader(bytes.NewReader(body), params["boundary"])
		for {
			p, e := reader.NextPart()
			if e == io.EOF {
				break
			}
			if e != nil {
				return nil, info, e
			}
			if p.FileName() != "" {
				return nil, info, errors.New("this account supports text-to-video only")
			}
			value, e := io.ReadAll(p)
			if e != nil {
				return nil, info, e
			}
			if err = add(p.FormName(), string(value)); err != nil {
				break
			}
		}
	default:
		return nil, info, errors.New("video create requires JSON or multipart form data")
	}
	if err != nil {
		return nil, info, err
	}
	info.Model = strings.TrimSpace(fields["model"])
	info.Prompt = strings.TrimSpace(fields["prompt"])
	if info.Model == "" || info.Prompt == "" {
		return nil, info, errors.New("model and prompt are required")
	}
	if seconds, supplied := fields["seconds"]; supplied {
		n, e := strconv.Atoi(seconds)
		if e != nil || n < 1 {
			return nil, info, errors.New("seconds must be a positive integer string")
		}
	}
	fields["model"] = info.Model
	encoded, err := json.Marshal(fields)
	return encoded, info, err
}

// The public task handle is scoped to the creating group, user and key. It never
// selects a new account for a lookup, even when the account's base URL is edited.
type StandardVideoTask struct {
	AccountID     int64
	BaseURL       string
	UpstreamID    string
	Model         string
	BillingModel  string
	UpstreamModel string
	OutputPrice   float64
	CreatedAt     time.Time
}

// StandardVideoReceiptLookup checks for the committed charge for this key and task.
type StandardVideoReceiptLookup func(ctx context.Context, keyID int64, taskID string) (bool, error)

func StandardVideoBillingID(id string) string { return "standard-video:" + id }

func standardVideoKey(key *APIKey, id string) string {
	return fmt.Sprintf("standard-video:%d:%d:%d:%s", derefGroupID(key.GroupID), key.UserID, key.ID, id)
}
func (s *OpenAIGatewayService) LoadStandardVideoTask(ctx context.Context, key *APIKey, id string) (*StandardVideoTask, error) {
	if _, err := uuid.Parse(id); err != nil {
		return nil, nil
	}
	if s.cache == nil {
		return nil, errors.New("video task storage unavailable")
	}
	data, err := s.cache.GetGrokVideoPendingBilling(ctx, standardVideoKey(key, id))
	if err != nil || len(data) == 0 || string(data) == "null" {
		return nil, err
	}
	var task StandardVideoTask
	if err = json.Unmarshal(data, &task); err != nil {
		return nil, err
	}
	return &task, nil
}

func (s *OpenAIGatewayService) standardVideoHTTP(ctx context.Context, account *Account, method, target string, body []byte) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, method, target, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	token := strings.TrimSpace(account.GetOpenAIProtocolAPIKey())
	if token == "" {
		return nil, errors.New("video account missing API key")
	}
	req.Header.Set("Authorization", "Bearer "+token)
	if len(body) > 0 {
		req.Header.Set("Content-Type", "application/json")
	}
	account.ApplyHeaderOverrides(req.Header)
	var proxy string
	if account.ProxyID != nil && account.Proxy != nil {
		proxy = account.Proxy.URL()
	}
	return s.httpUpstream.Do(req, proxy, account.ID, account.Concurrency)
}

func (s *OpenAIGatewayService) newStandardVideoTask(ctx context.Context, c *gin.Context, key *APIKey, account *Account, base string, body []byte, mapping ChannelMappingResult) (*StandardVideoTask, error) {
	if s.cache == nil {
		return nil, errors.New("video task storage unavailable")
	}
	model := gjson.GetBytes(body, "model").String()
	forward := firstNonEmpty(mapping.MappedModel, model)
	upstream := account.GetMappedModel(forward)
	billing := forward
	switch mapping.BillingModelSource {
	case BillingModelSourceRequested:
		billing = model
	case BillingModelSourceUpstream:
		billing = upstream
	case BillingModelSourceResponse:
		nativeProtocolError(c, 400, "video pricing must be known at creation")
		return nil, errors.New("response pricing unsupported")
	}
	if s.resolver == nil {
		return nil, errors.New("video pricing unavailable")
	}
	price := s.resolver.Resolve(ctx, PricingInput{Model: billing, Group: key.Group, GroupID: key.GroupID})
	if price == nil || price.Mode != BillingModeToken || price.channelPricing == nil || price.channelPricing.OutputPrice == nil || len(price.Intervals) > 0 || price.channelPricing.TimePricing != nil {
		nativeProtocolError(c, 400, "standard videos require an explicit flat output token price")
		return nil, errors.New("video output price missing")
	}

	return &StandardVideoTask{
		AccountID:     account.ID,
		BaseURL:       base,
		Model:         model,
		BillingModel:  billing,
		UpstreamModel: upstream,
		OutputPrice:   *price.channelPricing.OutputPrice,
		CreatedAt:     time.Now().UTC(),
	}, nil
}

func (s *OpenAIGatewayService) settleStandardVideo(ctx context.Context, id string, t *StandardVideoTask, data []byte, usage OpenAIRecordUsageInput) error {
	var response struct {
		Usage *struct {
			CompletionTokens *int `json:"completion_tokens"`
		} `json:"usage"`
	}
	if json.Unmarshal(data, &response) != nil || response.Usage == nil || response.Usage.CompletionTokens == nil || *response.Usage.CompletionTokens < 0 {
		return errors.New("completed video lacks valid output token usage")
	}
	usage.atomicReceipt = true
	usage.Result = &OpenAIForwardResult{
		Model:         t.Model,
		UpstreamModel: t.UpstreamModel,
		BillingModel:  t.BillingModel,
		RequestID:     StandardVideoBillingID(id),
		Usage:         OpenAIUsage{OutputTokens: *response.Usage.CompletionTokens},
		Duration:      time.Since(t.CreatedAt),
		NativePricing: &ResolvedPricing{Mode: BillingModeToken, BasePricing: &ModelPricing{OutputPricePerToken: t.OutputPrice}},
	}
	usage.OriginalModel = t.Model
	usage.RequestPayloadHash = HashUsageRequestPayload([]byte(id))
	usage.PricingAt = t.CreatedAt
	return s.RecordUsage(ctx, &usage)
}

func (s *OpenAIGatewayService) finishStandardVideo(ctx context.Context, c *gin.Context, endpoint GrokMediaEndpoint, target, status string, account *Account, key *APIKey, id string) error {
	method := http.MethodDelete
	if endpoint == StandardVideoContent {
		if status != "completed" {
			nativeProtocolError(c, http.StatusConflict, "video content is not ready")
			return nil
		}
		method = http.MethodGet
		target += "/content"
	} else if status == "in_progress" || status == "queued" {
		// A generic DELETE may erase a newly completed job between the status check and cancellation.
		nativeProtocolError(c, http.StatusConflict, "only terminal video tasks can be deleted")
		return nil
	}
	resp, err := s.standardVideoHTTP(ctx, account, method, target, nil)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if endpoint == StandardVideoContent && resp.StatusCode == http.StatusOK {
		return writeGrokMediaContentResponse(c, resp)
	}
	data, err := ReadUpstreamResponseBody(resp.Body, s.cfg, c, openAITooLargeError)
	if err != nil {
		return err
	}
	if endpoint == StandardVideoDelete && resp.StatusCode >= 200 && resp.StatusCode < 300 {
		if err = s.cache.SetGrokVideoPendingBilling(ctx, standardVideoKey(key, id), []byte("null"), standardVideoRetention); err != nil {
			return err
		}
	}
	writeGrokMediaResponse(c, resp, data, s.responseHeaderFilter)
	return nil
}

func (s *OpenAIGatewayService) ForwardStandardVideo(ctx context.Context, c *gin.Context, endpoint GrokMediaEndpoint, id string, task *StandardVideoTask, body []byte, mapping ChannelMappingResult, usage OpenAIRecordUsageInput, alreadySettled bool) error {
	key, account := usage.APIKey, usage.Account
	if !account.SupportsOpenAIEndpointCapability(OpenAIEndpointCapabilityStandardVideos) {
		return errors.New("account does not provide standard video tasks")
	}
	base, err := s.validateUpstreamBaseURL(account.GetCredential("base_url"))
	if err != nil {
		return err
	}
	target := buildOpenAIEndpointURL(base, "/v1/videos")
	method := http.MethodGet
	if endpoint == StandardVideoCreate {
		task, err = s.newStandardVideoTask(ctx, c, key, account, base, body, mapping)
		if err != nil {
			return err
		}
		id = uuid.NewString()
		body, err = sjson.SetBytes(body, "model", task.UpstreamModel)
		if err != nil {
			return err
		}
		method = http.MethodPost
	} else {
		if task == nil || task.AccountID != account.ID || task.BaseURL != base {
			return errors.New("video task account changed")
		}
		target += "/" + task.UpstreamID
	}
	resp, err := s.standardVideoHTTP(ctx, account, method, target, body)
	if err != nil {
		if endpoint == StandardVideoCreate {
			c.JSON(http.StatusBadGateway, gin.H{"error": gin.H{"type": "upstream_error", "code": "video_create_outcome_unknown", "message": "The upstream may have accepted the video. Do not automatically resubmit."}})
		}
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	data, err := ReadUpstreamResponseBody(resp.Body, s.cfg, c, openAITooLargeError)
	if err != nil {
		return err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		writeGrokMediaResponse(c, resp, data, s.responseHeaderFilter)
		return fmt.Errorf("video upstream HTTP %d", resp.StatusCode)
	}
	upstreamID := gjson.GetBytes(data, "id")
	status := gjson.GetBytes(data, "status").String()
	if !gjson.ValidBytes(data) || upstreamID.Type != gjson.String || upstreamID.String() == "" || !isSafeUpstreamPathSegment(upstreamID.String()) {
		return errors.New("invalid upstream video task")
	}
	if endpoint == StandardVideoCreate {
		task.UpstreamID = upstreamID.String()
		snapshot, err := json.Marshal(task)
		if err != nil {
			return err
		}
		saveCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		if err = s.cache.SetGrokVideoPendingBilling(saveCtx, standardVideoKey(key, id), snapshot, standardVideoRetention); err != nil {
			c.JSON(http.StatusServiceUnavailable, gin.H{"error": gin.H{"type": "api_error", "code": "video_task_binding_unavailable", "message": "The upstream accepted the video, but its task handle could not be saved. Do not resubmit."}})
			return err
		}
	} else if upstreamID.String() != task.UpstreamID {
		return errors.New("unexpected upstream video task")
	}
	switch status {
	case "completed":
		if !alreadySettled {
			if err = s.settleStandardVideo(ctx, id, task, data, usage); err != nil {
				return err
			}
		}
	case "queued", "in_progress", "failed", "cancelled", "canceled", "expired":
	default:
		return fmt.Errorf("invalid upstream video status %q", status)
	}
	if endpoint == StandardVideoContent || endpoint == StandardVideoDelete {
		return s.finishStandardVideo(ctx, c, endpoint, target, status, account, key, id)
	}
	data, err = sjson.SetBytes(data, "id", id)
	if err != nil {
		return err
	}
	c.Header("Cache-Control", "no-store")
	c.Data(resp.StatusCode, "application/json", data)
	return nil
}
