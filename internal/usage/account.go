package usage

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sort"
	"sync"
	"time"

	"github.com/DueGin/seat-hub/internal/store"
	"github.com/DueGin/seat-hub/internal/vm2api"
)

const accountCacheTTL = 30 * time.Second

type Service struct {
	db       *sql.DB
	client   vm2api.Client
	now      func() time.Time
	mu       sync.Mutex
	cache    map[cacheKey]cacheEntry
	inflight map[cacheKey]chan struct{}
}

type cacheKey struct {
	userID, resourceID           int64
	vmID, ownerID, legacyOwnerID string
	createdAt, releasedAt        time.Time
}

type cacheEntry struct {
	usage   *AccountUsage
	expires time.Time
}

func New(db *sql.DB, client vm2api.Client) *Service {
	return &Service{
		db: db, client: client, now: time.Now,
		cache: make(map[cacheKey]cacheEntry), inflight: make(map[cacheKey]chan struct{}),
	}
}

func (service *Service) AccountUsage(ctx context.Context, userID, resourceID int64) (*AccountUsage, error) {
	var label, status, created string
	var vmID, ownerID, released sql.NullString
	err := service.db.QueryRowContext(ctx, `SELECT r.label, r.status, r.vm_id, u.vm2api_user_id, r.created_at, r.released_at
		FROM resources r JOIN users u ON u.id = r.user_id
		WHERE r.id = ? AND r.user_id = ?`, resourceID, userID).Scan(&label, &status, &vmID, &ownerID, &created, &released)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("read account: %w", err)
	}
	if status != "active" && status != "needs_reauth" && status != "suspended" || vmID.String == "" || service.client == nil {
		return nil, nil
	}
	identity, err := store.LoadServiceIdentity(ctx, service.db)
	if err != nil {
		return nil, fmt.Errorf("load service identity: %w", err)
	}
	if identity.UserID == "" && ownerID.String == "" {
		return nil, nil
	}
	createdAt, err := time.Parse(time.RFC3339, created)
	if err != nil {
		return nil, fmt.Errorf("parse account creation time: %w", err)
	}
	key := cacheKey{
		userID: userID, resourceID: resourceID, vmID: vmID.String,
		ownerID: identity.UserID, legacyOwnerID: ownerID.String, createdAt: createdAt.UTC(),
	}
	if released.Valid {
		key.releasedAt, err = time.Parse(time.RFC3339, released.String)
		if err != nil {
			return nil, fmt.Errorf("parse account release time: %w", err)
		}
		key.releasedAt = key.releasedAt.UTC()
	}
	result, err := service.cachedAccountUsage(ctx, key)
	if err != nil {
		if ctx.Err() != nil {
			return nil, fmt.Errorf("fetch account usage: %w", ctx.Err())
		}
		return nil, nil
	}
	if result != nil {
		result.AccountID, result.Label = resourceID, label
	}
	return result, nil
}

func (service *Service) cachedAccountUsage(ctx context.Context, key cacheKey) (*AccountUsage, error) {
	for {
		service.mu.Lock()
		if entry, ok := service.cache[key]; ok && service.now().Before(entry.expires) {
			result := cloneAccountUsage(entry.usage)
			service.mu.Unlock()
			return result, nil
		}
		if ready, ok := service.inflight[key]; ok {
			service.mu.Unlock()
			select {
			case <-ctx.Done():
				return nil, fmt.Errorf("wait for account usage: %w", ctx.Err())
			case <-ready:
				continue
			}
		}
		ready := make(chan struct{})
		service.inflight[key] = ready
		service.mu.Unlock()

		upstream, err := service.client.GetAccountUsage(ctx, key.vmID)
		var result *AccountUsage
		owner := upstream.VM.OwnerUserID
		if err == nil && upstream.VM.ID == key.vmID && owner != "" && (owner == key.ownerID || owner == key.legacyOwnerID) {
			result = redactAccountUsage(upstream)
			err = service.scopeAccountSpend(ctx, key, result)
		}
		service.mu.Lock()
		now := service.now()
		for cachedKey, entry := range service.cache {
			if !now.Before(entry.expires) {
				delete(service.cache, cachedKey)
			}
		}
		if err == nil && result != nil {
			if result.FetchedAt.IsZero() {
				result.FetchedAt = now.UTC()
			}
			service.cache[key] = cacheEntry{usage: cloneAccountUsage(result), expires: now.Add(accountCacheTTL)}
		}
		delete(service.inflight, key)
		close(ready)
		service.mu.Unlock()
		if err != nil {
			return nil, fmt.Errorf("fetch account usage: %w", err)
		}
		return result, nil
	}
}

