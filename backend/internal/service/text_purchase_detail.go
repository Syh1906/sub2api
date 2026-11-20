package service

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	dbent "github.com/Wei-Shaw/sub2api/ent"
	infraerrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
	"github.com/lib/pq"
)

type TextVoucherDetail struct {
	Code            string     `json:"code,omitempty"`
	RedeemedAt      *time.Time `json:"redeemed_at,omitempty"`
	Spent           float64    `json:"spent"`
	HistoricalSpent float64    `json:"historical_spent"`
	AdjustedPoints  float64    `json:"adjusted_points"`
	Remaining       float64    `json:"remaining"`
	RefundEligible  bool       `json:"refund_eligible"`
	Reason          string     `json:"reason,omitempty"`
}

// TextVoucherSummaries loads a page of fulfillment states without retrieving redeem codes.
func (s *PaymentService) TextVoucherSummaries(ctx context.Context, orders []*dbent.PaymentOrder) (map[int64]*TextVoucherDetail, error) {
	details := make(map[int64]*TextVoucherDetail)
	paid := make(map[int64]*dbent.PaymentOrder)
	var ids []int64
	for _, o := range orders {
		if o == nil || o.OrderType != TextVoucherOrder {
			continue
		}
		if o.PaidAt == nil {
			details[o.ID] = &TextVoucherDetail{Reason: "unpaid"}
			continue
		}
		paid[o.ID] = o
		ids = append(ids, o.ID)
	}
	if len(ids) == 0 {
		return details, nil
	}
	rows, err := s.entClient.QueryContext(ctx, `
        SELECT order_id, redeemed_at, spent, points-spent, refund_hold, historical_spent, adjusted_points
        FROM text_purchase_vouchers WHERE order_id=ANY($1::bigint[])`, pq.Array(ids))
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	now := time.Now()
	for rows.Next() {
		var id int64
		var held bool
		d := &TextVoucherDetail{}
		if err := rows.Scan(&id, &d.RedeemedAt, &d.Spent, &d.Remaining, &held, &d.HistoricalSpent, &d.AdjustedPoints); err != nil {
			return nil, err
		}
		o := paid[id]
		switch {
		case held || o.Status == OrderStatusRefunded:
			d.Remaining = 0
			d.Reason = "refund"
		case d.Spent > 0:
			d.Reason = "spent"
		case textRefundExpired(*o.PaidAt, now):
			d.Reason = "expired"
		default:
			d.RefundEligible = o.Status == OrderStatusCompleted || o.Status == OrderStatusRefundFailed
		}
		details[id] = d
		delete(paid, id)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if len(paid) > 0 {
		return nil, fmt.Errorf("missing voucher for paid order")
	}
	return details, nil
}

func (s *PaymentService) TextDetail(ctx context.Context, o *dbent.PaymentOrder, owner bool) (*TextVoucherDetail, error) {
	summaries, err := s.TextVoucherSummaries(ctx, []*dbent.PaymentOrder{o})
	if err != nil {
		return nil, err
	}
	detail := summaries[o.ID]
	if detail != nil && owner && o.PaidAt != nil && detail.RedeemedAt == nil && detail.Reason != "refund" {
		detail.Code = o.RechargeCode
	}
	return detail, nil
}

type TextOrderUsageItem struct {
	ID        int64     `json:"id"`
	RequestID string    `json:"request_id"`
	CreatedAt time.Time `json:"created_at"`
	Points    float64   `json:"points"`
}

// AdminTextOrderUsage returns only the portion of each request paid from this order.
func (s *PaymentService) AdminTextOrderUsage(ctx context.Context, id int64, page, size int) ([]TextOrderUsageItem, int64, error) {
	order, err := s.GetOrderByID(ctx, id)
	if err != nil {
		return nil, 0, err
	}
	if order.OrderType != TextVoucherOrder {
		return nil, 0, infraerrors.BadRequest("NOT_TEXT_ORDER", "order does not contain text points")
	}
	// Return the total even for an empty or out-of-range page, using the same
	// snapshot for both the page and its count.
	rows, err := s.entClient.QueryContext(ctx, `
        SELECT tally.total, entry.id, entry.request_id, entry.created_at, entry.points
        FROM (SELECT count(*) AS total FROM text_purchase_usage WHERE order_id=$1) tally
        LEFT JOIN (
            SELECT id,request_id,created_at,points FROM text_purchase_usage WHERE order_id=$1
            ORDER BY created_at DESC,id DESC LIMIT $2 OFFSET $3
        ) entry ON true
        ORDER BY entry.created_at DESC,entry.id DESC`, id, size, (page-1)*size)
	if err != nil {
		return nil, 0, err
	}
	defer func() { _ = rows.Close() }()
	var total int64
	items := []TextOrderUsageItem{}
	for rows.Next() {
		var id sql.NullInt64
		var request sql.NullString
		var at sql.NullTime
		var points sql.NullFloat64
		if err = rows.Scan(&total, &id, &request, &at, &points); err != nil {
			return nil, 0, err
		}
		if id.Valid {
			items = append(items, TextOrderUsageItem{ID: id.Int64, RequestID: request.String, CreatedAt: at.Time, Points: points.Float64})
		}
	}
	return items, total, rows.Err()
}
