package app

import (
	"context"
	"database/sql"
	"net/url"
	"strings"

	"github.com/DueGin/vm2api-portal/internal/config"
	"github.com/DueGin/vm2api-portal/internal/payment"
)

// buildProviders builds the payment provider map from the stored settings.
// The EPay merchant credentials come from the settings document; when they are
// missing the provider is simply absent, which makes the storefront offer
// manual payment only.
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
	enabled, _ := entry["enabled"].(bool)
	if !enabled {
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
