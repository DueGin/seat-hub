package app

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/DueGin/seat-hub/internal/api"
	"github.com/DueGin/seat-hub/internal/store"
)

func TestActivePaymentMigrationAndConfiguredCallbacks(t *testing.T) {
	for _, tc := range []struct{ raw, want string }{
		{`{}`, "manual"},
		{`{"epay":{"enabled":true}}`, "manual"},
		{`{"epay":{"enabled":true,"gateway_url":"https://pay.example.com","pid":"1001","key":"secret"}}`, "epay"},
		{`{"epay":{"enabled":false,"gateway_url":"https://pay.example.com","pid":"1001","key":"secret"}}`, "manual"},
		{`{"active":"manual","epay":{"enabled":true,"gateway_url":"https://pay.example.com","pid":"1001","key":"secret"}}`, "manual"},
	} {
		if got := activePayment(decodeDoc([]byte(tc.raw))); got != tc.want {
			t.Errorf("active = %q; want %q", got, tc.want)
		}
	}
	providers, err := buildProvidersFrom(nil, []byte(`{"active":"manual","epay":{"enabled":false,"gateway_url":"https://pay.example.com","pid":"1001","key":"secret"}}`), false)
	if err != nil || providers["epay"] == nil || providers["manual"] == nil {
		t.Fatalf("configured callback providers missing: %v", err)
	}
}

