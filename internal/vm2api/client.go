// Package vm2api is the only place that talks to vm2api.
//
// Everything above this package depends on the Client interface and the
// portal-shaped types below, never on vm2api's raw JSON. The HTTP
// implementation (http.go) maps vm2api responses into these types; the fake
// (fake.go) backs tests and cmd/mock-vm2api.
//
// Panel admin calls use the master key; tenant key calls use a panel session.
// Hosted onboarding is OAuth
// authorization-link only: the flavor is fixed to "claude_code" inside the
// implementation and no session-key / setup-token / credential-import call
// exists on this interface. Do not add one.
package vm2api

import (
	"context"
	"encoding/json"
	"time"
)

type Client interface {
	Ping(ctx context.Context) (Health, error)

	// Users (vm2api role=user, vm_create_quota=0). Never delete: deleting a
	// vm2api user hands its admin_assigned VMs back to the platform pool.
	CreateUser(ctx context.Context, username, password string) (User, error)
	ListUsers(ctx context.Context) ([]User, error)
	SetUserEnabled(ctx context.Context, userID string, enabled bool) error
	SetUserPassword(ctx context.Context, userID, password string) error

	// Master key operations see only ownerless keys.
	KeyClient
	ForUser(ctx context.Context, userID, username, password string) (KeyClient, error)

	// Account pools (vm2api vm-pools). A VM belongs to at most one pool; a
	// pool referenced by a key cannot be deleted. SetPoolMembers replaces the
	// member list and also (re-)enables the pool.
	ListPools(ctx context.Context) ([]Pool, error)
	CreatePool(ctx context.Context, name string, vmIDs []string) (Pool, error)
	SetPoolMembers(ctx context.Context, poolID string, vmIDs []string) (Pool, error)
	DeletePool(ctx context.Context, poolID string) error

	// VMs.
	ListVMs(ctx context.Context, f VMFilter) ([]VM, error)
	GetAccountUsage(ctx context.Context, vmID string) (AccountUsage, error)
	CreateHostedVM(ctx context.Context, label string, template json.RawMessage) (VM, error)
	SetVMOwner(ctx context.Context, vmID string, userID *string) error
	AllocateProxy(ctx context.Context, vmID string) error
	SetSchedulable(ctx context.Context, vmID string, schedulable bool) error
	DeleteVM(ctx context.Context, vmID string) error

	// Slot self-check (docs/SELFCHECK.md). All of these go through vm2api, so
	// the outbound traffic uses the slot's own proxy, never the portal's.
	CollectIdentity(ctx context.Context, vmID string) (Fingerprint, error)
	ProbeExit(ctx context.Context, vmID string) (ExitProbe, error)
	// ResetVM destroys the slot's home directory and regenerates its
	// fingerprint (mode=traits), keeping the VM id and its proxy binding.
	ResetVM(ctx context.Context, vmID string) error
	// UnbindProxy detaches one VM from a pool proxy; DisableProxy keeps a bad
	// proxy from being handed out again. AllocateProxy then binds a new one.
	UnbindProxy(ctx context.Context, proxyID, vmID string) error
	DisableProxy(ctx context.Context, proxyID string) error

	// Hosted onboarding: OAuth authorization link (flavor claude_code) only.
	GenerateAuthURL(ctx context.Context, vmID string) (AuthURL, error)
	ExchangeCode(ctx context.Context, vmID, sessionID, code string) error
	BootstrapStatus(ctx context.Context, vmID string) (Bootstrap, error)

	// Usage and logs, scoped to one vm2api user; userID "" drops the user
	// filter (master view), which callers combine with LogQuery.KeyID.
	UserBilling(ctx context.Context, userID string, from, until time.Time) (UserBilling, error)
	UserRequestLogs(ctx context.Context, userID string, q LogQuery) (LogPage, error)
	UserRequestStats(ctx context.Context, userID string, from, until time.Time) (RequestStats, error)
}

