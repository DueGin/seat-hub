package payment

import (
	"context"
	"crypto/md5"
	"crypto/subtle"
	"encoding/hex"
	"fmt"
	"io"
	"math"
	"mime"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
)

const maxNoticeBytes = 64 << 10

type EPayConfig struct {
	GatewayURL string
	PID        string
	Key        string
	Types      []string
}

type EPay struct {
	submitURL url.URL
	pid       string
	key       string
	types     []string
}

func NewEPay(cfg EPayConfig) (*EPay, error) {
	u, err := absoluteHTTPURL(cfg.GatewayURL)
	if err != nil {
		return nil, fmt.Errorf("invalid gateway url: %w", err)
	}
	if u.RawQuery != "" || u.ForceQuery {
		return nil, fmt.Errorf("gateway url must not contain a query: %w", ErrInvalidRequest)
	}
	if strings.TrimSpace(cfg.PID) == "" || strings.TrimSpace(cfg.Key) == "" {
		return nil, fmt.Errorf("merchant credentials required: %w", ErrInvalidRequest)
	}
	types := append([]string(nil), cfg.Types...)
	if len(types) == 0 {
		types = []string{"alipay"}
	}
	for _, kind := range types {
		if strings.TrimSpace(kind) == "" || kind != strings.TrimSpace(kind) {
			return nil, fmt.Errorf("invalid payment type: %w", ErrInvalidRequest)
		}
	}
	if !strings.HasSuffix(u.Path, "/submit.php") {
		u.Path = strings.TrimRight(u.Path, "/") + "/submit.php"
	}
	u.RawPath = ""
	return &EPay{submitURL: *u, pid: cfg.PID, key: cfg.Key, types: types}, nil
}

func (e *EPay) Create(ctx context.Context, req CreateRequest) (Payment, error) {
	if err := ctx.Err(); err != nil {
		return Payment{}, fmt.Errorf("create payment: %w", err)
	}
	if err := validateCreate(req); err != nil {
		return Payment{}, err
	}
	if _, err := absoluteHTTPURL(req.NotifyURL); err != nil {
		return Payment{}, fmt.Errorf("invalid notify url: %w", err)
	}
	if _, err := absoluteHTTPURL(req.ReturnURL); err != nil {
		return Payment{}, fmt.Errorf("invalid return url: %w", err)
	}
	kind := req.PayType
	if kind == "" {
		kind = e.types[0]
	}
	if !contains(e.types, kind) {
		return Payment{}, fmt.Errorf("payment type unavailable: %w", ErrInvalidRequest)
	}
	params := url.Values{
		"pid":          {e.pid},
		"type":         {kind},
		"out_trade_no": {req.OrderNo},
		"notify_url":   {req.NotifyURL},
		"return_url":   {req.ReturnURL},
		"name":         {req.ProductName},
		"money":        {formatCents(req.AmountCents)},
		"sign_type":    {"MD5"},
	}
	params.Set("sign", epaySignature(params, e.key))
	u := e.submitURL
	u.RawQuery = params.Encode()
	return Payment{Type: "redirect", URL: u.String()}, nil
}

func (e *EPay) Notify(r *http.Request) (Notice, error) {
	params, raw, err := noticeParams(r)
	notice := Notice{Raw: raw}
	if err != nil {
		return notice, err
	}
	notice.OrderNo = params.Get("out_trade_no")
	notice.TradeNo = params.Get("trade_no")
	if kind := params.Get("sign_type"); kind != "" && !strings.EqualFold(kind, "MD5") {
		return notice, ErrInvalidSignature
	}
	want, err := hex.DecodeString(epaySignature(params, e.key))
	if err != nil {
		return notice, fmt.Errorf("decode expected signature: %w", err)
	}
	got, err := hex.DecodeString(params.Get("sign"))
	if err != nil || subtle.ConstantTimeCompare(got, want) != 1 {
		return notice, ErrInvalidSignature
	}
	notice.verified = true
	if params.Get("pid") != e.pid {
		return notice, ErrMerchantMismatch
	}
	if strings.TrimSpace(notice.OrderNo) == "" || strings.TrimSpace(notice.TradeNo) == "" {
		return notice, ErrInvalidNotice
	}
	notice.AmountCents, err = parseCents(params.Get("money"))
	if err != nil {
		return notice, fmt.Errorf("invalid payment amount: %w", err)
	}
	if params.Get("trade_status") != "TRADE_SUCCESS" {
		return notice, ErrNotPaid
	}
	notice.Paid = true
	return notice, nil
}

