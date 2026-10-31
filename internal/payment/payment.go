// Package payment implements payment providers and verified notification handling.
package payment

import (
	"context"
	"errors"
	"net/http"
)

var (
	ErrProviderUnavailable = errors.New("provider unavailable")
	ErrNotifyUnsupported   = errors.New("public notification unsupported")
	ErrInvalidRequest      = errors.New("invalid payment request")
	ErrInvalidNotice       = errors.New("invalid payment notice")
	ErrInvalidSignature    = errors.New("invalid payment signature")
	ErrMerchantMismatch    = errors.New("payment merchant mismatch")
	ErrAmountMismatch      = errors.New("payment amount mismatch")
	ErrNotPaid             = errors.New("payment not successful")
	ErrOrderNotFound       = errors.New("order not found")
	ErrOrderNotPayable     = errors.New("order not payable")
	ErrTradeConflict       = errors.New("payment trade already used")
)

type Provider interface {
	Create(ctx context.Context, req CreateRequest) (Payment, error)
	Notify(r *http.Request) (Notice, error)
}

type CreateRequest struct {
	OrderNo     string
	ProductName string
	AmountCents int64
	NotifyURL   string
	ReturnURL   string
	PayType     string
}

type Payment struct {
	Type         string `json:"type"`
	URL          string `json:"url,omitempty"`
	QR           string `json:"qr,omitempty"`
	Instructions string `json:"instructions,omitempty"`
}

type Notice struct {
	OrderNo     string
	TradeNo     string
	AmountCents int64
	Paid        bool
	Raw         string
	verified    bool
}

// PaidMarker owns the order transition and idempotent fulfillment scheduling.
// It must return nil for an already-paid order, without fulfilling it again.
type PaidMarker interface {
	MarkPaid(ctx context.Context, orderID int64, providerTradeNo string) error
}

type PaidMarkerFunc func(ctx context.Context, orderID int64, providerTradeNo string) error

func (f PaidMarkerFunc) MarkPaid(ctx context.Context, orderID int64, providerTradeNo string) error {
	return f(ctx, orderID, providerTradeNo)
}
