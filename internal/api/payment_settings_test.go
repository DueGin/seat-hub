package api

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

type queriedOrders struct {
	fakeOrders
	order OrderDTO
}

func (o *queriedOrders) Get(context.Context, string) (OrderDTO, error) { return o.order, nil }

type queryingPayments struct {
	fakePayments
	orders *queriedOrders
	calls  int
	err    error
}

func (p *queryingPayments) QueryPending(context.Context, OrderDTO) error {
	p.calls++
	if p.err == nil {
		p.orders.order.Status = "paid"
	}
	return p.err
}

func TestOrderReadQueriesOnlyOwnedPendingAlipay(t *testing.T) {
	for _, tc := range []struct {
		name, provider, status string
		owner                  int64
		queryErr               error
		wantStatus, wantCalls  int
		wantOrder              string
	}{
		{"paid fallback", "alipay_f2f", "pending", 1, nil, 200, 1, "paid"},
		{"query failure keeps read", "alipay_f2f", "pending", 1, errors.New("gateway unavailable"), 200, 1, "pending"},
		{"other user", "alipay_f2f", "pending", 2, nil, 404, 0, ""},
		{"already paid", "alipay_f2f", "paid", 1, nil, 200, 0, "paid"},
		{"other method", "epay", "pending", 1, nil, 200, 0, "pending"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			orders := &queriedOrders{order: OrderDTO{OrderNo: "PT1", UserID: tc.owner, Status: tc.status, Provider: tc.provider}}
			payments := &queryingPayments{orders: orders, err: tc.queryErr}
			s := &Server{Deps: Deps{Auth: &fakeAuth{}, Orders: orders, Payments: payments}, Log: slog.New(slog.NewTextHandler(io.Discard, nil))}
			r := httptest.NewRequest(http.MethodGet, "/api/orders/PT1", nil)
			r.SetPathValue("order_no", "PT1")
			r = r.WithContext(context.WithValue(r.Context(), fakeUserKey, UserDTO{ID: 1, Role: "user"}))
			w := httptest.NewRecorder()
			s.handleGetOrder(w, r)
			if w.Code != tc.wantStatus || payments.calls != tc.wantCalls {
				t.Fatalf("read = %d, queries = %d", w.Code, payments.calls)
			}
			if tc.wantOrder != "" {
				var result struct {
					Data struct {
						Order OrderDTO `json:"order"`
					} `json:"data"`
				}
				if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil {
					t.Fatal(err)
				}
				if result.Data.Order.Status != tc.wantOrder {
					t.Fatalf("order status = %s", result.Data.Order.Status)
				}
			}
		})
	}
}

func TestPaymentSettingsMaskMergeAndActive(t *testing.T) {
	previous := json.RawMessage(`{"active":"epay","epay":{"key":"epay-secret","pid":"old"},"alipay_f2f":{"private_key":"app-secret","alipay_public_key":"public","app_id":"123"},"manual":{"instructions":"转账说明"}}`)
	for _, secret := range []string{"***", ""} {
		patch, _ := json.Marshal(map[string]any{"active": "alipay_f2f", "epay": map[string]any{"key": secret}, "alipay_f2f": map[string]any{"private_key": secret, "sandbox": true}})
		merged, err := adminMergePayment(previous, patch)
		if err != nil {
			t.Fatal(err)
		}
		var doc map[string]any
		if err := json.Unmarshal(merged, &doc); err != nil {
			t.Fatal(err)
		}
		if doc["active"] != "alipay_f2f" || doc["epay"].(map[string]any)["key"] != "epay-secret" || doc["alipay_f2f"].(map[string]any)["private_key"] != "app-secret" || doc["manual"].(map[string]any)["instructions"] != "转账说明" {
			t.Fatalf("incorrect merged config")
		}
		masked, err := adminMaskPayment(merged)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(masked), "epay-secret") || strings.Contains(string(masked), "app-secret") || !strings.Contains(string(masked), `"private_key":"***"`) {
			t.Fatal("payment secret leaked")
		}
		if !strings.Contains(string(masked), `"alipay_public_key":"public"`) {
			t.Fatal("public key should remain editable")
		}
	}
	for _, raw := range []string{`{"active":"unknown"}`, `{"active":{}}`, `{"epay":null}`, `[]`, `null`} {
		if _, err := adminPaymentFields(json.RawMessage(raw)); err == nil {
			t.Fatalf("accepted %s", raw)
		}
	}
	if _, err := adminMergePayment(previous, json.RawMessage(`{"alipay_f2f":{"private_key":true}}`)); err == nil {
		t.Fatal("accepted non-string secret")
	}
}
