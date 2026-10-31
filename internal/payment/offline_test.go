package payment

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
)

var (
	_ Provider   = (*EPay)(nil)
	_ Provider   = (*Manual)(nil)
	_ Provider   = (*Mock)(nil)
	_ PaidMarker = PaidMarkerFunc(nil)
)

func TestOfflineProviders(t *testing.T) {
	manual := NewManual("请按订单金额转账，备注订单号。")
	mock, err := NewMock(true)
	if err != nil {
		t.Fatal(err)
	}
	for name, provider := range map[string]Provider{"manual": manual, "mock": mock} {
		t.Run(name, func(t *testing.T) {
			payment, err := provider.Create(context.Background(), testCreateRequest())
			if err != nil || payment.Type != name || payment.Instructions == "" || payment.URL != "" || payment.QR != "" {
				t.Fatalf("create = %+v, %v", payment, err)
			}
			if _, err := provider.Notify(httptest.NewRequest(http.MethodGet, "/notify?paid=true", nil)); !errors.Is(err, ErrNotifyUnsupported) {
				t.Fatalf("public notify error = %v", err)
			}
			if _, err := provider.Notify(nil); !errors.Is(err, ErrNotifyUnsupported) {
				t.Fatalf("nil notify error = %v", err)
			}
			req := testCreateRequest()
			req.AmountCents = -1
			if _, err := provider.Create(context.Background(), req); !errors.Is(err, ErrInvalidRequest) {
				t.Fatalf("negative amount error = %v", err)
			}
			ctx, cancel := context.WithCancel(context.Background())
			cancel()
			if _, err := provider.Create(ctx, testCreateRequest()); !errors.Is(err, context.Canceled) {
				t.Fatalf("canceled create error = %v", err)
			}
		})
	}
	if manual.instructions != "请按订单金额转账，备注订单号。" || NewManual(" ").instructions == "" {
		t.Fatal("manual instructions not preserved or defaulted")
	}
	if _, err := NewMock(false); !errors.Is(err, ErrProviderUnavailable) {
		t.Fatalf("production constructor error = %v", err)
	}
	if _, err := (&Mock{}).Create(context.Background(), testCreateRequest()); !errors.Is(err, ErrProviderUnavailable) {
		t.Fatalf("zero-value mock bypasses dev guard: %v", err)
	}
}
