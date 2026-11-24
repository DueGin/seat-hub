package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"sync"
	"time"
)

// UserError is an error the HTTP layer knows how to render.
type UserError struct {
	Status  int
	Code    string
	Message string
}

func (e *UserError) Error() string { return e.Message }

type errMapping struct {
	sentinel error
	u        *UserError
}

var (
	errMappings []errMapping
	errMu       sync.RWMutex
)

// Err registers a sentinel error with its HTTP mapping and returns the
// sentinel, so packages can write `var ErrFoo = api.Err(errors.New("foo"), 404, "foo", "…")`.
//
// Prefer package-level registration. Callers do construct mappings on the
// request path, so the registry is guarded and deduplicated by sentinel: the
// same sentinel re-registered with the same code is a no-op instead of an
// unbounded append.
func Err(sentinel error, status int, code, message string) error {
	errMu.Lock()
	defer errMu.Unlock()
	for i := range errMappings {
		if errMappings[i].sentinel == sentinel && errMappings[i].u.Code == code {
			return sentinel
		}
	}
	errMappings = append(errMappings, errMapping{sentinel: sentinel, u: &UserError{Status: status, Code: code, Message: message}})
	return sentinel
}

// AsUserError maps a known sentinel to its UserError; nil when unknown.
func AsUserError(err error) *UserError {
	errMu.RLock()
	mappings := errMappings
	errMu.RUnlock()
	for _, m := range mappings {
		if errors.Is(err, m.sentinel) {
			return m.u
		}
	}
	var u *UserError
	if errors.As(err, &u) {
		return u
	}
	return nil
}

// --- data shapes (subset of docs/PORTAL_API.md the UI consumes) ---

type UserDTO struct {
	ID        int64  `json:"id"`
	Email     string `json:"email"`
	Role      string `json:"role"`
	Status    string `json:"status"`
	CreatedAt string `json:"created_at"`
}

type ProductDTO struct {
	ID           int64  `json:"id"`
	Kind         string `json:"kind"`
	Name         string `json:"name"`
	Description  string `json:"description"`
	Badge        string `json:"badge"`
	UnitQuantity int    `json:"unit_quantity"`
	DurationDays int    `json:"duration_days"`
	PriceCents   int64  `json:"price_cents"`
	Currency     string `json:"currency"`
	MinQty       int    `json:"min_qty"`
	MaxQty       int    `json:"max_qty"`
	Enabled      bool   `json:"enabled"`
	SortOrder    int    `json:"sort_order"`
	Stock        *int   `json:"stock"`
	// ReservationOnly products cannot be bought; users leave a notification
	// email instead (docs/FEATURES_V05.md → 预约).
	ReservationOnly bool `json:"reservation_only"`
}

type ProductInput struct {
	Kind         string `json:"kind"`
	Name         string `json:"name"`
	Description  string `json:"description"`
	Badge        string `json:"badge"`
	UnitQuantity int    `json:"unit_quantity"`
	DurationDays int    `json:"duration_days"`
	PriceCents   int64  `json:"price_cents"`
	Currency     string `json:"currency"`
	MinQty       int    `json:"min_qty"`
	MaxQty       int    `json:"max_qty"`
	Enabled      *bool  `json:"enabled"`
	SortOrder    int    `json:"sort_order"`
	// ReservationOnly nil leaves the flag unchanged on update; false on create.
	ReservationOnly *bool `json:"reservation_only"`
}

type OrderDTO struct {
	OrderNo             string  `json:"order_no"`
	ProductID           int64   `json:"product_id"`
	ProductName         string  `json:"product_name"`
	Kind                string  `json:"kind"`
	Quantity            int     `json:"quantity"`
	Units               int     `json:"units"`
	AmountCents         int64   `json:"amount_cents"`
	Currency            string  `json:"currency"`
	Provider            string  `json:"provider"`
	Status              string  `json:"status"`
	RenewSubscriptionID *int64  `json:"renew_subscription_id"`
	CreatedAt           string  `json:"created_at"`
	ExpiresAt           string  `json:"expires_at"`
	PaidAt              *string `json:"paid_at"`
	FulfilledAt         *string `json:"fulfilled_at"`
	UserEmail           string  `json:"user_email,omitempty"`
	UserID              int64   `json:"user_id,omitempty"`
	FulfilError         string  `json:"fulfil_error,omitempty"`
}

