package app

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"sync"

	"github.com/DueGin/vm2api-portal/internal/api"
	"github.com/DueGin/vm2api-portal/internal/order"
	"github.com/DueGin/vm2api-portal/internal/payment"
)

// paymentAdapter bridges payment.Service to api.PaymentService.
//
// Provider enablement is read from the admin settings document on every call so
// changing it takes effect without a restart.
type paymentAdapter struct {
	db           *sql.DB
	marker       payment.PaidMarker
	settings     *settingsAdapter
	dev          bool
	callbackBase string // e.g. https://portal.example.com, no trailing slash

	// svc is rebuilt whenever the stored payment document changes, so editing
	// the merchant credentials in the admin console takes effect without a
	// restart. The provider list and the charge path must agree on what is
	// enabled, or an order is created for a provider that cannot charge.
	mu      sync.Mutex
	svc     *payment.Service
	svcHash string
}

// service returns a payment service matching the current settings.
func (a *paymentAdapter) service(ctx context.Context) (*payment.Service, error) {
	raw := deepcopyRaw(a.rawPayment(ctx))
	sum := sha256.Sum256(raw)
	hash := hex.EncodeToString(sum[:])
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.svc != nil && a.svcHash == hash {
		return a.svc, nil
	}
	providers, err := buildProvidersFrom(a.db, a.rawPayment(ctx), a.dev)
	if err != nil {
		return nil, err
	}
	svc, err := payment.NewService(a.db, a.marker, payment.ServiceOptions{Providers: providers, Dev: a.dev})
	if err != nil {
		// Keep the previous service rather than breaking charging entirely.
		if a.svc != nil {
			return a.svc, nil
		}
		return nil, err
	}
	a.svc, a.svcHash = svc, hash
	return svc, nil
}

func (a *paymentAdapter) rawPayment(ctx context.Context) []byte {
	site, err := a.settings.Get(ctx)
	if err != nil {
		return nil
	}
	return site.Payment
}

func deepcopyRaw(raw []byte) []byte { return append([]byte(nil), raw...) }

func (a *paymentAdapter) Providers(ctx context.Context) []api.PaymentProviderDTO {
	cfg := a.paymentConfig(ctx)
	out := []api.PaymentProviderDTO{}
	if enabled(cfg, "epay") {
		out = append(out, api.PaymentProviderDTO{ID: "epay", Name: "在线支付"})
	}
	if _, raw := cfg["manual"]; !raw || enabled(cfg, "manual") {
		// Manual is opt-out: it stays available unless the admin explicitly
		// disables it.
		out = append(out, api.PaymentProviderDTO{ID: "manual", Name: "人工转账"})
	}
	if a.dev {
		out = append(out, api.PaymentProviderDTO{ID: "mock", Name: "模拟支付"})
	}
	return out
}

// paymentConfig returns the stored payment document, parsing the shape the
// admin settings page writes ({"epay":{"enabled":…}, "manual":{…}}).
func decodeDoc(raw []byte) map[string]any {
	if len(raw) == 0 {
		return map[string]any{}
	}
	var doc map[string]any
	if err := json.Unmarshal(raw, &doc); err != nil || doc == nil {
		return map[string]any{}
	}
	return doc
}

func (a *paymentAdapter) paymentConfig(ctx context.Context) map[string]any {
	site, err := a.settings.Get(ctx)
	if err != nil || len(site.Payment) == 0 {
		return map[string]any{}
	}
	return decodeDoc(site.Payment)
}

func enabled(cfg map[string]any, provider string) bool {
	entry, ok := cfg[provider].(map[string]any)
	if !ok {
		return false
	}
	v, ok := entry["enabled"].(bool)
	return ok && v
}

func (a *paymentAdapter) CreatePayment(ctx context.Context, o api.OrderDTO) (api.PaymentDTO, error) {
	svc, err := a.service(ctx)
	if err != nil {
		return api.PaymentDTO{}, mapPaymentErr(err)
	}
	p, err := svc.Create(ctx, o.Provider, payment.CreateRequest{
		OrderNo:     o.OrderNo,
		ProductName: o.ProductName,
		AmountCents: o.AmountCents,
		NotifyURL:   a.notifyURL(o.Provider),
		ReturnURL:   a.returnURL(o.Provider),
	})
	if err != nil {
		return api.PaymentDTO{}, mapPaymentErr(err)
	}
	return api.PaymentDTO{Type: p.Type, URL: p.URL, QR: p.QR, Instructions: p.Instructions}, nil
}

func (a *paymentAdapter) Notify(ctx context.Context, provider string, r *http.Request) (int, string, error) {
	svc, svcErr := a.service(ctx)
	if svcErr != nil {
		return http.StatusBadRequest, "fail", svcErr
	}
	notice, err := svc.Notify(ctx, provider, r)
	if err != nil {
		return http.StatusBadRequest, "fail", err
	}
	_ = notice
	return http.StatusOK, "success", nil
}

// ReturnURL sends the browser back to the order page after a redirect payment.
func (a *paymentAdapter) MockPay(ctx context.Context, userID int64, orderNo string) error {
	svc, err := a.service(ctx)
	if err != nil {
		return mapPaymentErr(err)
	}
	if err := svc.MockPay(ctx, userID, orderNo); err != nil {
		return mapPaymentErr(err)
	}
	return nil
}

func (a *paymentAdapter) ReturnURL(ctx context.Context, provider string, r *http.Request) (string, error) {
	orderNo := strings.TrimSpace(r.URL.Query().Get("out_trade_no"))
	if orderNo == "" {
		orderNo = strings.TrimSpace(r.URL.Query().Get("order_no"))
	}
	if orderNo == "" {
		return "/orders", nil
	}
	return "/orders/" + orderNo, nil
}

func (a *paymentAdapter) notifyURL(provider string) string {
	return a.callbackBase + "/api/payments/" + provider + "/notify"
}
func (a *paymentAdapter) returnURL(provider string) string {
	return a.callbackBase + "/api/payments/" + provider + "/return"
}

func mapPaymentErr(err error) error {
	switch {
	case errors.Is(err, payment.ErrProviderUnavailable):
		return api.Err(err, 422, "provider_unavailable", "支付方式不可用")
	case errors.Is(err, payment.ErrOrderNotFound):
		return api.Err(err, 404, "order_not_found", "订单不存在")
	case errors.Is(err, payment.ErrOrderNotPayable):
		return api.Err(err, 409, "order_not_payable", "订单当前状态不可支付")
	case errors.Is(err, payment.ErrAmountMismatch):
		return api.Err(err, 400, "amount_mismatch", "支付金额不匹配")
	case errors.Is(err, payment.ErrInvalidSignature):
		return api.Err(err, 400, "invalid_signature", "支付回调签名校验失败")
	case errors.Is(err, payment.ErrInvalidRequest):
		return api.Err(err, 422, "invalid_payment", "支付参数不合法")
	case errors.Is(err, payment.ErrNotPaid):
		return api.Err(err, 400, "payment_not_successful", "支付未成功")
	}
	return err
}

// orderFulfiller adapts the provisioning service to order.Fulfiller.
type orderFulfiller struct {
	apply func(ctx context.Context, orderID int64) error
}

func (f orderFulfiller) ApplyPaidOrder(ctx context.Context, orderID int64) error {
	if f.apply == nil {
		return errors.New("fulfiller not wired")
	}
	return f.apply(ctx, orderID)
}

var _ order.Fulfiller = orderFulfiller{}
