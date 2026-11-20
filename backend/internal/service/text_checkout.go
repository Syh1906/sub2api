package service

import (
	"context"
	"encoding/json"
	"net/url"
	"strconv"
	"strings"
	"time"

	"entgo.io/ent/dialect/sql"
	"entgo.io/ent/dialect/sql/sqljson"
	dbent "github.com/Wei-Shaw/sub2api/ent"
	"github.com/Wei-Shaw/sub2api/ent/paymentorder"
	infraerrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
)

// TextCheckoutState describes a missing link without publishing the private checkout snapshot.
// Missing state on an older order cannot prove that the merchant request was never sent.
func TextCheckoutState(o *dbent.PaymentOrder) string {
	if o.OrderType != TextVoucherOrder || o.Status != OrderStatusPending || o.PayURL != nil {
		return ""
	}
	switch o.ProviderSnapshot["checkout_phase"] {
	case "ready":
		return "ready"
	case "waiting_payment":
		return "waiting_payment"
	default:
		return "needs_review"
	}
}

func (s *PaymentService) LookupTextReference(ctx context.Context, userID int64, requestID string) (int64, error) {
	o, err := s.entClient.PaymentOrder.Query().Where(paymentorder.UserIDEQ(userID), paymentorder.OrderTypeEQ(TextVoucherOrder),
		paymentorder.OutTradeNoEQ(requestID)).Only(ctx)
	if dbent.IsNotFound(err) {
		return 0, infraerrors.NotFound("NOT_FOUND", "order not found")
	}
	if err != nil {
		return 0, err
	}
	return o.ID, nil
}

// ContinueTextCheckout is an explicit purchase action. GET lookup and resume remain read-only.
func (s *PaymentService) ContinueTextCheckout(ctx context.Context, id, userID int64) (*CreateOrderResponse, error) {
	o, err := s.GetOrder(ctx, id, userID)
	if err != nil {
		return nil, err
	}
	if o.OrderType != TextVoucherOrder {
		return nil, infraerrors.BadRequest("INVALID_ORDER_TYPE", "not a text purchase")
	}
	return s.startTextCheckout(ctx, o)
}

func (s *PaymentService) startTextCheckout(ctx context.Context, o *dbent.PaymentOrder) (*CreateOrderResponse, error) {
	if TextCheckoutState(o) != "ready" || !o.ExpiresAt.After(time.Now()) {
		return s.checkoutForOrder(ctx, o)
	}
	c := s.creemClient()
	product, _ := o.ProviderSnapshot["product"].(string)
	ret, _ := o.ProviderSnapshot["return_url"].(string)
	if ret == "" || !c.ready(int(o.Amount)) || product != c.products[int(o.Amount)] || o.ProviderSnapshot["mode"] != c.mode {
		return nil, infraerrors.Conflict("TEXT_OFFER_UNAVAILABLE", "original checkout product is unavailable")
	}
	cfg, err := s.configService.GetPaymentConfig(ctx)
	if err != nil {
		return nil, err
	}
	if !cfg.Enabled {
		return nil, infraerrors.Forbidden("PAYMENT_DISABLED", "payment system is disabled")
	}
	// ret was validated and stripped of all query parameters before order creation.
	success := ret + "?local_order_id=" + strconv.FormatInt(o.ID, 10)
	snapshot := o.ProviderSnapshot
	snapshot["checkout_phase"] = "dispatching"
	n, err := s.entClient.PaymentOrder.Update().Where(paymentorder.ID(o.ID), paymentorder.StatusEQ(OrderStatusPending),
		paymentorder.ExpiresAtGT(time.Now()), paymentorder.PayURLIsNil(), func(sel *sql.Selector) {
			sel.Where(sqljson.ValueEQ(paymentorder.FieldProviderSnapshot, "ready", sqljson.Path("checkout_phase")))
		}).SetProviderSnapshot(snapshot).Save(ctx)
	if err != nil {
		return nil, err
	}
	if n == 0 {
		return s.ResumeCheckout(ctx, o.ID, o.UserID)
	}
	hosted, callErr := c.checkout(ctx, product, o.OutTradeNo, success)
	// If the browser disconnects after dispatch, we still have the response and can
	// preserve it for another device. A process loss is resolved by merchant lookup.
	saveCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	if callErr != nil {
		if creemRequestRejected(callErr) || creemRequestNotSent(callErr) {
			snapshot["checkout_phase"] = "ready"
			if _, err := s.entClient.PaymentOrder.Update().Where(paymentorder.ID(o.ID), paymentorder.StatusEQ(OrderStatusPending), paymentorder.PayURLIsNil(), paymentorder.PaymentTradeNoEQ("")).SetProviderSnapshot(snapshot).Save(saveCtx); err != nil {
				return nil, err
			}
			return nil, infraerrors.New(502, "CHECKOUT_REJECTED", "checkout did not start; retry the original order")
		}
		// Without a checkout ID Creem cannot search by our request_id. Never resend.
		return s.ResumeCheckout(saveCtx, o.ID, o.UserID)
	}
	if _, err = s.entClient.PaymentOrder.Update().Where(paymentorder.ID(o.ID), paymentorder.StatusEQ(OrderStatusPending), paymentorder.PayURLIsNil(), paymentorder.PaymentTradeNoEQ("")).SetPayURL(hosted.URL).SetPaymentTradeNo(hosted.ID).Save(saveCtx); err != nil {
		return nil, err
	}
	return s.ResumeCheckout(saveCtx, o.ID, o.UserID)
}