func TestPaymentSettingsValidationAndSecretPreservationHTTP(t *testing.T) {
	h := newHarness(t)
	h.login("admin@example.com", "admin-pass-123")
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	publicDER, err := x509.MarshalPKIXPublicKey(&key.PublicKey)
	if err != nil {
		t.Fatal(err)
	}
	private := base64.StdEncoding.EncodeToString(x509.MarshalPKCS1PrivateKey(key))
	public := base64.StdEncoding.EncodeToString(publicDER)
	put := func(doc map[string]any) (int, envelope) {
		raw, _ := json.Marshal(map[string]any{"payment": doc})
		return h.call(http.MethodPut, "/api/admin/settings", string(raw), nil)
	}
	for _, tc := range []struct {
		doc   map[string]any
		field string
	}{
		{map[string]any{"active": "alipay_f2f"}, "AppID"},
		{map[string]any{"active": "alipay_f2f", "alipay_f2f": map[string]any{"app_id": "123"}}, "应用私钥"},
		{map[string]any{"active": "alipay_f2f", "alipay_f2f": map[string]any{"app_id": "123", "private_key": private}}, "支付宝公钥"},
		{map[string]any{"active": "alipay_f2f", "alipay_f2f": map[string]any{"app_id": "123", "private_key": "invalid", "alipay_public_key": public}}, "应用私钥格式"},
		{map[string]any{"active": "alipay_f2f", "alipay_f2f": map[string]any{"app_id": "123", "private_key": private, "alipay_public_key": "invalid"}}, "支付宝公钥格式"},
	} {
		status, env := put(tc.doc)
		if status != 422 || env.Error == nil || env.Error.Code != "invalid_settings" || !strings.Contains(env.Error.Message, tc.field) {
			t.Fatalf("field %s: %d %+v", tc.field, status, env.Error)
		}
	}
	status, env := put(map[string]any{"active": "alipay_f2f", "alipay_f2f": map[string]any{"app_id": "2026111600000001", "private_key": private, "alipay_public_key": public, "sandbox": true}, "epay": map[string]any{"gateway_url": "https://pay.example.com", "pid": "1001", "key": "epay-secret"}})
	if status != 200 || !env.OK {
		t.Fatalf("save alipay = %d %+v", status, env.Error)
	}
	if strings.Contains(string(env.Data), private) || strings.Contains(string(env.Data), "epay-secret") {
		t.Fatal("save response leaked a key")
	}
	status, env = h.call(http.MethodGet, "/api/admin/settings", "", nil)
	if status != 200 || strings.Contains(string(env.Data), private) || strings.Contains(string(env.Data), "epay-secret") || !strings.Contains(string(env.Data), `"private_key":"***"`) {
		t.Fatal("settings read did not mask private keys")
	}
	status, env = put(map[string]any{"active": "manual", "alipay_f2f": map[string]any{"private_key": "***"}, "epay": map[string]any{"key": ""}})
	if status != 200 || !env.OK {
		t.Fatalf("switch manual = %d %+v", status, env.Error)
	}
	var site store.Site
	if _, err := store.NewSettings(h.app.DB).Get(context.Background(), "site", &site); err != nil {
		t.Fatal(err)
	}
	doc := decodeDoc(site.Payment)
	if doc["active"] != "manual" || doc["alipay_f2f"].(map[string]any)["private_key"] != private || doc["epay"].(map[string]any)["key"] != "epay-secret" {
		t.Fatal("switch lost provider config")
	}
	var audit string
	rows, err := h.app.DB.Query(`SELECT detail FROM audit_log WHERE action = 'settings.update'`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	for rows.Next() {
		if err := rows.Scan(&audit); err != nil {
			t.Fatal(err)
		}
		if strings.Contains(audit, private) || strings.Contains(audit, "epay-secret") {
			t.Fatal("audit leaked a key")
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	rows.Close()
	var config api.SiteConfig
	h.mustOK(http.MethodGet, "/api/public/config", "", &config)
	if len(config.PaymentProviders) != 2 || config.PaymentProviders[0].ID != "manual" || config.PaymentProviders[1].ID != "mock" {
		t.Fatalf("providers = %+v", config.PaymentProviders)
	}
	var before, after int
	if err := h.app.DB.QueryRow(`SELECT COUNT(*) FROM orders`).Scan(&before); err != nil {
		t.Fatal(err)
	}
	status, env = h.call(http.MethodPost, "/api/orders", `{"product_id":1,"quantity":1,"provider":"epay"}`, nil)
	if status != 422 || env.Error == nil || env.Error.Code != "payment_provider_unavailable" || env.Error.Message != "当前不支持该收款方式" {
		t.Fatalf("inactive create = %d %+v", status, env.Error)
	}
	if err := h.app.DB.QueryRow(`SELECT COUNT(*) FROM orders`).Scan(&after); err != nil {
		t.Fatal(err)
	}
	if before != after {
		t.Fatal("inactive provider reserved an order")
	}
}

func TestPaymentValidationRequiresOnlyActiveConfig(t *testing.T) {
	if err := validatePayment([]byte(`{"active":"manual","manual":{"instructions":"转账说明"},"epay":{"gateway_url":"bad"},"alipay_f2f":{"private_key":"bad"}}`)); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct{ raw, field string }{
		{`{"active":"epay"}`, "网关地址"},
		{`{"active":"epay","epay":{"gateway_url":"https://pay.example.com"}}`, "商户编号"},
		{`{"active":"epay","epay":{"gateway_url":"https://pay.example.com","pid":"1001"}}`, "密钥"},
		{`{"active":"manual"}`, "转账说明"},
	} {
		var userErr *api.UserError
		err := validatePayment([]byte(tc.raw))
		if !errors.As(err, &userErr) || userErr.Status != 422 || !strings.Contains(userErr.Message, tc.field) {
			t.Fatalf("%s: %v", tc.field, err)
		}
	}
	merged, err := mergeSecrets(json.RawMessage(`{"active":"epay","epay":{"key":"stored","pid":"1001"},"manual":{"instructions":"说明"}}`), json.RawMessage(`{"active":"manual","epay":{"key":"***"}}`))
	if err != nil {
		t.Fatal(err)
	}
	if got := decodeDoc(merged); got["epay"].(map[string]any)["key"] != "stored" || got["manual"] == nil {
		t.Fatal(fmt.Sprint(got))
	}
}
