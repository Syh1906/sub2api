//go:build integration

package integration_test

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	dbent "github.com/Wei-Shaw/sub2api/ent"
	"github.com/Wei-Shaw/sub2api/ent/paymentauditlog"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
)

func verifyRefundRecovery(t *testing.T, ent *dbent.Client, svc *service.PaymentService, redeem *service.RedeemService, purchase func(int, string) (*dbent.PaymentOrder, string), merchantStatus *string, merchantTransaction *map[string]any, posts *atomic.Int32) {
	t.Helper()
	ctx := context.Background()
	o, code := purchase(10, "interrupted-refund")
	_, err := redeem.Redeem(ctx, o.UserID, code)
	require.NoError(t, err)
	balance := func() float64 { return ent.User.GetX(ctx, o.UserID).Balance }
	startBalance := balance()
	request := func() {
		t.Helper()
		require.NoError(t, svc.RequestRefund(ctx, o.ID, o.UserID, "unused"))
		require.InDelta(t, startBalance-10, balance(), 1e-10)
	}
	process := func() (*service.RefundResult, error) {
		t.Helper()
		plan, _, err := svc.PrepareRefund(ctx, o.ID, o.Amount, "unused", false, true)
		require.NoError(t, err)
		return svc.ExecuteRefund(ctx, plan)
	}
	// A crash after approval, before dispatch: resume this attempt once.
	request()
	ent.PaymentOrder.UpdateOneID(o.ID).SetStatus(service.OrderStatusRefunding).SaveX(ctx)
	*merchantStatus = "unauthorized"
	startPosts := posts.Load()
	result, err := process()
	require.NoError(t, err)
	require.Equal(t, "provider_rejected", result.Warning)
	require.Equal(t, service.OrderStatusRefundFailed, ent.PaymentOrder.GetX(ctx, o.ID).Status)
	require.Equal(t, startBalance, balance())
	require.Equal(t, startPosts+1, posts.Load())
	request()
	// Process died just after claiming dispatch. A paid transaction by itself
	// cannot decide whether the merchant has accepted an asynchronous refund.
	ent.PaymentOrder.UpdateOneID(o.ID).SetStatus(service.OrderStatusRefundPending).SaveX(ctx)
	*merchantTransaction = map[string]any{"id": "tran_" + o.OutTradeNo, "order": "ord_" + o.OutTradeNo, "currency": "USD", "amount_paid": 1000, "refunded_amount": 0, "status": "paid"}
	query, err := svc.QueryAndFinalizeRefund(ctx, o.ID)
	require.NoError(t, err)
	require.Equal(t, "merchant_confirmation_required", query.Warning)
	require.InDelta(t, startBalance-10, balance(), 1e-10)
	evidence := service.TextRefundFailureEvidence{TransactionID: query.TransactionID, RequestedAt: *query.RequestedAt, MerchantReference: "case-123"}
	bad := evidence
	bad.RequestedAt = bad.RequestedAt.Add(-time.Second)
	_, err = svc.ResolveTextRefundFailure(ctx, o.ID, bad, "admin:1")
	require.Error(t, err)
	(*merchantTransaction)["refunded_amount"] = 100
	_, err = svc.ResolveTextRefundFailure(ctx, o.ID, evidence, "admin:1")
	require.Error(t, err)
	require.InDelta(t, startBalance-10, balance(), 1e-10)
	(*merchantTransaction)["refunded_amount"] = 0
	for i := 0; i < 2; i++ {
		result, err = svc.ResolveTextRefundFailure(ctx, o.ID, evidence, "admin:1")
		require.NoError(t, err)
		require.False(t, result.Success)
		require.Equal(t, startBalance, balance())
	}
	require.Equal(t, 1, ent.PaymentAuditLog.Query().Where(paymentauditlog.ActionHasPrefix("TEXT_REFUND_FAILED_"), paymentauditlog.OperatorEQ("admin:1")).CountX(ctx))
	require.Equal(t, startPosts+1, posts.Load(), "reconciliation must not send any merchant POST")
	request()
	*merchantStatus = "disconnected"
	_, err = process()
	require.Error(t, err)
	_, err = svc.ResolveTextRefundFailure(ctx, o.ID, evidence, "admin:1")
	require.Error(t, err, "stale merchant evidence must never release a new hold")
	query, err = svc.QueryAndFinalizeRefund(ctx, o.ID)
	require.NoError(t, err)
	evidence.RequestedAt = *query.RequestedAt
	(*merchantTransaction)["refunded_amount"] = 1000
	(*merchantTransaction)["status"] = "refunded"
	result, err = svc.ResolveTextRefundFailure(ctx, o.ID, evidence, "admin:1")
	require.NoError(t, err)
	require.True(t, result.Success)
	require.InDelta(t, startBalance-10, balance(), 1e-10)
	require.Equal(t, startPosts+2, posts.Load())
}