// KeyClient operates only on keys owned by the authenticated panel identity.
type KeyClient interface {
	CreateKey(ctx context.Context, in CreateKeyInput) (Key, error)
	UpdateKey(ctx context.Context, keyID string, in UpdateKeyInput) error
	DeleteKey(ctx context.Context, keyID string) error
	KeyStats(ctx context.Context, keyID string) (UsageBreakdown, error)
	GetKey(ctx context.Context, keyID string) (KeyInfo, error)
	ListKeys(ctx context.Context, userID string) ([]KeyInfo, error)
}

type Health struct {
	OK        bool   `json:"ok"`
	Version   string `json:"version"`
	LatencyMS int64  `json:"latency_ms"`
}

type User struct {
	ID       string `json:"id"`
	Username string `json:"username"`
	Role     string `json:"role"`
	Enabled  bool   `json:"enabled"`
}

type CreateKeyInput struct {
	Name           string
	UserID         string
	MaxConcurrency int // 0..128, clamped by the implementation
	Scope          KeyScope
}

type UpdateKeyInput struct {
	Name           *string
	Active         *bool
	MaxConcurrency *int
	Scope          *KeyScope // nil = leave the scope alone
}

// KeyScope limits which VMs a key may schedule onto (docs/KEY_SCOPE.md).
// Exactly one field is set:
//
//   - All: group_type=all, subject only to VM ownership.
//   - PoolID: vm_pool_id + group_type=anthropic; the pool's live members.
//   - AllowedVMs: group_type=anthropic + allowed_vms (non-empty), vm_pool_id
//     cleared.
//
// The zero value is invalid and must never reach vm2api: it would mean
// group_type=all, i.e. every VM the key's owner can see.
type KeyScope struct {
	All        bool     `json:"all,omitempty"`
	PoolID     string   `json:"pool_id,omitempty"`
	AllowedVMs []string `json:"allowed_vms,omitempty"`
}

// Valid reports whether exactly one scope is set.
func (scope KeyScope) Valid() bool {
	if scope.All {
		return scope.PoolID == "" && len(scope.AllowedVMs) == 0
	}
	return (scope.PoolID != "") != (len(scope.AllowedVMs) > 0)
}

// KeyInfo is a key's live upstream state, used by reconciliation.
type KeyInfo struct {
	ID             string   `json:"id"`
	Name           string   `json:"name"`
	UserID         string   `json:"user_id"`
	Active         bool     `json:"active"` // status == "active"
	MaxConcurrency int      `json:"max_concurrency"`
	GroupType      string   `json:"group_type"` // all | anthropic | openai
	PoolID         string   `json:"vm_pool_id"`
	AllowedVMs     []string `json:"allowed_vms"`
}

// Scope returns the key's scope in KeyScope form; ok is false when the key
// uses a scope the portal does not support.
func (info KeyInfo) Scope() (scope KeyScope, ok bool) {
	if info.PoolID != "" {
		return KeyScope{PoolID: info.PoolID}, info.GroupType == "anthropic" || info.GroupType == "all"
	}
	if info.GroupType == "all" {
		return KeyScope{All: true}, true
	}
	if info.GroupType != "anthropic" || len(info.AllowedVMs) == 0 {
		return KeyScope{}, false
	}
	return KeyScope{AllowedVMs: append([]string(nil), info.AllowedVMs...)}, true
}

// Pool is a vm2api account pool.
type Pool struct {
	ID      string   `json:"id"` // pool_xxxxxxxx
	Name    string   `json:"name"`
	Enabled bool     `json:"enabled"`
	VMIDs   []string `json:"vm_ids"`
}

type Key struct {
	ID     string `json:"id"`
	Secret string `json:"-"` // sk-vm-…, only set by CreateKey
	Prefix string `json:"prefix"`
}

type VMFilter struct {
	OwnerUserID *string // nil = any owner
	Unowned     bool    // only platform VMs (no owner)
	ClaudeOnly  bool
}