func epaySignature(params url.Values, key string) string {
	keys := make([]string, 0, len(params))
	for name := range params {
		if name != "sign" && name != "sign_type" && params.Get(name) != "" {
			keys = append(keys, name)
		}
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, name := range keys {
		parts = append(parts, name+"="+params.Get(name))
	}
	sum := md5.Sum([]byte(strings.Join(parts, "&") + key))
	return hex.EncodeToString(sum[:])
}

// Parse the wire data ourselves so cached Request.Form cannot bypass verification.
func noticeParams(r *http.Request) (url.Values, string, error) {
	if r == nil || r.URL == nil {
		return nil, "", ErrInvalidNotice
	}
	raw := r.URL.RawQuery
	if len(raw) > maxNoticeBytes {
		return nil, raw[:maxNoticeBytes], ErrInvalidNotice
	}
	params, err := url.ParseQuery(raw)
	if err != nil {
		return nil, raw, fmt.Errorf("parse notification query: %w", ErrInvalidNotice)
	}
	switch r.Method {
	case http.MethodGet:
	case http.MethodPost:
		contentType, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
		if err != nil || contentType != "application/x-www-form-urlencoded" {
			return nil, raw, ErrInvalidNotice
		}
		if r.Body == nil {
			return nil, raw, ErrInvalidNotice
		}
		body, err := io.ReadAll(io.LimitReader(r.Body, int64(maxNoticeBytes-len(raw)+1)))
		if err != nil {
			return nil, raw, fmt.Errorf("read notification: %w", err)
		}
		combined := raw
		if combined != "" && len(body) != 0 {
			combined += "&"
		}
		combined += string(body)
		if len(combined) > maxNoticeBytes {
			return nil, combined[:maxNoticeBytes], ErrInvalidNotice
		}
		raw = combined
		form, err := url.ParseQuery(string(body))
		if err != nil {
			return nil, raw, fmt.Errorf("parse notification form: %w", ErrInvalidNotice)
		}
		for name, values := range form {
			params[name] = append(params[name], values...)
		}
	default:
		return nil, raw, ErrInvalidNotice
	}
	for _, values := range params {
		if len(values) != 1 {
			return nil, raw, ErrInvalidNotice
		}
	}
	return params, raw, nil
}

func validateCreate(req CreateRequest) error {
	if strings.TrimSpace(req.OrderNo) == "" || strings.TrimSpace(req.ProductName) == "" || req.AmountCents < 0 {
		return ErrInvalidRequest
	}
	return nil
}

func absoluteHTTPURL(value string) (*url.URL, error) {
	u, err := url.Parse(value)
	if err != nil {
		return nil, fmt.Errorf("parse url: %w", err)
	}
	if (u.Scheme != "https" && u.Scheme != "http") || u.Hostname() == "" || u.User != nil || u.Fragment != "" {
		return nil, ErrInvalidRequest
	}
	return u, nil
}

func contains(values []string, value string) bool {
	for _, candidate := range values {
		if candidate == value {
			return true
		}
	}
	return false
}

func formatCents(cents int64) string {
	return fmt.Sprintf("%d.%02d", cents/100, cents%100)
}

func parseCents(value string) (int64, error) {
	whole, fraction, found := strings.Cut(value, ".")
	if whole == "" || (found && (fraction == "" || len(fraction) > 2)) {
		return 0, ErrInvalidNotice
	}
	for _, part := range []string{whole, fraction} {
		for _, char := range part {
			if char < '0' || char > '9' {
				return 0, ErrInvalidNotice
			}
		}
	}
	units, err := strconv.ParseInt(whole, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("parse amount units: %w", ErrInvalidNotice)
	}
	var cents int64
	if fraction != "" {
		cents = int64(fraction[0]-'0') * 10
		if len(fraction) == 2 {
			cents += int64(fraction[1] - '0')
		}
	}
	if units > (math.MaxInt64-cents)/100 {
		return 0, ErrInvalidNotice
	}
	return units*100 + cents, nil
}
