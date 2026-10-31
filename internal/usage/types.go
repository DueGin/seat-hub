package usage

import (
	"errors"
	"time"

	"github.com/DueGin/vm2api-portal/internal/vm2api"
)

var (
	ErrNotFound          = errors.New("not found")
	ErrInvalidRange      = errors.New("invalid time range")
	ErrInvalidQuery      = errors.New("invalid log query")
	ErrInvalidPagination = errors.New("invalid pagination")
)

type Spend = vm2api.Spend
type DailyUsage = vm2api.DailyUsage
type ModelUsage = vm2api.ModelUsage
type Window = vm2api.Window
type Estimate = vm2api.Estimate

type AccountUsage struct {
	AccountID       int64        `json:"account_id"`
	Label           string       `json:"label"`
	Available       bool         `json:"available"`
	ScheduleState   string       `json:"schedule_state"`
	RestrictionText *string      `json:"restriction_text"`
	FiveHour        *Window      `json:"five_hour"`
	SevenDay        *Window      `json:"seven_day"`
	SevenDayFable   *Window      `json:"seven_day_fable"`
	Estimate5h      *Estimate    `json:"estimate_5h"`
	Estimate7d      *Estimate    `json:"estimate_7d"`
	Today           *Spend       `json:"today"`
	Window5h        *Spend       `json:"window_5h"`
	Window7d        *Spend       `json:"window_7d"`
	SeatsUsed       int          `json:"seats_used"`
	SeatsMax        int          `json:"seats_max"`
	Inflight        int          `json:"inflight"`
	FetchedAt       time.Time    `json:"fetched_at"`
	Daily           []DailyUsage `json:"daily"`
	ByModel         []ModelUsage `json:"by_model"`
}

type Overview struct {
	UsageToday *Spend `json:"usage_today"`
}

type Summary struct {
	From      time.Time      `json:"from"`
	Until     time.Time      `json:"until"`
	Totals    Spend          `json:"totals"`
	Daily     []DailyUsage   `json:"daily"`
	ByModel   []ModelUsage   `json:"by_model"`
	ByKey     []KeyUsage     `json:"by_key"`
	ByAccount []AccountSpend `json:"by_account"`
	Stats     Stats          `json:"stats"`
}

type KeyUsage struct {
	KeyID    int64   `json:"key_id"`
	KeyName  string  `json:"key_name"`
	Requests int64   `json:"requests"`
	Tokens   int64   `json:"tokens"`
	CostUSD  float64 `json:"cost_usd"`
}

type AccountSpend struct {
	AccountID int64   `json:"account_id"`
	Label     string  `json:"label"`
	Requests  int64   `json:"requests"`
	Tokens    int64   `json:"tokens"`
	CostUSD   float64 `json:"cost_usd"`
}

type Stats struct {
	SuccessRate float64 `json:"success_rate"`
	AvgTTFTMS   float64 `json:"avg_ttft_ms"`
	P95TTFTMS   float64 `json:"p95_ttft_ms"`
	ErrorCount  int64   `json:"error_count"`
}

type LogQuery struct {
	From, Until time.Time
	Page        int
	PageSize    int
	Model       string
	KeyID       int64
	Status      string
}

type LogPage struct {
	Items    []LogRow `json:"items"`
	Total    int64    `json:"total"`
	Page     int      `json:"page"`
	PageSize int      `json:"page_size"`
}

type LogRow struct {
	RequestID           string    `json:"request_id"`
	CreatedAt           time.Time `json:"created_at"`
	Model               string    `json:"model"`
	KeyID               *int64    `json:"key_id"`
	KeyName             *string   `json:"key_name"`
	AccountID           *int64    `json:"account_id"`
	AccountLabel        *string   `json:"account_label"`
	Stream              bool      `json:"stream"`
	StatusCode          int       `json:"status_code"`
	ErrorLabel          *string   `json:"error_label"`
	InputTokens         int64     `json:"input_tokens"`
	OutputTokens        int64     `json:"output_tokens"`
	CacheReadTokens     int64     `json:"cache_read_tokens"`
	CacheCreationTokens int64     `json:"cache_creation_tokens"`
	CostUSD             float64   `json:"cost_usd"`
	DurationMS          int64     `json:"duration_ms"`
	FirstTokenMS        *int64    `json:"first_token_ms"`
}