type creemCheckoutLookup struct {
	creemCheckout
	Request string          `json:"request_id"`
	Product json.RawMessage `json:"product"`
	Units   int             `json:"units"`
	Status  string          `json:"status"`
}

// CloseUnsentTextCheckout requires the operator's merchant confirmation. Local expiry,
// an HTTP timeout, and a missing search result do not establish non-creation.
func (s *PaymentService) CloseUnsentTextCheckout(ctx context.Context, id int64, evidence, actor string) error {
	evidence = strings.TrimSpace(evidence)
	if evidence == "" || len(evidence) > 256 {
		return infraerrors.BadRequest("MERCHANT_REFERENCE_REQUIRED", "merchant confirmation reference required")
	}
	o, err := s.GetOrderByID(ctx, id)
	if err != nil {
		return err
	}
	if TextCheckoutState(o) != "needs_review" || o.PaymentTradeNo != "" {
		return infraerrors.Conflict("CHECKOUT_UNAVAILABLE", "no unconfirmed checkout to close")
	}
	// Allow the outgoing HTTP call and its local save deadline to finish first.
	if time.Now().Before(o.UpdatedAt.Add(30 * time.Second)) {
		return infraerrors.Conflict("CHECKOUT_IN_PROGRESS", "checkout request may still be in progress")
	}
	tx, err := s.entClient.Tx(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	n, err := tx.PaymentOrder.Update().Where(paymentorder.ID(id), paymentorder.StatusEQ(OrderStatusPending), paymentorder.PayURLIsNil(),
		paymentorder.PaymentTradeNoEQ(""), paymentorder.UpdatedAtEQ(o.UpdatedAt)).SetStatus(OrderStatusCancelled).Save(ctx)
	if err != nil {
		return err
	}
	if n != 1 {
		return infraerrors.Conflict("CHECKOUT_CHANGED", "refresh the order")
	}
	details, err := json.Marshal(map[string]string{"merchant_confirmation": evidence})
	if err != nil {
		return err
	}
	err = tx.PaymentAuditLog.Create().SetOrderID(strconv.FormatInt(id, 10)).SetAction("TEXT_CHECKOUT_NOT_CREATED").SetOperator(actor).SetDetail(string(details)).Exec(ctx)
	if err != nil {
		return err
	}
	return tx.Commit()
}

// ReconcileTextCheckout only attaches a session read from the configured merchant
// account. An operator-supplied URL or payment status is never accepted.
func (s *PaymentService) ReconcileTextCheckout(ctx context.Context, id int64, checkoutID, actor string) error {
	o, err := s.GetOrderByID(ctx, id)
	if err != nil {
		return err
	}
	checkoutID = strings.TrimSpace(checkoutID)
	if checkoutID == "" || len(checkoutID) > 200 {
		return infraerrors.BadRequest("CHECKOUT_ID_REQUIRED", "merchant checkout ID required")
	}
	if o.OrderType != TextVoucherOrder || o.Status != OrderStatusPending || (o.PaymentTradeNo != "" && o.PaymentTradeNo != checkoutID) {
		return infraerrors.Conflict("CHECKOUT_UNAVAILABLE", "order cannot accept this checkout")
	}
	if o.PayURL != nil && o.PaymentTradeNo == checkoutID {
		return nil
	}
	if TextCheckoutState(o) == "ready" {
		return infraerrors.Conflict("CHECKOUT_NOT_SENT", "checkout was not sent")
	}
	remote, err := s.readOriginalTextCheckout(ctx, o, checkoutID)
	if err != nil {
		return err
	}
	update := s.entClient.PaymentOrder.Update().Where(paymentorder.ID(o.ID), paymentorder.StatusEQ(OrderStatusPending), paymentorder.PayURLIsNil(),
		paymentorder.Or(paymentorder.PaymentTradeNoEQ(""), paymentorder.PaymentTradeNoEQ(checkoutID))).SetPaymentTradeNo(checkoutID)
	switch remote.Status {
	case "pending":
		if !o.ExpiresAt.After(time.Now()) {
			// The local payment window has ended. Keep the reference for late callbacks.
			update.SetStatus(OrderStatusExpired)
			break
		}
		if !remote.validURL() {
			return infraerrors.Conflict("CHECKOUT_UNAVAILABLE", "invalid merchant checkout URL")
		}
		update.SetPayURL(remote.URL)
	case "processing", "completed":
		o.ProviderSnapshot["checkout_phase"] = "waiting_payment"
		update.SetProviderSnapshot(o.ProviderSnapshot)
	case "expired":
		update.SetStatus(OrderStatusExpired)
	default:
		return infraerrors.Conflict("CHECKOUT_UNAVAILABLE", "unknown merchant checkout status")
	}
	n, err := update.Save(ctx)
	if err != nil {
		return err
	}
	if n == 0 {
		return infraerrors.Conflict("CHECKOUT_CHANGED", "refresh the order")
	}
	s.writeAuditLog(ctx, id, "TEXT_CHECKOUT_RECONCILED", actor, map[string]any{"checkout_id": checkoutID, "status": remote.Status})
	return nil
}

func (s *PaymentService) readOriginalTextCheckout(ctx context.Context, o *dbent.PaymentOrder, checkoutID string) (*creemCheckoutLookup, error) {
	c := s.creemClient()
	if c.mode != o.ProviderSnapshot["mode"] {
		return nil, infraerrors.Conflict("CREEM_UNAVAILABLE", "wrong merchant environment")
	}
	var remote creemCheckoutLookup
	if err := c.call(ctx, "GET", "/v1/checkouts?checkout_id="+url.QueryEscape(checkoutID), nil, &remote); err != nil {
		return nil, err
	}
	var productID string
	if err := json.Unmarshal(remote.Product, &productID); err != nil {
		var product struct {
			ID string `json:"id"`
		}
		if err := json.Unmarshal(remote.Product, &product); err != nil {
			return nil, err
		}
		productID = product.ID
	}
	if remote.ID != checkoutID || remote.Request != o.OutTradeNo || productID == "" || productID != o.ProviderSnapshot["product"] || remote.Units != 1 {
		return nil, infraerrors.Conflict("CHECKOUT_MISMATCH", "merchant checkout does not match this purchase")
	}
	return &remote, nil
}
