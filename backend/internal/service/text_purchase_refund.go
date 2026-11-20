package service

import (
	"context"
	"database/sql"
	"fmt"
	dbent "github.com/Wei-Shaw/sub2api/ent"
	"github.com/Wei-Shaw/sub2api/ent/paymentorder"
	infraerrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
	"net/url"
	"strings"
	"time"
)

type textRefundEvent struct {
	ID          string           `json:"id"`
	Status      string           `json:"status"`
	Amount      int64            `json:"refund_amount"`
	Currency    string           `json:"refund_currency"`
	Transaction creemTransaction `json:"transaction"`
}
type creemTransaction struct {
	ID       string `json:"id"`
	Order    string `json:"order"`
	Currency string `json:"currency"`
	Status   string `json:"status"`
	Refunded int64  `json:"refunded_amount"`
	Paid     int64  `json:"amount_paid"`
}
type textRefundLot struct {
	txID, merchantOrder string
	cents               int64
	redeemed            sql.NullTime
	spent               float64
	held                bool
}

func readRefundLot(ctx context.Context, c *dbent.Client, id int64) (textRefundLot, error) {
	var lot textRefundLot
	rows, err := c.QueryContext(ctx, "SELECT transaction_id,merchant_order_id,paid_cents,redeemed_at,spent,refund_hold FROM text_purchase_vouchers WHERE order_id=$1", id)
	if err != nil {
		return lot, err
	}
	defer func() { _ = rows.Close() }()
	if !rows.Next() {
		if err = rows.Err(); err != nil {
			return lot, err
		}
		return lot, sql.ErrNoRows
	}
	err = rows.Scan(&lot.txID, &lot.merchantOrder, &lot.cents, &lot.redeemed, &lot.spent, &lot.held)
	return lot, err
}

