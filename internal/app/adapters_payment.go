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
	"time"

	"github.com/DueGin/seat-hub/internal/api"
	"github.com/DueGin/seat-hub/internal/order"
	"github.com/DueGin/seat-hub/internal/payment"
)

// paymentAdapter bridges payment.Service to api.PaymentService.
//
// The active provider is read from the admin settings document on every call so
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
	providers, err := buildProvidersFrom(a.db, raw, a.dev)
	if err != nil {
		return nil, err
	}
	svc, err := payment.NewService(a.db, a.marker, payment.ServiceOptions{Providers: providers, Dev: a.dev, Active: activePayment(decodeDoc(raw))})
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
	raw := a.rawPayment(ctx)
	cfg := decodeDoc(raw)
	out := []api.PaymentProviderDTO{}
	active := activePayment(cfg)
	names := map[string]string{"epay": "易支付", "alipay_f2f": "支付宝当面付", "manual": "人工转账"}
	providers, _ := buildProvidersFrom(a.db, raw, a.dev)
	if providers[active] != nil {
		out = append(out, api.PaymentProviderDTO{ID: active, Name: names[active]})
	}
	if a.dev {
		out = append(out, api.PaymentProviderDTO{ID: "mock", Name: "模拟支付"})
	}
	return out
}

// decodeDoc tolerates missing documents from older deployments.
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

func (a *paymentAdapter) QueryPending(ctx context.Context, o api.OrderDTO) error {
	if o.Provider != "alipay_f2f" || o.Status != "pending" {
		return nil
	}
	svc, err := a.service(ctx)
	if err != nil {
		return err
	}
	return svc.QueryPending(ctx, o.OrderNo)
}

func (a *paymentAdapter) CloseOrder(ctx context.Context, provider, orderNo string) error {
	svc, err := a.service(ctx)
	if err != nil {
		return err
	}
	closeCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	return svc.Close(closeCtx, provider, orderNo)
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
		return api.Err(err, 422, "payment_provider_unavailable", "当前不支持该收款方式")
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
	var gateway *payment.GatewayError
	if errors.As(err, &gateway) {
		return &api.UserError{Status: 422, Code: "payment_gateway_error", Message: gateway.Message}
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
