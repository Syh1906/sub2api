package service

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"time"

	dbent "github.com/Wei-Shaw/sub2api/ent"
	"github.com/Wei-Shaw/sub2api/ent/paymentorder"
	"github.com/Wei-Shaw/sub2api/internal/payment"
	infraerrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
)

func (s *PaymentService) createTextOrder(ctx context.Context, req CreateOrderRequest, cfg *PaymentConfig) (*CreateOrderResponse, error) {
	tier := int(req.Amount)
	c := s.creemClient()
	if req.PaymentType != "creem" || req.PlanID != 0 || req.purchaseKeyHash == "" || req.Amount != float64(tier) || !c.ready(tier) {
		return nil, infraerrors.BadRequest("TEXT_OFFER_UNAVAILABLE", "text offer is unavailable")
	}
	ret, err := CanonicalizeReturnURL(req.ReturnURL, req.SrcHost, req.SrcURL)
	if err != nil {
		return nil, err
	}
	if ret == "" {
		return nil, infraerrors.BadRequest("INVALID_RETURN_URL", "missing checkout return address")
	}
	user, err := s.userRepo.GetByID(ctx, req.UserID)
	if err != nil {
		return nil, err
	}
	if user.Status != payment.EntityStatusActive {
		return nil, infraerrors.Forbidden("USER_INACTIVE", "user account disabled")
	}
	if err = s.checkCancelRateLimit(ctx, req.UserID, cfg); err != nil {
		return nil, err
	}
	sel := &payment.InstanceSelection{ProviderKey: "creem", Config: map[string]string{"product": c.products[tier], "mode": c.mode, "return_url": ret}}
	o, err := s.createOrderInTx(ctx, req, user, nil, cfg, req.Amount, req.Amount, 0, req.Amount, 0, sel)
	if err != nil {
		if prior, lookupErr := s.findPurchase(ctx, req); prior != nil || lookupErr != nil {
			return prior, lookupErr
		}
		return nil, err
	}
	return s.startTextCheckout(ctx, o)
}

func textRefundExpired(paidAt, at time.Time) bool {
	return at.After(paidAt.Add(14 * 24 * time.Hour))
}

type textPaidOrder struct {
	UpdatedAt   time.Time `json:"updated_at"`
	ID          string    `json:"id"`
	Product     string    `json:"product"`
	Currency    string    `json:"currency"`
	Amount      int64     `json:"amount"`
	Subtotal    int64     `json:"sub_total"`
	Paid        int64     `json:"amount_paid"`
	Due         int64     `json:"amount_due"`
	Tax         int64     `json:"tax_amount"`
	Discount    int64     `json:"discount_amount"`
	Status      string    `json:"status"`
	Type        string    `json:"type"`
	Transaction string    `json:"transaction"`
}
type textPaymentEvent struct {
	ID      string        `json:"id"`
	Request string        `json:"request_id"`
	Status  string        `json:"status"`
	Units   int           `json:"units"`
	Order   textPaidOrder `json:"order"`
	Product struct {
		ID       string `json:"id"`
		Billing  string `json:"billing_type"`
		Currency string `json:"currency"`
		Price    int64  `json:"price"`
	} `json:"product"`
}

func (s *PaymentService) HandleCreemEvent(ctx context.Context, raw []byte, signature string) error {
	c := s.creemClient()
	if !c.validSignature(raw, signature) {
		return infraerrors.Forbidden("INVALID_SIGNATURE", "invalid webhook signature")
	}
	var event struct {
		ID     string          `json:"id"`
		Type   string          `json:"eventType"`
		Object json.RawMessage `json:"object"`
	}
	if json.Unmarshal(raw, &event) != nil || event.ID == "" {
		return infraerrors.BadRequest("INVALID_WEBHOOK", "invalid event")
	}
	switch event.Type {
	case "checkout.completed":
		var v textPaymentEvent
		if err := json.Unmarshal(event.Object, &v); err != nil {
			return err
		}
		return s.deliverTextPurchase(ctx, v)
	case "refund.created":
		var v textRefundEvent
		if err := json.Unmarshal(event.Object, &v); err != nil {
			return err
		}
		return s.confirmTextRefundEvent(ctx, v)
	default:
		return nil
	}
}

