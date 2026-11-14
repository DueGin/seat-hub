// The panel mixes {ok,data} with top-level {ok,item,usage_stats} responses.
// Its name/account_tier/has_token fields become Label/Tier/HasCredential;
// utilization_* ratios become window percentages and reset_* become reset times;
// total_cost becomes CostUSD,
// history/models become Daily/ByModel, and auth expiry milliseconds become UTC.
// Billing exposes one grouping per call, so both vm and key groups are fetched;
// it currently exposes no daily/model breakdown. Request stats use the scoped
// window rather than the panel's unscoped totals. Bootstrap ok/error become
// succeeded/failed. The structured cred_status is reduced to its display text.
// These mappings follow panel-routes.mjs and api-keys.mjs.
package vm2api

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

type HTTPClient struct {
	baseURL   string
	masterKey string
	client    *http.Client
}

var _ Client = (*HTTPClient)(nil)

func NewHTTPClient(baseURL, masterKey string, client *http.Client) *HTTPClient {
	if client == nil {
		client = &http.Client{Timeout: 30 * time.Second}
	}
	return &HTTPClient{baseURL: strings.TrimRight(baseURL, "/"), masterKey: masterKey, client: client}
}

func (client *HTTPClient) call(ctx context.Context, method, path string, query url.Values, input, output any) error {
	var body io.Reader
	if input != nil {
		encoded, err := json.Marshal(input)
		if err != nil {
			return fmt.Errorf("encode panel request: %w", err)
		}
		body = bytes.NewReader(encoded)
	}
	endpoint := client.baseURL + "/api/panel" + path
	if len(query) > 0 {
		endpoint += "?" + query.Encode()
	}
	request, err := http.NewRequestWithContext(ctx, method, endpoint, body)
	if err != nil {
		return fmt.Errorf("create panel request: %w", err)
	}
	request.Header.Set("Authorization", "Bearer "+client.masterKey)
	request.Header.Set("Accept", "application/json")
	if input != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	response, err := client.client.Do(request)
	if err != nil {
		return fmt.Errorf("panel %s %s: %w: %w", method, path, ErrUnavailable, err)
	}
	defer response.Body.Close()
	var envelope struct {
		OK    *bool           `json:"ok"`
		Data  json.RawMessage `json:"data"`
		Error struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	raw, readErr := io.ReadAll(io.LimitReader(response.Body, 8<<20))
	decodeErr := json.Unmarshal(raw, &envelope)
	if response.StatusCode < 200 || response.StatusCode >= 300 || (envelope.OK != nil && !*envelope.OK) {
		message := envelope.Error.Message
		if message == "" {
			message = http.StatusText(response.StatusCode)
		}
		return &Error{Status: response.StatusCode, Code: envelope.Error.Code, Message: message, Op: method + " " + path}
	}
	if readErr != nil {
		return fmt.Errorf("read panel response: %w: %w", ErrUnavailable, readErr)
	}
	if decodeErr != nil {
		return fmt.Errorf("decode panel response: %w", decodeErr)
	}
	if envelope.OK == nil {
		return fmt.Errorf("decode panel response: missing ok field")
	}
	if output == nil {
		return nil
	}
	if len(envelope.Data) > 0 && string(envelope.Data) != "null" {
		raw = envelope.Data
	}
	if err := json.Unmarshal(raw, output); err != nil {
		return fmt.Errorf("decode panel data: %w", err)
	}
	return nil
}

func (client *HTTPClient) Ping(ctx context.Context) (Health, error) {
	started := time.Now()
	var data struct {
		Current string `json:"current"`
		Version string `json:"version"`
	}
	err := client.call(ctx, http.MethodGet, "/version", nil, nil, &data)
	version := data.Current
	if version == "" {
		version = data.Version
	}
	return Health{OK: err == nil, Version: version, LatencyMS: time.Since(started).Milliseconds()}, err
}

func (client *HTTPClient) CreateUser(ctx context.Context, username, password string) (User, error) {
	var data struct {
		Item User `json:"item"`
	}
	err := client.call(ctx, http.MethodPost, "/users", nil, map[string]any{
		"username": username, "password": password, "role": "user", "enabled": true, "vm_create_quota": 0,
	}, &data)
	return data.Item, err
}

func (client *HTTPClient) SetUserEnabled(ctx context.Context, userID string, enabled bool) error {
	return client.call(ctx, http.MethodPatch, "/users/"+url.PathEscape(userID), nil, map[string]bool{"enabled": enabled}, nil)
}

func (client *HTTPClient) CreateKey(ctx context.Context, input CreateKeyInput) (Key, error) {
	var data struct {
		Item struct {
			ID     string `json:"id"`
			Key    string `json:"key"`
			Prefix string `json:"key_prefix"`
		} `json:"item"`
	}
	err := client.call(ctx, http.MethodPost, "/api-keys", nil, map[string]any{
		"name": input.Name, "user_id": input.UserID, "max_concurrency": clampConcurrency(input.MaxConcurrency),
	}, &data)
	if err != nil {
		return Key{}, err
	}
	if !strings.HasPrefix(data.Item.Key, "sk-vm-") || strings.ContainsAny(data.Item.Key, "…* \t\r\n") {
		return Key{}, fmt.Errorf("decode created key: missing plaintext key")
	}
	return Key{ID: data.Item.ID, Secret: data.Item.Key, Prefix: data.Item.Prefix}, nil
}

func clampConcurrency(value int) int {
	return max(0, min(128, value))
}

func (client *HTTPClient) UpdateKey(ctx context.Context, keyID string, input UpdateKeyInput) error {
	body := map[string]any{}
	if input.Name != nil {
		body["name"] = *input.Name
	}
	if input.Active != nil {
		body["status"] = "disabled"
		if *input.Active {
			body["status"] = "active"
		}
	}
	if input.MaxConcurrency != nil {
		body["max_concurrency"] = clampConcurrency(*input.MaxConcurrency)
	}
	return client.call(ctx, http.MethodPatch, "/api-keys/"+url.PathEscape(keyID), nil, body, nil)
}

func (client *HTTPClient) DeleteKey(ctx context.Context, keyID string) error {
	return client.call(ctx, http.MethodDelete, "/api-keys/"+url.PathEscape(keyID), nil, nil, nil)
}

type panelSpend struct {
	Spend
	TokensIn  *int64   `json:"tokens_in"`
	TokensOut *int64   `json:"tokens_out"`
	TotalCost *float64 `json:"total_cost"`
}

func (spend panelSpend) mapped() Spend {
	result := spend.Spend
	if spend.TokensIn != nil {
		result.InputTokens = *spend.TokensIn
	}
	if spend.TokensOut != nil {
		result.OutputTokens = *spend.TokensOut
	}
	if spend.TotalCost != nil {
		result.CostUSD = *spend.TotalCost
	}
	if result.Tokens == 0 {
		result.Tokens = result.InputTokens + result.OutputTokens + result.CacheReadTokens + result.CacheCreationTokens
	}
	return result
}

func mappedSpend(spend *panelSpend) *Spend {
	if spend == nil {
		return nil
	}
	result := spend.mapped()
	return &result
}

type panelUsage struct {
	History []struct {
		Day string `json:"day"`
		panelSpend
	} `json:"history"`
	Models []struct {
		Name string `json:"name"`
		panelSpend
	} `json:"models"`
}

func (usage panelUsage) mapped() UsageBreakdown {
	result := UsageBreakdown{Daily: []DailyUsage{}, ByModel: []ModelUsage{}}
	for _, row := range usage.History {
		spend := row.mapped()
		result.Daily = append(result.Daily, DailyUsage{Date: row.Day, Requests: spend.Requests, Tokens: spend.Tokens, CostUSD: spend.CostUSD})
	}
	for _, row := range usage.Models {
		spend := row.mapped()
		result.ByModel = append(result.ByModel, ModelUsage{Model: row.Name, Requests: spend.Requests, Tokens: spend.Tokens, CostUSD: spend.CostUSD})
	}
	return result
}

func (client *HTTPClient) KeyStats(ctx context.Context, keyID string) (UsageBreakdown, error) {
	var data struct {
		Usage panelUsage `json:"usage_stats"`
	}
	err := client.call(ctx, http.MethodGet, "/api-keys/"+url.PathEscape(keyID)+"/stats", nil, nil, &data)
	return data.Usage.mapped(), err
}

type panelVM struct {
	VM
	CredentialStatus panelCredentialStatus `json:"cred_status"`
	Name             string                `json:"name"`
	AccountTier      string                `json:"account_tier"`
	Email            string                `json:"email"`
	HasToken         bool                  `json:"has_token"`
	HasRefresh       bool                  `json:"has_refresh"`
	Utilization5h    *float64              `json:"utilization_5h"`
	Utilization7d    *float64              `json:"utilization_7d"`
	UtilizationOI    *float64              `json:"utilization_7d_oi"`
	Reset5h          panelTime             `json:"reset_5h"`
	Reset7d          panelTime             `json:"reset_7d"`
	ResetOI          panelTime             `json:"reset_7d_oi"`
	Status5h         string                `json:"status_5h"`
	Status7d         string                `json:"status_7d"`
	StatusOI         string                `json:"status_7d_oi"`
	Estimate5h       *Estimate             `json:"estimate_5h"`
	Estimate7d       *Estimate             `json:"estimate_7d"`
	// Proxy shadows VM.Proxy: vm2api nests geo under proxy.geo.
	Proxy *panelProxy `json:"proxy"`
}

type panelProxy struct {
	ID            string `json:"id"`
	Scheme        string `json:"scheme"`
	Status        string `json:"status"`
	Enabled       *bool  `json:"enabled"`
	BlockedReason string `json:"blocked_reason"`
	Geo           *struct {
		IP       string `json:"ip"`
		Timezone string `json:"timezone"`
	} `json:"geo"`
}

func (raw *panelProxy) mapped() *ProxyView {
	if raw == nil {
		return nil
	}
	view := &ProxyView{
		ID:            raw.ID,
		Scheme:        raw.Scheme,
		Status:        raw.Status,
		Enabled:       raw.Enabled == nil || *raw.Enabled,
		BlockedReason: raw.BlockedReason,
	}
	if raw.Geo != nil {
		view.GeoIP, view.GeoTimezone = raw.Geo.IP, raw.Geo.Timezone
	}
	return view
}

type panelCredentialStatus string

func (status *panelCredentialStatus) UnmarshalJSON(raw []byte) error {
	if string(raw) == "null" {
		return nil
	}
	if len(raw) > 0 && raw[0] == '"' {
		var value string
		if err := json.Unmarshal(raw, &value); err != nil {
			return fmt.Errorf("decode credential status: %w", err)
		}
		*status = panelCredentialStatus(value)
		return nil
	}
	var data struct {
		Text string `json:"text"`
		Key  string `json:"key"`
	}
	if err := json.Unmarshal(raw, &data); err != nil {
		return fmt.Errorf("decode credential status: %w", err)
	}
	if data.Text == "" {
		data.Text = data.Key
	}
	*status = panelCredentialStatus(data.Text)
	return nil
}

type panelTime struct{ time.Time }

// UnmarshalJSON accepts every shape vm2api emits for a reset time: RFC3339
// text, or a Unix timestamp in seconds or milliseconds, as a JSON number or a
// numeric string (reset_5h is copied verbatim from Anthropic's rate-limit
// headers, which are epoch seconds as text). These times are informational,
// so an unrecognised value is left empty rather than failing the whole
// response it is part of.
func (value *panelTime) UnmarshalJSON(raw []byte) error {
	text := strings.TrimSpace(string(raw))
	if text == "null" {
		return nil
	}
	if strings.HasPrefix(text, `"`) {
		var decoded string
		if err := json.Unmarshal(raw, &decoded); err != nil {
			return nil
		}
		text = strings.TrimSpace(decoded)
	}
	if text == "" {
		return nil
	}
	if parsed, err := time.Parse(time.RFC3339Nano, text); err == nil {
		value.Time = parsed.UTC()
		return nil
	}
	if epoch, err := strconv.ParseFloat(text, 64); err == nil && epoch > 0 && !math.IsInf(epoch, 0) {
		// Seconds until the year 5138; anything larger is milliseconds.
		if epoch < 1e11 {
			value.Time = time.UnixMilli(int64(epoch * 1000)).UTC()
		} else {
			value.Time = time.UnixMilli(int64(epoch)).UTC()
		}
	}
	return nil
}

func panelWindow(utilization *float64, reset panelTime, fallback *Window) *Window {
	if utilization == nil && reset.IsZero() {
		return fallback
	}
	window := &Window{}
	if utilization != nil {
		window.Utilization = *utilization * 100
	}
	if !reset.IsZero() {
		window.ResetsAt = &reset.Time
	}
	return window
}

func credentialRejected(reason string) bool {
	return strings.HasPrefix(reason, "oauth_") || strings.HasPrefix(reason, "credential_")
}

func (raw panelVM) mapped() VM {
	result := raw.VM
	result.CredStatus = string(raw.CredentialStatus)
	if raw.Name != "" {
		result.Label = raw.Name
	}
	if raw.AccountTier != "" {
		result.Tier = raw.AccountTier
	}
	if result.EmailMasked == "" && raw.Email != "" {
		local, domain, found := strings.Cut(raw.Email, "@")
		if found && local != "" {
			result.EmailMasked = string([]rune(local)[:1]) + "***@" + domain
		}
	}
	result.HasCredential = result.HasCredential || raw.HasToken || raw.HasRefresh
	result.FiveHour = panelWindow(raw.Utilization5h, raw.Reset5h, result.FiveHour)
	result.SevenDay = panelWindow(raw.Utilization7d, raw.Reset7d, result.SevenDay)
	result.SevenDayFable = panelWindow(raw.UtilizationOI, raw.ResetOI, result.SevenDayFable)
	if result.FiveHour != nil {
		result.FiveHour.Status = raw.Status5h
	}
	if result.SevenDay != nil {
		result.SevenDay.Status = raw.Status7d
	}
	if result.SevenDayFable != nil {
		result.SevenDayFable.Status = raw.StatusOI
	}
	result.CredentialRejected = credentialRejected(result.UnschedulableCode)
	result.Email = raw.Email
	result.Proxy = raw.Proxy.mapped()
	return result
}

func (client *HTTPClient) ListVMs(ctx context.Context, filter VMFilter) ([]VM, error) {
	var data struct {
		Items []panelVM `json:"items"`
	}
	if err := client.call(ctx, http.MethodGet, "/vms", nil, nil, &data); err != nil {
		return nil, err
	}
	items := []VM{}
	for _, raw := range data.Items {
		item := raw.mapped()
		if filter.OwnerUserID != nil && item.OwnerUserID != *filter.OwnerUserID {
			continue
		}
		if filter.Unowned && (item.OwnerUserID != "" || item.Origin != "platform") {
			continue
		}
		if filter.ClaudeOnly && item.Platform != "anthropic" {
			continue
		}
		items = append(items, item)
	}
	return items, nil
}

func (client *HTTPClient) GetAccountUsage(ctx context.Context, vmID string) (AccountUsage, error) {
	var data struct {
		VM      panelVM `json:"vm"`
		Billing struct {
			Today    *panelSpend `json:"today"`
			Window5h *panelSpend `json:"window_5h"`
			Window7d *panelSpend `json:"window_7d"`
			Usage    panelUsage  `json:"usage_stats"`
		} `json:"billing"`
	}
	if err := client.call(ctx, http.MethodGet, vmPath(vmID), nil, nil, &data); err != nil {
		return AccountUsage{}, err
	}
	usage := data.Billing.Usage.mapped()
	return AccountUsage{
		VM: data.VM.mapped(), Estimate5h: data.VM.Estimate5h, Estimate7d: data.VM.Estimate7d,
		Today: mappedSpend(data.Billing.Today), Window5h: mappedSpend(data.Billing.Window5h), Window7d: mappedSpend(data.Billing.Window7d),
		Daily: usage.Daily, ByModel: usage.ByModel, FetchedAt: time.Now().UTC(),
	}, nil
}

func (client *HTTPClient) CreateHostedVM(ctx context.Context, label string, template json.RawMessage) (VM, error) {
	body := map[string]json.RawMessage{}
	if len(template) > 0 {
		if err := json.Unmarshal(template, &body); err != nil {
			return VM{}, fmt.Errorf("decode vm template: %w", err)
		}
		if body == nil {
			return VM{}, fmt.Errorf("decode vm template: expected object")
		}
	}
	name, err := json.Marshal(label)
	if err != nil {
		return VM{}, fmt.Errorf("encode vm label: %w", err)
	}
	body["name"] = name
	var data struct {
		VM panelVM `json:"vm"`
	}
	err = client.call(ctx, http.MethodPost, "/vms/create", nil, body, &data)
	return data.VM.mapped(), err
}

func vmPath(vmID string) string {
	return "/vms/" + url.PathEscape(vmID)
}

func (client *HTTPClient) SetVMOwner(ctx context.Context, vmID string, userID *string) error {
	return client.call(ctx, http.MethodPatch, vmPath(vmID)+"/owner", nil, map[string]any{"user_id": userID}, nil)
}

func (client *HTTPClient) AllocateProxy(ctx context.Context, vmID string) error {
	return client.call(ctx, http.MethodPost, vmPath(vmID)+"/allocate-proxy", nil, map[string]any{}, nil)
}

func (client *HTTPClient) SetSchedulable(ctx context.Context, vmID string, schedulable bool) error {
	return client.call(ctx, http.MethodPost, vmPath(vmID)+"/schedulable", nil, map[string]bool{"schedulable": schedulable}, nil)
}

func (client *HTTPClient) DeleteVM(ctx context.Context, vmID string) error {
	return client.call(ctx, http.MethodDelete, vmPath(vmID), nil, nil, nil)
}

// CollectIdentity asks the slot to re-report its fingerprint
// (POST /vms/:id/collect-identity) and returns the merged result.
func (client *HTTPClient) CollectIdentity(ctx context.Context, vmID string) (Fingerprint, error) {
	var data struct {
		Fingerprint Fingerprint `json:"fingerprint"`
	}
	err := client.call(ctx, http.MethodPost, vmPath(vmID)+"/collect-identity", nil, map[string]any{}, &data)
	return data.Fingerprint, err
}

// ProbeExit geo-probes the slot's real egress (POST /vms/:id/exit-probe).
func (client *HTTPClient) ProbeExit(ctx context.Context, vmID string) (ExitProbe, error) {
	var data struct {
		Via string `json:"via"`
		Geo struct {
			IP          string `json:"ip"`
			CountryCode string `json:"country_code"`
			Timezone    string `json:"timezone"`
		} `json:"geo"`
	}
	err := client.call(ctx, http.MethodPost, vmPath(vmID)+"/exit-probe", nil, map[string]any{}, &data)
	return ExitProbe{Via: data.Via, IP: data.Geo.IP, CountryCode: data.Geo.CountryCode, Timezone: data.Geo.Timezone}, err
}

// ResetVM rebuilds the slot with fresh traits (POST /vms/:id/reset
// {mode:"traits"}): new fingerprint, wiped home, same id and proxy.
func (client *HTTPClient) ResetVM(ctx context.Context, vmID string) error {
	return client.call(ctx, http.MethodPost, vmPath(vmID)+"/reset", nil, map[string]string{"mode": "traits"}, nil)
}

func (client *HTTPClient) UnbindProxy(ctx context.Context, proxyID, vmID string) error {
	return client.call(ctx, http.MethodPost, "/proxies/"+url.PathEscape(proxyID)+"/unbind", nil, map[string]string{"vm_id": vmID}, nil)
}

func (client *HTTPClient) DisableProxy(ctx context.Context, proxyID string) error {
	return client.call(ctx, http.MethodPost, "/proxies/"+url.PathEscape(proxyID)+"/disable", nil, map[string]any{}, nil)
}

func (client *HTTPClient) GenerateAuthURL(ctx context.Context, vmID string) (AuthURL, error) {
	var data struct {
		URL       string    `json:"auth_url"`
		SessionID string    `json:"session_id"`
		ExpiresAt panelTime `json:"expires_at"`
	}
	err := client.call(ctx, http.MethodPost, vmPath(vmID)+"/oauth/generate-auth-url", nil, map[string]string{"flavor": "claude_code"}, &data)
	return AuthURL{URL: data.URL, SessionID: data.SessionID, ExpiresAt: data.ExpiresAt.Time}, err
}

func (client *HTTPClient) ExchangeCode(ctx context.Context, vmID, sessionID, code string) error {
	return client.call(ctx, http.MethodPost, vmPath(vmID)+"/oauth/exchange-code", nil, map[string]string{
		"session_id": sessionID, "code": code, "flavor": "claude_code",
	}, nil)
}

func (client *HTTPClient) BootstrapStatus(ctx context.Context, vmID string) (Bootstrap, error) {
	var data struct {
		Status *struct {
			Status string `json:"status"`
			Error  string `json:"error"`
		} `json:"status"`
	}
	if err := client.call(ctx, http.MethodGet, vmPath(vmID)+"/official-cc-bootstrap", nil, nil, &data); err != nil {
		return Bootstrap{}, err
	}
	if data.Status == nil {
		return Bootstrap{State: "pending"}, nil
	}
	state := data.Status.Status
	switch state {
	case "ok", "succeeded":
		state = "succeeded"
	case "error", "failed":
		state = "failed"
	case "running":
	default:
		state = "pending"
	}
	return Bootstrap{State: state, Message: data.Status.Error}, nil
}

func scopedQuery(userID, startKey string, from, until time.Time) url.Values {
	query := url.Values{"user_id": {userID}}
	if !from.IsZero() {
		query.Set(startKey, from.UTC().Format(time.RFC3339Nano))
	}
	if !until.IsZero() {
		query.Set("until", until.UTC().Format(time.RFC3339Nano))
	}
	return query
}

func (client *HTTPClient) UserBilling(ctx context.Context, userID string, from, until time.Time) (UserBilling, error) {
	query := scopedQuery(userID, "from", from, until)
	result := UserBilling{Daily: []DailyUsage{}, ByModel: []ModelUsage{}, ByKey: []KeyUsage{}, ByVM: []VMUsage{}}
	for _, group := range []string{"vm", "key"} {
		query.Set("group_by", group)
		var data struct {
			Totals panelSpend `json:"totals"`
			Items  []struct {
				VMID  string `json:"vm_id"`
				KeyID string `json:"api_key_id"`
				panelSpend
			} `json:"items"`
		}
		if err := client.call(ctx, http.MethodGet, "/billing", query, nil, &data); err != nil {
			return UserBilling{}, err
		}
		if group == "vm" {
			result.Totals = data.Totals.mapped()
		}
		for _, row := range data.Items {
			spend := row.mapped()
			if group == "vm" {
				result.ByVM = append(result.ByVM, VMUsage{VMID: row.VMID, Requests: spend.Requests, Tokens: spend.Tokens, CostUSD: spend.CostUSD})
			} else {
				result.ByKey = append(result.ByKey, KeyUsage{KeyID: row.KeyID, Requests: spend.Requests, Tokens: spend.Tokens, CostUSD: spend.CostUSD})
			}
		}
	}
	return result, nil
}

func (client *HTTPClient) UserRequestLogs(ctx context.Context, userID string, input LogQuery) (LogPage, error) {
	page := max(1, input.Page)
	pageSize := input.PageSize
	if pageSize <= 0 {
		pageSize = 50
	}
	pageSize = min(500, pageSize)
	query := scopedQuery(userID, "since", input.From, input.Until)
	query.Set("limit", strconv.Itoa(pageSize))
	query.Set("offset", strconv.FormatInt(int64(page-1)*int64(pageSize), 10))
	if input.Model != "" {
		query.Set("model", input.Model)
	}
	if input.KeyID != "" {
		query.Set("api_key_id", input.KeyID)
	}
	if input.VMID != "" {
		query.Set("vm_id", input.VMID)
	}
	if input.Status != "" {
		query.Set("status", input.Status)
	}
	var data struct {
		Total int64 `json:"total"`
		Items []struct {
			LogRow
			APIKeyID  string   `json:"api_key_id"`
			Status    int      `json:"status"`
			ErrorCode string   `json:"error_code"`
			TotalCost *float64 `json:"total_cost"`
		} `json:"items"`
	}
	if err := client.call(ctx, http.MethodGet, "/request-logs", query, nil, &data); err != nil {
		return LogPage{}, err
	}
	result := LogPage{Items: []LogRow{}, Total: data.Total, Page: page, PageSize: pageSize}
	for _, raw := range data.Items {
		row := raw.LogRow
		row.CreatedAt = row.CreatedAt.UTC()
		row.KeyID = raw.APIKeyID
		row.StatusCode = raw.Status
		row.ErrorLabel = raw.ErrorCode
		if raw.TotalCost != nil {
			row.CostUSD = *raw.TotalCost
		}
		result.Items = append(result.Items, row)
	}
	return result, nil
}

func (client *HTTPClient) UserRequestStats(ctx context.Context, userID string, from, until time.Time) (RequestStats, error) {
	var data struct {
		Window struct {
			Requests int64   `json:"requests"`
			Errors   int64   `json:"errors"`
			SLA      float64 `json:"sla"`
			TTFT     struct {
				Average float64 `json:"avg_ms"`
				P95     float64 `json:"p95_ms"`
			} `json:"ttft"`
		} `json:"window"`
	}
	err := client.call(ctx, http.MethodGet, "/request-logs/stats", scopedQuery(userID, "since", from, until), nil, &data)
	return RequestStats{Requests: data.Window.Requests, ErrorCount: data.Window.Errors, SuccessRate: data.Window.SLA,
		AvgTTFTMS: data.Window.TTFT.Average, P95TTFTMS: data.Window.TTFT.P95}, err
}
