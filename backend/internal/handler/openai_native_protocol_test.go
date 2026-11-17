package handler

import (
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestNativeProtocolRequiresCustomerIdentity(t *testing.T) {
	gin.SetMode(gin.TestMode)
	router := gin.New()
	handler := &OpenAIGatewayHandler{}
	router.POST("/v1/completions", handler.NativeProtocol("/v1/completions"))
	request := httptest.NewRequest(http.MethodPost, "/v1/completions", strings.NewReader(`{"model":"m","prompt":"hello"}`))
	result := httptest.NewRecorder()
	router.ServeHTTP(result, request)
	require.Equal(t, 401, result.Code)
	require.Contains(t, result.Body.String(), "Invalid API key")
}
