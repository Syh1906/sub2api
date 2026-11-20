package service

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strconv"
	"time"

	dbent "github.com/Wei-Shaw/sub2api/ent"
	"github.com/Wei-Shaw/sub2api/ent/paymentorder"

	infraerrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
)

func (req *CreateOrderRequest) preparePurchaseIdentity() error {
	key, err := NormalizeIdempotencyKey(req.PurchaseKey)
	if err != nil || key == "" {
		return err
	}
	req.purchaseKeyHash = HashIdempotencyKey(key)
	// 设备、IP 和显示语言可在重新登录后变化，订单的商品、支付方式及返回站点须相同。
	payload := struct {
		Amount                                                float64
		PlanID                                                int64
		OrderType, PaymentType, ReturnURL, SourceHost, OpenID string
	}{req.Amount, req.PlanID, req.OrderType, req.PaymentType, req.ReturnURL, req.SrcHost, req.OpenID}
	req.purchaseRequestHash, err = BuildIdempotencyFingerprint("POST", "/payment/orders", strconv.FormatInt(req.UserID, 10), payload)
	return err
}

func (s *PaymentService) findPurchase(ctx context.Context, req CreateOrderRequest) (*CreateOrderResponse, error) {
	if req.purchaseKeyHash == "" {
		return nil, nil
	}
	order, err := s.entClient.PaymentOrder.Query().Where(paymentorder.UserIDEQ(req.UserID), paymentorder.PurchaseKeyHashEQ(req.purchaseKeyHash)).Only(ctx)
	if dbent.IsNotFound(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if order.PurchaseRequestHash == nil || *order.PurchaseRequestHash != req.purchaseRequestHash {
		return nil, infraerrors.Conflict("PURCHASE_KEY_CONFLICT", "purchase key already used for a different order")
	}
	return s.checkoutForOrder(order)
}

func (s *PaymentService) LookupPurchase(ctx context.Context, userID int64, rawKey string) (*CreateOrderResponse, error) {
	key, err := NormalizeIdempotencyKey(rawKey)
	if err != nil {
		return nil, err
	}
	if key == "" {
		return nil, infraerrors.BadRequest("PURCHASE_KEY_REQUIRED", "missing purchase key")
	}
	order, err := s.entClient.PaymentOrder.Query().Where(paymentorder.UserIDEQ(userID), paymentorder.PurchaseKeyHashEQ(HashIdempotencyKey(key))).Only(ctx)
	if dbent.IsNotFound(err) {
		return nil, infraerrors.NotFound("PURCHASE_NOT_FOUND", "purchase not found")
	}
	if err != nil {
		return nil, err
	}
	return s.checkoutForOrder(order)
}

// ResumeCheckout 只读取本人的现有订单，不向支付商重新创建交易。
func (s *PaymentService) ResumeCheckout(ctx context.Context, id, userID int64) (*CreateOrderResponse, error) {
	order, err := s.GetOrder(ctx, id, userID)
	if err != nil {
		return nil, err
	}
	return s.checkoutForOrder(order)
}

func (s *PaymentService) checkoutForOrder(order *dbent.PaymentOrder) (*CreateOrderResponse, error) {
	result := &CreateOrderResponse{
		OrderID: order.ID, OutTradeNo: order.OutTradeNo,
		Amount: order.Amount, PayAmount: order.PayAmount,
		BonusAmount: order.BonusAmount, FeeRate: order.FeeRate,
		Currency: PaymentOrderCurrency(order), PaymentType: order.PaymentType,
		ExpiresAt: order.ExpiresAt, Status: order.Status,
	}
	if result.Status != OrderStatusPending {
		return result, nil
	}
	if !order.ExpiresAt.After(time.Now()) {
		result.Status = OrderStatusExpired
		return result, nil
	}
	if order.CheckoutCiphertext != nil {
		for _, key := range s.paymentResume().verifyKeys {
			ciphertext, err := base64.RawStdEncoding.DecodeString(*order.CheckoutCiphertext)
			if err != nil {
				return nil, fmt.Errorf("invalid checkout encoding")
			}
			aead, err := checkoutCipher(key, order.ID)
			if err != nil {
				return nil, err
			}
			if len(ciphertext) < aead.Overhead() {
				return nil, fmt.Errorf("incomplete checkout session")
			}
			decoded, err := aead.Open(nil, nil, ciphertext, nil)
			if err != nil {
				continue
			}
			var saved CreateOrderResponse
			if err := json.Unmarshal(decoded, &saved); err != nil {
				return nil, err
			}
			if saved.OrderID != order.ID {
				return nil, fmt.Errorf("checkout order mismatch")
			}
			return &saved, nil
		}
		return nil, infraerrors.Conflict("CHECKOUT_UNAVAILABLE", "checkout session cannot be restored")
	}
	if order.PayURL != nil {
		result.PayURL = *order.PayURL
	}
	if order.QrCode != nil {
		result.QRCode = *order.QrCode
	}
	return result, nil
}

func checkoutCipher(secret []byte, orderID int64) (cipher.AEAD, error) {
	mac := hmac.New(sha256.New, secret)
	_, _ = fmt.Fprintf(mac, "payment-checkout-v1/%d", orderID)
	block, err := aes.NewCipher(mac.Sum(nil))
	if err != nil {
		return nil, err
	}
	return cipher.NewGCMWithRandomNonce(block)
}

func (s *PaymentService) sealCheckout(result *CreateOrderResponse) (string, error) {
	resume := s.paymentResume()
	// 没有恢复签名配置时，原设备仍可使用支付商返回的数据。
	if !resume.isSigningConfigured() {
		return "", nil
	}
	raw, err := json.Marshal(result)
	if err != nil {
		return "", err
	}
	aead, err := checkoutCipher(resume.signingKey, result.OrderID)
	if err != nil {
		return "", err
	}
	// NewGCMWithRandomNonce generates a fresh nonce internally and requires an empty nonce argument.
	sealed := aead.Seal(nil, nil, raw, nil) //nolint:gosec // G407: the AEAD generates and prepends the random nonce.
	return base64.RawStdEncoding.EncodeToString(sealed), nil
}