func (service *Service) scopeAccountSpend(ctx context.Context, key cacheKey, result *AccountUsage) error {
	keys, err := service.keyMappings(ctx, key.userID)
	if err != nil {
		return fmt.Errorf("read account keys: %w", err)
	}
	until := service.now().UTC()
	if !key.releasedAt.IsZero() && until.After(key.releasedAt) {
		until = key.releasedAt
	}
	from := until.AddDate(0, 0, -30)
	if key.createdAt.After(from) {
		from = key.createdAt
	}
	dayStart := time.Date(until.Year(), until.Month(), until.Day(), 0, 0, 0, 0, time.UTC)
	fiveHourStart := accountWindowStart(result.FiveHour, until, 5*time.Hour)
	sevenDayStart := accountWindowStart(result.SevenDay, until, 7*24*time.Hour)
	today, window5h, window7d := &Spend{}, &Spend{}, &Spend{}
	daily := make(map[string]DailyUsage)
	models := make(map[string]ModelUsage)
	if len(keys) > 0 && !from.After(until) {
		for page := 1; ; page++ {
			logs, err := service.client.UserRequestLogs(ctx, "", vm2api.LogQuery{
				VMID: key.vmID, From: from, Until: until, Page: page, PageSize: 100,
			})
			if err != nil {
				return fmt.Errorf("fetch account history: %w", err)
			}
			for _, row := range logs.Items {
				_, owned := keys[row.KeyID]
				if !owned || row.VMID != key.vmID || row.CreatedAt.Before(from) || row.CreatedAt.After(until) {
					continue
				}
				if !row.CreatedAt.Before(dayStart) {
					addSpend(today, row)
				}
				if !row.CreatedAt.Before(fiveHourStart) {
					addSpend(window5h, row)
				}
				if !row.CreatedAt.Before(sevenDayStart) {
					addSpend(window7d, row)
				}
				date := row.CreatedAt.UTC().Format("2006-01-02")
				day := daily[date]
				day.Date, day.Requests, day.Tokens, day.CostUSD = date, day.Requests+1, day.Tokens+logTokens(row), day.CostUSD+row.CostUSD
				daily[date] = day
				model := models[row.Model]
				model.Model, model.Requests, model.Tokens, model.CostUSD = row.Model, model.Requests+1, model.Tokens+logTokens(row), model.CostUSD+row.CostUSD
				models[row.Model] = model
			}
			if len(logs.Items) == 0 || int64(page)*100 >= logs.Total {
				break
			}
		}
	}
	result.Today, result.Window5h, result.Window7d = today, window5h, window7d
	result.Daily = make([]DailyUsage, 0, len(daily))
	for _, day := range daily {
		result.Daily = append(result.Daily, day)
	}
	result.ByModel = make([]ModelUsage, 0, len(models))
	for _, model := range models {
		result.ByModel = append(result.ByModel, model)
	}
	sort.Slice(result.Daily, func(left, right int) bool { return result.Daily[left].Date < result.Daily[right].Date })
	sort.Slice(result.ByModel, func(left, right int) bool { return result.ByModel[left].Model < result.ByModel[right].Model })
	if result.Estimate5h != nil && result.Estimate5h.SpentUSD != window5h.CostUSD {
		result.Estimate5h = nil
	}
	if result.Estimate7d != nil && result.Estimate7d.SpentUSD != window7d.CostUSD {
		result.Estimate7d = nil
	}
	return nil
}