// VM is the redacted subset of a vm2api slot the portal needs.
type VM struct {
	ID            string `json:"id"`
	Label         string `json:"label"`
	Platform      string `json:"platform"` // anthropic | openai
	OwnerUserID   string `json:"owner_user_id"`
	Origin        string `json:"origin"` // platform | admin_assigned | user_created
	EmailMasked   string `json:"email_masked"`
	Tier          string `json:"tier"`        // pro | max | ""
	CredStatus    string `json:"cred_status"` // vm2api cred_status text
	HasCredential bool   `json:"has_credential"`
	// on | off | unschedulable | restricted
	ScheduleState      string  `json:"schedule_state"`
	UnschedulableCode  string  `json:"unschedulable_reason"`
	RestrictionText    string  `json:"restriction_text"`
	ProxyConfigured    bool    `json:"proxy_configured"`
	FiveHour           *Window `json:"five_hour"`
	SevenDay           *Window `json:"seven_day"`
	SevenDayFable      *Window `json:"seven_day_fable"`
	SeatsUsed          int     `json:"seats_used"`
	SeatsMax           int     `json:"seats_max"`
	Inflight           int     `json:"inflight"`
	CredentialRejected bool    `json:"credential_rejected"` // unschedulable for oauth_* / credential_* reasons

	// Fingerprint is the slot's device identity as vm2api tracks it. The
	// self-check compares these keys across slots.
	Fingerprint Fingerprint `json:"fingerprint"`
	Timezone    string      `json:"timezone"`
	Locale      string      `json:"locale"`
	// Proxy is the bound exit; nil when nothing is bound.
	Proxy *ProxyView `json:"proxy"`
	// Account identifies the Claude account on the slot (empty until signed in).
	AccountUUID string `json:"account_uuid"`
	Email       string `json:"email"`
}

// Fingerprint is the subset of vm2api's fingerprint the self-check needs.
// Keys the portal never inspects are deliberately absent.
type Fingerprint struct {
	DeviceID       string `json:"device_id"`
	GuestMachineID string `json:"guest_machine_id"`
	Hostname       string `json:"hostname"`
	Timezone       string `json:"timezone"`
	Locale         string `json:"locale"`
	CollectedAt    string `json:"collected_at"`
}

// ProxyView is the bound exit as vm2api reports it.
type ProxyView struct {
	ID            string `json:"id"`
	Scheme        string `json:"scheme"` // socks5 | local
	Status        string `json:"status"` // ok | unknown | dead | fail
	Enabled       bool   `json:"enabled"`
	BlockedReason string `json:"blocked_reason"`
	GeoIP         string `json:"geo_ip"`
	GeoTimezone   string `json:"geo_timezone"`
}

// ExitProbe is the slot's real egress as observed by vm2api's exit probe.
type ExitProbe struct {
	Via         string `json:"via"` // socks5 | node | local | deploy_proxy
	IP          string `json:"ip"`
	CountryCode string `json:"country_code"`
	Timezone    string `json:"timezone"`
}

type Window struct {
	Utilization float64    `json:"utilization"` // 0..100
	ResetsAt    *time.Time `json:"resets_at"`
	Status      string     `json:"status"`
}

type Estimate struct {
	Status       string   `json:"status"`
	SpentUSD     float64  `json:"spent_usd"`
	TotalUSD     *float64 `json:"total_usd"`
	RemainingUSD *float64 `json:"remaining_usd"`
	UsedRatio    *float64 `json:"used_ratio"`
}

type Spend struct {
	Requests            int64   `json:"requests"`
	InputTokens         int64   `json:"input_tokens"`
	OutputTokens        int64   `json:"output_tokens"`
	CacheReadTokens     int64   `json:"cache_read_tokens"`
	CacheCreationTokens int64   `json:"cache_creation_tokens"`
	Tokens              int64   `json:"tokens"`
	CostUSD             float64 `json:"cost_usd"`
}

