package routes

import (
	"bytes"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
)

func TestNativeProtocolRoutesRespectModelAllowlist(t *testing.T) {
	router := newGatewayRoutesTestRouterWithGroup(allowlistGroup(service.PlatformOpenAI, true, "allowed"))
	for _, prefix := range []string{"/v1", ""} {
		for _, path := range []string{"/completions", "/audio/speech", "/moderations", "/rerank", "/decisions"} {
			request := httptest.NewRequest(http.MethodPost, prefix+path, strings.NewReader(`{"model":"blocked","input":"hello","prompt":"hello","query":"hello"}`))
			request.Header.Set("Content-Type", "application/json")
			result := httptest.NewRecorder()
			router.ServeHTTP(result, request)
			require.Equal(t, 404, result.Code)
			require.Contains(t, result.Body.String(), "not available for this group")
		}
		for _, path := range []string{"/audio/transcriptions", "/audio/translations"} {
			var body bytes.Buffer
			writer := multipart.NewWriter(&body)
			require.NoError(t, writer.WriteField("model", "blocked"))
			part, err := writer.CreateFormFile("file", "sample.wav")
			require.NoError(t, err)
			_, err = part.Write([]byte("synthetic"))
			require.NoError(t, err)
			require.NoError(t, writer.Close())
			request := httptest.NewRequest(http.MethodPost, prefix+path, &body)
			request.Header.Set("Content-Type", writer.FormDataContentType())
			result := httptest.NewRecorder()
			router.ServeHTTP(result, request)
			require.Equal(t, 404, result.Code)
			require.Contains(t, result.Body.String(), "not available for this group")
		}
	}
}

func TestNativeProtocolDefaultModerationRespectsAllowlist(t *testing.T) {
	router := newGatewayRoutesTestRouterWithGroup(allowlistGroup(service.PlatformOpenAI, true, "different-model"))
	for _, path := range []string{"/v1/moderations", "/moderations"} {
		for _, body := range []string{`{"input":"hello"}`, `{"model":"","input":"hello"}`, `{"input":"hello","session":{"model":"different-model"}}`} {
			req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
			req.Header.Set("Content-Type", "application/json")
			rec := httptest.NewRecorder()
			router.ServeHTTP(rec, req)
			require.Equal(t, 404, rec.Code, rec.Body.String())
			require.Contains(t, rec.Body.String(), "omni-moderation-latest")
		}
	}
}
