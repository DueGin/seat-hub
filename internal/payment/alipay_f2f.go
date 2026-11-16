package payment

import (
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"
)

const (
	alipayGateway        = "https://openapi.alipay.com/gateway.do"
	alipaySandboxGateway = "https://openapi-sandbox.dl.alipaydev.com/gateway.do"
)

var shanghai = time.FixedZone("Asia/Shanghai", 8*60*60)

type AlipayF2FConfig struct {
	AppID           string
	PrivateKey      string
	AlipayPublicKey string
	Sandbox         bool
	GatewayURL      string
	HTTPClient      *http.Client
	Now             func() time.Time
	Logger          *slog.Logger
}

type ConfigError struct{ Field, Message string }

func (e *ConfigError) Error() string { return e.Message }

type GatewayError struct{ Code, SubCode, Message string }

func (e *GatewayError) Error() string { return e.Message }

type AlipayF2F struct {
	appID      string
	privateKey *rsa.PrivateKey
	publicKey  *rsa.PublicKey
	gateway    string
	client     *http.Client
	now        func() time.Time
	log        *slog.Logger
}

func NewAlipayF2F(cfg AlipayF2FConfig) (*AlipayF2F, error) {
	if strings.TrimSpace(cfg.AppID) == "" {
		return nil, &ConfigError{"app_id", "请填写支付宝当面付 AppID"}
	}
	for _, c := range cfg.AppID {
		if c < '0' || c > '9' {
			return nil, &ConfigError{"app_id", "支付宝当面付 AppID 只能包含数字"}
		}
	}
	private, err := parseAlipayPrivateKey(cfg.PrivateKey)
	if err != nil {
		return nil, &ConfigError{"private_key", "支付宝当面付应用私钥格式无效，请填写 RSA 的 PKCS1 或 PKCS8 私钥"}
	}
	public, err := parseAlipayPublicKey(cfg.AlipayPublicKey)
	if err != nil {
		return nil, &ConfigError{"alipay_public_key", "支付宝当面付支付宝公钥格式无效，请填写 RSA 公钥"}
	}
	gateway := cfg.GatewayURL
	if gateway == "" {
		gateway = alipayGateway
		if cfg.Sandbox {
			gateway = alipaySandboxGateway
		}
	}
	if _, err := absoluteHTTPURL(gateway); err != nil {
		return nil, fmt.Errorf("invalid alipay gateway: %w", err)
	}
	client := cfg.HTTPClient
	if client == nil {
		client = &http.Client{Timeout: 10 * time.Second}
	}
	now := cfg.Now
	if now == nil {
		now = time.Now
	}
	log := cfg.Logger
	if log == nil {
		log = slog.Default()
	}
	return &AlipayF2F{cfg.AppID, private, public, gateway, client, now, log}, nil
}

func keyDER(value string) ([]byte, error) {
	value = strings.TrimSpace(value)
	if block, rest := pem.Decode([]byte(value)); block != nil {
		if strings.TrimSpace(string(rest)) != "" {
			return nil, ErrInvalidRequest
		}
		return block.Bytes, nil
	}
	return base64.StdEncoding.DecodeString(strings.Join(strings.Fields(value), ""))
}

func parseAlipayPrivateKey(value string) (*rsa.PrivateKey, error) {
	der, err := keyDER(value)
	if err != nil {
		return nil, err
	}
	key, err := x509.ParsePKCS1PrivateKey(der)
	if err != nil {
		parsed, parseErr := x509.ParsePKCS8PrivateKey(der)
		if parseErr != nil {
			return nil, parseErr
		}
		var ok bool
		key, ok = parsed.(*rsa.PrivateKey)
		if !ok {
			return nil, ErrInvalidRequest
		}
	}
	if err := key.Validate(); err != nil {
		return nil, err
	}
	if key.N.BitLen() < 2048 {
		return nil, ErrInvalidRequest
	}
	return key, nil
}

func parseAlipayPublicKey(value string) (*rsa.PublicKey, error) {
	der, err := keyDER(value)
	if err != nil {
		return nil, err
	}
	parsed, err := x509.ParsePKIXPublicKey(der)
	var key *rsa.PublicKey
	if err == nil {
		key, _ = parsed.(*rsa.PublicKey)
	} else {
		key, _ = x509.ParsePKCS1PublicKey(der)
	}
	if key == nil || key.N.BitLen() < 2048 {
		return nil, ErrInvalidRequest
	}
	return key, nil
}