// Hold this order's unused points before an operator contacts the provider.
func (s *PaymentService) requestTextRefund(ctx context.Context, oid, uid int64, reason string) error {
	tx, err := s.entClient.Tx(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if err = lockTextBuyer(ctx, tx, uid); err != nil {
		return err
	}
	o, err := tx.PaymentOrder.Query().Where(paymentorder.ID(oid), paymentorder.UserIDEQ(uid), paymentorder.OrderTypeEQ(TextVoucherOrder)).ForUpdate().Only(ctx)
	if err != nil {
		return err
	}
	if o.Status == OrderStatusRefundRequested {
		return nil
	}
	if (o.Status != OrderStatusCompleted && o.Status != OrderStatusRefundFailed) || o.PaidAt == nil || time.Now().After(o.PaidAt.Add(14*24*time.Hour)) {
		return infraerrors.Conflict("REFUND_INELIGIBLE", "order cannot be refunded")
	}
	lot, err := readRefundLot(ctx, tx.Client(), oid)
	if err != nil {
		return err
	}
	if lot.held || lot.spent != 0 {
		return infraerrors.Conflict("REFUND_INELIGIBLE", "points already used or held")
	}
	if _, err = tx.ExecContext(ctx, "UPDATE text_purchase_vouchers SET refund_hold=true WHERE order_id=$1", oid); err != nil {
		return err
	}
	if lot.redeemed.Valid {
		res, err := tx.ExecContext(ctx, "UPDATE users SET balance=balance-$1,updated_at=now() WHERE id=$2 AND balance >= $1", o.Amount, uid)
		if err != nil {
			return err
		}
		n, err := res.RowsAffected()
		if err != nil {
			return err
		}
		if n != 1 {
			return infraerrors.Conflict("REFUND_INELIGIBLE", "insufficient available points")
		}
	}
	now := time.Now()
	_, err = tx.PaymentOrder.UpdateOneID(oid).SetStatus(OrderStatusRefundRequested).SetRefundRequestedAt(now).SetRefundRequestReason(strings.TrimSpace(reason)).SetRefundAmount(o.Amount).SetRefundRequestedBy(fmt.Sprint(uid)).Save(ctx)
	if err != nil {
		return err
	}
	if err = tx.Commit(); err == nil && lot.redeemed.Valid {
		s.invalidateRefundBalanceCache(ctx, uid)
	}
	return err
}

func (s *PaymentService) sendTextRefund(ctx context.Context, o *dbent.PaymentOrder) (*RefundResult, error) {
	lot, err := readRefundLot(ctx, s.entClient, o.ID)
	if err != nil {
		return nil, err
	}
	if !lot.held {
		return nil, infraerrors.Conflict("REFUND_INELIGIBLE", "no refund request")
	}
	client := s.creemClient()
	if client.key == "" || client.base == "" || o.ProviderSnapshot["mode"] != client.mode {
		return nil, infraerrors.ServiceUnavailable("CREEM_UNAVAILABLE", "refund provider not configured")
	}
	n, err := s.entClient.PaymentOrder.Update().Where(paymentorder.ID(o.ID), paymentorder.StatusEQ(OrderStatusRefundRequested)).SetStatus(OrderStatusRefundPending).Save(ctx)
	if err != nil {
		return nil, err
	}
	if n == 0 {
		return nil, infraerrors.Conflict("INVALID_STATUS", "refund already submitted")
	}
	var result struct {
		Status string `json:"status"`
	}
	err = client.call(ctx, "POST", "/v1/refunds", map[string]string{"transaction_id": lot.txID}, &result)
	if err != nil {
		return nil, err
	} // Unknown outcome: keep the hold and query this same transaction.
	switch result.Status {
	case "succeeded":
		return s.finishTextRefund(ctx, o, lot, true)
	case "failed", "canceled":
		return s.finishTextRefund(ctx, o, lot, false)
	case "pending", "requiresAction":
		return &RefundResult{Warning: "pending"}, nil
	default:
		return nil, fmt.Errorf("unknown refund status")
	}
}

func (s *PaymentService) finishTextRefund(ctx context.Context, o *dbent.PaymentOrder, lot textRefundLot, success bool) (*RefundResult, error) {
	tx, err := s.entClient.Tx(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }()
	if err = lockTextBuyer(ctx, tx, o.UserID); err != nil {
		return nil, err
	}
	current, err := tx.PaymentOrder.Query().Where(paymentorder.ID(o.ID)).ForUpdate().Only(ctx)
	if err != nil {
		return nil, err
	}
	if current.Status == OrderStatusRefunded {
		return &RefundResult{Success: true}, nil
	}
	if current.Status != OrderStatusRefundPending && current.Status != OrderStatusRefundRequested {
		return nil, infraerrors.Conflict("REFUND_RECONCILIATION_REQUIRED", "refund requires an original order hold")
	}
	if !lot.held {
		return nil, infraerrors.Conflict("REFUND_RECONCILIATION_REQUIRED", "missing refund hold")
	}
	update := tx.PaymentOrder.UpdateOneID(o.ID)
	if success {
		update.SetStatus(OrderStatusRefunded).SetRefundAt(time.Now())
		if _, err = tx.ExecContext(ctx, "UPDATE redeem_codes SET status='disabled' WHERE id=(SELECT redeem_id FROM text_purchase_vouchers WHERE order_id=$1) AND status='unused'", o.ID); err != nil {
			return nil, err
		}
	} else {
		update.SetStatus(OrderStatusRefundFailed)
		if lot.redeemed.Valid {
			if _, err = tx.ExecContext(ctx, "UPDATE users SET balance=balance+$1,updated_at=now() WHERE id=$2", o.Amount, o.UserID); err != nil {
				return nil, err
			}
		}
		if _, err = tx.ExecContext(ctx, "UPDATE text_purchase_vouchers SET refund_hold=false WHERE order_id=$1", o.ID); err != nil {
			return nil, err
		}
	}
	if _, err = update.Save(ctx); err != nil {
		return nil, err
	}
	if err = tx.Commit(); err != nil {
		return nil, err
	}
	if !success && lot.redeemed.Valid {
		s.invalidateRefundBalanceCache(ctx, o.UserID)
	}
	return &RefundResult{Success: success}, nil
}
func (s *PaymentService) confirmTextRefundEvent(ctx context.Context, v textRefundEvent) error {
	if v.ID == "" || v.Status != "succeeded" || v.Currency != "USD" || v.Transaction.ID == "" {
		return infraerrors.BadRequest("INVALID_REFUND", "invalid refund event")
	}
	rows, err := s.entClient.QueryContext(ctx, "SELECT order_id FROM text_purchase_vouchers WHERE transaction_id=$1", v.Transaction.ID)
	if err != nil {
		return err
	}
	var id int64
	if !rows.Next() {
		_ = rows.Close()
		return infraerrors.NotFound("ORDER_NOT_FOUND", "refund order not found")
	}
	err = rows.Scan(&id)
	_ = rows.Close()
	if err != nil {
		return err
	}
	o, err := s.entClient.PaymentOrder.Get(ctx, id)
	if err != nil {
		return err
	}
	lot, err := readRefundLot(ctx, s.entClient, id)
	if err != nil {
		return err
	}
	if o.ProviderSnapshot["mode"] != s.creemClient().mode || v.Amount != lot.cents || !lot.matchesRefund(v.Transaction) {
		return infraerrors.Conflict("REFUND_MISMATCH", "refund does not match transaction")
	}
	_, err = s.finishTextRefund(ctx, o, lot, true)
	return err
}
func (lot textRefundLot) matchesRefund(t creemTransaction) bool {
	return t.ID == lot.txID && t.Order == lot.merchantOrder && t.Currency == "USD" && t.Status == "refunded" && t.Refunded == lot.cents && t.Paid == lot.cents
}
func (s *PaymentService) queryTextRefund(ctx context.Context, o *dbent.PaymentOrder) (*RefundResult, error) {
	if o.Status == OrderStatusRefunded {
		return &RefundResult{Success: true}, nil
	}
	if o.Status != OrderStatusRefundPending {
		return nil, infraerrors.Conflict("INVALID_STATUS", "no pending refund")
	}
	lot, err := readRefundLot(ctx, s.entClient, o.ID)
	if err != nil {
		return nil, err
	}
	var t creemTransaction
	c := s.creemClient()
	if o.ProviderSnapshot["mode"] != c.mode {
		return nil, infraerrors.Conflict("CREEM_UNAVAILABLE", "wrong store mode")
	}
	if err = c.call(ctx, "GET", "/v1/transactions?transaction_id="+url.QueryEscape(lot.txID), nil, &t); err != nil {
		return nil, err
	}
	if lot.matchesRefund(t) {
		return s.finishTextRefund(ctx, o, lot, true)
	}
	return &RefundResult{Warning: "pending"}, nil
}
