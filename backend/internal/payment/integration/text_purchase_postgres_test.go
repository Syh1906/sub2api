//go:build integration

package integration_test

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"entgo.io/ent/dialect"
	entsql "entgo.io/ent/dialect/sql"
	dbent "github.com/Wei-Shaw/sub2api/ent"
	_ "github.com/Wei-Shaw/sub2api/ent/runtime"
	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/payment"
	"github.com/Wei-Shaw/sub2api/internal/repository"
	"github.com/Wei-Shaw/sub2api/internal/service"
	_ "github.com/lib/pq"
	"github.com/stretchr/testify/require"
)

type voucherTransport func(*http.Request) (*http.Response, error)

func (t voucherTransport) RoundTrip(r *http.Request) (*http.Response, error) { return t(r) }

func TestTextPurchasePostgres(t *testing.T) {
	dsn := os.Getenv("TEXT_VOUCHER_TEST_DSN")
	if dsn == "" {
		t.Skip("requires a dedicated empty PostgreSQL test database")
	}
	db, err := sql.Open("postgres", dsn)
	require.NoError(t, err)
	defer func() { _ = db.Close() }()
	ctx := context.Background()
	var existing bool
	require.NoError(t, db.QueryRow("SELECT to_regclass('public.users') IS NOT NULL").Scan(&existing))
	require.False(t, existing, "use a new test database")
	require.NoError(t, repository.ApplyMigrations(ctx, db))
	ent := dbent.NewClient(dbent.Driver(entsql.OpenDB(dialect.Postgres, db)))
	defer func() { _ = ent.Close() }()
	settings := repository.NewSettingRepository(ent)
	require.NoError(t, settings.Set(ctx, service.SettingPaymentEnabled, "true"))
	users := repository.NewUserRepository(ent, db)
	redeem := service.NewRedeemService(repository.NewRedeemCodeRepository(ent), users, nil, nil, nil, ent, nil, nil)
	svc := service.NewPaymentService(ent, payment.NewRegistry(), nil, redeem, nil, service.NewPaymentConfigService(ent, settings, nil), users, nil, nil)
	u := ent.User.Create().SetEmail("voucher@example.invalid").SetUsername("voucher").SetPasswordHash("fixture").SetBalance(0).SaveX(ctx)
	other := ent.User.Create().SetEmail("other@example.invalid").SetUsername("other").SetPasswordHash("fixture").SaveX(ctx)
	for key, val := range map[string]string{"CREEM_MODE": "test", "CREEM_API_KEY": "fixture", "CREEM_WEBHOOK_SECRET": "fixture-signature", "CREEM_PRODUCT_10": "prod_10", "CREEM_PRODUCT_20": "prod_20", "CREEM_PRODUCT_50": "prod_50", "CREEM_PRODUCT_100": "prod_100"} {
		t.Setenv(key, val)
	}
	var creates, refunds atomic.Int32
	merchantStatus := "succeeded"
	var queryTransaction map[string]any
	// Intercept only the synthetic merchant calls in this test process.
	original := http.DefaultTransport
	http.DefaultTransport = voucherTransport(func(r *http.Request) (*http.Response, error) {
		if r.URL.Host != "test-api.creem.io" || r.Header.Get("x-api-key") != "fixture" {
			return nil, fmt.Errorf("unexpected merchant request")
		}
		var raw any
		switch r.URL.Path {
		case "/v1/checkouts":
			var input map[string]any
			if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
				return nil, err
			}
			trade, ok := input["request_id"].(string)
			if !ok {
				return nil, fmt.Errorf("missing request ID")
			}
			creates.Add(1)
			success, ok := input["success_url"].(string)
			if !ok {
				return nil, fmt.Errorf("missing return URL")
			}
			target, err := url.Parse(success)
			if err != nil || target.Query().Get("local_order_id") == "" || target.Query().Has("order_id") {
				return nil, fmt.Errorf("missing return order")
			}
			raw = map[string]string{"id": "ch_" + trade, "checkout_url": "https://checkout.example/" + trade}
		case "/v1/refunds":
			refunds.Add(1)
			if merchantStatus == "unauthorized" {
				return &http.Response{StatusCode: http.StatusUnauthorized, Body: io.NopCloser(strings.NewReader("")), Header: http.Header{}}, nil
			}
			if merchantStatus == "disconnected" {
				return nil, io.ErrUnexpectedEOF
			}
			raw = map[string]string{"status": merchantStatus}
		case "/v1/transactions":
			if r.URL.Query().Get("transaction_id") != queryTransaction["id"] {
				return nil, fmt.Errorf("wrong transaction query")
			}
			raw = queryTransaction
		default:
			return nil, fmt.Errorf("unexpected merchant path %s", r.URL.Path)
		}
		b, _ := json.Marshal(raw)
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(string(b))), Header: http.Header{}}, nil
	})
	defer func() { http.DefaultTransport = original }()
	send := func(kind string, obj any, signed bool) error {
		raw, _ := json.Marshal(map[string]any{"id": "evt_fixture", "eventType": kind, "object": obj})
		mac := hmac.New(sha256.New, []byte("fixture-signature"))
		_, _ = mac.Write(raw)
		sig := hex.EncodeToString(mac.Sum(nil))
		if !signed {
			sig = "invalid"
		}
		return svc.HandleCreemEvent(ctx, raw, sig)
	}
	balance := func() float64 { return ent.User.GetX(ctx, u.ID).Balance }
	purchase := func(n int, key string) (*dbent.PaymentOrder, string) {
		before := balance()
		req := service.CreateOrderRequest{UserID: u.ID, PurchaseKey: key, Amount: float64(n), OrderType: service.TextVoucherOrder, PaymentType: "creem", ReturnURL: "https://example.invalid/payment/result", SrcHost: "example.invalid"}
		start := creates.Load()
		res, err := svc.CreateOrder(ctx, req)
		require.NoError(t, err)
		require.NotEmpty(t, res.PayURL)
		dup, err := svc.CreateOrder(ctx, req)
		require.NoError(t, err)
		require.Equal(t, res.OrderID, dup.OrderID)
		require.Equal(t, start+1, creates.Load())
		trade := res.OutTradeNo
		prod := fmt.Sprintf("prod_%d", n)
		paidTime := time.Now().UTC().Add(-time.Hour).Truncate(time.Microsecond)
		if key == "expired" {
			paidTime = time.Now().UTC().Add(-14*24*time.Hour - time.Second).Truncate(time.Microsecond)
		}
		order := map[string]any{"updated_at": paidTime, "id": "ord_" + trade, "product": prod, "transaction": "tran_" + trade, "amount": n * 100, "sub_total": n * 100, "amount_due": n * 100, "amount_paid": n * 100, "currency": "USD", "status": "paid", "type": "onetime"}
		v := map[string]any{"request_id": trade, "id": "ch_" + trade, "units": 1, "status": "completed", "product": map[string]any{"id": prod, "billing_type": "onetime", "price": n * 100, "currency": "USD"}, "order": order}
		require.Error(t, send("checkout.completed", v, false))
		order["currency"] = "EUR"
		require.Error(t, send("checkout.completed", v, true))
		order["currency"] = "USD"
		order["type"] = "recurring"
		require.Error(t, send("checkout.completed", v, true))
		order["type"] = "onetime"
		order["amount_paid"] = n*100 - 1
		require.Error(t, send("checkout.completed", v, true))
		order["amount_paid"] = n * 100
		delete(order, "updated_at")
		require.Error(t, send("checkout.completed", v, true))
		order["updated_at"] = paidTime
		require.NoError(t, send("checkout.completed", v, true))
		require.NoError(t, send("checkout.completed", v, true))
		o := ent.PaymentOrder.GetX(ctx, res.OrderID)
		require.True(t, o.PaidAt.Equal(paidTime))
		require.True(t, o.CompletedAt.After(*o.PaidAt))
		d, err := svc.TextDetail(ctx, o, true)
		require.NoError(t, err)
		require.Len(t, d.Code, 32)
		require.Equal(t, before, balance())
		_, err = svc.GetOrder(ctx, o.ID, other.ID)
		require.Error(t, err)
		_, err = redeem.Redeem(ctx, other.ID, d.Code)
		require.Error(t, err)
		return o, d.Code
	}
	// Start a balance-funded model request before the buyer has any redeemed purchase.
	ent.User.UpdateOneID(u.ID).SetBalance(1).SaveX(ctx)
	cache := service.NewBillingCacheService(nil, users, nil, nil, nil, nil, &config.Config{}, nil)
	defer cache.Stop()
	tracked, handlerDone, err := cache.AdmitTextUsage(ctx, u.ID)
	require.NoError(t, err)
	defer handlerDone()
	a, ac := purchase(20, "one")
	b, bc := purchase(10, "two")
	_, err = redeem.Redeem(ctx, u.ID, ac)
	require.NoError(t, err)
	_, err = redeem.Redeem(ctx, u.ID, ac)
	require.Error(t, err)
	_, err = redeem.Redeem(ctx, u.ID, bc)
	require.NoError(t, err)
	// Simulate another billed request exhausting the initial ordinary balance.
	_, err = db.Exec("UPDATE users SET balance=balance-1 WHERE id=$1", u.ID)
	require.NoError(t, err)
	require.Equal(t, 30.0, balance())
	billing := repository.NewUsageBillingRepository(ent, db)
	cmd := &service.UsageBillingCommand{RequestID: "text-usage-1", UserID: u.ID, APIKeyID: 1, Model: "text-fixture", InputTokens: 4, OutputTokens: 3, BalanceCost: 3}
	verifyTextUsageAdmission(t, db, svc, billing, a, cmd, tracked, handlerDone)
	require.Equal(t, 27.0, balance())
	detail, err := svc.TextDetail(ctx, a, false)
	require.NoError(t, err)
	require.Equal(t, 3.0, detail.Spent)
	require.Empty(t, detail.Code)
	require.Error(t, svc.RequestRefund(ctx, a.ID, u.ID, "used"))
	verifyExpiredTextUsage(t, db, svc, billing, b, cmd)
	require.Equal(t, 17.0, balance())
	refund := func(o *dbent.PaymentOrder) {
		plan, _, err := svc.PrepareRefund(ctx, o.ID, o.Amount, "unused", false, true)
		require.NoError(t, err)
		_, err = svc.ExecuteRefund(ctx, plan)
		require.NoError(t, err)
	}
	refund(b)
	require.Equal(t, service.OrderStatusRefunded, ent.PaymentOrder.GetX(ctx, b.ID).Status)
	require.Equal(t, 17.0, balance())
	c, cc := purchase(50, "unredeemed")
	require.NoError(t, svc.RequestRefund(ctx, c.ID, u.ID, "unused"))
	_, err = redeem.Redeem(ctx, u.ID, cc)
	require.Error(t, err)
	merchantStatus = "pending"
	refund(c)
	require.Equal(t, service.OrderStatusRefundPending, ent.PaymentOrder.GetX(ctx, c.ID).Status)
	event := map[string]any{"id": "ref_fixture", "status": "succeeded", "refund_currency": "USD", "refund_amount": 5000, "transaction": map[string]any{"id": "tran_" + c.OutTradeNo, "order": "ord_" + c.OutTradeNo, "amount_paid": 5000, "refunded_amount": 5000, "currency": "USD", "status": "refunded"}}
	transaction := event["transaction"].(map[string]any)
	transaction["status"], transaction["refunded_amount"] = "paid", nil
	queryTransaction = map[string]any{"id": transaction["id"], "order": transaction["order"], "amount_paid": 5000, "currency": "USD", "status": "paid"}
	require.Error(t, send("refund.created", event, true))
	require.Equal(t, service.OrderStatusRefundPending, ent.PaymentOrder.GetX(ctx, c.ID).Status)
	queryTransaction["status"], queryTransaction["refunded_amount"] = "refunded", 5000
	require.NoError(t, send("refund.created", event, true))
	require.Equal(t, service.OrderStatusRefunded, ent.PaymentOrder.GetX(ctx, c.ID).Status)
	queryTransaction = nil // A duplicate need not contact the merchant after settlement.
	require.NoError(t, send("refund.created", event, true))
	transaction["status"], transaction["refunded_amount"] = "refunded", 5000
	require.NoError(t, send("refund.created", event, true))
	_, err = redeem.Redeem(ctx, u.ID, cc)
	require.Error(t, err)
	d, _ := purchase(100, "expired")
	require.Error(t, svc.RequestRefund(ctx, d.ID, u.ID, "late"))
	require.Equal(t, int32(2), refunds.Load())

	// A lost refund reply keeps the hold; querying the original transaction releases it exactly once.
	e, ec := purchase(10, "lost-reply")
	_, err = redeem.Redeem(ctx, u.ID, ec)
	require.NoError(t, err)
	require.NoError(t, svc.RequestRefund(ctx, e.ID, u.ID, "unused"))
	merchantStatus = "disconnected"
	plan, _, err := svc.PrepareRefund(ctx, e.ID, e.Amount, "unused", false, true)
	require.NoError(t, err)
	_, err = svc.ExecuteRefund(ctx, plan)
	require.Error(t, err)
	require.Equal(t, service.OrderStatusRefundPending, ent.PaymentOrder.GetX(ctx, e.ID).Status)
	queryTransaction = map[string]any{"id": "tran_" + e.OutTradeNo, "order": "ord_" + e.OutTradeNo, "amount_paid": 1000, "refunded_amount": 1000, "currency": "USD", "status": "refunded"}
	got, err := svc.QueryAndFinalizeRefund(ctx, e.ID)
	require.NoError(t, err)
	require.True(t, got.Success)
	require.Equal(t, 17.0, balance())

	// Refused refund restores only the held lot. A merchant receipt cannot charge it twice.
	f, fc := purchase(20, "declined")
	_, err = redeem.Redeem(ctx, u.ID, fc)
	require.NoError(t, err)
	require.NoError(t, svc.RequestRefund(ctx, f.ID, u.ID, "unused"))
	merchantStatus = "failed"
	refund(f)
	require.Equal(t, service.OrderStatusRefundFailed, ent.PaymentOrder.GetX(ctx, f.ID).Status)
	require.Equal(t, 37.0, balance())
	require.NoError(t, svc.RequestRefund(ctx, f.ID, u.ID, "retry after explicit failure"))
	merchantStatus = "succeeded"
	refund(f)
	require.Equal(t, 17.0, balance())

	// Both operations lock the same account before touching its voucher and balance.
	g, gc := purchase(10, "race")
	start := make(chan struct{})
	done := make(chan error, 2)
	go func() { <-start; _, err := redeem.Redeem(ctx, u.ID, gc); done <- err }()
	go func() { <-start; done <- svc.RequestRefund(ctx, g.ID, u.ID, "concurrent") }()
	close(start)
	err1, err2 := <-done, <-done
	require.True(t, err1 == nil || err2 == nil)
	require.Equal(t, service.OrderStatusRefundRequested, ent.PaymentOrder.GetX(ctx, g.ID).Status)
	require.Equal(t, 17.0, balance())
	refund(g)

	h, hc := purchase(10, "usage-detail")
	summary, err := svc.TextVoucherSummaries(ctx, []*dbent.PaymentOrder{h})
	require.NoError(t, err)
	require.Empty(t, summary[h.ID].Code)
	require.Nil(t, summary[h.ID].RedeemedAt)
	_, err = redeem.Redeem(ctx, u.ID, hc)
	require.NoError(t, err)
	verifyOrderUsage(t, db, svc, billing, a, h, cmd)
	t.Run("checkout recovery", func(t *testing.T) { verifyTextCheckoutRecovery(t, db, ent, svc, u.ID, other.ID) })
	verifyRefundRecovery(t, ent, svc, redeem, purchase, &merchantStatus, &queryTransaction, &refunds)
	operator := ent.User.Create().SetEmail("orders-admin@example.invalid").SetUsername("operator").SetRole("admin").SetPasswordHash("fixture").SaveX(ctx)
	verifyOrderUsageHTTP(t, svc, users, u.ID, operator.ID, h.ID, hc)
}
