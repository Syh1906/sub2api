package service

import (
	"context"
	"encoding/json"
	"strconv"
	"strings"
	"time"

	infraerrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
)

// TextRefundFailureEvidence identifies the specific request and the provider's
// final rejection or confirmation of non-acceptance, checked by a refund operator.
// A transaction which still says "paid" is insufficient on its own.
type TextRefundFailureEvidence struct {
	TransactionID     string    `json:"transaction_id" binding:"required,max=255"`
	RequestedAt       time.Time `json:"requested_at" binding:"required"`
	MerchantReference string    `json:"merchant_reference" binding:"required,max=200"`
}

func (s *PaymentService) ResolveTextRefundFailure(ctx context.Context, oid int64, evidence TextRefundFailureEvidence, actor string) (*RefundResult, error) {
	evidence.MerchantReference = strings.TrimSpace(evidence.MerchantReference)
	if evidence.MerchantReference == "" || len([]rune(evidence.MerchantReference)) > 200 || evidence.TransactionID == "" || evidence.RequestedAt.IsZero() || actor == "" {
		return nil, infraerrors.BadRequest("REFUND_EVIDENCE_REQUIRED", "provide the original transaction, request time and final merchant confirmation reference")
	}
	o, err := s.entClient.PaymentOrder.Get(ctx, oid)
	if err != nil {
		return nil, err
	}
	if o.OrderType != TextVoucherOrder {
		return nil, infraerrors.Conflict("INVALID_ORDER_TYPE", "requires a text voucher refund")
	}
	tx, err := s.entClient.Tx(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }()
	current, err := lockTextRefundOrder(ctx, tx, o.UserID, o.ID)
	if err != nil {
		return nil, err
	}
	if current.RefundRequestedAt == nil || !current.RefundRequestedAt.Equal(evidence.RequestedAt) {
		return nil, infraerrors.Conflict("REFUND_ATTEMPT_CHANGED", "refund request changed")
	}
	lot, err := readRefundLot(ctx, tx.Client(), oid)
	if err != nil {
		return nil, err
	}
	if lot.txID != evidence.TransactionID {
		return nil, infraerrors.Conflict("REFUND_MISMATCH", "wrong transaction")
	}
	switch current.Status {
	case OrderStatusRefundFailed:
		return &RefundResult{Warning: "provider_rejected"}, nil
	case OrderStatusRefunded:
		return &RefundResult{Success: true}, nil
	case OrderStatusRefundPending:
	default:
		return nil, infraerrors.Conflict("INVALID_STATUS", "requires a pending refund")
	}
	// The order lock also guards the external POST. It cannot start after this
	// transaction releases the hold, or concurrently with the final provider check.
	t, err := s.textRefundTransaction(ctx, current, lot)
	if err != nil {
		return nil, err
	}
	if lot.matchesRefund(t) {
		return s.settleTextRefund(ctx, tx, current, true)
	}
	if !lot.matchesUnrefunded(t) {
		return nil, infraerrors.Conflict("REFUND_MISMATCH", "original transaction is not fully paid with no refund")
	}
	raw, err := json.Marshal(evidence)
	if err != nil {
		return nil, err
	}
	_, err = tx.PaymentAuditLog.Create().SetOrderID(strconv.FormatInt(oid, 10)).SetAction("TEXT_REFUND_FAILED_" + strconv.FormatInt(evidence.RequestedAt.UnixMicro(), 10)).SetDetail(string(raw)).SetOperator(actor).Save(ctx)
	if err != nil {
		return nil, err
	}
	return s.settleTextRefund(ctx, tx, current, false)
}
