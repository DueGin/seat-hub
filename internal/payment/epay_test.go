package payment

import (
	"context"
	"errors"
	"math"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

func testEPay(t *testing.T) *EPay {
	t.Helper()
	p, err := NewEPay(EPayConfig{GatewayURL: "https://pay.example.com/gateway/", PID: "1001", Key: "secret", Types: []string{"alipay", "wxpay"}})
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func testCreateRequest() CreateRequest {
	return CreateRequest{OrderNo: "PT1", ProductName: "并发包 & 30天", AmountCents: 123,
		NotifyURL: "https://portal.example.com/api/payments/epay/notify", ReturnURL: "https://portal.example.com/api/payments/epay/return?order_no=PT1"}
}

func testNoticeParams() url.Values {
	return url.Values{
		"pid": {"1001"}, "trade_no": {"gateway-1"}, "out_trade_no": {"PT1"},
		"money": {"1.23"}, "trade_status": {"TRADE_SUCCESS"}, "sign_type": {"MD5"},
	}
}

func signedRequest(params url.Values, method string) *http.Request {
	params.Set("sign", epaySignature(params, "secret"))
	if method == http.MethodPost {
		r := httptest.NewRequest(method, "/notify", strings.NewReader(params.Encode()))
		r.Header.Set("Content-Type", "application/x-www-form-urlencoded; charset=UTF-8")
		return r
	}
	return httptest.NewRequest(method, "/notify?"+params.Encode(), nil)
}

func TestEPaySignature(t *testing.T) {
	params := url.Values{"pid": {"1001"}, "money": {"1.23"}, "out_trade_no": {"PT1"}, "type": {"alipay"}, "empty": {""}, "sign": {"ignored"}, "sign_type": {"MD5"}}
	if got := epaySignature(params, "secret"); got != "630b4207106fb413a9c1ecf71ec84941" {
		t.Fatalf("signature = %s", got)
	}
}

func TestEPayCreate(t *testing.T) {
	p := testEPay(t)
	for _, cents := range []int64{0, 1, 99, 100, 123, 12345, math.MaxInt64} {
		req := testCreateRequest()
		req.AmountCents = cents
		req.PayType = "wxpay"
		payment, err := p.Create(context.Background(), req)
		if err != nil {
			t.Fatal(err)
		}
		u, err := url.Parse(payment.URL)
		if err != nil {
			t.Fatal(err)
		}
		params := u.Query()
		if payment.Type != "redirect" || u.Scheme != "https" || u.Host != "pay.example.com" || u.Path != "/gateway/submit.php" {
			t.Fatalf("unexpected payment: %+v", payment)
		}
		if params.Get("name") != req.ProductName || params.Get("notify_url") != req.NotifyURL || params.Get("return_url") != req.ReturnURL || params.Get("out_trade_no") != req.OrderNo || params.Get("pid") != "1001" || params.Get("type") != "wxpay" {
			t.Fatalf("unexpected submit parameters: %v", params)
		}
		if params.Get("money") != formatCents(cents) || params.Get("sign") != epaySignature(params, "secret") || params.Get("sign_type") != "MD5" {
			t.Fatalf("invalid amount or signature: %v", params)
		}
		parsed, err := parseCents(params.Get("money"))
		if err != nil || parsed != cents {
			t.Fatalf("amount did not round trip: %d, %v", parsed, err)
		}
	}
	payment, err := p.Create(context.Background(), testCreateRequest())
	if err != nil {
		t.Fatal(err)
	}
	u, _ := url.Parse(payment.URL)
	if u.Query().Get("type") != "alipay" || u.Query().Get("money") != "1.23" {
		t.Fatalf("unexpected defaults: %s", payment.URL)
	}
}

func TestEPayConfigValidation(t *testing.T) {
	for _, gateway := range []string{"", "javascript:alert(1)", "/relative", "https://user:password@example.com", "https://example.com/#fragment", "https://example.com/?pid=attacker"} {
		if _, err := NewEPay(EPayConfig{GatewayURL: gateway, PID: "1", Key: "key"}); err == nil {
			t.Fatalf("accepted invalid gateway %q", gateway)
		}
	}
	for _, cfg := range []EPayConfig{
		{GatewayURL: "https://pay.example.com", Key: "secret"},
		{GatewayURL: "https://pay.example.com", PID: "1"},
		{GatewayURL: "https://pay.example.com", PID: "1", Key: "secret", Types: []string{""}},
	} {
		if _, err := NewEPay(cfg); !errors.Is(err, ErrInvalidRequest) {
			t.Fatalf("invalid config error = %v", err)
		}
	}
	p, err := NewEPay(EPayConfig{GatewayURL: "https://pay.example.com/submit.php", PID: "1", Key: "secret"})
	if err != nil || p.submitURL.Path != "/submit.php" {
		t.Fatalf("submit.php duplicated: %+v, %v", p, err)
	}
}

func TestEPayCreateValidation(t *testing.T) {
	p := testEPay(t)
	for _, mutate := range []func(*CreateRequest){
		func(r *CreateRequest) { r.OrderNo = "" },
		func(r *CreateRequest) { r.ProductName = " " },
		func(r *CreateRequest) { r.AmountCents = -1 },
		func(r *CreateRequest) { r.NotifyURL = "/relative" },
		func(r *CreateRequest) { r.ReturnURL = "javascript:bad" },
		func(r *CreateRequest) { r.PayType = "unknown" },
	} {
		req := testCreateRequest()
		mutate(&req)
		if _, err := p.Create(context.Background(), req); !errors.Is(err, ErrInvalidRequest) {
			t.Fatalf("invalid create error = %v", err)
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := p.Create(ctx, testCreateRequest()); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled create error = %v", err)
	}
}

func TestEPayNotify(t *testing.T) {
	p := testEPay(t)
	for _, method := range []string{http.MethodGet, http.MethodPost} {
		t.Run(method, func(t *testing.T) {
			params := testNoticeParams()
			notice, err := p.Notify(signedRequest(params, method))
			if err != nil {
				t.Fatal(err)
			}
			if notice.OrderNo != "PT1" || notice.TradeNo != "gateway-1" || notice.AmountCents != 123 || !notice.Paid || !notice.verified || notice.Raw != params.Encode() {
				t.Fatalf("notice = %+v", notice)
			}
		})
	}
	params := testNoticeParams()
	r := signedRequest(params, http.MethodGet)
	r.URL.RawQuery = strings.ReplaceAll(r.URL.RawQuery, params.Get("sign"), strings.ToUpper(params.Get("sign")))
	if _, err := p.Notify(r); err != nil {
		t.Fatalf("uppercase hex signature: %v", err)
	}
}

func TestEPayNotifyRejectsInvalid(t *testing.T) {
	p := testEPay(t)
	cases := []struct {
		name     string
		mutate   func(url.Values)
		want     error
		verified bool
	}{
		{"wrong merchant", func(p url.Values) { p.Set("pid", "2002") }, ErrMerchantMismatch, true},
		{"pending", func(p url.Values) { p.Set("trade_status", "WAIT_BUYER_PAY") }, ErrNotPaid, true},
		{"missing status", func(p url.Values) { p.Del("trade_status") }, ErrNotPaid, true},
		{"wrong signature algorithm", func(p url.Values) { p.Set("sign_type", "SHA256") }, ErrInvalidSignature, false},
		{"missing order", func(p url.Values) { p.Del("out_trade_no") }, ErrInvalidNotice, true},
		{"missing trade", func(p url.Values) { p.Del("trade_no") }, ErrInvalidNotice, true},
		{"negative amount", func(p url.Values) { p.Set("money", "-1.23") }, ErrInvalidNotice, true},
		{"fractional cent", func(p url.Values) { p.Set("money", "1.231") }, ErrInvalidNotice, true},
		{"overflow amount", func(p url.Values) { p.Set("money", "92233720368547758.08") }, ErrInvalidNotice, true},
		{"repeated parameter", func(p url.Values) { p.Add("money", "999") }, ErrInvalidNotice, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			params := testNoticeParams()
			tc.mutate(params)
			notice, err := p.Notify(signedRequest(params, http.MethodGet))
			if !errors.Is(err, tc.want) || notice.verified != tc.verified || notice.Paid {
				t.Fatalf("notice = %+v, error = %v; want %v, verified %v", notice, err, tc.want, tc.verified)
			}
		})
	}
	for _, signature := range []string{"", "bad", strings.Repeat("0", 32), strings.Repeat("f", 34)} {
		params := testNoticeParams()
		params.Set("sign", signature)
		if _, err := p.Notify(httptest.NewRequest(http.MethodGet, "/?"+params.Encode(), nil)); !errors.Is(err, ErrInvalidSignature) {
			t.Fatalf("accepted signature %q: %v", signature, err)
		}
	}
	params := testNoticeParams()
	r := signedRequest(params, http.MethodGet)
	r.URL.RawQuery = strings.ReplaceAll(r.URL.RawQuery, "money=1.23", "money=9.99")
	r.Form = params
	if _, err := p.Notify(r); !errors.Is(err, ErrInvalidSignature) {
		t.Fatalf("cached form bypassed tamper detection: %v", err)
	}
}

func TestEPayNotifyMalformedRequest(t *testing.T) {
	p := testEPay(t)
	postJSON := httptest.NewRequest(http.MethodPost, "/notify", strings.NewReader("{}"))
	postJSON.Header.Set("Content-Type", "application/json")
	oversized := httptest.NewRequest(http.MethodPost, "/notify", strings.NewReader(strings.Repeat("x", maxNoticeBytes+1)))
	oversized.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	polluted := signedRequest(testNoticeParams(), http.MethodPost)
	polluted.URL.RawQuery = "money=1.23"
	for _, r := range []*http.Request{
		nil,
		{Method: http.MethodGet},
		httptest.NewRequest(http.MethodPut, "/notify", nil),
		httptest.NewRequest(http.MethodGet, "/notify?bad=%zz", nil),
		httptest.NewRequest(http.MethodGet, "/notify?"+strings.Repeat("x", maxNoticeBytes+1), nil),
		postJSON, oversized, polluted,
	} {
		if _, err := p.Notify(r); !errors.Is(err, ErrInvalidNotice) {
			t.Fatalf("malformed request accepted: %v", err)
		}
	}
}

func TestParseCents(t *testing.T) {
	for value, want := range map[string]int64{"0": 0, "0.00": 0, "0.01": 1, "1.2": 120, "01.23": 123, "999": 99900, "92233720368547758.07": math.MaxInt64} {
		if got, err := parseCents(value); err != nil || got != want {
			t.Errorf("parseCents(%q) = %d, %v; want %d", value, got, err, want)
		}
	}
	for _, value := range []string{"", ".01", "1.", "-1", "+1", "1e2", "NaN", " 1.23", "1.23 ", "1.234", "1..2", "1.2.3", "1,23", "92233720368547758.08", "9223372036854775808", "一"} {
		if _, err := parseCents(value); err == nil {
			t.Errorf("accepted invalid amount %q", value)
		}
	}
}