type PaymentDTO struct {
	Type         string `json:"type"` // redirect | qrcode | manual | mock
	URL          string `json:"url,omitempty"`
	QR           string `json:"qr,omitempty"`
	Instructions string `json:"instructions,omitempty"`
}

type SubscriptionDTO struct {
	ID              int64   `json:"id"`
	Kind            string  `json:"kind"`
	Quantity        int     `json:"quantity"`
	Status          string  `json:"status"`
	StartsAt        string  `json:"starts_at"`
	ExpiresAt       string  `json:"expires_at"`
	SourceOrderNo   string  `json:"source_order_no,omitempty"`
	Renewable       bool    `json:"renewable"`
	RenewProductIDs []int64 `json:"renew_product_ids"`
}

type WindowUsage struct {
	Utilization float64 `json:"utilization"`
	ResetsAt    string  `json:"resets_at,omitempty"`
	Status      string  `json:"status"`
}

type EstimateDTO struct {
	Status       string   `json:"status"`
	SpentUSD     float64  `json:"spent_usd"`
	TotalUSD     *float64 `json:"total_usd"`
	RemainingUSD *float64 `json:"remaining_usd"`
	UsedRatio    *float64 `json:"used_ratio"`
}

type SpendDTO struct {
	Requests            int64   `json:"requests"`
	InputTokens         int64   `json:"input_tokens"`
	OutputTokens        int64   `json:"output_tokens"`
	CacheReadTokens     int64   `json:"cache_read_tokens"`
	CacheCreationTokens int64   `json:"cache_creation_tokens"`
	Tokens              int64   `json:"tokens"`
	CostUSD             float64 `json:"cost_usd"`
}

type DailyDTO struct {
	Date     string  `json:"date"`
	Requests int64   `json:"requests"`
	Tokens   int64   `json:"tokens"`
	CostUSD  float64 `json:"cost_usd"`
}

type ModelDTO struct {
	Model    string  `json:"model"`
	Requests int64   `json:"requests"`
	Tokens   int64   `json:"tokens"`
	CostUSD  float64 `json:"cost_usd"`
}

type AccountUsageDTO struct {
	Available       bool         `json:"available"`
	ScheduleState   string       `json:"schedule_state"`
	RestrictionText string       `json:"restriction_text"`
	CredStatus      string       `json:"cred_status"`
	FiveHour        *WindowUsage `json:"five_hour"`
	SevenDay        *WindowUsage `json:"seven_day"`
	SevenDayFable   *WindowUsage `json:"seven_day_fable"`
	Estimate5h      *EstimateDTO `json:"estimate_5h"`
	Estimate7d      *EstimateDTO `json:"estimate_7d"`
	Today           *SpendDTO    `json:"today"`
	Window5h        *SpendDTO    `json:"window_5h"`
	Window7d        *SpendDTO    `json:"window_7d"`
	SeatsUsed       int          `json:"seats_used"`
	SeatsMax        int          `json:"seats_max"`
	Inflight        int          `json:"inflight"`
	FetchedAt       string       `json:"fetched_at"`
	Error           string       `json:"error,omitempty"`
}

type AccountDTO struct {
	ID           int64            `json:"id"`
	Kind         string           `json:"kind"`
	Label        string           `json:"label"`
	EmailMasked  string           `json:"email_masked"`
	Status       string           `json:"status"`
	StatusReason string           `json:"status_reason,omitempty"`
	Tier         string           `json:"tier"`
	CreatedAt    string           `json:"created_at"`
	ExpiresAt    *string          `json:"expires_at"`
	Usage        *AccountUsageDTO `json:"usage"`
	Selfcheck    *SelfcheckDTO    `json:"selfcheck"`
}

// SelfcheckDTO is a slot's latest self-check (docs/SELFCHECK.md).
// State: pending | pass | fail | exhausted. Attempts counts rebuilds so far;
// MaxAttempts is the configured budget (default 3). Checks carry user-safe
// messages only; admin endpoints add Detail.
type SelfcheckDTO struct {
	State       string              `json:"state"`
	Attempts    int                 `json:"attempts"`
	MaxAttempts int                 `json:"max_attempts"`
	CheckedAt   *string             `json:"checked_at"`
	Checks      []SelfcheckCheckDTO `json:"checks"`
}

