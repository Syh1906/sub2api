//go:build unit

package service

import (
	"bytes"
	"context"
	"mime/multipart"
	"net/http"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestStandardVideoTextFormAndJSONAgree(t *testing.T) {
	var body bytes.Buffer
	form := multipart.NewWriter(&body)
	for name, value := range map[string]string{"model": "public-model", "prompt": "waves", "seconds": "4", "size": "1280x720"} {
		require.NoError(t, form.WriteField(name, value))
	}
	require.NoError(t, form.Close())
	data, info, err := NormalizeStandardVideoCreate(form.FormDataContentType(), body.Bytes())
	require.NoError(t, err)
	expected := []byte(`{"prompt":"waves","model":"public-model","seconds":"4","size":"1280x720"}`)
	require.JSONEq(t, string(expected), string(data))
	_, jsonInfo, err := NormalizeStandardVideoCreate("application/json", expected)
	require.NoError(t, err)
	require.Equal(t, jsonInfo, info)
	require.Equal(t, "waves", info.Prompt)
	for _, bad := range []string{`{}`, `{"model":"a","prompt":"x","prompt":"y"}`, `{"model":"a","prompt":"x","seconds":"? "}`, `{"model":"a","prompt":"x","input_reference":"asset"}`} {
		_, _, err := NormalizeStandardVideoCreate("application/json", []byte(bad))
		require.Error(t, err)
	}
}

func TestStandardVideoCapabilityAndBillingID(t *testing.T) {
	a := seedanceTestAccount()
	require.False(t, a.SupportsOpenAIEndpointCapability(OpenAIEndpointCapabilityStandardVideos))
	a.Credentials["openai_capabilities"] = []string{"standard_videos"}
	require.True(t, a.SupportsOpenAIEndpointCapability(OpenAIEndpointCapabilityStandardVideos))
	a.Type = AccountTypeOAuth
	require.False(t, a.SupportsOpenAIEndpointCapability(OpenAIEndpointCapabilityStandardVideos))
	require.Equal(t, "standard-video:task", resolveUsageBillingRequestID(context.Background(), "standard-video:task"))
}

func TestStandardVideoDeletionRequiresTerminalTask(t *testing.T) {
	for _, status := range []string{"queued", "in_progress"} {
		c, w := grokMediaContentTestContext(http.MethodDelete, "/v1/videos/task", nil)
		s := &OpenAIGatewayService{}
		require.NoError(t, s.finishStandardVideo(context.Background(), c, StandardVideoDelete, "", status, nil, nil, ""))
		require.Equal(t, http.StatusConflict, w.Code)
	}
}

func TestStandardVideoSettlementRequiresMeasuredOutput(t *testing.T) {
	s := &OpenAIGatewayService{}
	for _, data := range []string{`{}`, `{"usage":{}}`, `{"usage":{"completion_tokens":-1}}`, `{"usage":{"completion_tokens":1.2}}`} {
		require.Error(t, s.settleStandardVideo(context.Background(), "task", nil, []byte(data), OpenAIRecordUsageInput{}))
	}
}