func (s *PaymentService) deliverTextPurchase(ctx context.Context, event textPaymentEvent) error {
	tx, err := s.entClient.Tx(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	o, err := tx.PaymentOrder.Query().Where(paymentorder.OutTradeNoEQ(event.Request), paymentorder.OrderTypeEQ(TextVoucherOrder)).ForUpdate().Only(ctx)
	if err != nil {
		return err
	}
	ord := event.Order
	base := int64(o.Amount * 100)
	product, _ := o.ProviderSnapshot["product"].(string)
	correctProduct := product != "" && event.Units == 1 && event.Product.Billing == "onetime" &&
		event.Product.ID == product && event.Product.Price == base && event.Product.Currency == "USD"
	correctAmount := ord.Amount == base && ord.Subtotal == base && ord.Discount == 0 && ord.Tax >= 0 &&
		ord.Paid == ord.Due && (ord.Paid == base || ord.Paid == base+ord.Tax)
	paid := ord.Type == "onetime" && ord.Status == "paid" && ord.Product == product && ord.Currency == "USD" && ord.ID != "" && ord.Transaction != ""
	checkoutMatches := event.Status == "completed" && event.ID != "" && (o.PaymentTradeNo == "" || o.PaymentTradeNo == event.ID)
	if !correctProduct || !correctAmount || !paid || !checkoutMatches || o.ProviderSnapshot["mode"] != s.creemClient().mode {
		return infraerrors.Conflict("PAYMENT_MISMATCH", "checkout does not match the purchased product")
	}
	// The signed checkout.completed snapshot contains the paid order's update time.
	// A manually resent event has a new envelope timestamp.
	now := time.Now()
	if ord.UpdatedAt.IsZero() || ord.UpdatedAt.After(now) {
		return infraerrors.Conflict("PAYMENT_TIME_UNVERIFIED", "missing or future paid order timestamp")
	}
	if o.PaidAt != nil {
		rows, err := tx.QueryContext(ctx, "SELECT 1 FROM text_purchase_vouchers WHERE order_id=$1 AND transaction_id=$2 AND merchant_order_id=$3 AND paid_cents=$4", o.ID, ord.Transaction, ord.ID, ord.Paid)
		if err != nil {
			return err
		}
		matched := rows.Next()
		if err = rows.Close(); err != nil {
			return err
		}
		if !matched {
			return infraerrors.Conflict("PAYMENT_MISMATCH", "payment already fulfilled by another transaction")
		}
		// Earlier releases used the local fulfillment time. A matching replay may
		// correct it backwards but can never extend the refund period.
		if o.CompletedAt != nil && o.PaidAt.Equal(*o.CompletedAt) && ord.UpdatedAt.Before(*o.PaidAt) {
			if err = tx.PaymentOrder.UpdateOneID(o.ID).SetPaidAt(ord.UpdatedAt).Exec(ctx); err != nil {
				return err
			}
			return tx.Commit()
		}
		return nil
	}
	if o.Status != OrderStatusPending && o.Status != OrderStatusExpired && o.Status != OrderStatusCancelled {
		return infraerrors.Conflict("INVALID_STATUS", "order cannot receive payment")
	}
	var random [14]byte
	if _, err = rand.Read(random[:]); err != nil {
		return err
	}
	code := "TXT-" + hex.EncodeToString(random[:])
	rc, err := tx.RedeemCode.Create().SetCode(code).SetType(TextVoucherOrder).SetValue(o.Amount).SetStatus(StatusUnused).Save(ctx)
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, "INSERT INTO text_purchase_vouchers(order_id,user_id,redeem_id,transaction_id,merchant_order_id,paid_cents,points) VALUES($1,$2,$3,$4,$5,$6,$7)", o.ID, o.UserID, rc.ID, ord.Transaction, ord.ID, ord.Paid, o.Amount)
	if err != nil {
		return err
	}
	_, err = tx.PaymentOrder.UpdateOneID(o.ID).SetPaidAt(ord.UpdatedAt).SetCompletedAt(now).SetStatus(OrderStatusCompleted).SetPaymentTradeNo(event.ID).SetPayAmount(float64(ord.Paid) / 100).ClearPayURL().SetRechargeCode(code).Save(ctx)
	if err != nil {
		return err
	}
	return tx.Commit()
}

func lockTextBuyer(ctx context.Context, tx *dbent.Tx, id int64) error {
	rows, err := tx.QueryContext(ctx, "SELECT id FROM users WHERE id=$1 AND deleted_at IS NULL FOR UPDATE", id)
	if err != nil {
		return err
	}
	defer func() { _ = rows.Close() }()
	if !rows.Next() {
		return sql.ErrNoRows
	}
	return rows.Err()
}
func claimTextPurchase(ctx context.Context, tx *dbent.Tx, uid, redeemID int64) error {
	if err := lockTextBuyer(ctx, tx, uid); err != nil {
		return err
	}
	res, err := tx.ExecContext(ctx, "UPDATE text_purchase_vouchers SET redeemed_at=now() WHERE user_id=$1 AND redeem_id=$2 AND NOT refund_hold AND redeemed_at IS NULL", uid, redeemID)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n != 1 {
		return ErrRedeemCodeNotFound
	}
	return nil
}