type SelfcheckCheckDTO struct {
	Code    string `json:"code"`
	OK      bool   `json:"ok"`
	Skipped bool   `json:"skipped,omitempty"`
	Remedy  string `json:"remedy,omitempty"`
	Message string `json:"message"`
	Detail  string `json:"detail,omitempty"`
}

// SelfcheckReportDTO is one recorded run, for the admin history view.
type SelfcheckReportDTO struct {
	ID        int64               `json:"id"`
	VMID      string              `json:"vm_id"`
	Attempt   int                 `json:"attempt"`
	Passed    bool                `json:"passed"`
	Checks    []SelfcheckCheckDTO `json:"checks"`
	CreatedAt string              `json:"created_at"`
}

type OnboardingDTO struct {
	AccountID        int64         `json:"account_id"`
	Status           string        `json:"status"`
	AuthURL          string        `json:"auth_url,omitempty"`
	AuthURLExpiresAt string        `json:"auth_url_expires_at,omitempty"`
	Bootstrap        *BootstrapDTO `json:"bootstrap"`
	Error            *ErrorDTO     `json:"error,omitempty"`
	CanRegenerate    bool          `json:"can_regenerate"`
	Selfcheck        *SelfcheckDTO `json:"selfcheck"`
}

type BootstrapDTO struct {
	State   string `json:"state"`
	Message string `json:"message,omitempty"`
}

type KeyDTO struct {
	ID         int64       `json:"id"`
	Name       string      `json:"name"`
	Prefix     string      `json:"prefix"`
	Status     string      `json:"status"`
	CreatedAt  string      `json:"created_at"`
	LastUsedAt *string     `json:"last_used_at"`
	Scope      KeyScopeDTO `json:"scope"`
}

// KeyScopeDTO is which of the user's accounts a key may call
// (docs/KEY_SCOPE.md).
type KeyScopeDTO struct {
	Mode     string  `json:"mode"`     // all | selected
	Accounts []int64 `json:"accounts"` // resource ids; [] when mode is all
	// Usable is false when mode is selected and none of the selected accounts
	// is currently available; such a key is rejected with key_no_accounts.
	Usable bool `json:"usable"`
}

// KeyScopeInput is the requested scope. Mode "" means all.
type KeyScopeInput struct {
	Mode     string  `json:"mode"`
	Accounts []int64 `json:"accounts"`
}

// KeyInput creates a key. The zero Scope means all accounts.
type KeyInput struct {
	Name  string
	Scope KeyScopeInput
}

// KeyPatch changes a key; nil fields are left alone.
type KeyPatch struct {
	Name   *string
	Status *string // active | disabled
	Scope  *KeyScopeInput
}

type OverviewDTO struct {
	BaseURL        string            `json:"base_url"`
	Concurrency    ConcurrencyDTO    `json:"concurrency"`
	HostedSeats    SeatDTO           `json:"hosted_seats"`
	RentalAccounts SeatDTO           `json:"rental_accounts"`
	AccountsActive int               `json:"accounts_active"`
	Keys           int               `json:"keys"`
	ExpiringSoon   []SubscriptionDTO `json:"expiring_soon"`
	UsageToday     *SpendDTO         `json:"usage_today"`
}

type ConcurrencyDTO struct {
	Total        int `json:"total"`
	InUse        int `json:"in_use"`
	Base         int `json:"base"`
	Purchased    int `json:"purchased"`
	FromAccounts int `json:"from_accounts"`
}

type SeatDTO struct {
	Total int `json:"total"`
	Used  int `json:"used"`
}

type PaymentProviderDTO struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

type SiteConfig struct {
	SiteName                  string               `json:"site_name"`
	BaseURL                   string               `json:"base_url"`
	RegisterEmailVerification bool                 `json:"register_email_verification"`
	RegisterEmailDomains      []string             `json:"register_email_domains"`
	RegisterEmailWhitelist    []string             `json:"-"`
	RegistrationOpen          bool                 `json:"registration_open"`
	PaymentProviders          []PaymentProviderDTO `json:"payment_providers"`
	Dev                       bool                 `json:"dev"`
	// Version is the running build. Open pages compare it with the version
	// they loaded with and offer a reload after an in-site update.
	Version string `json:"version"`
}

