//go:build unit

package service

import (
	"context"
	"sync"
	"testing"
	"time"

	dbent "github.com/Wei-Shaw/sub2api/ent"
	"github.com/Wei-Shaw/sub2api/internal/payment"
	infraerrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
	"github.com/stretchr/testify/require"
)

// 两个相同的创建事务竞争同一个购买标识，只有胜者能继续联系支付商。
func TestPaymentPurchaseConcurrentReservation(t *testing.T) {
	ctx := context.Background()
	client := newPaymentConfigServiceTestClient(t)
	svc := &PaymentService{entClient: client}
	user := client.User.Create().SetEmail("parallel@example.invalid").SetPasswordHash("test").SetUsername("buyer").SaveX(ctx)
	req := CreateOrderRequest{PurchaseKey: "parallel", UserID: user.ID, Amount: 20, OrderType: payment.OrderTypeBalance, PaymentType: payment.TypeAlipay}
	require.NoError(t, req.preparePurchaseIdentity())
	start := make(chan struct{})
	results := make(chan error, 2)
	var wg sync.WaitGroup
	for range 2 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			_, err := svc.createOrderInTx(ctx, req, &User{ID: user.ID, Email: user.Email, Username: user.Username}, nil, &PaymentConfig{MaxPendingOrders: 5, OrderTimeoutMin: 30}, 20, 20, 0, 20, 0, nil)
			results <- err
		}()
	}
	close(start)
	wg.Wait()
	close(results)
	successes := 0
	for err := range results {
		if err == nil {
			successes++
		}
	}
	require.Equal(t, 1, successes)
	require.Equal(t, 1, client.PaymentOrder.Query().CountX(ctx))
	result, err := svc.LookupPurchase(ctx, user.ID, req.PurchaseKey)
	require.NoError(t, err)
	require.Positive(t, result.OrderID)
}

func TestPaymentPurchaseReplayAndCheckout(t *testing.T) {
	ctx := context.Background()
	client := newPaymentConfigServiceTestClient(t)
	svc := &PaymentService{entClient: client, resumeService: NewPaymentResumeService([]byte("test-checkout-signing-material"))}
	user, err := client.User.Create().SetEmail("buyer@example.invalid").SetPasswordHash("test").SetUsername("buyer").Save(ctx)
	require.NoError(t, err)
	req := CreateOrderRequest{UserID: user.ID, PurchaseKey: "one-purchase", PaymentType: payment.TypeAlipay, OrderType: payment.OrderTypeBalance, Amount: 20, SrcHost: "test.example"}
	require.NoError(t, req.preparePurchaseIdentity())
	create := func() (*dbent.PaymentOrder, error) {
		return svc.createOrderInTx(ctx, req, &User{ID: user.ID, Email: user.Email, Username: user.Username}, nil, &PaymentConfig{MaxPendingOrders: 5, OrderTimeoutMin: 30}, 20, 20, 0, 20, 0, nil)
	}
	order, err := create()
	require.NoError(t, err)
	_, err = create()
	require.Error(t, err)
	require.Equal(t, 1, client.PaymentOrder.Query().CountX(ctx))
	// 首次调用可能已经落单，但支付商尚未返回收银资料。
	result, err := svc.CreateOrder(ctx, req)
	require.NoError(t, err)
	require.Equal(t, order.ID, result.OrderID)
	require.Empty(t, result.PayURL)
	client.PaymentOrder.UpdateOneID(order.ID).SetPayURL("https://checkout.example/pay").SaveX(ctx)
	result, err = svc.LookupPurchase(ctx, user.ID, req.PurchaseKey)
	require.NoError(t, err)
	require.Equal(t, "https://checkout.example/pay", result.PayURL)
	result, err = svc.ResumeCheckout(ctx, order.ID, user.ID)
	require.NoError(t, err)
	require.Equal(t, order.ID, result.OrderID)
	_, err = svc.ResumeCheckout(ctx, order.ID, user.ID+1)
	require.Equal(t, 403, infraerrors.Code(err))
	_, err = svc.LookupPurchase(ctx, user.ID+1, req.PurchaseKey)
	require.Equal(t, 404, infraerrors.Code(err))
	changed := req
	changed.Amount = 50
	_, err = svc.CreateOrder(ctx, changed)
	require.Equal(t, 409, infraerrors.Code(err))
	require.Equal(t, 1, client.PaymentOrder.Query().CountX(ctx))

	sdk := *result
	sdk.PayURL = ""
	sdk.ClientSecret = "client-only-test-session"
	sdk.IntentID = "sample-intent"
	sdk.Currency = "USD"
	sdk.CountryCode = "US"
	sdk.PaymentEnv = "demo"
	encrypted, err := svc.sealCheckout(&sdk)
	require.NoError(t, err)
	require.NotContains(t, encrypted, sdk.ClientSecret)
	client.PaymentOrder.UpdateOneID(order.ID).SetCheckoutCiphertext(encrypted).SaveX(ctx)
	restored, err := svc.ResumeCheckout(ctx, order.ID, user.ID)
	require.NoError(t, err)
	require.Equal(t, sdk.ClientSecret, restored.ClientSecret)
	require.Equal(t, sdk.IntentID, restored.IntentID)
	for _, status := range []string{OrderStatusCancelled, OrderStatusPaid, OrderStatusRefunding, OrderStatusCompleted, OrderStatusFailed} {
		client.PaymentOrder.UpdateOneID(order.ID).SetStatus(status).SaveX(ctx)
		result, err = svc.CreateOrder(ctx, req)
		require.NoError(t, err)
		require.Equal(t, status, result.Status)
		require.Empty(t, result.PayURL)
		require.Empty(t, result.ClientSecret)
	}
	client.PaymentOrder.UpdateOneID(order.ID).SetStatus(OrderStatusPending).SetExpiresAt(time.Now().Add(-time.Minute)).SaveX(ctx)
	result, err = svc.ResumeCheckout(ctx, order.ID, user.ID)
	require.NoError(t, err)
	require.Equal(t, OrderStatusExpired, result.Status)
	require.Empty(t, result.ClientSecret)
}
