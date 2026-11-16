package payment

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func alipayTestKey(t *testing.T) *rsa.PrivateKey {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	return key
}

func alipayTestConfig(t *testing.T, app, gateway *rsa.PrivateKey) AlipayF2FConfig {
	t.Helper()
	public, err := x509.MarshalPKIXPublicKey(&gateway.PublicKey)
	if err != nil {
		t.Fatal(err)
	}
	return AlipayF2FConfig{AppID: "2026111600000001", PrivateKey: base64.StdEncoding.EncodeToString(x509.MarshalPKCS1PrivateKey(app)), AlipayPublicKey: base64.StdEncoding.EncodeToString(public)}
}

func alipayTestProvider(t *testing.T, app, gateway *rsa.PrivateKey) *AlipayF2F {
	t.Helper()
	p, err := NewAlipayF2F(alipayTestConfig(t, app, gateway))
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func alipaySignedNotice(t *testing.T, key *rsa.PrivateKey, change func(url.Values)) *http.Request {
	t.Helper()
	params := url.Values{"app_id": {"2026111600000001"}, "out_trade_no": {"PT1"}, "trade_no": {"alipay-1"}, "total_amount": {"1.23"}, "trade_status": {"TRADE_SUCCESS"}, "sign_type": {"RSA2"}, "subject": {"并发包 + 测试&=你好"}}
	if change != nil {
		change(params)
	}
	sign, err := signAlipay(key, alipaySigningString(params, true))
	if err != nil {
		t.Fatal(err)
	}
	params.Set("sign", sign)
	req := httptest.NewRequest(http.MethodPost, "/api/payments/alipay_f2f/notify", strings.NewReader(params.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	return req
}

// The gateway checks the real wire request and signs the exact JSON bytes returned.
func alipayFakeGateway(t *testing.T, p *AlipayF2F, gateway *rsa.PrivateKey, answer func(url.Values, map[string]string) string) {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			t.Error(err)
			w.WriteHeader(400)
			return
		}
		params := r.PostForm
		if err := verifyAlipay(&p.privateKey.PublicKey, []byte(alipaySigningString(params, false)), params.Get("sign")); err != nil {
			t.Error("invalid request signature")
			w.WriteHeader(400)
			return
		}
		for name, expected := range map[string]string{"app_id": p.appID, "charset": "utf-8", "sign_type": "RSA2", "version": "1.0", "format": "JSON"} {
			if params.Get(name) != expected {
				t.Errorf("%s = %q", name, params.Get(name))
			}
		}
		if _, err := time.ParseInLocation("2006-01-02 15:04:05", params.Get("timestamp"), shanghai); err != nil {
			t.Error(err)
		}
		var biz map[string]string
		if err := json.Unmarshal([]byte(params.Get("biz_content")), &biz); err != nil {
			t.Error(err)
			w.WriteHeader(400)
			return
		}
		raw := answer(params, biz)
		sign, err := signAlipay(gateway, raw)
		if err != nil {
			t.Error(err)
			w.WriteHeader(500)
			return
		}
		name := strings.ReplaceAll(params.Get("method"), ".", "_") + "_response"
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprintf(w, `{%q:%s,"sign":%q}`, name, raw, sign)
	}))
	t.Cleanup(server.Close)
	p.gateway, p.client = server.URL, server.Client()
}

func TestAlipayPrecreateAndErrors(t *testing.T) {
	app, gateway := alipayTestKey(t), alipayTestKey(t)
	p := alipayTestProvider(t, app, gateway)
	now := time.Date(2026, 11, 16, 2, 3, 4, 0, time.UTC)
	p.now = func() time.Time { return now }
	var failure atomic.Bool
	alipayFakeGateway(t, p, gateway, func(params url.Values, biz map[string]string) string {
		if params.Get("method") != "alipay.trade.precreate" || params.Get("timestamp") != "2026-11-16 10:03:04" || params.Get("notify_url") != "https://example.com/api/payments/alipay_f2f/notify" {
			t.Errorf("request fields = %v", params)
		}
		if biz["out_trade_no"] != "PT1" || biz["total_amount"] != "12.34" || biz["subject"] != "中文商品" || biz["time_expire"] != "2026-11-16 10:33:04" {
			t.Errorf("biz = %v", biz)
		}
		if failure.Load() {
			return `{"code":"40004","sub_code":"ACQ.ACCESS_FORBIDDEN","sub_msg":"无权限"}`
		}

		return `{ "code" : "10000", "out_trade_no":"PT1", "qr_code":"https://qr.alipay.com/test" }`
	})
	req := CreateRequest{OrderNo: "PT1", ProductName: "中文商品", AmountCents: 1234, NotifyURL: "https://example.com/api/payments/alipay_f2f/notify", ExpiresAt: now.Add(30 * time.Minute)}
	got, err := p.Create(context.Background(), req)
	if err != nil || got.Type != "qrcode" || got.QR != "https://qr.alipay.com/test" {
		t.Fatalf("precreate = %+v, %v", got, err)
	}
	failure.Store(true)
	_, err = p.Create(context.Background(), req)
	var rejected *GatewayError
	if !errors.As(err, &rejected) || rejected.Code != "40004" || rejected.SubCode != "ACQ.ACCESS_FORBIDDEN" || !strings.Contains(err.Error(), "权限") {
		t.Fatalf("gateway error = %v", err)
	}
	failure.Store(false)
	p.publicKey = &app.PublicKey
	if _, err := p.Create(context.Background(), req); !errors.Is(err, ErrInvalidSignature) {
		t.Fatalf("bad response signature = %v", err)
	}
}

