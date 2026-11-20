package service

import (
	"context"
	dbent "github.com/Wei-Shaw/sub2api/ent"
	infraerrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
)

// Called under the buyer lock. Revoke abandoned tickets in this transaction only
// if the refund proceeds. An active bill or a rejected refund keeps its original funds.
func guardRedeemedRefund(ctx context.Context, tx *dbent.Tx, buyer int64) error {
	if _, err := tx.ExecContext(ctx, "DELETE FROM text_usage_admissions WHERE user_id=$1 AND expires_at<=clock_timestamp()", buyer); err != nil {
		return err
	}
	rows, err := tx.QueryContext(ctx, "SELECT 1 FROM text_usage_admissions WHERE user_id=$1 LIMIT 1", buyer)
	if err != nil {
		return err
	}
	defer func() { _ = rows.Close() }()
	if rows.Next() {
		return infraerrors.Conflict("REFUND_USAGE_IN_FLIGHT", "a balance-billed request is still being settled; retry after it finishes")
	}
	return rows.Err()
}