type SettingsPayload struct {
	SiteName                  string          `json:"site_name"`
	RegisterEmailVerification bool            `json:"register_email_verification"`
	RegisterEmailWhitelist    []string        `json:"register_email_whitelist"`
	RegistrationOpen          bool            `json:"registration_open"`
	BaseConcurrency           int             `json:"base_concurrency"`
	ConcurrencyPerHostedSeat  int             `json:"concurrency_per_hosted_seat"`
	ConcurrencyPerRental      int             `json:"concurrency_per_rental_account"`
	HostedGraceHours          int             `json:"hosted_grace_hours"`
	RentalGraceHours          int             `json:"rental_grace_hours"`
	GatewayQueueTimeoutMS     int             `json:"gateway_queue_timeout_ms"`
	HostedVMTemplate          json.RawMessage `json:"hosted_vm_template"`
	Payment                   json.RawMessage `json:"payment"`

	SelfcheckRequireDedicatedProxy bool `json:"selfcheck_require_dedicated_proxy"`
	SelfcheckRequireTimezoneMatch  bool `json:"selfcheck_require_timezone_match"`
	SelfcheckMaxRebuilds           int  `json:"selfcheck_max_rebuilds"`
	SelfcheckRecheckMinutes        int  `json:"selfcheck_recheck_minutes"`
}

// --- service interfaces (implemented in internal/app) ---

type UserStore interface {
	ByID(ctx context.Context, id int64) (UserDTO, error)
	ByEmail(ctx context.Context, email string) (UserDTO, error)
	Create(ctx context.Context, email, passwordHash, role string) (UserDTO, error)
	Count(ctx context.Context) (int64, error)
	CountAdmins(ctx context.Context) (int64, error)
	SetPassword(ctx context.Context, id int64, hash string) error
	SetStatus(ctx context.Context, id int64, status string) error
	SetRole(ctx context.Context, id int64, role string) error
	List(ctx context.Context, q string, page, pageSize int) ([]UserDTO, int64, error)
	AdminList(ctx context.Context, filter AdminUserFilter, page, pageSize int) ([]AdminUserDTO, int64, error)
	AdminDetail(ctx context.Context, id int64) (AdminUserDTO, error)
	SetEmail(ctx context.Context, id int64, email string) error
	SetNote(ctx context.Context, id int64, note string) error
	// Records counts orders, subscriptions, resources and ticket history; any of them blocks Delete.
	Records(ctx context.Context, id int64) (UserRecordsDTO, error)
	// Delete removes a user without business records, its keys (upstream too) and sessions.
	Delete(ctx context.Context, id int64) error
	Sessions(ctx context.Context, id int64) ([]SessionDTO, error)
	RevokeSessions(ctx context.Context, id int64) error
}

// AdminUserFilter narrows the admin user list; empty fields match everything.
type AdminUserFilter struct {
	Q      string
	Role   string
	Status string
}

type UserRecordsDTO struct {
	Orders        int64 `json:"orders"`
	Subscriptions int64 `json:"subscriptions"`
	Resources     int64 `json:"resources"`
	Tickets       int64 `json:"tickets"` // tickets opened, messages written, files uploaded
}

type SessionDTO struct {
	CreatedAt string `json:"created_at"`
	ExpiresAt string `json:"expires_at"`
	IP        string `json:"ip"`
	UserAgent string `json:"user_agent"`
}

type AdminUserDTO struct {
	UserDTO
	ConcurrencyTotal int    `json:"concurrency_total"`
	HostedUsed       int    `json:"hosted_used"`
	HostedTotal      int    `json:"hosted_total"`
	RentalUsed       int    `json:"rental_used"`
	RentalTotal      int    `json:"rental_total"`
	VM2APIUserID     string `json:"vm2api_user_id"`
	VM2APIUsername   string `json:"vm2api_username"`
	BaseURL          string `json:"-"`
	Note             string `json:"note"`
	LastLoginAt      string `json:"last_login_at"`
	LastLoginIP      string `json:"last_login_ip"`
	ActiveSessions   int64  `json:"active_sessions"`
}

type SettingsStore interface {
	Get(ctx context.Context) (SettingsPayload, error)
	Put(ctx context.Context, p SettingsPayload, actorID int64) error
}

type AuditLog interface {
	Write(ctx context.Context, actorID int64, action, target string, detail any)
}

