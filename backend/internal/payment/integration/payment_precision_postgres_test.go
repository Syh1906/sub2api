//go:build integration

package integration_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"strings"
	"testing"

	"entgo.io/ent/dialect"
	entsql "entgo.io/ent/dialect/sql"
	dbent "github.com/Wei-Shaw/sub2api/ent"
	"github.com/Wei-Shaw/sub2api/internal/handler"
	"github.com/Wei-Shaw/sub2api/internal/payment"
	"github.com/Wei-Shaw/sub2api/internal/repository"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	stripe "github.com/stripe/stripe-go/v85"
	"github.com/stripe/stripe-go/v85/webhook"
)

func TestPaymentPrecisionPostgres(t *testing.T) {
	dsn := os.Getenv("PAYMENT_PRECISION_TEST_DSN")
	if dsn == "" {
		t.Skip("requires an empty dedicated PostgreSQL database")
	}
	ctx := context.Background()
	db, err := sql.Open("postgres", dsn)
	require.NoError(t, err)
	defer func() { _ = db.Close() }()
	var empty bool
	require.NoError(t, db.QueryRow("SELECT to_regclass('public.users') IS NULL AND to_regclass('public.payment_orders') IS NULL").Scan(&empty))
	require.True(t, empty)
	require.NoError(t, repository.ApplyMigrations(ctx, db))
	ent := dbent.NewClient(dbent.Driver(entsql.OpenDB(dialect.Postgres, db)))
	settings := repository.NewSettingRepository(ent)
	require.NoError(t, settings.Set(ctx, service.SettingPaymentEnabled, "true"))
	cfg := service.NewPaymentConfigService(ent, settings, []byte("test-only-checkout"))
	signatures := map[string]string{}
	for _, name := range []string{"A", "B"} {
		secret := "fixture-signature-" + name
		inst, err := cfg.CreateProviderInstance(ctx, service.CreateProviderInstanceRequest{
			ProviderKey: "stripe", Name: name, Enabled: true, SupportedTypes: []string{"card"}, RefundEnabled: true,
			Config: map[string]string{"currency": "KWD", "secretKey": "fixture", "webhookSecret": secret},
		})
		require.NoError(t, err)
		signatures[strconv.FormatInt(inst.ID, 10)] = secret
	}
	users := repository.NewUserRepository(ent, db)
	redeem := service.NewRedeemService(repository.NewRedeemCodeRepository(ent), users, nil, nil, nil, ent, nil, nil)
	newService := func() *service.PaymentService {
		return service.NewPaymentService(ent, payment.NewRegistry(), payment.NewDefaultLoadBalancer(ent, nil), redeem, nil, cfg, users, nil, nil)
	}
	svc := newService()
	seenInstances := map[string]bool{}
	for _, tc := range []struct {
		name, rate            string
		input, credit, refund float64
		refundMinor           string
	}{
		{"direct", "0", 12.345, 12.35, 12.35, "12345"},
		{"with_fee", "2.87", 12, 12, 6, "6173"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			require.NoError(t, settings.Set(ctx, service.SettingRechargeFeeRate, tc.rate))
			u := ent.User.Create().SetEmail(tc.name + "@example.invalid").SetPasswordHash("fixture").SetBalance(0).SaveX(ctx)
			var createAmounts, refundAmounts []string
			intent := "pi_" + tc.name
			merchant := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				switch r.URL.Path {
				case "/v1/payment_intents":
					createAmounts = append(createAmounts, r.FormValue("amount"))
					if r.FormValue("currency") != "kwd" {
						t.Error("unexpected payment currency")
					}
					_ = json.NewEncoder(w).Encode(map[string]string{"id": intent, "client_secret": intent + "_secret_fixture"})
				case "/v1/refunds":
					if r.FormValue("payment_intent") != intent {
						t.Error("refund refers to different payment")
					}
					refundAmounts = append(refundAmounts, r.FormValue("amount"))
					_ = json.NewEncoder(w).Encode(map[string]string{"id": "re_fixture", "status": "succeeded"})
				default:
					http.NotFound(w, r)
				}
			}))
			defer merchant.Close()
			old := stripe.GetBackend(stripe.APIBackend)
			stripe.SetBackend(stripe.APIBackend, stripe.GetBackendWithConfig(stripe.APIBackend, &stripe.BackendConfig{URL: stripe.String(merchant.URL)}))
			defer stripe.SetBackend(stripe.APIBackend, old)
			created, err := svc.CreateOrder(ctx, service.CreateOrderRequest{UserID: u.ID, Amount: tc.input, PaymentType: "stripe", PurchaseKey: tc.name})
			require.NoError(t, err)
			require.Equal(t, 12.345, created.PayAmount)
			order := ent.PaymentOrder.GetX(ctx, created.OrderID)
			require.Equal(t, 12.345, order.PayAmount)
			require.Equal(t, tc.credit, order.Amount)
			fresh := newService()
			for range 2 {
				resumed, err := fresh.ResumeCheckout(ctx, order.ID, u.ID)
				require.NoError(t, err)
				require.Equal(t, intent+"_secret_fixture", resumed.ClientSecret)
				require.Equal(t, order.ID, resumed.OrderID)
			}
			require.Equal(t, []string{"12345"}, createAmounts)
			h := handler.NewPaymentWebhookHandler(fresh, nil)
			notify := func(minor int, secret string) int {
				payload := fmt.Sprintf(`{"id":"evt_%d","object":"event","type":"payment_intent.succeeded","api_version":%q,"data":{"object":{"id":%q,"amount":%d,"currency":"kwd","metadata":{"orderId":%q}}}}`, minor, stripe.APIVersion, intent, minor, order.OutTradeNo)
				sig := webhook.GenerateTestSignedPayload(&webhook.UnsignedPayload{Payload: []byte(payload), Secret: secret})
				w := httptest.NewRecorder()
				c, _ := gin.CreateTestContext(w)
				c.Request = httptest.NewRequest(http.MethodPost, "/api/v1/payment/webhook/stripe", strings.NewReader(payload))
				c.Request.Header.Set("Stripe-Signature", sig.Header)
				h.StripeWebhook(c)
				return w.Code
			}
			require.NotNil(t, order.ProviderInstanceID)
			seenInstances[*order.ProviderInstanceID] = true
			secret := signatures[*order.ProviderInstanceID]
			require.NotEmpty(t, secret)
			for _, other := range signatures {
				if other != secret {
					require.Equal(t, http.StatusBadRequest, notify(12345, other))
				}
			}
			require.Equal(t, http.StatusInternalServerError, notify(12344, secret))
			require.Zero(t, ent.User.GetX(ctx, u.ID).Balance)
			require.Equal(t, http.StatusOK, notify(12345, secret))
			require.Equal(t, http.StatusOK, notify(12345, secret))
			require.Equal(t, tc.credit, ent.User.GetX(ctx, u.ID).Balance)
			plan, warning, err := svc.PrepareRefund(ctx, order.ID, tc.refund, "test", false, true)
			require.NoError(t, err)
			require.Nil(t, warning)
			result, err := svc.ExecuteRefund(ctx, plan)
			require.NoError(t, err)
			require.True(t, result.Success)
			require.Equal(t, []string{tc.refundMinor}, refundAmounts)
			require.Equal(t, tc.credit-tc.refund, ent.User.GetX(ctx, u.ID).Balance)
			require.Equal(t, tc.refund, ent.PaymentOrder.GetX(ctx, order.ID).RefundAmount)
			ent.PaymentOrder.UpdateOneID(order.ID).ClearProviderInstanceID().ClearProviderSnapshot().SaveX(ctx)
			require.Equal(t, http.StatusServiceUnavailable, notify(12345, secret))
		})
	}
	require.Len(t, seenInstances, 2)
}
