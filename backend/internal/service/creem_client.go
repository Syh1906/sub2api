package service

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"
)

const TextVoucherOrder = "text_voucher"

var textTiers = [...]int{10, 20, 50, 100}

type creemClient struct {
	base, key, webhook, mode string
	products                 map[int]string
	http                     *http.Client
}

func loadCreem() *creemClient {
	mode := os.Getenv("CREEM_MODE")
	base := map[string]string{"test": "https://test-api.creem.io", "prod": "https://api.creem.io"}[mode]
	c := &creemClient{base: base, mode: mode, key: os.Getenv("CREEM_API_KEY"), webhook: os.Getenv("CREEM_WEBHOOK_SECRET"), products: map[int]string{}, http: &http.Client{Timeout: 20 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}}
	for _, n := range textTiers {
		c.products[n] = strings.TrimSpace(os.Getenv("CREEM_PRODUCT_" + strconv.Itoa(n)))
	}
	return c
}
func (s *PaymentService) creemClient() *creemClient {
	return loadCreem()
}
func (c *creemClient) ready(n int) bool {
	return c.base != "" && c.key != "" && c.webhook != "" && c.products[n] != ""
}

type TextOffer struct {
	USD       int  `json:"usd"`
	Points    int  `json:"points"`
	Available bool `json:"available"`
}

func (s *PaymentService) TextOffers(ctx context.Context) ([]TextOffer, error) {
	c := s.creemClient()
	enabled := false
	if c.base != "" && c.key != "" && c.webhook != "" {
		cfg, err := s.configService.GetPaymentConfig(ctx)
		if err != nil {
			return nil, err
		}
		enabled = cfg.Enabled
	}
	offers := make([]TextOffer, 0, len(textTiers))
	for _, n := range textTiers {
		offers = append(offers, TextOffer{n, n, enabled && c.ready(n)})
	}
	return offers, nil
}
func (c *creemClient) call(ctx context.Context, method, path string, input, output any) error {
	if c.base == "" || c.key == "" {
		return fmt.Errorf("creem is not configured")
	}
	var body io.Reader
	if input != nil {
		encoded, err := json.Marshal(input)
		if err != nil {
			return err
		}
		body = bytes.NewReader(encoded)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.base+path, body)
	if err != nil {
		return err
	}
	req.Header.Set("x-api-key", c.key)
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("creem request did not complete: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("creem returned HTTP %d", resp.StatusCode)
	}
	return json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(output)
}

type creemCheckout struct {
	ID  string `json:"id"`
	URL string `json:"checkout_url"`
}

func (c *creemClient) checkout(ctx context.Context, product, trade, success string) (creemCheckout, error) {
	var result creemCheckout
	err := c.call(ctx, "POST", "/v1/checkouts", map[string]any{"product_id": product, "units": 1, "request_id": trade, "success_url": success}, &result)
	if err != nil {
		return result, err
	}
	u, err := url.Parse(result.URL)
	if result.ID == "" || err != nil || u.Scheme != "https" || u.Host == "" || u.User != nil {
		return result, fmt.Errorf("invalid hosted checkout response")
	}
	return result, nil
}
func (c *creemClient) validSignature(body []byte, signature string) bool {
	if c.webhook == "" || c.base == "" {
		return false
	}
	got, err := hex.DecodeString(signature)
	if err != nil {
		return false
	}
	mac := hmac.New(sha256.New, []byte(c.webhook))
	_, _ = mac.Write(body)
	return hmac.Equal(mac.Sum(nil), got)
}