// AuthService is the session layer; implemented by internal/auth.Service.
type AuthService interface {
	Login(ctx context.Context, email, password, ip, ua string) (token string, user UserDTO, err error)
	Logout(ctx context.Context, tokenHash string) error
	SetCookie(w http.ResponseWriter, token string)
	ClearCookie(w http.ResponseWriter)
	Middleware(next http.Handler) http.Handler
	UserFrom(ctx context.Context) (UserDTO, bool)
	TokenHashFrom(ctx context.Context) string
	Allowed(ip, email string) bool
	HashPassword(password string) (string, error)
	VerifyPassword(userID int64, password string) bool
}

// SessionRevoker lets the password-change handler drop every session of a user
// except the one that performed the change (PORTAL_API.md: 撤销其它会话).
//
// It is a separate interface rather than a member of AuthService so a service
// that cannot revoke sessions still satisfies AuthService; the handler only
// enables revocation when the implementation supports it.
type SessionRevoker interface {
	RevokeOthers(ctx context.Context, userID int64, keepTokenHash string) error
}

type CatalogService interface {
	Public(ctx context.Context) ([]ProductDTO, error)
	All(ctx context.Context) ([]ProductDTO, error)
	Get(ctx context.Context, id int64) (ProductDTO, error)
	Create(ctx context.Context, in ProductInput) (ProductDTO, error)
	Update(ctx context.Context, id int64, in ProductInput) (ProductDTO, error)
	Delete(ctx context.Context, id int64) error
	Stock(ctx context.Context, productID int64) (int, error)
}

type OrderService interface {
	Create(ctx context.Context, userID int64, productID int64, quantity int, provider string, renewSubscriptionID *int64) (OrderDTO, error)
	Get(ctx context.Context, orderNo string) (OrderDTO, error)
	ListByUser(ctx context.Context, userID int64, page, pageSize int) ([]OrderDTO, int64, error)
	AdminList(ctx context.Context, status string, userID int64, page, pageSize int) ([]OrderDTO, int64, error)
	Cancel(ctx context.Context, orderNo string) error
	MarkPaidByAdmin(ctx context.Context, orderNo, tradeNo string) error
	Refulfill(ctx context.Context, orderNo string) error
}

type PaymentService interface {
	Providers(ctx context.Context) []PaymentProviderDTO
	CreatePayment(ctx context.Context, o OrderDTO) (PaymentDTO, error)
	Notify(ctx context.Context, provider string, r *http.Request) (statusCode int, body string, err error)
	// ReturnURL is the synchronous browser landing page for a provider, if any.
	ReturnURL(ctx context.Context, provider string, r *http.Request) (string, error)
	// MockPay settles a dev-only mock order after checking that the order
	// belongs to the user and was created for the mock provider.
	MockPay(ctx context.Context, userID int64, orderNo string) error
}

type AccountService interface {
	List(ctx context.Context, userID int64, includeReleased bool) ([]AccountDTO, error)
	Get(ctx context.Context, userID, id int64) (AccountDTO, []DailyDTO, []ModelDTO, error)
	Rename(ctx context.Context, userID, id int64, label string) error
	CreateHosted(ctx context.Context, userID int64, label string) (AccountDTO, OnboardingDTO, error)
	Onboarding(ctx context.Context, userID, id int64) (OnboardingDTO, error)
	RegenerateAuthURL(ctx context.Context, userID, id int64) (OnboardingDTO, error)
	SubmitAuthCode(ctx context.Context, userID, id int64, code string) (OnboardingDTO, error)
	RetryHosted(ctx context.Context, userID, id int64) (OnboardingDTO, error)
	DeleteHosted(ctx context.Context, userID, id int64) error
	AdminList(ctx context.Context, kind, status string, userID int64, page, pageSize int) ([]AdminAccountDTO, int64, error)
	AdminRetry(ctx context.Context, id int64) error
	AdminRelease(ctx context.Context, id int64) error
}

type AdminAccountDTO struct {
	AccountDTO
	UserID    int64  `json:"user_id"`
	UserEmail string `json:"user_email"`
	VMID      string `json:"vm_id"`
	LastError string `json:"last_error,omitempty"`
}

