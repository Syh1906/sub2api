//go:build unit

package service

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/payment"
	infraerrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
	"github.com/stretchr/testify/require"
	stripe "github.com/stripe/stripe-go/v85"
)

type checkoutTestUsers struct {
	UserRepository
	buyer *User
}

func (u checkoutTestUsers) GetByID(context.Context, int64) (*User, error) {
	return u.buyer, nil
}

func TestStripeCheckoutKeepsOrderMerchant(t *testing.T) {
	ctx := context.Background()
	db := newPaymentConfigServiceTestClient(t)
	user := db.User.Create().SetEmail("buyer@example.invalid").SetPasswordHash("fixture").SaveX(ctx)
	buyer := checkoutTestUsers{buyer: &User{ID: user.ID, Email: user.Email, Status: payment.EntityStatusActive}}
	settings := &paymentConfigSettingRepoStub{values: map[string]string{SettingPaymentEnabled: "true"}}
	cfg := NewPaymentConfigService(db, settings, []byte("test-only-checkout"))
	for _, name := range []string{"A", "B"} {
		_, err := cfg.CreateProviderInstance(ctx, CreateProviderInstanceRequest{
			ProviderKey: payment.TypeStripe, Name: name, Enabled: true, SupportedTypes: []string{"card"},
			Config: map[string]string{"secretKey": "sk_test_fixture_" + name, "publishableKey": "pk_test_fixture_" + name, "currency": "USD"},
		})
		require.NoError(t, err)
	}
	var calls atomic.Int32
	merchant := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		name := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer sk_test_fixture_")
		if r.URL.Path != "/v1/payment_intents" || (name != "A" && name != "B") {
			http.Error(w, "unexpected test request", http.StatusBadRequest)
			return
		}
		calls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]string{"id": "pi_" + name, "client_secret": "pi_" + name + "_secret_fixture"})
	}))
	defer merchant.Close()
	oldBackend := stripe.GetBackend(stripe.APIBackend)
	stripe.SetBackend(stripe.APIBackend, stripe.GetBackendWithConfig(stripe.APIBackend, &stripe.BackendConfig{URL: stripe.String(merchant.URL)}))
	defer stripe.SetBackend(stripe.APIBackend, oldBackend)

	newService := func() *PaymentService {
		return NewPaymentService(db, payment.NewRegistry(), payment.NewDefaultLoadBalancer(db, nil), nil, nil, cfg, buyer, nil, nil)
	}
	svc := newService()
	fresh := newService()
	seen := map[string]bool{}
	for _, key := range []string{"first-purchase", "second-purchase"} {
		req := CreateOrderRequest{UserID: user.ID, Amount: 20, PaymentType: payment.TypeStripe, PurchaseKey: key}
		created, err := svc.CreateOrder(ctx, req)
		require.NoError(t, err)
		name := map[string]string{"pi_A_secret_fixture": "A", "pi_B_secret_fixture": "B"}[created.ClientSecret]
		require.NotEmpty(t, name)
		seen[name] = true
		want := "pk_test_fixture_" + name
		require.Equal(t, want, created.StripePublishableKey)
		resumed, err := fresh.ResumeCheckout(ctx, created.OrderID, user.ID)
		require.NoError(t, err)
		require.Equal(t, want, resumed.StripePublishableKey)
		require.Equal(t, created.ClientSecret, resumed.ClientSecret)

		// 模拟升级前的加密会话：付款意图存在，JSON 中没有新增公钥字段。
		legacy := *created
		legacy.StripePublishableKey = ""
		ciphertext, err := svc.sealCheckout(&legacy)
		require.NoError(t, err)
		db.PaymentOrder.UpdateOneID(created.OrderID).SetCheckoutCiphertext(ciphertext).SaveX(ctx)
		replay, err := fresh.CreateOrder(ctx, req)
		require.NoError(t, err)
		require.Equal(t, want, replay.StripePublishableKey)
		found, err := fresh.LookupPurchase(ctx, user.ID, key)
		require.NoError(t, err)
		require.Equal(t, want, found.StripePublishableKey)
		resumed, err = fresh.ResumeCheckout(ctx, created.OrderID, user.ID)
		require.NoError(t, err)
		require.Equal(t, want, resumed.StripePublishableKey)
		require.Equal(t, created.ClientSecret, resumed.ClientSecret)
		_, err = fresh.ResumeCheckout(ctx, created.OrderID, user.ID+1)
		require.Equal(t, 403, infraerrors.Code(err))
		// 未保存会话时，原设备可配对自己的缓存；服务端不会生成新的付款意图。
		db.PaymentOrder.UpdateOneID(created.OrderID).ClearCheckoutCiphertext().SaveX(ctx)
		resumed, err = fresh.ResumeCheckout(ctx, created.OrderID, user.ID)
		require.NoError(t, err)
		require.Equal(t, want, resumed.StripePublishableKey)
		require.Empty(t, resumed.ClientSecret)
		db.PaymentOrder.UpdateOneID(created.OrderID).SetExpiresAt(time.Now().Add(-time.Minute)).SaveX(ctx)
		expired, err := fresh.ResumeCheckout(ctx, created.OrderID, user.ID)
		require.NoError(t, err)
		require.Empty(t, expired.StripePublishableKey)
		require.Empty(t, expired.ClientSecret)

		db.PaymentOrder.UpdateOneID(created.OrderID).
			SetExpiresAt(created.ExpiresAt).ClearProviderInstanceID().ClearProviderSnapshot().SaveX(ctx)
		unbound, err := fresh.ResumeCheckout(ctx, created.OrderID, user.ID)
		require.NoError(t, err)
		require.Empty(t, unbound.StripePublishableKey)
	}
	require.Equal(t, map[string]bool{"A": true, "B": true}, seen)
	require.Equal(t, int32(2), calls.Load())
	require.Equal(t, 2, db.PaymentOrder.Query().CountX(ctx))
}
