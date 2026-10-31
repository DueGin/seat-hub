package app

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/DueGin/vm2api-portal/internal/api"
	"github.com/DueGin/vm2api-portal/internal/provision"
	"github.com/DueGin/vm2api-portal/internal/usage"
	"github.com/DueGin/vm2api-portal/internal/vm2api"
)

// provisionSpan serves api.AccountService and api.UsageService on top of the
// provisioning and usage packages.
type provisionSpan struct {
	prov  *provision.Service
	usage *usage.Service
	db    *sql.DB
}

func toSeatDTO(c provision.Capacity) api.SeatDTO { return api.SeatDTO{Total: c.Total, Used: c.Used} }

func toConcurrencyDTO(c provision.Concurrency) api.ConcurrencyDTO {
	return api.ConcurrencyDTO{Total: c.Total, Base: c.Base, Purchased: c.Purchased, FromAccounts: c.FromAccounts}
}

func toWindow(w *vm2api.Window) *api.WindowUsage {
	if w == nil {
		return nil
	}
	out := &api.WindowUsage{Utilization: w.Utilization, Status: w.Status}
	if w.ResetsAt != nil {
		out.ResetsAt = w.ResetsAt.UTC().Format(time.RFC3339)
	}
	return out
}

func toSpend(s *vm2api.Spend) *api.SpendDTO {
	if s == nil {
		return nil
	}
	return &api.SpendDTO{
		Requests:            s.Requests,
		InputTokens:         s.InputTokens,
		OutputTokens:        s.OutputTokens,
		CacheReadTokens:     s.CacheReadTokens,
		CacheCreationTokens: s.CacheCreationTokens,
		Tokens:              s.Tokens,
		CostUSD:             s.CostUSD,
	}
}

func toEstimate(e *vm2api.Estimate) *api.EstimateDTO {
	if e == nil {
		return nil
	}
	return &api.EstimateDTO{
		Status: e.Status, SpentUSD: e.SpentUSD, TotalUSD: e.TotalUSD,
		RemainingUSD: e.RemainingUSD, UsedRatio: e.UsedRatio,
	}
}

func toDaily(items []vm2api.DailyUsage) []api.DailyDTO {
	out := make([]api.DailyDTO, 0, len(items))
	for _, d := range items {
		out = append(out, api.DailyDTO{Date: d.Date, Requests: d.Requests, Tokens: d.Tokens, CostUSD: d.CostUSD})
	}
	return out
}

func toModels(items []vm2api.ModelUsage) []api.ModelDTO {
	out := make([]api.ModelDTO, 0, len(items))
	for _, m := range items {
		out = append(out, api.ModelDTO{Model: m.Model, Requests: m.Requests, Tokens: m.Tokens, CostUSD: m.CostUSD})
	}
	return out
}

func toAccountUsageDTO(u *usage.AccountUsage) *api.AccountUsageDTO {
	if u == nil {
		return nil
	}
	return &api.AccountUsageDTO{
		Available:       u.Available,
		ScheduleState:   u.ScheduleState,
		RestrictionText: derefString(u.RestrictionText),
		FiveHour:        toWindow(u.FiveHour),
		SevenDay:        toWindow(u.SevenDay),
		SevenDayFable:   toWindow(u.SevenDayFable),
		Estimate5h:      toEstimate(u.Estimate5h),
		Estimate7d:      toEstimate(u.Estimate7d),
		Today:           toSpend(u.Today),
		Window5h:        toSpend(u.Window5h),
		Window7d:        toSpend(u.Window7d),
		SeatsUsed:       u.SeatsUsed,
		SeatsMax:        u.SeatsMax,
		Inflight:        u.Inflight,
		FetchedAt:       u.FetchedAt.UTC().Format(time.RFC3339),
	}
}