type UsageService interface {
	Overview(ctx context.Context, userID int64) (OverviewDTO, error)
	Subscriptions(ctx context.Context, userID int64, status string) ([]SubscriptionDTO, error)
	Summary(ctx context.Context, userID int64, from, until time.Time) (UsageSummaryDTO, error)
	Logs(ctx context.Context, userID int64, q UsageLogQuery) (UsageLogPageDTO, error)
	KeyStats(ctx context.Context, userID, keyID int64) ([]DailyDTO, []ModelDTO, error)
}

type UsageSummaryDTO struct {
	From      string               `json:"from"`
	Until     string               `json:"until"`
	Totals    SpendDTO             `json:"totals"`
	Daily     []DailyDTO           `json:"daily"`
	ByModel   []ModelDTO           `json:"by_model"`
	ByKey     []KeyUsageDTO        `json:"by_key"`
	ByAccount []AccountUsageRowDTO `json:"by_account"`
	Stats     StatsDTO             `json:"stats"`
}

type KeyUsageDTO struct {
	KeyID    int64   `json:"key_id"`
	KeyName  string  `json:"key_name"`
	Requests int64   `json:"requests"`
	Tokens   int64   `json:"tokens"`
	CostUSD  float64 `json:"cost_usd"`
}

type AccountUsageRowDTO struct {
	AccountID int64   `json:"account_id"`
	Label     string  `json:"label"`
	Requests  int64   `json:"requests"`
	Tokens    int64   `json:"tokens"`
	CostUSD   float64 `json:"cost_usd"`
}

type StatsDTO struct {
	SuccessRate float64 `json:"success_rate"`
	AvgTTFTMS   float64 `json:"avg_ttft_ms"`
	P95TTFTMS   float64 `json:"p95_ttft_ms"`
	ErrorCount  int64   `json:"error_count"`
}

type UsageLogQuery struct {
	Page     int
	PageSize int
	From     time.Time
	Until    time.Time
	Model    string
	KeyID    int64
	// AccountID limits the log to one of the user's hosted or rental accounts.
	AccountID int64
	Status    string
}

type UsageLogPageDTO struct {
	Items    []UsageLogRowDTO `json:"items"`
	Total    int64            `json:"total"`
	Page     int              `json:"page"`
	PageSize int              `json:"page_size"`
}

type UsageLogRowDTO struct {
	RequestID           string  `json:"request_id"`
	CreatedAt           string  `json:"created_at"`
	Model               string  `json:"model"`
	KeyID               int64   `json:"key_id"`
	KeyName             string  `json:"key_name"`
	AccountID           int64   `json:"account_id"`
	AccountLabel        string  `json:"account_label"`
	Stream              bool    `json:"stream"`
	StatusCode          int     `json:"status_code"`
	ErrorLabel          string  `json:"error_label,omitempty"`
	InputTokens         int64   `json:"input_tokens"`
	OutputTokens        int64   `json:"output_tokens"`
	CacheReadTokens     int64   `json:"cache_read_tokens"`
	CacheCreationTokens int64   `json:"cache_creation_tokens"`
	CostUSD             float64 `json:"cost_usd"`
	DurationMS          int64   `json:"duration_ms"`
	FirstTokenMS        *int64  `json:"first_token_ms"`
}

type KeyService interface {
	List(ctx context.Context, userID int64) ([]KeyDTO, error)
	Create(ctx context.Context, userID int64, in KeyInput) (KeyDTO, string, error)
	Update(ctx context.Context, userID, id int64, in KeyPatch) (KeyDTO, error)
	Delete(ctx context.Context, userID, id int64) error
}

// AdminService is everything the admin console calls. AdminCoreService is the
// part internal/portal implements; the self-check methods are implemented in
// internal/app, which owns the provision + selfcheck wiring.
type AdminService interface {
	AdminCoreService

	// Self-check (docs/SELFCHECK.md). RunSelfcheck re-checks a resource's slot
	// now; rebuild=true resets the retry budget and rebuilds first.
	RunSelfcheck(ctx context.Context, resourceID int64, rebuild bool) (SelfcheckDTO, error)
	SelfcheckHistory(ctx context.Context, resourceID int64, limit int) ([]SelfcheckReportDTO, error)
	RunPoolSelfcheck(ctx context.Context, vmID string) (SelfcheckDTO, error)
}