// Requests include sign_type; notifications exclude it. Values are never URL-escaped here.
func alipaySigningString(params url.Values, notify bool) string {
	keys := make([]string, 0, len(params))
	for key := range params {
		if key != "sign" && (!notify || key != "sign_type") && params.Get(key) != "" {
			keys = append(keys, key)
		}
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, key := range keys {
		parts = append(parts, key+"="+params.Get(key))
	}
	return strings.Join(parts, "&")
}

func signAlipay(key *rsa.PrivateKey, text string) (string, error) {
	digest := sha256.Sum256([]byte(text))
	sign, err := rsa.SignPKCS1v15(rand.Reader, key, crypto.SHA256, digest[:])
	return base64.StdEncoding.EncodeToString(sign), err
}

func verifyAlipay(key *rsa.PublicKey, text []byte, signature string) error {
	sign, err := base64.StdEncoding.DecodeString(signature)
	if err != nil {
		return ErrInvalidSignature
	}
	digest := sha256.Sum256(text)
	if rsa.VerifyPKCS1v15(key, crypto.SHA256, digest[:], sign) != nil {
		return ErrInvalidSignature
	}
	return nil
}

type alipayResponse struct {
	Code    string `json:"code"`
	SubCode string `json:"sub_code"`
	SubMsg  string `json:"sub_msg"`
	OrderNo string `json:"out_trade_no"`
	TradeNo string `json:"trade_no"`
	Amount  string `json:"total_amount"`
	Status  string `json:"trade_status"`
	QR      string `json:"qr_code"`
}

func (a *AlipayF2F) call(ctx context.Context, method string, biz any, notifyURL string) (alipayResponse, error) {
	var result alipayResponse
	body, err := json.Marshal(biz)
	if err != nil {
		return result, fmt.Errorf("encode alipay request: %w", err)
	}
	params := url.Values{
		"app_id": {a.appID}, "method": {method}, "charset": {"utf-8"}, "sign_type": {"RSA2"},
		"version": {"1.0"}, "format": {"JSON"}, "timestamp": {a.now().In(shanghai).Format("2006-01-02 15:04:05")}, "biz_content": {string(body)},
	}
	if notifyURL != "" {
		params.Set("notify_url", notifyURL)
	}
	sign, err := signAlipay(a.privateKey, alipaySigningString(params, false))
	if err != nil {
		return result, fmt.Errorf("sign alipay request: %w", err)
	}
	params.Set("sign", sign)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, a.gateway, strings.NewReader(params.Encode()))
	if err != nil {
		return result, fmt.Errorf("build alipay request: %w", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded; charset=utf-8")
	resp, err := a.client.Do(req)
	if err != nil {
		return result, fmt.Errorf("request alipay gateway: %w", err)
	}
	defer resp.Body.Close()
	wire, err := io.ReadAll(io.LimitReader(resp.Body, (1<<20)+1))
	if err != nil {
		return result, fmt.Errorf("read alipay response: %w", err)
	}
	if resp.StatusCode != http.StatusOK || len(wire) > 1<<20 {
		return result, &GatewayError{Message: "支付宝暂时无法响应，请稍后重试"}
	}
	var envelope map[string]json.RawMessage
	if err := json.Unmarshal(wire, &envelope); err != nil {
		return result, fmt.Errorf("decode alipay response: %w", err)
	}
	raw := envelope[strings.ReplaceAll(method, ".", "_")+"_response"]
	if len(raw) == 0 {
		raw = envelope["error_response"]
	}
	var signature string
	if json.Unmarshal(envelope["sign"], &signature) != nil || len(raw) == 0 {
		return result, ErrInvalidSignature
	}
	if err := verifyAlipay(a.publicKey, raw, signature); err != nil {
		return result, err
	}
	if err := json.Unmarshal(raw, &result); err != nil {
		return result, fmt.Errorf("decode alipay result: %w", err)
	}
	if result.Code != "10000" {
		a.log.Warn("alipay gateway rejected request", "method", method, "code", result.Code, "sub_code", result.SubCode)
		return result, &GatewayError{Code: result.Code, SubCode: result.SubCode, Message: alipayErrorMessage(result.SubCode)}
	}
	return result, nil
}

func alipayErrorMessage(code string) string {
	switch code {
	case "ACQ.ACCESS_FORBIDDEN", "isv.insufficient-isv-permissions", "isv.app-id-invalid", "isv.app-id-inexistent":
		return "支付宝当面付暂不可用，请联系管理员检查应用配置和当面付权限"
	case "isv.invalid-signature", "isv.sign-invalid":
		return "支付宝支付配置校验失败，请联系管理员检查应用密钥"
	case "ACQ.TRADE_HAS_CLOSE":
		return "支付宝交易已关闭，请重新下单"
	case "ACQ.TRADE_NOT_EXIST":
		return "支付宝交易尚未创建，请稍后重试"
	case "ACQ.TOTAL_FEE_EXCEED":
		return "支付金额超过支付宝限制，请联系管理员"
	default:
		return "支付宝暂未受理此笔交易，请稍后重试或联系管理员"
	}
}

func (a *AlipayF2F) Create(ctx context.Context, req CreateRequest) (Payment, error) {
	if err := validateCreate(req); err != nil {
		return Payment{}, err
	}
	if _, err := absoluteHTTPURL(req.NotifyURL); err != nil {
		return Payment{}, fmt.Errorf("invalid notify url: %w", err)
	}
	if req.ExpiresAt.IsZero() || !req.ExpiresAt.After(a.now()) {
		return Payment{}, ErrOrderNotPayable
	}
	result, err := a.call(ctx, "alipay.trade.precreate", map[string]string{
		"out_trade_no": req.OrderNo, "total_amount": formatCents(req.AmountCents), "subject": req.ProductName,
		"time_expire": req.ExpiresAt.In(shanghai).Format("2006-01-02 15:04:05"),
	}, req.NotifyURL)
	if err != nil {
		return Payment{}, err
	}
	if result.OrderNo != req.OrderNo || strings.TrimSpace(result.QR) == "" {
		return Payment{}, ErrInvalidNotice
	}
	return Payment{Type: "qrcode", QR: result.QR}, nil
}

func (a *AlipayF2F) Notify(r *http.Request) (Notice, error) {
	if r == nil || r.Method != http.MethodPost || r.URL == nil || r.URL.RawQuery != "" {
		return Notice{}, ErrInvalidNotice
	}
	params, _, err := noticeParams(r)
	if err != nil {
		return Notice{}, err
	}
	// Persist only reconciliation fields, never buyer details or the signature.
	notice := Notice{OrderNo: params.Get("out_trade_no"), TradeNo: params.Get("trade_no")}
	if params.Get("sign_type") != "RSA2" {
		return notice, ErrInvalidSignature
	}
	if err := verifyAlipay(a.publicKey, []byte(alipaySigningString(params, true)), params.Get("sign")); err != nil {
		return notice, err
	}
	notice.verified = true
	if params.Get("app_id") != a.appID {
		return notice, ErrMerchantMismatch
	}
	notice.AmountCents, err = parseCents(params.Get("total_amount"))
	if err != nil {
		return notice, err
	}
	notice.Paid = params.Get("trade_status") == "TRADE_SUCCESS" || params.Get("trade_status") == "TRADE_FINISHED"
	if !notice.Paid {
		return notice, ErrNotPaid
	}
	if notice.OrderNo == "" || notice.TradeNo == "" {
		return notice, ErrInvalidNotice
	}
	notice.Raw = alipayNoticeRaw(notice, "notify")
	return notice, nil
}

func alipayNoticeRaw(n Notice, source string) string {
	raw, _ := json.Marshal(map[string]any{"source": source, "out_trade_no": n.OrderNo, "trade_no": n.TradeNo, "amount_cents": n.AmountCents})
	return string(raw)
}

func (a *AlipayF2F) Query(ctx context.Context, orderNo string) (Notice, error) {
	result, err := a.call(ctx, "alipay.trade.query", map[string]string{"out_trade_no": orderNo}, "")
	if err != nil {
		return Notice{}, err
	}
	if result.OrderNo != orderNo {
		return Notice{}, ErrInvalidNotice
	}
	amount, err := parseCents(result.Amount)
	if err != nil {
		return Notice{}, err
	}
	notice := Notice{OrderNo: result.OrderNo, TradeNo: result.TradeNo, AmountCents: amount,
		Paid: result.Status == "TRADE_SUCCESS" || result.Status == "TRADE_FINISHED", verified: true}
	if !notice.Paid {
		return notice, ErrNotPaid
	}
	notice.Raw = alipayNoticeRaw(notice, "query")
	return notice, nil
}

func (a *AlipayF2F) Close(ctx context.Context, orderNo string) error {
	_, err := a.call(ctx, "alipay.trade.close", map[string]string{"out_trade_no": orderNo}, "")
	return err
}
