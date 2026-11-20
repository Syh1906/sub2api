//go:build integration

package integration_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/handler"
	"github.com/Wei-Shaw/sub2api/internal/handler/admin"
	"github.com/Wei-Shaw/sub2api/internal/server/middleware"
	"github.com/Wei-Shaw/sub2api/internal/server/routes"
	"github.com/gin-gonic/gin"
	"testing"

	dbent "github.com/Wei-Shaw/sub2api/ent"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
)

// The first order has 17 points left after a previous three-point request.
// The second has ten. A five-point ordinary credit must be spent before either order.
func verifyOrderUsage(t *testing.T, db *sql.DB, svc *service.PaymentService, billing service.UsageBillingBatchRepository, first, second *dbent.PaymentOrder, previous *service.UsageBillingCommand) {
	t.Helper()
	ctx := context.Background()
	_, err := db.ExecContext(ctx, "UPDATE users SET balance=balance+5 WHERE id=$1", first.UserID)
	require.NoError(t, err)
	cmd := func(key string, cost float64) *service.UsageBillingCommand {
		return &service.UsageBillingCommand{RequestID: key, UserID: first.UserID, APIKeyID: 11, Model: "text-fixture", BalanceCost: cost}
	}
	ordinary := cmd("ordinary-only", 2)
	_, err = billing.Apply(ctx, ordinary)
	require.NoError(t, err)
	cross := cmd("across-orders", 22.5)
	follow := cmd("next-request", 1)
	for i := 0; i < 2; i++ {
		outcomes, err := billing.ApplyBatch(ctx, []*service.UsageBillingCommand{previous, cross, follow})
		require.NoError(t, err)
		for j, o := range outcomes {
			require.NoError(t, o.Err)
			require.Equal(t, i == 0 && j != 0, o.Result.Applied)
		}
	}
	_, err = db.ExecContext(ctx, "UPDATE users SET balance=balance-0.5 WHERE id=$1", first.UserID)
	require.NoError(t, err)
	summary, err := svc.TextVoucherSummaries(ctx, []*dbent.PaymentOrder{first, second})
	require.NoError(t, err)
	for _, order := range []*dbent.PaymentOrder{first, second} {
		d, err := svc.TextDetail(ctx, order, false)
		require.NoError(t, err)
		require.Equal(t, d, summary[order.ID])
		require.Empty(t, d.Code)
		require.NotNil(t, d.RedeemedAt)
	}
	require.Equal(t, 20., summary[first.ID].Spent)
	require.Equal(t, 4., summary[second.ID].Spent)
	require.Equal(t, 0.5, summary[second.ID].AdjustedPoints)
	require.Equal(t, 0., summary[second.ID].HistoricalSpent)

	a, total, err := svc.AdminTextOrderUsage(ctx, first.ID, 1, 20)
	require.NoError(t, err)
	require.EqualValues(t, 2, total)
	require.Len(t, a, 2)
	require.Equal(t, cross.RequestID, a[0].RequestID)
	require.Equal(t, 17., a[0].Points)
	require.Equal(t, previous.RequestID, a[1].RequestID)
	require.Equal(t, 3., a[1].Points)
	b, total, err := svc.AdminTextOrderUsage(ctx, second.ID, 1, 1)
	require.NoError(t, err)
	require.EqualValues(t, 2, total)
	require.Len(t, b, 1)
	require.Equal(t, follow.RequestID, b[0].RequestID)
	require.Equal(t, 1., b[0].Points)
	b, total, err = svc.AdminTextOrderUsage(ctx, second.ID, 2, 1)
	require.NoError(t, err)
	require.EqualValues(t, 2, total)
	require.Len(t, b, 1)
	require.Equal(t, cross.RequestID, b[0].RequestID)
	require.Equal(t, 2.5, b[0].Points)
	require.NotZero(t, b[0].CreatedAt)
	require.NotEqual(t, a[0].ID, b[0].ID)

	// ApplyBatch quantizes each debit before summing it. A rounded-away
	// request must never be recorded or abort another request in its batch.
	tiny, rounded := cmd("below-scale", 1e-9), cmd("half-unit", 5e-9)
	_, err = billing.ApplyBatch(ctx, []*service.UsageBillingCommand{tiny, rounded})
	require.NoError(t, err)
	b, total, err = svc.AdminTextOrderUsage(ctx, second.ID, 1, 10)
	require.NoError(t, err)
	require.EqualValues(t, 3, total)
	require.Equal(t, rounded.RequestID, b[0].RequestID)
	require.Equal(t, 1e-8, b[0].Points)
	d, err := svc.TextDetail(ctx, second, false)
	require.NoError(t, err)
	require.Equal(t, 4.00000001, d.Spent)
}