type AdminCoreService interface {
	Dashboard(ctx context.Context) (AdminDashboardDTO, error)
	RentalPool(ctx context.Context) ([]RentalPoolRowDTO, error)
	RentalCandidates(ctx context.Context) ([]RentalCandidateDTO, error)
	RentalAdd(ctx context.Context, vmIDs []string, note string) error
	RentalPatch(ctx context.Context, vmID, status, note string) error
	RentalRemove(ctx context.Context, vmID string) error
	Jobs(ctx context.Context, status string, page, pageSize int) ([]JobDTO, int64, error)
	RetryJob(ctx context.Context, id int64) error
	GrantSubscription(ctx context.Context, adminID, userID int64, kind string, quantity, durationDays int, note string) error
	PatchSubscription(ctx context.Context, id int64, expiresAt *string, status string) error
	// RecheckScopes queues an account-scope reconcile now.
	RecheckScopes(ctx context.Context) error
}

type AdminDashboardDTO struct {
	Users         int64          `json:"users"`
	PayingUsers   int64          `json:"paying_users"`
	RevenueCents  int64          `json:"revenue_cents_30d"`
	OrdersPending int64          `json:"orders_pending"`
	Accounts      AccountsCounts `json:"accounts"`
	RentalPool    RentalCounts   `json:"rental_pool"`
	VM2API        VM2APIHealth   `json:"vm2api"`
	// ScopeWarnings are upstream ownership or migration problems.
	ScopeWarnings ScopeWarningsDTO `json:"scope_warnings"`
}

type ScopeWarningsDTO struct {
	CheckedAt string            `json:"checked_at"` // last reconcile; "" before the first
	Items     []ScopeWarningDTO `json:"items"`
}

type ScopeWarningDTO struct {
	Kind      string `json:"kind"`
	UserID    int64  `json:"user_id,omitempty"`
	UserEmail string `json:"user_email,omitempty"`
	VMID      string `json:"vm_id,omitempty"`
	KeyID     string `json:"key_id,omitempty"`
	Message   string `json:"message"`
}

type AccountsCounts struct {
	HostedActive int `json:"hosted_active"`
	RentalActive int `json:"rental_active"`
	Failed       int `json:"failed"`
}

type RentalCounts struct {
	Total       int `json:"total"`
	Available   int `json:"available"`
	Leased      int `json:"leased"`
	Maintenance int `json:"maintenance"`
}

type VM2APIHealth struct {
	OK        bool   `json:"ok"`
	Version   string `json:"version"`
	LatencyMS int64  `json:"latency_ms"`
	Error     string `json:"error,omitempty"`
}

type RentalPoolRowDTO struct {
	VMID           string        `json:"vm_id"`
	Tier           string        `json:"tier"`
	Status         string        `json:"status"`
	LeasedToUserID *int64        `json:"leased_to_user_id"`
	LeasedToEmail  string        `json:"leased_to_email,omitempty"`
	Note           string        `json:"note"`
	VM             *RentalVMDTO  `json:"vm"`
	Selfcheck      *SelfcheckDTO `json:"selfcheck"`
	OwnedBy        string        `json:"owned_by,omitempty"`
}

type RentalVMDTO struct {
	Label         string       `json:"label"`
	EmailMasked   string       `json:"email_masked"`
	CredStatus    string       `json:"cred_status"`
	ScheduleState string       `json:"schedule_state"`
	Tier          string       `json:"tier"`
	FiveHour      *WindowUsage `json:"five_hour"`
	SevenDay      *WindowUsage `json:"seven_day"`
}

type RentalCandidateDTO struct {
	VMID          string `json:"vm_id"`
	Label         string `json:"label"`
	EmailMasked   string `json:"email_masked"`
	CredStatus    string `json:"cred_status"`
	ScheduleState string `json:"schedule_state"`
	Tier          string `json:"tier"`
}

type JobDTO struct {
	ID        int64  `json:"id"`
	Type      string `json:"type"`
	Status    string `json:"status"`
	Attempts  int    `json:"attempts"`
	RunAfter  string `json:"run_after"`
	LastError string `json:"last_error"`
	Payload   string `json:"payload"`
	CreatedAt string `json:"created_at"`
	UpdatedAt string `json:"updated_at"`
}

// RegistrationService issues codes and consumes them atomically with user creation.
type RegistrationService interface {
	Issue(ctx context.Context, email, ip, siteName string) error
	Register(ctx context.Context, email, passwordHash, role, code string) (UserDTO, error)
}