type DailyUsage struct {
	Date     string  `json:"date"` // YYYY-MM-DD (vm2api Shanghai day)
	Requests int64   `json:"requests"`
	Tokens   int64   `json:"tokens"`
	CostUSD  float64 `json:"cost_usd"`
}

type ModelUsage struct {
	Model    string  `json:"model"`
	Requests int64   `json:"requests"`
	Tokens   int64   `json:"tokens"`
	CostUSD  float64 `json:"cost_usd"`
}

type UsageBreakdown struct {
	Daily   []DailyUsage `json:"daily"`
	ByModel []ModelUsage `json:"by_model"`
}

// AccountUsage = GET /api/panel/vms/:id mapped to what a tenant may see.
type AccountUsage struct {
	VM         VM           `json:"vm"`
	Estimate5h *Estimate    `json:"estimate_5h"`
	Estimate7d *Estimate    `json:"estimate_7d"`
	Today      *Spend       `json:"today"`
	Window5h   *Spend       `json:"window_5h"`
	Window7d   *Spend       `json:"window_7d"`
	Daily      []DailyUsage `json:"daily"`
	ByModel    []ModelUsage `json:"by_model"`
	FetchedAt  time.Time    `json:"fetched_at"`
}

type AuthURL struct {
	URL       string    `json:"url"`
	SessionID string    `json:"session_id"`
	ExpiresAt time.Time `json:"expires_at"`
}

type Bootstrap struct {
	State   string `json:"state"` // pending | running | succeeded | failed
	Message string `json:"message"`
}

type KeyUsage struct {
	KeyID    string  `json:"key_id"`
	Requests int64   `json:"requests"`
	Tokens   int64   `json:"tokens"`
	CostUSD  float64 `json:"cost_usd"`
}

type VMUsage struct {
	VMID     string  `json:"vm_id"`
	Requests int64   `json:"requests"`
	Tokens   int64   `json:"tokens"`
	CostUSD  float64 `json:"cost_usd"`
}

type UserBilling struct {
	Totals  Spend        `json:"totals"`
	Daily   []DailyUsage `json:"daily"`
	ByModel []ModelUsage `json:"by_model"`
	ByKey   []KeyUsage   `json:"by_key"`
	ByVM    []VMUsage    `json:"by_vm"`
}

type LogQuery struct {
	From, Until time.Time
	Page        int
	PageSize    int
	Model       string
	KeyID       string // vm2api key id
	VMID        string // only requests served by this slot
	Status      string // "" | success | error
}

type LogRow struct {
	RequestID           string    `json:"request_id"`
	CreatedAt           time.Time `json:"created_at"`
	Model               string    `json:"model"`
	KeyID               string    `json:"key_id"` // vm2api key id
	VMID                string    `json:"vm_id"`
	Stream              bool      `json:"stream"`
	StatusCode          int       `json:"status_code"`
	ErrorLabel          string    `json:"error_label"`
	InputTokens         int64     `json:"input_tokens"`
	OutputTokens        int64     `json:"output_tokens"`
	CacheReadTokens     int64     `json:"cache_read_tokens"`
	CacheCreationTokens int64     `json:"cache_creation_tokens"`
	CostUSD             float64   `json:"cost_usd"`
	DurationMS          int64     `json:"duration_ms"`
	FirstTokenMS        *int64    `json:"first_token_ms"`
}

type LogPage struct {
	Items    []LogRow `json:"items"`
	Total    int64    `json:"total"`
	Page     int      `json:"page"`
	PageSize int      `json:"page_size"`
}

type RequestStats struct {
	Requests    int64   `json:"requests"`
	ErrorCount  int64   `json:"error_count"`
	SuccessRate float64 `json:"success_rate"`
	AvgTTFTMS   float64 `json:"avg_ttft_ms"`
	P95TTFTMS   float64 `json:"p95_ttft_ms"`
}