func TestAlipayNotifyValidationAndIdempotency(t *testing.T) {
	app, gateway := alipayTestKey(t), alipayTestKey(t)
	cases := []struct {
		name   string
		change func(url.Values)
		key    *rsa.PrivateKey
		want   error
	}{
		{"success", nil, gateway, nil},
		{"finished", func(v url.Values) { v.Set("trade_status", "TRADE_FINISHED") }, gateway, nil},
		{"bad signature", nil, app, ErrInvalidSignature},
		{"wrong app", func(v url.Values) { v.Set("app_id", "111") }, gateway, ErrMerchantMismatch},
		{"wrong amount", func(v url.Values) { v.Set("total_amount", "1.24") }, gateway, ErrAmountMismatch},
		{"pending", func(v url.Values) { v.Set("trade_status", "WAIT_BUYER_PAY") }, gateway, ErrNotPaid},
		{"no trade", func(v url.Values) { v.Del("trade_no") }, gateway, ErrInvalidNotice},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newServiceFixture(t)
			f.exec(t, `UPDATE orders SET provider = 'alipay_f2f'`)
			f.svc.providers["alipay_f2f"] = alipayTestProvider(t, app, gateway)
			f.svc.active = "manual"
			if _, err := f.svc.Create(context.Background(), "alipay_f2f", CreateRequest{OrderNo: "PT1", ProductName: "测试", AmountCents: 123}); !errors.Is(err, ErrProviderUnavailable) {
				t.Fatalf("inactive create = %v", err)
			}
			_, err := f.svc.Notify(context.Background(), "alipay_f2f", alipaySignedNotice(t, tc.key, tc.change))
			if !errors.Is(err, tc.want) {
				t.Fatalf("notify = %v; want %v", err, tc.want)
			}
			if tc.want != nil {
				if f.calls.Load() != 0 {
					t.Fatal("rejected notice settled order")
				}
				return
			}
			if _, err := f.svc.Notify(context.Background(), "alipay_f2f", alipaySignedNotice(t, tc.key, tc.change)); err != nil {
				t.Fatal(err)
			}
			if f.calls.Load() != 1 {
				t.Fatalf("marker calls = %d", f.calls.Load())
			}
			f.assertEvent(t, "duplicate", true)
			f.addOrder(t, "PT2", "alipay_f2f")
			if _, err := f.svc.Notify(context.Background(), "alipay_f2f", alipaySignedNotice(t, gateway, func(v url.Values) { v.Set("out_trade_no", "PT2") })); !errors.Is(err, ErrTradeConflict) {
				t.Fatalf("reused trade = %v", err)
			}
		})
	}
}

func TestAlipayQueryFallbackThrottleAndClose(t *testing.T) {
	f := newServiceFixture(t)
	f.exec(t, `UPDATE orders SET provider = 'alipay_f2f'`)
	app, gateway := alipayTestKey(t), alipayTestKey(t)
	p := alipayTestProvider(t, app, gateway)
	f.svc.providers["alipay_f2f"] = p
	var queries, closes atomic.Int64
	var paid atomic.Bool
	alipayFakeGateway(t, p, gateway, func(params url.Values, biz map[string]string) string {
		if biz["out_trade_no"] != "PT1" {
			t.Error("wrong queried order")
		}
		if params.Get("method") == "alipay.trade.close" {
			closes.Add(1)
			return `{"code":"10000"}`
		}
		if params.Get("method") != "alipay.trade.query" {
			t.Error("wrong query method")
		}
		queries.Add(1)
		status := "WAIT_BUYER_PAY"
		if paid.Load() {
			status = "TRADE_SUCCESS"
		}
		return fmt.Sprintf(`{"code":"10000","out_trade_no":"PT1","trade_no":"alipay-1","total_amount":"1.23","trade_status":%q}`, status)
	})
	if err := f.svc.QueryPending(context.Background(), "PT1"); !errors.Is(err, ErrNotPaid) {
		t.Fatal(err)
	}
	paid.Store(true)
	for range 4 {
		if err := f.svc.QueryPending(context.Background(), "PT1"); err != nil {
			t.Fatal(err)
		}
	}
	if queries.Load() != 1 || f.calls.Load() != 0 {
		t.Fatal("query not throttled")
	}
	f.now = f.now.Add(5 * time.Second)
	if err := f.svc.QueryPending(context.Background(), "PT1"); err != nil {
		t.Fatal(err)
	}
	if queries.Load() != 2 || f.calls.Load() != 1 {
		t.Fatalf("queries=%d marker=%d", queries.Load(), f.calls.Load())
	}
	if _, err := f.svc.Notify(context.Background(), "alipay_f2f", alipaySignedNotice(t, gateway, nil)); err != nil {
		t.Fatal(err)
	}
	if f.calls.Load() != 1 {
		t.Fatal("query/notify settled twice")
	}
	if err := f.svc.Close(context.Background(), "alipay_f2f", "PT1"); err != nil || closes.Load() != 1 {
		t.Fatalf("close = %v, calls = %d", err, closes.Load())
	}
}

