package store

import (
	"context"
	"encoding/json"
)

// Site is the admin-editable configuration stored under settings key "site".
// Every package that needs these numbers reads them through Site.Load so the
// defaults and the JSON shape live in exactly one place.
type Site struct {
	SiteName                 string          `json:"site_name"`
	RegistrationOpen         bool            `json:"registration_open"`
	BaseConcurrency          int             `json:"base_concurrency"`
	ConcurrencyPerHostedSeat int             `json:"concurrency_per_hosted_seat"`
	ConcurrencyPerRental     int             `json:"concurrency_per_rental_account"`
	HostedGraceHours         int             `json:"hosted_grace_hours"`
	RentalGraceHours         int             `json:"rental_grace_hours"`
	GatewayQueueTimeoutMS    int             `json:"gateway_queue_timeout_ms"`
	HostedVMTemplate         json.RawMessage `json:"hosted_vm_template"`
	Payment                  json.RawMessage `json:"payment"`
}

func DefaultSite() Site {
	return Site{
		SiteName:         "vm2api 门面",
		RegistrationOpen: true,
		BaseConcurrency:  1,
		HostedGraceHours: 24,
		RentalGraceHours: 0,
		HostedVMTemplate: json.RawMessage(`{"runtime_type":"docker"}`),
		Payment:          DefaultPaymentDocument(),
	}
}

// DefaultPaymentDocument is the payment config a fresh install starts with.
// The settings page edits this shape directly, so it must always contain both
// provider blocks even before anything is configured.
func DefaultPaymentDocument() json.RawMessage {
	return json.RawMessage(`{
  "epay": {"enabled": false, "gateway_url": "", "pid": "", "key": "", "types": ["alipay", "wxpay"]},
  "manual": {"enabled": true, "instructions": "转账后请联系管理员确认收款。"}
}`)
}

// LoadSite merges the stored "site" document over defaults.
func LoadSite(ctx context.Context, s *Settings) (Site, error) {
	site := DefaultSite()
	if _, err := s.Get(ctx, "site", &site); err != nil {
		return site, err
	}
	return site, nil
}
