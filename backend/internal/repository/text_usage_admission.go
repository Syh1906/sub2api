package repository

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/lib/pq"
)

func (r *userRepository) AdmitTextUsage(ctx context.Context, userID int64, ticket string, minimum float64, ttl time.Duration) error {
	// The buyer row serializes this decision with refund and redemption. Even a
	// buyer paying with ordinary balance can redeem a first voucher mid-request.
	result, err := r.sql.ExecContext(ctx, `
		WITH buyer AS (
			SELECT balance FROM users WHERE id=$1 AND deleted_at IS NULL FOR UPDATE
		)
		INSERT INTO text_usage_admissions(id, user_id, expires_at)
		SELECT $2, $1, clock_timestamp() + make_interval(secs => $4)
		FROM buyer WHERE balance > 0 AND balance >= $3`, userID, ticket, minimum, int(ttl.Seconds()))
	if err != nil {
		return service.ErrBillingServiceUnavailable.WithCause(err)
	}
	n, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return service.ErrInsufficientBalance
	}
	return nil
}

func (r *userRepository) RenewTextUsage(ctx context.Context, ticket string, ttl time.Duration) (bool, error) {
	res, err := r.sql.ExecContext(ctx, "UPDATE text_usage_admissions SET expires_at=clock_timestamp()+make_interval(secs => $2) WHERE id=$1", ticket, int(ttl.Seconds()))
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n == 1, err
}

func (r *userRepository) ReleaseTextUsage(ctx context.Context, ticket string) error {
	_, err := r.sql.ExecContext(ctx, "DELETE FROM text_usage_admissions WHERE id=$1", ticket)
	return err
}

// A delayed worker may settle after lease expiry unless a refund actually reclaimed it.
// Take the same buyer lock as the refund, then use a fresh statement snapshot.
func checkTextUsageAdmissions(ctx context.Context, tx *sql.Tx, buyer int64, tickets []string) error {
	if len(tickets) == 0 {
		return nil
	}
	var id int64
	if err := tx.QueryRowContext(ctx, "SELECT id FROM users WHERE id=$1 AND deleted_at IS NULL FOR UPDATE", buyer).Scan(&id); err != nil {
		return err
	}
	var missing bool
	err := tx.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM unnest($2::uuid[]) AS requested(id)
      WHERE NOT EXISTS (SELECT 1 FROM text_usage_admissions a WHERE a.id=requested.id AND a.user_id=$1))`, buyer, pq.Array(tickets)).Scan(&missing)
	if err != nil {
		return err
	}
	if missing {
		return fmt.Errorf("text usage admission was released before settlement for user %d", buyer)
	}
	return nil
}