func TestAlipayKeyFormatsAndSigningVector(t *testing.T) {
	key := alipayTestKey(t)
	pkcs8, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	for _, der := range [][]byte{x509.MarshalPKCS1PrivateKey(key), pkcs8} {
		for _, text := range []string{string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der})), base64.StdEncoding.EncodeToString(der)} {
			parsed, err := parseAlipayPrivateKey(text)
			if err != nil || !key.Equal(parsed) {
				t.Fatalf("parse private key = %v", err)
			}
		}
	}
	pkix, err := x509.MarshalPKIXPublicKey(&key.PublicKey)
	if err != nil {
		t.Fatal(err)
	}
	for _, der := range [][]byte{pkix, x509.MarshalPKCS1PublicKey(&key.PublicKey)} {
		for _, text := range []string{string(pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: der})), base64.StdEncoding.EncodeToString(der)} {
			parsed, err := parseAlipayPublicKey(text)
			if err != nil || !key.PublicKey.Equal(parsed) {
				t.Fatalf("parse public key = %v", err)
			}
		}
	}
	if _, err := parseAlipayPrivateKey("***"); err == nil {
		t.Fatal("accepted invalid private key")
	}
	if _, err := parseAlipayPublicKey("***"); err == nil {
		t.Fatal("accepted invalid public key")
	}
	params := url.Values{"subject": {"测试 + A&B"}, "app_id": {"2026111600000001"}, "sign_type": {"RSA2"}, "sign": {"ignored"}, "empty": {""}, "total_amount": {"12.34"}}
	const want = "app_id=2026111600000001&subject=测试 + A&B&total_amount=12.34"
	if got := alipaySigningString(params, true); got != want {
		t.Fatalf("signing string = %q", got)
	}
	if got := alipaySigningString(params, false); got != "app_id=2026111600000001&sign_type=RSA2&subject=测试 + A&B&total_amount=12.34" {
		t.Fatalf("request signing string = %q", got)
	}
}

// This disposable fixture and expected RSA2 signature were generated with OpenSSL.
func TestAlipayFixedRSA2Vector(t *testing.T) {
	raw, err := os.ReadFile("testdata/alipay_vector_private.pem")
	if err != nil {
		t.Fatal(err)
	}
	key, err := parseAlipayPrivateKey(string(raw))
	if err != nil {
		t.Fatal(err)
	}
	params := url.Values{"subject": {"测试 + A&B"}, "app_id": {"2026111600000001"}, "sign_type": {"RSA2"}, "sign": {"ignored"}, "empty": {""}, "total_amount": {"12.34"}}
	const canonical = "app_id=2026111600000001&subject=测试 + A&B&total_amount=12.34"
	if got := alipaySigningString(params, true); got != canonical {
		t.Fatalf("signing string = %q", got)
	}
	const expected = "WdZvgjMuQ2CU9pqmEGLjV0SzhuntDvpp5w/fr1nUwEmEKkX7IsinAUGUl6JTRLVl/dxQVx4sanYeDU0y/oONf+fnj/668Rr07b6AJ68chllm7gLoMQb0MwWI5nVmRn27Fi2ENQzJyk9TDHHISCrrQcTWtLk/XEZkgIsyupogGGT2bjiXXca/VAQbwZT6405MNDS+w7mTLcDFeCQl6R3V4lBeFEtkxbQqbk5X5aJM/rwhdG7PXnaWr74mNUUMNksNNYDAKS93vnkpEX/y4Z848qs3DrS3AwkgTgjQ2hAgmaTesTwwFGsJwHdGPCf1olKtfbIiRUNdnSQomy+Ue9EuVg=="
	actual, err := signAlipay(key, canonical)
	if err != nil || actual != expected {
		t.Fatalf("fixed RSA2 vector mismatch: %v", err)
	}
	if err := verifyAlipay(&key.PublicKey, []byte(canonical), expected); err != nil {
		t.Fatal(err)
	}
}
