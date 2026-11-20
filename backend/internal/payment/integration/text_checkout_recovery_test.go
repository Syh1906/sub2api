//go:build integration

package integration_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	dbent "github.com/Wei-Shaw/sub2api/ent"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
)

func verifyTextCheckoutRecovery(t *testing.T, db *sql.DB, ent *dbent.Client, svc *service.PaymentService, uid, other int64) {
	t.Helper()
	ctx := context.Background()
	previous := http.DefaultTransport
	defer func() { http.DefaultTransport = previous }()
	lastTrade, outcome, remoteStatus := "", "", "pending"
	var posts int
	http.DefaultTransport = voucherTransport(func(r *http.Request) (*http.Response, error) {
		if r.URL.Host != "test-api.creem.io" || r.URL.Path != "/v1/checkouts" {
			return nil, errors.New("unexpected request")
		}
		reply := map[string]any{}
		status := 200
		switch r.Method {
		case http.MethodPost:
			var input struct {
				Request string `json:"request_id"`
			}
			if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
				return nil, err
			}
			lastTrade = input.Request
			posts++
			switch outcome {
			case "disconnected":
				return nil, io.ErrUnexpectedEOF
			case "refused":
				status = http.StatusUnauthorized
			}
		case http.MethodGet:
			if r.URL.Query().Get("checkout_id") != "ch_fixture" {
				return nil, errors.New("incorrect checkout ID")
			}
			reply["request_id"], reply["product"], reply["status"], reply["units"] = lastTrade, "prod_10", remoteStatus, 1
		default:
			return nil, errors.New("unexpected request")
		}
		reply["id"], reply["checkout_url"] = "ch_fixture", "https://checkout.example/pay"
		raw, err := json.Marshal(reply)
		if err != nil {
			return nil, err
		}
		return &http.Response{StatusCode: status, Body: io.NopCloser(strings.NewReader(string(raw))), Header: http.Header{}}, nil
	})
	create := func(key string) (*service.CreateOrderResponse, error) {
		return svc.CreateOrder(ctx, service.CreateOrderRequest{UserID: uid, PurchaseKey: key, Amount: 10, OrderType: service.TextVoucherOrder, PaymentType: "creem", ReturnURL: "https://example.invalid/payment/result", SrcHost: "example.invalid"})
	}
	lookup := func(key string) *service.CreateOrderResponse {
		o, err := svc.LookupPurchase(ctx, uid, key)
		require.NoError(t, err)
		return o
	}
	// Rejecting the claim statement simulates a committed local order and a crash before dispatch.
	trigger := func(statement string) {
		_, err := db.Exec(statement)
		require.NoError(t, err)
	}
	trigger(`CREATE FUNCTION interrupt_checkout_fixture() RETURNS TRIGGER AS $$ BEGIN RAISE EXCEPTION 'fixture checkout interruption'; END $$ LANGUAGE plpgsql`)
	trigger(`CREATE TRIGGER interrupt_claim BEFORE UPDATE ON payment_orders FOR EACH ROW WHEN (NEW.provider_snapshot->>'checkout_phase'='dispatching') EXECUTE FUNCTION interrupt_checkout_fixture()`)
	_, err := create("checkout-before-send")
	require.Error(t, err)
	first := lookup("checkout-before-send")
	require.Equal(t, "ready", first.CheckoutState)
	require.Zero(t, posts)
	trigger("DROP TRIGGER interrupt_claim ON payment_orders")
	_, err = svc.ContinueTextCheckout(ctx, first.OrderID, other)
	require.Error(t, err)
	// A concurrent dispatch can claim the ready order after cancellation reads it.
	trigger(`CREATE FUNCTION skip_cancel_fixture() RETURNS TRIGGER AS $$ BEGIN RETURN NULL; END $$ LANGUAGE plpgsql`)
	trigger(`CREATE TRIGGER skip_cancel BEFORE UPDATE ON payment_orders FOR EACH ROW WHEN (NEW.status='CANCELLED') EXECUTE FUNCTION skip_cancel_fixture()`)
	_, err = svc.CancelOrder(ctx, first.OrderID, uid)
	require.Error(t, err)
	require.Equal(t, service.OrderStatusPending, lookup("checkout-before-send").Status)
	trigger("DROP TRIGGER skip_cancel ON payment_orders")
	trigger("DROP FUNCTION skip_cancel_fixture()")
	done, err := svc.ContinueTextCheckout(ctx, first.OrderID, uid)
	require.NoError(t, err)
	require.NotEmpty(t, done.PayURL)
	require.Equal(t, 1, posts)
	_, err = create("checkout-before-send")
	require.NoError(t, err)
	require.Equal(t, 1, posts)
	_, err = svc.CancelOrder(ctx, first.OrderID, uid)
	require.NoError(t, err)

	outcome = "disconnected"
	lost, err := create("checkout-lost-response")
	require.NoError(t, err)
	require.Equal(t, "needs_review", lost.CheckoutState)
	id, err := svc.LookupTextReference(ctx, uid, lastTrade)
	require.NoError(t, err)
	require.Equal(t, lost.OrderID, id)
	_, err = svc.LookupTextReference(ctx, other, lastTrade)
	require.Error(t, err)
	require.Equal(t, 2, posts)
	_, err = create("checkout-lost-response")
	require.NoError(t, err)
	_, err = svc.ContinueTextCheckout(ctx, lost.OrderID, uid)
	require.NoError(t, err)
	require.Equal(t, 2, posts)
	_, err = svc.CancelOrder(ctx, lost.OrderID, uid)
	require.Error(t, err)
	trade := lastTrade
	lastTrade = "different"
	require.Error(t, svc.ReconcileTextCheckout(ctx, lost.OrderID, "ch_fixture", "admin:42"))
	lastTrade = trade
	require.NoError(t, svc.ReconcileTextCheckout(ctx, lost.OrderID, "ch_fixture", "admin:42"))
	require.Equal(t, "https://checkout.example/pay", lookup("checkout-lost-response").PayURL)
	require.Equal(t, 2, posts)
	_, err = svc.CancelOrder(ctx, lost.OrderID, uid)
	require.NoError(t, err)

	outcome = ""
	trigger(`CREATE TRIGGER interrupt_save BEFORE UPDATE ON payment_orders FOR EACH ROW WHEN (NEW.pay_url IS NOT NULL) EXECUTE FUNCTION interrupt_checkout_fixture()`)
	_, err = create("checkout-save-failed")
	require.Error(t, err)
	trigger("DROP TRIGGER interrupt_save ON payment_orders")
	unsaved := lookup("checkout-save-failed")
	require.Equal(t, "needs_review", unsaved.CheckoutState)
	require.NoError(t, svc.ReconcileTextCheckout(ctx, unsaved.OrderID, "ch_fixture", "admin:42"))
	require.NotEmpty(t, lookup("checkout-save-failed").PayURL)
	require.Equal(t, 3, posts)
	_, err = svc.CancelOrder(ctx, unsaved.OrderID, uid)
	require.NoError(t, err)
	trigger("DROP FUNCTION interrupt_checkout_fixture()")

	outcome = "refused"
	_, err = create("checkout-rejected")
	require.Error(t, err)
	refused := lookup("checkout-rejected")
	require.Equal(t, "ready", refused.CheckoutState)
	outcome = ""
	done, err = create("checkout-rejected")
	require.NoError(t, err)
	require.Equal(t, refused.OrderID, done.OrderID)
	require.NotEmpty(t, done.PayURL)
	_, err = svc.CancelOrder(ctx, done.OrderID, uid)
	require.NoError(t, err)

	outcome = "disconnected"
	old, err := create("checkout-old")
	require.NoError(t, err)
	_, err = db.Exec("UPDATE payment_orders SET expires_at=now()-interval '1 minute', updated_at=now()-interval '1 minute' WHERE id=$1", old.OrderID)
	require.NoError(t, err)
	_, err = svc.ExpireTimedOutOrders(ctx)
	require.NoError(t, err)
	require.Equal(t, "needs_review", lookup("checkout-old").CheckoutState)
	require.Equal(t, service.OrderStatusPending, lookup("checkout-old").Status)
	require.NoError(t, svc.CloseUnsentTextCheckout(ctx, old.OrderID, "merchant-support-reference", "admin:42"))
	require.Equal(t, service.OrderStatusCancelled, ent.PaymentOrder.GetX(ctx, old.OrderID).Status)
	logs, err := svc.GetOrderAuditLogs(ctx, old.OrderID)
	require.NoError(t, err)
	require.Contains(t, logs[0].Detail, "merchant-support-reference")
	require.Equal(t, "admin:42", logs[0].Operator)
	require.Error(t, svc.CloseUnsentTextCheckout(ctx, old.OrderID, "merchant-support-reference", "admin:42"))
	processing, err := create("checkout-processing")
	require.NoError(t, err)
	remoteStatus = "processing"
	require.NoError(t, svc.ReconcileTextCheckout(ctx, processing.OrderID, "ch_fixture", "admin:42"))
	confirmed := lookup("checkout-processing")
	require.Equal(t, "waiting_payment", confirmed.CheckoutState)
	require.Empty(t, confirmed.PayURL)
	require.Nil(t, ent.PaymentOrder.GetX(ctx, processing.OrderID).PaidAt)
	require.True(t, confirmed.ExpiresAt.After(time.Now()))
	remoteStatus = "expired"
	require.NoError(t, svc.ReconcileTextCheckout(ctx, processing.OrderID, "ch_fixture", "admin:42"))
	require.Equal(t, service.OrderStatusExpired, lookup("checkout-processing").Status)

	outcome = ""
	paid, err := create("cancel-before-paid-notification")
	require.NoError(t, err)
	remoteStatus = "completed"
	_, err = svc.CancelOrder(ctx, paid.OrderID, uid)
	require.ErrorContains(t, err, "CHECKOUT_UNCONFIRMED")
	waiting := lookup("cancel-before-paid-notification")
	require.Equal(t, "waiting_payment", waiting.CheckoutState)
	require.Empty(t, waiting.PayURL)
	require.Nil(t, ent.PaymentOrder.GetX(ctx, paid.OrderID).PaidAt)
}
