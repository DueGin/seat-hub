package app

import (
	"context"
	"database/sql"
	"encoding/json"
	"net/url"
	"strings"

	"github.com/DueGin/seat-hub/internal/api"
	"github.com/DueGin/seat-hub/internal/config"
	"github.com/DueGin/seat-hub/internal/payment"
)

// buildProviders builds the payment provider map from the stored settings.
// Every configured provider stays registered to receive existing orders' callbacks.
func buildProviders(cfg config.Config, settings *settingsAdapter) map[string]payment.Provider {
	site, err := settings.Get(context.Background())
	if err != nil {
		site.Payment = nil
	}
	providers, _ := buildProvidersFrom(nil, site.Payment, cfg.Dev)
	return providers
}

// buildProvidersFrom builds the provider map from a payment document. It is
// shared by the boot path and the hot-reload path so the two never drift.
func buildProvidersFrom(_ *sql.DB, raw []byte, dev bool) (map[string]payment.Provider, error) {
	out := map[string]payment.Provider{}
	if epay := epayFromSettings(raw); epay != nil {
		if p, err := payment.NewEPay(*epay); err == nil {
			out["epay"] = p
		}
	}
	if cfg := alipayFromSettings(raw); cfg != nil {
		if p, err := payment.NewAlipayF2F(*cfg); err == nil {
			out["alipay_f2f"] = p
		}
	}
	if manual, ok := manualFromSettings(raw); ok {
		out["manual"] = payment.NewManual(manual)
	} else {
		// Manual transfer is the always-available fallback: a deployment with no
		// gateway configured still has a way to take money.
		out["manual"] = payment.NewManual("")
	}
	if dev {
		if mock, err := payment.NewMock(true); err == nil {
			out["mock"] = mock
		}
	}
	return out, nil
}

func epayFromSettings(raw []byte) *payment.EPayConfig {
	doc := decodeDoc(raw)
	entry, ok := doc["epay"].(map[string]any)
	if !ok {
		return nil
	}
	gateway, _ := entry["gateway_url"].(string)
	pid, _ := entry["pid"].(string)
	key, _ := entry["key"].(string)
	if gateway == "" || pid == "" || key == "" {
		return nil
	}
	var types []string
	if rawTypes, ok := entry["types"].([]any); ok {
		for _, t := range rawTypes {
			if s, ok := t.(string); ok && strings.TrimSpace(s) != "" {
				types = append(types, s)
			}
		}
	}
	return &payment.EPayConfig{GatewayURL: gateway, PID: pid, Key: key, Types: types}
}

func alipayFromSettings(raw []byte) *payment.AlipayF2FConfig {
	entry, ok := decodeDoc(raw)["alipay_f2f"].(map[string]any)
	if !ok {
		return nil
	}
	appID, _ := entry["app_id"].(string)
	private, _ := entry["private_key"].(string)
	public, _ := entry["alipay_public_key"].(string)
	sandbox, _ := entry["sandbox"].(bool)
	return &payment.AlipayF2FConfig{AppID: appID, PrivateKey: private, AlipayPublicKey: public, Sandbox: sandbox}
}

func activePayment(doc map[string]any) string {
	if active, ok := doc["active"].(string); ok {
		return active
	}
	if enabled(doc, "epay") {
		raw, _ := json.Marshal(doc)
		if cfg := epayFromSettings(raw); cfg != nil {
			if _, err := payment.NewEPay(*cfg); err == nil {
				return "epay"
			}
		}
	}
	return "manual"
}

func normalizePayment(raw []byte) ([]byte, error) {
	doc := decodeDoc(raw)
	doc["active"] = activePayment(doc)
	return json.Marshal(doc)
}

func validatePayment(raw []byte) error {
	doc := decodeDoc(raw)
	invalid := func(message string) error {
		return &api.UserError{Status: 422, Code: "invalid_settings", Message: message}
	}
	entry, _ := doc[activePayment(doc)].(map[string]any)
	text := func(name string) string { value, _ := entry[name].(string); return strings.TrimSpace(value) }
	switch activePayment(doc) {
	case "epay":
		for _, field := range []struct{ key, label string }{{"gateway_url", "易支付网关地址"}, {"pid", "易支付商户编号"}, {"key", "易支付密钥"}} {
			if text(field.key) == "" {
				return invalid("请填写" + field.label)
			}
		}
		if rawTypes, exists := entry["types"]; exists {
			types, ok := rawTypes.([]any)
			if !ok || len(types) == 0 {
				return invalid("易支付支付类型必须是至少包含一种方式的文本数组")
			}
			for _, rawType := range types {
				kind, ok := rawType.(string)
				if !ok || strings.TrimSpace(kind) == "" || kind != strings.TrimSpace(kind) {
					return invalid("易支付支付类型必须是非空文本，且不能有首尾空格")
				}
			}
		}
		cfg := epayFromSettings(raw)
		if cfg == nil {
			return invalid("请完整填写易支付配置")
		}
		if _, err := payment.NewEPay(*cfg); err != nil {
			return invalid("易支付网关地址格式无效")
		}
	case "alipay_f2f":
		for _, field := range []struct{ key, label string }{{"app_id", "支付宝当面付 AppID"}, {"private_key", "支付宝当面付应用私钥"}, {"alipay_public_key", "支付宝当面付支付宝公钥"}} {
			if text(field.key) == "" {
				return invalid("请填写" + field.label)
			}
		}
		if _, ok := entry["sandbox"].(bool); entry["sandbox"] != nil && !ok {
			return invalid("支付宝当面付沙箱环境必须为开关值")
		}
		if _, err := payment.NewAlipayF2F(*alipayFromSettings(raw)); err != nil {
			return invalid(err.Error())
		}
	case "manual":
		if text("instructions") == "" {
			return invalid("请填写人工转账说明")
		}
	default:
		return invalid("请选择有效的收款方式")
	}
	return nil
}

func manualFromSettings(raw []byte) (string, bool) {
	entry, ok := decodeDoc(raw)["manual"].(map[string]any)
	if !ok {
		return "", false
	}
	instructions, _ := entry["instructions"].(string)
	return instructions, instructions != ""
}

func callbackBase(cfg config.Config) string {
	if cfg.PublicBaseURL != "" {
		return cfg.PublicBaseURL
	}
	return "http://localhost" + cfg.Listen
}

func mustURL(raw string) *url.URL {
	u, err := url.Parse(raw)
	if err != nil {
		panic("invalid VM2API_BASE_URL: " + err.Error())
	}
	return u
}