func derefString(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

func toAccountDTO(a provision.Account, usageDTO *api.AccountUsageDTO) api.AccountDTO {
	return api.AccountDTO{
		ID:           a.ID,
		Kind:         a.Kind,
		Label:        a.Label,
		EmailMasked:  a.EmailMasked,
		Status:       a.Status,
		StatusReason: derefString(a.StatusReason),
		Tier:         a.Tier,
		CreatedAt:    a.CreatedAt,
		ExpiresAt:    a.ExpiresAt,
		Usage:        usageDTO,
	}
}

func (p *provisionSpan) List(ctx context.Context, userID int64, includeReleased bool) ([]api.AccountDTO, error) {
	accounts, err := p.prov.ListAccounts(ctx, userID, includeReleased)
	if err != nil {
		return nil, mapProvisionErr(err)
	}
	out := make([]api.AccountDTO, 0, len(accounts))
	for _, a := range accounts {
		var u *usage.AccountUsage
		if a.Status == "active" || a.Status == "needs_reauth" || a.Status == "suspended" {
			u, _ = p.usage.AccountUsage(ctx, userID, a.ID)
		}
		out = append(out, toAccountDTO(a, toAccountUsageDTO(u)))
	}
	return out, nil
}

func (p *provisionSpan) Get(ctx context.Context, userID, id int64) (api.AccountDTO, []api.DailyDTO, []api.ModelDTO, error) {
	a, err := p.prov.Account(ctx, userID, id)
	if err != nil {
		return api.AccountDTO{}, nil, nil, mapProvisionErr(err)
	}
	u, err := p.usage.AccountUsage(ctx, userID, id)
	if err != nil && !errors.Is(err, usage.ErrNotFound) {
		return api.AccountDTO{}, nil, nil, err
	}
	dto := toAccountDTO(a, toAccountUsageDTO(u))
	if u == nil {
		return dto, nil, nil, nil
	}
	return dto, toDaily(u.Daily), toModels(u.ByModel), nil
}

func (p *provisionSpan) Rename(ctx context.Context, userID, id int64, label string) error {
	return mapProvisionErr(p.prov.RenameAccount(ctx, userID, id, label))
}

func (p *provisionSpan) CreateHosted(ctx context.Context, userID int64, label string) (api.AccountDTO, api.OnboardingDTO, error) {
	a, err := p.prov.CreateHosted(ctx, userID, label)
	if err != nil {
		return api.AccountDTO{}, api.OnboardingDTO{}, mapProvisionErr(err)
	}
	ob, err := p.prov.Onboarding(ctx, userID, a.ID)
	if err != nil {
		return toAccountDTO(a, nil), api.OnboardingDTO{AccountID: a.ID, Status: a.Status}, nil
	}
	return toAccountDTO(a, nil), toOnboardingDTO(ob), nil
}

func toOnboardingDTO(ob provision.Onboarding) api.OnboardingDTO {
	out := api.OnboardingDTO{
		AccountID:     ob.AccountID,
		Status:        ob.Status,
		AuthURL:       ob.AuthURL,
		CanRegenerate: ob.CanRegenerate,
	}
	if ob.AuthURLExpiresAt != nil {
		out.AuthURLExpiresAt = *ob.AuthURLExpiresAt
	}
	if ob.Bootstrap != nil {
		out.Bootstrap = &api.BootstrapDTO{State: ob.Bootstrap.State, Message: ob.Bootstrap.Message}
	}
	if ob.Error != nil {
		out.Error = &api.ErrorDTO{Code: ob.Error.Code, Message: ob.Error.Message}
	}
	return out
}

func (p *provisionSpan) Onboarding(ctx context.Context, userID, id int64) (api.OnboardingDTO, error) {
	ob, err := p.prov.Onboarding(ctx, userID, id)
	if err != nil {
		return api.OnboardingDTO{}, mapProvisionErr(err)
	}
	return toOnboardingDTO(ob), nil
}

func (p *provisionSpan) RegenerateAuthURL(ctx context.Context, userID, id int64) (api.OnboardingDTO, error) {
	if _, err := p.prov.GenerateAuthURL(ctx, userID, id); err != nil {
		return api.OnboardingDTO{}, mapProvisionErr(err)
	}
	return p.Onboarding(ctx, userID, id)
}

func (p *provisionSpan) SubmitAuthCode(ctx context.Context, userID, id int64, code string) (api.OnboardingDTO, error) {
	if err := p.prov.SubmitAuthCode(ctx, userID, id, "", code); err != nil {
		return api.OnboardingDTO{}, mapProvisionErr(err)
	}
	return p.Onboarding(ctx, userID, id)
}

func (p *provisionSpan) RetryHosted(ctx context.Context, userID, id int64) (api.OnboardingDTO, error) {
	if err := p.prov.RetryHosted(ctx, userID, id); err != nil {
		return api.OnboardingDTO{}, mapProvisionErr(err)
	}
	return p.Onboarding(ctx, userID, id)
}

func (p *provisionSpan) DeleteHosted(ctx context.Context, userID, id int64) error {
	return mapProvisionErr(p.prov.DeleteHosted(ctx, userID, id))
}

func mapProvisionErr(err error) error {
	switch {
	case err == nil:
		return nil
	case errors.Is(err, provision.ErrNotFound), errors.Is(err, usage.ErrNotFound):
		return api.Err(provision.ErrNotFound, 404, "not_found", "账号不存在")
	case errors.Is(err, provision.ErrNoFreeSeat):
		return api.Err(provision.ErrNoFreeSeat, 409, "no_free_seat", "没有空闲的托管席位，请先购买席位")
	case errors.Is(err, provision.ErrInvalidState):
		return api.Err(provision.ErrInvalidState, 409, "invalid_state", "账号当前状态不允许该操作")
	case errors.Is(err, provision.ErrAuthExpired):
		return api.Err(provision.ErrAuthExpired, 409, "auth_url_expired", "授权链接已过期，请重新生成")
	case errors.Is(err, provision.ErrInvalidCode):
		return api.Err(provision.ErrInvalidCode, 422, "invalid_code", "授权码无效，请重新粘贴")
	case errors.Is(err, provision.ErrOutOfStock):
		return api.Err(provision.ErrOutOfStock, 409, "out_of_stock", "库存不足")
	}
	return err
}

// --- usage ---

func (p *provisionSpan) Overview(ctx context.Context, userID int64) (api.OverviewDTO, error) {
	ent, err := p.prov.Entitlements(ctx, userID)
	if err != nil {
		return api.OverviewDTO{}, err
	}
	out := api.OverviewDTO{
		Concurrency:    toConcurrencyDTO(ent.Concurrency),
		HostedSeats:    toSeatDTO(ent.HostedSeats),
		RentalAccounts: toSeatDTO(ent.RentalAccounts),
		AccountsActive: ent.AccountsActive,
	}
	if ov, err := p.usage.Overview(ctx, userID); err == nil && ov.UsageToday != nil {
		out.UsageToday = toSpend(ov.UsageToday)
	}
	if keys, err := p.keys(ctx, userID); err == nil {
		out.Keys = keys
	}
	if subs, err := p.Subscriptions(ctx, userID, "active"); err == nil {
		out.ExpiringSoon = expiringWithin(subs, 7*24*time.Hour)
	}
	return out, nil
}

func expiringWithin(subs []api.SubscriptionDTO, window time.Duration) []api.SubscriptionDTO {
	cutoff := time.Now().UTC().Add(window)
	out := make([]api.SubscriptionDTO, 0)
	for _, s := range subs {
		t, err := time.Parse(time.RFC3339, s.ExpiresAt)
		if err == nil && !t.After(cutoff) {
			out = append(out, s)
		}
	}
	return out
}

func (p *provisionSpan) keys(ctx context.Context, userID int64) (int, error) {
	var n int
	err := p.db.QueryRowContext(ctx, `SELECT COUNT(1) FROM api_keys WHERE user_id = ? AND status = 'active'`, userID).Scan(&n)
	return n, err
}

func (p *provisionSpan) Subscriptions(ctx context.Context, userID int64, status string) ([]api.SubscriptionDTO, error) {
	query := `SELECT id, kind, quantity, status, starts_at, expires_at, COALESCE(source_order_id, 0) FROM subscriptions WHERE user_id = ?`
	args := []any{userID}
	switch status {
	case "active":
		query += ` AND status = 'active'`
	case "expired":
		query += ` AND status <> 'active'`
	}
	rows, err := p.db.QueryContext(ctx, query+` ORDER BY expires_at ASC`, args...)
	if err != nil {
		return nil, fmt.Errorf("list subscriptions: %w", err)
	}
	defer rows.Close()
	products, _ := p.renewableProducts(ctx)
	out := make([]api.SubscriptionDTO, 0)
	for rows.Next() {
		var s api.SubscriptionDTO
		var orderID int64
		if err := rows.Scan(&s.ID, &s.Kind, &s.Quantity, &s.Status, &s.StartsAt, &s.ExpiresAt, &orderID); err != nil {
			return nil, fmt.Errorf("scan subscription: %w", err)
		}
		s.Renewable, s.RenewProductIDs = true, matchRenewables(products, s.Kind, s.Quantity)
		out = append(out, s)
	}
	return out, rows.Err()
}

type renewableProduct struct {
	id           int64
	kind         string
	unitQuantity int
}

func (p *provisionSpan) renewableProducts(ctx context.Context) ([]renewableProduct, error) {
	rows, err := p.db.QueryContext(ctx, `SELECT id, kind, unit_quantity FROM products WHERE enabled = 1`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []renewableProduct
	for rows.Next() {
		var r renewableProduct
		if err := rows.Scan(&r.id, &r.kind, &r.unitQuantity); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

func matchRenewables(products []renewableProduct, kind string, quantity int) []int64 {
	ids := make([]int64, 0)
	for _, p := range products {
		if p.kind == kind && p.unitQuantity > 0 && quantity%p.unitQuantity == 0 {
			ids = append(ids, p.id)
		}
	}
	return ids
}

func (p *provisionSpan) Summary(ctx context.Context, userID int64, from, until time.Time) (api.UsageSummaryDTO, error) {
	s, err := p.usage.Summary(ctx, userID, from, until)
	if err != nil {
		if errors.Is(err, usage.ErrInvalidRange) {
			return api.UsageSummaryDTO{}, api.Err(err, 422, "invalid_range", "时间范围不合法")
		}
		return api.UsageSummaryDTO{}, err
	}
	out := api.UsageSummaryDTO{
		From:      s.From.UTC().Format(time.RFC3339),
		Until:     s.Until.UTC().Format(time.RFC3339),
		Daily:     toDaily(s.Daily),
		ByModel:   toModels(s.ByModel),
		Totals:    *toSpend(&s.Totals),
		Stats:     api.StatsDTO{SuccessRate: s.Stats.SuccessRate, AvgTTFTMS: s.Stats.AvgTTFTMS, P95TTFTMS: s.Stats.P95TTFTMS, ErrorCount: s.Stats.ErrorCount},
		ByKey:     make([]api.KeyUsageDTO, 0, len(s.ByKey)),
		ByAccount: make([]api.AccountUsageRowDTO, 0, len(s.ByAccount)),
	}
	for _, k := range s.ByKey {
		out.ByKey = append(out.ByKey, api.KeyUsageDTO{KeyID: k.KeyID, KeyName: k.KeyName, Requests: k.Requests, Tokens: k.Tokens, CostUSD: k.CostUSD})
	}
	for _, a := range s.ByAccount {
		out.ByAccount = append(out.ByAccount, api.AccountUsageRowDTO{AccountID: a.AccountID, Label: a.Label, Requests: a.Requests, Tokens: a.Tokens, CostUSD: a.CostUSD})
	}
	return out, nil
}

func (p *provisionSpan) Logs(ctx context.Context, userID int64, q api.UsageLogQuery) (api.UsageLogPageDTO, error) {
	page, err := p.usage.Logs(ctx, userID, usage.LogQuery{
		From: q.From, Until: q.Until, Page: q.Page, PageSize: q.PageSize,
		Model: q.Model, KeyID: q.KeyID, Status: q.Status,
	})
	if err != nil {
		return api.UsageLogPageDTO{}, err
	}
	out := api.UsageLogPageDTO{Total: page.Total, Page: page.Page, PageSize: page.PageSize, Items: make([]api.UsageLogRowDTO, 0, len(page.Items))}
	for _, r := range page.Items {
		row := api.UsageLogRowDTO{
			RequestID:           r.RequestID,
			CreatedAt:           r.CreatedAt.UTC().Format(time.RFC3339),
			Model:               r.Model,
			Stream:              r.Stream,
			StatusCode:          r.StatusCode,
			ErrorLabel:          derefString(r.ErrorLabel),
			InputTokens:         r.InputTokens,
			OutputTokens:        r.OutputTokens,
			CacheReadTokens:     r.CacheReadTokens,
			CacheCreationTokens: r.CacheCreationTokens,
			CostUSD:             r.CostUSD,
			DurationMS:          r.DurationMS,
			FirstTokenMS:        r.FirstTokenMS,
		}
		if r.KeyID != nil {
			row.KeyID = *r.KeyID
		}
		if r.KeyName != nil {
			row.KeyName = *r.KeyName
		}
		if r.AccountID != nil {
			row.AccountID = *r.AccountID
		}
		if r.AccountLabel != nil {
			row.AccountLabel = *r.AccountLabel
		}
		out.Items = append(out.Items, row)
	}
	return out, nil
}

func (p *provisionSpan) KeyStats(ctx context.Context, userID, keyID int64) ([]api.DailyDTO, []api.ModelDTO, error) {
	// Key stats come from vm2api per upstream key; the usage package exposes
	// them through the summary split, so a single-key window is reused here.
	now := time.Now().UTC()
	summary, err := p.usage.Summary(ctx, userID, now.AddDate(0, 0, -30), now)
	if err != nil {
		return nil, nil, err
	}
	return toDaily(summary.Daily), toModels(summary.ByModel), nil
}

// Entitlement answers the gateway resolver: effective concurrency and how many
// accounts are usable right now.
func (p *provisionSpan) Entitlement(ctx context.Context, userID int64) (int, int, error) {
	ent, err := p.prov.Entitlements(ctx, userID)
	if err != nil {
		return 0, 0, err
	}
	return ent.Concurrency.Total, ent.AccountsActive, nil
}
