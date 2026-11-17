//go:build unit

package handler

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/pkg/ctxkey"
	"github.com/Wei-Shaw/sub2api/internal/pkg/tlsfingerprint"
	middleware "github.com/Wei-Shaw/sub2api/internal/server/middleware"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

type nativeBillingCache struct{ *handlerInflightCache }

func (cache *nativeBillingCache) DeductUserBalance(_ context.Context, _ int64, amount float64) error {
	cache.mu.Lock()
	defer cache.mu.Unlock()
	cache.balance -= amount
	return nil
}

type nativeBillingUpstream struct {
	service.HTTPUpstream
	calls int
}

func (u *nativeBillingUpstream) Do(request *http.Request, proxy string, accountID int64, concurrency int) (*http.Response, error) {
	return u.DoWithTLS(request, proxy, accountID, concurrency, nil)
}

func (u *nativeBillingUpstream) DoWithTLS(request *http.Request, _ string, _ int64, _ int, _ *tlsfingerprint.Profile) (*http.Response, error) {
	u.calls++
	return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"audio/mpeg"}, "X-Request-Id": {"echoed-client-id"}}, Body: io.NopCloser(strings.NewReader("ID3synthetic-audio"))}, nil
}

func TestNativeProtocolHandlerBillsSeparateExecutions(t *testing.T) {
	gin.SetMode(gin.TestMode)
	cfg := &config.Config{}
	cfg.Default.RateMultiplier = 1
	groupID := int64(9410)
	price := 0.25
	group := &service.Group{ID: groupID, Hydrated: true, Platform: service.PlatformOpenAI, Status: service.StatusActive, RateMultiplier: 1, ModelPricing: []service.ChannelModelPricing{{Models: []string{"test-speech"}, BillingMode: service.BillingModePerRequest, PerRequestPrice: &price}}}
	account := service.Account{ID: 9411, Platform: service.PlatformOpenAI, Type: service.AccountTypeAPIKey, Status: service.StatusActive, Schedulable: true, Concurrency: 1, Credentials: map[string]any{"api_key": "synthetic", "base_url": "https://provider.example"}}
	accountRepo := &openAIWSFailoverHandlerAccountRepoStub{accounts: []service.Account{account}}
	usageRepo := &openAIWSUsageHandlerUsageLogRepoStub{created: make(chan *service.UsageLog, 2)}
	billingRepo := &compatStreamUsageBillingRepo{}
	balanceCache := &nativeBillingCache{newHandlerInflightCache(100)}
	cache := service.NewBillingCacheService(balanceCache, nil, nil, nil, nil, nil, cfg, nil)
	t.Cleanup(cache.Stop)
	billing := service.NewBillingService(cfg, nil)
	upstream := &nativeBillingUpstream{}
	concurrency := service.NewConcurrencyService(&fakeConcurrencyCache{})
	gateway := service.NewOpenAIGatewayService(accountRepo, usageRepo, billingRepo, nil, nil, nil, nil, cfg, nil, concurrency, billing, nil, cache, upstream, &service.DeferredService{}, nil, nil, service.NewModelPricingResolver(nil, billing), nil, nil, nil, nil)
	handler := NewOpenAIGatewayHandler(gateway, concurrency, cache, service.NewAPIKeyService(nil, nil, nil, nil, nil, nil, cfg), nil, nil, nil, nil, cfg)
	key := &service.APIKey{ID: 9412, UserID: 9413, GroupID: &groupID, Group: group, User: &service.User{ID: 9413, Status: service.StatusActive, Balance: 100}}
	for range 2 {
		rec := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(rec)
		c.Request = httptest.NewRequest(http.MethodPost, "/v1/audio/speech", bytes.NewBufferString(`{"model":"test-speech","input":"hello","voice":"alloy"}`))
		c.Request = c.Request.WithContext(context.WithValue(c.Request.Context(), ctxkey.ClientRequestID, "same-client-id"))
		c.Request.Header.Set("Content-Type", "application/json")
		c.Set(string(middleware.ContextKeyAPIKey), key)
		c.Set(string(middleware.ContextKeyUser), middleware.AuthSubject{UserID: key.UserID, Concurrency: 1})
		handler.NativeProtocol("/v1/audio/speech")(c)
		require.Equal(t, 200, rec.Code, rec.Body.String())
		require.Equal(t, "ID3synthetic-audio", rec.Body.String())
		select {
		case usage := <-usageRepo.created:
			require.InDelta(t, 0.25, usage.ActualCost, 0.000001)
			require.False(t, usage.Stream)
			require.NotNil(t, usage.BillingMode)
			require.Equal(t, string(service.BillingModePerRequest), *usage.BillingMode)
		case <-time.After(2 * time.Second):
			t.Fatal("usage was not recorded")
		}
	}
	require.Equal(t, 2, upstream.calls)
	require.Eventually(t, func() bool {
		balance, _ := balanceCache.GetUserBalance(t.Context(), key.UserID)
		return balance == 99.5
	}, time.Second, 10*time.Millisecond)
	billingRepo.mu.Lock()
	defer billingRepo.mu.Unlock()
	require.Len(t, billingRepo.commands, 2)
	for _, command := range billingRepo.commands {
		require.Equal(t, key.UserID, command.UserID)
		require.Equal(t, key.ID, command.APIKeyID)
		require.InDelta(t, 0.25, command.BalanceCost, 0.000001)
	}
	require.NotEqual(t, billingRepo.commands[0].RequestID, billingRepo.commands[1].RequestID)
}