// Exercise the registered routes with ordinary and administrator tokens.
func verifyOrderUsageHTTP(t *testing.T, svc *service.PaymentService, users service.UserRepository, customer, operator, orderID int64, code string) {
	t.Helper()
	ctx := context.Background()
	cfg := &config.Config{JWT: config.JWTConfig{Secret: "in-memory-signing-fixture", ExpireHour: 1}}
	auth := service.NewAuthService(nil, users, nil, nil, cfg, nil, nil, nil, nil, nil, nil, nil, nil)
	userSvc := service.NewUserService(users, nil, nil, nil)
	token := func(id int64) string {
		u, err := users.GetByID(ctx, id)
		require.NoError(t, err)
		value, err := auth.GenerateToken(ctx, u)
		require.NoError(t, err)
		return value
	}
	engine := gin.New()
	routes.RegisterPaymentRoutes(engine.Group("/api/v1"), handler.NewPaymentHandler(svc, nil), &handler.PaymentWebhookHandler{}, admin.NewPaymentHandler(svc, nil),
		middleware.NewJWTAuthMiddleware(auth, userSvc, nil, nil), middleware.NewAdminAuthMiddleware(auth, userSvc, nil, nil), func(c *gin.Context) { c.Next() }, nil, nil, nil)
	get := func(path, token string, status int) json.RawMessage {
		req := httptest.NewRequest(http.MethodGet, "/api/v1/"+path, nil)
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		rec := httptest.NewRecorder()
		engine.ServeHTTP(rec, req)
		require.Equal(t, status, rec.Code)
		if status != http.StatusOK {
			return nil
		}
		var out struct{ Data json.RawMessage }
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &out))
		return out.Data
	}
	customerToken, adminToken := token(customer), token(operator)
	usage := fmt.Sprintf("admin/payment/orders/%d/usage?page_size=1", orderID)
	get(usage, "", http.StatusUnauthorized)
	get(usage, customerToken, http.StatusForbidden)
	var page struct {
		Total int
		Items []service.TextOrderUsageItem
	}
	require.NoError(t, json.Unmarshal(get(usage, adminToken, http.StatusOK), &page))
	require.Equal(t, 3, page.Total)
	require.Len(t, page.Items, 1)
	require.Equal(t, "half-unit", page.Items[0].RequestID)

	for _, list := range []struct{ path, token string }{
		{"payment/orders/my", customerToken}, {"admin/payment/orders", adminToken},
	} {
		body := get(list.path, list.token, http.StatusOK)
		require.NotContains(t, string(body), code)
		var result struct {
			Items []struct {
				ID      int64
				Voucher *service.TextVoucherDetail
			}
		}
		require.NoError(t, json.Unmarshal(body, &result))
		found := false
		for _, item := range result.Items {
			if item.ID != orderID {
				continue
			}
			require.NotNil(t, item.Voucher)
			require.NotNil(t, item.Voucher.RedeemedAt)
			require.Empty(t, item.Voucher.Code)
			require.Equal(t, 4.00000001, item.Voucher.Spent)
			found = true
		}
		require.True(t, found)
	}
}
