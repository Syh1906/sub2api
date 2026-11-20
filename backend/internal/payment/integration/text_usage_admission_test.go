//go:build integration

package integration_test

import (
	"context"
	"database/sql"
	"testing"
	"time"

	dbent "github.com/Wei-Shaw/sub2api/ent"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

// The payment and billing services share only PostgreSQL. Keep the original units
// for the downstream order-allocation assertions after exercising the race.
func verifyTextUsageAdmission(t *testing.T, db *sql.DB, payments *service.PaymentService, billing service.UsageBillingBatchRepository, first *dbent.PaymentOrder, charge *service.UsageBillingCommand, tracked context.Context, handlerDone func()) {
	t.Helper()
	ctx := context.Background()
	ticket := service.TextUsageAdmissionFromContext(tracked)
	require.NotNil(t, ticket)
	workerDone := ticket.HoldTask()
	defer workerDone()

	conflict := func() {
		t.Helper()
		err := payments.RequestRefund(ctx, first.ID, first.UserID, "unused")
		require.ErrorContains(t, err, "REFUND_USAGE_IN_FLIGHT")
	}
	conflict() // Model still running.
	handlerDone()
	conflict() // Response finished, bill is queued.
	require.NoError(t, db.QueryRow("SELECT id FROM text_usage_admissions WHERE user_id=$1", first.UserID).Scan(&charge.TextUsageTicket))
	result, err := billing.Apply(ctx, charge)
	require.NoError(t, err)
	require.True(t, result.Applied)
	workerDone()
	retry, err := billing.Apply(ctx, charge)
	require.NoError(t, err)
	require.False(t, retry.Applied)
	require.ErrorContains(t, payments.RequestRefund(ctx, first.ID, first.UserID, "used"), "REFUND_INELIGIBLE")

	charge.TextUsageTicket = ""
}

func verifyExpiredTextUsage(t *testing.T, db *sql.DB, payments *service.PaymentService, billing service.UsageBillingBatchRepository, second *dbent.PaymentOrder, charge *service.UsageBillingCommand) {
	t.Helper()
	ctx := context.Background()
	// Simulate an instance disappearing without a finalizer: a successful refund can
	// reclaim an expired ticket, and a later worker cannot move that bill to another lot.
	lost := uuid.NewString()
	_, err := db.Exec("INSERT INTO text_usage_admissions(id,user_id,expires_at) VALUES($1,$2,$3)", lost, second.UserID, time.Now().Add(-time.Hour))
	require.NoError(t, err)
	require.NoError(t, payments.RequestRefund(ctx, second.ID, second.UserID, "abandoned request"))
	late := *charge
	late.TextUsageTicket = lost
	late.RequestID = "late-revoked"
	late.RequestFingerprint = ""
	_, err = billing.Apply(ctx, &late)
	require.ErrorContains(t, err, "released before settlement")
	_, err = billing.ApplyBatch(ctx, []*service.UsageBillingCommand{&late})
	require.ErrorContains(t, err, "released before settlement")

}