func accountWindowStart(window *Window, until time.Time, duration time.Duration) time.Time {
	if window != nil && window.ResetsAt != nil && window.ResetsAt.After(until) {
		return window.ResetsAt.Add(-duration)
	}
	return until.Add(-duration)
}

func logTokens(row vm2api.LogRow) int64 {
	return row.InputTokens + row.OutputTokens + row.CacheReadTokens + row.CacheCreationTokens
}

func addSpend(spend *Spend, row vm2api.LogRow) {
	spend.Requests++
	spend.InputTokens += row.InputTokens
	spend.OutputTokens += row.OutputTokens
	spend.CacheReadTokens += row.CacheReadTokens
	spend.CacheCreationTokens += row.CacheCreationTokens
	spend.Tokens += logTokens(row)
	spend.CostUSD += row.CostUSD
}

func redactAccountUsage(upstream vm2api.AccountUsage) *AccountUsage {
	var restriction *string
	if upstream.VM.RestrictionText != "" {
		text := "账号当前受到限制，请稍后重试"
		restriction = &text
	}
	return &AccountUsage{
		Available: true, ScheduleState: upstream.VM.ScheduleState, RestrictionText: restriction,
		FiveHour: cloneWindow(upstream.VM.FiveHour), SevenDay: cloneWindow(upstream.VM.SevenDay),
		SevenDayFable: cloneWindow(upstream.VM.SevenDayFable),
		Estimate5h:    cloneEstimate(upstream.Estimate5h), Estimate7d: cloneEstimate(upstream.Estimate7d),
		Today: clonePointer(upstream.Today), Window5h: clonePointer(upstream.Window5h), Window7d: clonePointer(upstream.Window7d),
		SeatsUsed: upstream.VM.SeatsUsed, SeatsMax: upstream.VM.SeatsMax, Inflight: upstream.VM.Inflight,
		FetchedAt: upstream.FetchedAt.UTC(),
		Daily:     append([]DailyUsage{}, upstream.Daily...), ByModel: append([]ModelUsage{}, upstream.ByModel...),
	}
}

func clonePointer[Value any](value *Value) *Value {
	if value == nil {
		return nil
	}
	copyValue := *value
	return &copyValue
}

func cloneWindow(window *Window) *Window {
	result := clonePointer(window)
	if result != nil && result.ResetsAt != nil {
		reset := result.ResetsAt.UTC()
		result.ResetsAt = &reset
	}
	return result
}

func cloneEstimate(estimate *Estimate) *Estimate {
	result := clonePointer(estimate)
	if result != nil {
		result.TotalUSD = clonePointer(result.TotalUSD)
		result.RemainingUSD = clonePointer(result.RemainingUSD)
		result.UsedRatio = clonePointer(result.UsedRatio)
	}
	return result
}

func cloneAccountUsage(value *AccountUsage) *AccountUsage {
	result := clonePointer(value)
	result.RestrictionText = clonePointer(value.RestrictionText)
	result.FiveHour, result.SevenDay, result.SevenDayFable = cloneWindow(value.FiveHour), cloneWindow(value.SevenDay), cloneWindow(value.SevenDayFable)
	result.Estimate5h, result.Estimate7d = cloneEstimate(value.Estimate5h), cloneEstimate(value.Estimate7d)
	result.Today, result.Window5h, result.Window7d = clonePointer(value.Today), clonePointer(value.Window5h), clonePointer(value.Window7d)
	result.Daily, result.ByModel = append([]DailyUsage{}, value.Daily...), append([]ModelUsage{}, value.ByModel...)
	return result
}
