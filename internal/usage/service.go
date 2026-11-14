package usage

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sort"
	"time"

	"github.com/DueGin/vm2api-portal/internal/vm2api"
)

func (service *Service) owner(ctx context.Context, userID int64) (string, error) {
	var owner sql.NullString
	err := service.db.QueryRowContext(ctx, `SELECT vm2api_user_id FROM users WHERE id = ?`, userID).Scan(&owner)
	if errors.Is(err, sql.ErrNoRows) {
		return "", ErrNotFound
	}
	if err != nil {
		return "", fmt.Errorf("read user: %w", err)
	}
	return owner.String, nil
}

func (service *Service) Overview(ctx context.Context, userID int64) (Overview, error) {
	owner, err := service.owner(ctx, userID)
	if err != nil {
		return Overview{}, err
	}
	if owner == "" || service.client == nil {
		return Overview{}, nil
	}
	until := service.now().UTC()
	from := time.Date(until.Year(), until.Month(), until.Day(), 0, 0, 0, 0, time.UTC)
	billing, err := service.client.UserBilling(ctx, owner, from, until)
	if err != nil {
		if ctx.Err() != nil {
			return Overview{}, fmt.Errorf("fetch today's usage: %w", ctx.Err())
		}
		return Overview{}, nil
	}
	return Overview{UsageToday: &billing.Totals}, nil
}

type keyMapping struct {
	id   int64
	name string
}

type accountMapping struct {
	id       int64
	label    string
	created  time.Time
	released time.Time
}

type mappings struct {
	keys     map[string]keyMapping
	accounts map[string][]accountMapping
}

func (service *Service) mappings(ctx context.Context, userID int64) (mappings, error) {
	result := mappings{keys: make(map[string]keyMapping), accounts: make(map[string][]accountMapping)}
	rows, err := service.db.QueryContext(ctx, `SELECT id, name, vm2api_key_id FROM api_keys WHERE user_id = ?`, userID)
	if err != nil {
		return result, fmt.Errorf("read key mappings: %w", err)
	}
	for rows.Next() {
		var key keyMapping
		var upstreamID string
		if err := rows.Scan(&key.id, &key.name, &upstreamID); err != nil {
			rows.Close()
			return result, fmt.Errorf("scan key mapping: %w", err)
		}
		result.keys[upstreamID] = key
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return result, fmt.Errorf("read key mappings: %w", err)
	}
	if err := rows.Close(); err != nil {
		return result, fmt.Errorf("close key mappings: %w", err)
	}
	rows, err = service.db.QueryContext(ctx, `SELECT id, label, vm_id, created_at, released_at
		FROM resources WHERE user_id = ? AND vm_id IS NOT NULL ORDER BY id`, userID)
	if err != nil {
		return result, fmt.Errorf("read account mappings: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var account accountMapping
		var upstreamID, created string
		var released sql.NullString
		if err := rows.Scan(&account.id, &account.label, &upstreamID, &created, &released); err != nil {
			return result, fmt.Errorf("scan account mapping: %w", err)
		}
		account.created, err = time.Parse(time.RFC3339, created)
		if err != nil {
			return result, fmt.Errorf("parse account creation time: %w", err)
		}
		if released.Valid {
			account.released, err = time.Parse(time.RFC3339, released.String)
			if err != nil {
				return result, fmt.Errorf("parse account release time: %w", err)
			}
		}
		result.accounts[upstreamID] = append(result.accounts[upstreamID], account)
	}
	if err := rows.Err(); err != nil {
		return result, fmt.Errorf("read account mappings: %w", err)
	}
	return result, nil
}

func (mapping mappings) account(upstreamID string, from, until time.Time) (accountMapping, bool) {
	var match accountMapping
	found := false
	for _, account := range mapping.accounts[upstreamID] {
		if !until.After(account.created) || !account.released.IsZero() && !from.Before(account.released) {
			continue
		}
		if found {
			return accountMapping{}, false
		}
		match, found = account, true
	}
	return match, found
}

// accountByID finds one of the user's accounts that has (or had) a slot.
func (mapping mappings) accountByID(id int64) (string, accountMapping, bool) {
	for upstreamID, accounts := range mapping.accounts {
		for _, account := range accounts {
			if account.id == id {
				return upstreamID, account, true
			}
		}
	}
	return "", accountMapping{}, false
}

func (service *Service) ownsAccount(ctx context.Context, userID, accountID int64) (bool, error) {
	var owned bool
	err := service.db.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM resources WHERE id = ? AND user_id = ?)`, accountID, userID).Scan(&owned)
	if err != nil {
		return false, fmt.Errorf("check account owner: %w", err)
	}
	return owned, nil
}

func emptySummary(from, until time.Time) Summary {
	return Summary{
		From: from, Until: until,
		Daily: []DailyUsage{}, ByModel: []ModelUsage{}, ByKey: []KeyUsage{}, ByAccount: []AccountSpend{},
	}
}

func (service *Service) Summary(ctx context.Context, userID int64, from, until time.Time) (Summary, error) {
	from, until = from.UTC(), until.UTC()
	if from.IsZero() || until.IsZero() || !until.After(from) {
		return Summary{}, ErrInvalidRange
	}
	owner, err := service.owner(ctx, userID)
	if err != nil {
		return Summary{}, err
	}
	result := emptySummary(from, until)
	if owner == "" {
		return result, nil
	}
	if service.client == nil {
		return Summary{}, fmt.Errorf("fetch billing: %w", vm2api.ErrUnavailable)
	}
	mapping, err := service.mappings(ctx, userID)
	if err != nil {
		return Summary{}, err
	}
	billing, err := service.client.UserBilling(ctx, owner, from, until)
	if err != nil {
		return Summary{}, fmt.Errorf("fetch billing: %w", err)
	}
	stats, err := service.client.UserRequestStats(ctx, owner, from, until)
	if err != nil {
		return Summary{}, fmt.Errorf("fetch request stats: %w", err)
	}
	result.Totals = billing.Totals
	result.Daily = append(result.Daily, billing.Daily...)
	result.ByModel = append(result.ByModel, billing.ByModel...)
	resolveAccountHistory := false
	for _, row := range billing.ByVM {
		if _, ok := mapping.account(row.VMID, from, until); !ok && len(mapping.accounts[row.VMID]) > 1 {
			resolveAccountHistory = true
		}
	}
	var breakdown requestBreakdown
	if billing.Totals.Requests > 0 && (len(billing.Daily) == 0 || len(billing.ByModel) == 0 || resolveAccountHistory) {
		breakdown, err = service.breakdown(ctx, owner, from, until, mapping)
		if err != nil {
			return Summary{}, err
		}
		if len(billing.Daily) == 0 {
			result.Daily = breakdown.daily
		}
		if len(billing.ByModel) == 0 {
			result.ByModel = breakdown.byModel
		}
	}
	for _, row := range billing.ByKey {
		if key, ok := mapping.keys[row.KeyID]; ok {
			result.ByKey = append(result.ByKey, KeyUsage{
				KeyID: key.id, KeyName: key.name, Requests: row.Requests, Tokens: row.Tokens, CostUSD: row.CostUSD,
			})
		}
	}
	for _, row := range billing.ByVM {
		if account, ok := mapping.account(row.VMID, from, until); ok {
			result.ByAccount = append(result.ByAccount, AccountSpend{
				AccountID: account.id, Label: account.label, Requests: row.Requests, Tokens: row.Tokens, CostUSD: row.CostUSD,
			})
		}
	}
	if resolveAccountHistory {
		result.ByAccount = breakdown.byAccount
	}
	sort.Slice(result.ByKey, func(left, right int) bool { return result.ByKey[left].KeyID < result.ByKey[right].KeyID })
	sort.Slice(result.ByAccount, func(left, right int) bool {
		return result.ByAccount[left].AccountID < result.ByAccount[right].AccountID
	})
	result.Stats = Stats{
		SuccessRate: stats.SuccessRate, AvgTTFTMS: stats.AvgTTFTMS, P95TTFTMS: stats.P95TTFTMS, ErrorCount: stats.ErrorCount,
	}
	return result, nil
}

type requestBreakdown struct {
	daily     []DailyUsage
	byModel   []ModelUsage
	byAccount []AccountSpend
}

func (service *Service) breakdown(ctx context.Context, owner string, from, until time.Time, mapping mappings) (requestBreakdown, error) {
	daily := make(map[string]DailyUsage)
	models := make(map[string]ModelUsage)
	accounts := make(map[int64]AccountSpend)
	for page := 1; ; page++ {
		result, err := service.client.UserRequestLogs(ctx, owner, vm2api.LogQuery{
			From: from, Until: until, Page: page, PageSize: 100,
		})
		if err != nil {
			return requestBreakdown{}, fmt.Errorf("fetch usage breakdown: %w", err)
		}
		for _, row := range result.Items {
			tokens := logTokens(row)
			date := row.CreatedAt.UTC().Format("2006-01-02")
			day := daily[date]
			day.Date, day.Requests, day.Tokens, day.CostUSD = date, day.Requests+1, day.Tokens+tokens, day.CostUSD+row.CostUSD
			daily[date] = day
			model := models[row.Model]
			model.Model, model.Requests, model.Tokens, model.CostUSD = row.Model, model.Requests+1, model.Tokens+tokens, model.CostUSD+row.CostUSD
			models[row.Model] = model
			if account, ok := mapping.account(row.VMID, row.CreatedAt, row.CreatedAt.Add(time.Nanosecond)); ok {
				spend := accounts[account.id]
				spend.AccountID, spend.Label = account.id, account.label
				spend.Requests, spend.Tokens, spend.CostUSD = spend.Requests+1, spend.Tokens+tokens, spend.CostUSD+row.CostUSD
				accounts[account.id] = spend
			}
		}
		if len(result.Items) == 0 || int64(page)*100 >= result.Total {
			break
		}
	}
	days := make([]DailyUsage, 0, len(daily))
	for _, day := range daily {
		days = append(days, day)
	}
	byModel := make([]ModelUsage, 0, len(models))
	for _, model := range models {
		byModel = append(byModel, model)
	}
	byAccount := make([]AccountSpend, 0, len(accounts))
	for _, account := range accounts {
		byAccount = append(byAccount, account)
	}
	sort.Slice(days, func(left, right int) bool { return days[left].Date < days[right].Date })
	sort.Slice(byModel, func(left, right int) bool { return byModel[left].Model < byModel[right].Model })
	return requestBreakdown{daily: days, byModel: byModel, byAccount: byAccount}, nil
}

func (service *Service) Logs(ctx context.Context, userID int64, query LogQuery) (LogPage, error) {
	if query.KeyID < 0 || query.AccountID < 0 || query.Status != "" && query.Status != "success" && query.Status != "error" {
		return LogPage{}, ErrInvalidQuery
	}
	if query.Page < 0 || query.PageSize < 0 {
		return LogPage{}, ErrInvalidPagination
	}
	if query.Page == 0 {
		query.Page = 1
	}
	if query.PageSize == 0 {
		query.PageSize = 20
	}
	query.PageSize = min(query.PageSize, 100)
	if query.Page-1 > int(^uint(0)>>1)/query.PageSize {
		return LogPage{}, ErrInvalidPagination
	}
	if query.Until.IsZero() {
		query.Until = service.now().UTC()
	}
	query.Until = query.Until.UTC()
	if query.From.IsZero() {
		query.From = query.Until.AddDate(0, 0, -7)
	}
	query.From, query.Until = query.From.UTC(), query.Until.UTC()
	if !query.Until.After(query.From) {
		return LogPage{}, ErrInvalidRange
	}
	owner, err := service.owner(ctx, userID)
	if err != nil {
		return LogPage{}, err
	}
	mapping, err := service.mappings(ctx, userID)
	if err != nil {
		return LogPage{}, err
	}
	var upstreamKeyID string
	if query.KeyID != 0 {
		found := false
		for upstreamID, key := range mapping.keys {
			if key.id == query.KeyID {
				upstreamKeyID, found = upstreamID, true
				break
			}
		}
		if !found {
			return LogPage{}, ErrNotFound
		}
	}
	result := LogPage{Items: []LogRow{}, Page: query.Page, PageSize: query.PageSize}
	var upstreamVMID string
	if query.AccountID != 0 {
		vmID, account, found := mapping.accountByID(query.AccountID)
		if !found {
			// An account that never got a slot has no requests; one that is not
			// the caller's must look exactly like one that does not exist.
			owned, err := service.ownsAccount(ctx, userID, query.AccountID)
			if err != nil {
				return LogPage{}, err
			}
			if !owned {
				return LogPage{}, ErrNotFound
			}
			return result, nil
		}
		if query.From.Before(account.created) {
			query.From = account.created
		}
		if !account.released.IsZero() && query.Until.After(account.released) {
			query.Until = account.released
		}
		if !query.Until.After(query.From) {
			return result, nil
		}
		upstreamVMID = vmID
	}
	if owner == "" {
		return result, nil
	}
	if service.client == nil {
		return LogPage{}, fmt.Errorf("fetch request logs: %w", vm2api.ErrUnavailable)
	}
	page, err := service.client.UserRequestLogs(ctx, owner, vm2api.LogQuery{
		From: query.From, Until: query.Until, Page: query.Page, PageSize: query.PageSize,
		Model: query.Model, KeyID: upstreamKeyID, VMID: upstreamVMID, Status: query.Status,
	})
	if err != nil {
		return LogPage{}, fmt.Errorf("fetch request logs: %w", err)
	}
	result.Total = page.Total
	for _, row := range page.Items {
		item := LogRow{
			RequestID: row.RequestID, CreatedAt: row.CreatedAt.UTC(), Model: row.Model,
			Stream: row.Stream, StatusCode: row.StatusCode, ErrorLabel: errorLabel(row.StatusCode),
			InputTokens: row.InputTokens, OutputTokens: row.OutputTokens,
			CacheReadTokens: row.CacheReadTokens, CacheCreationTokens: row.CacheCreationTokens,
			CostUSD: row.CostUSD, DurationMS: row.DurationMS, FirstTokenMS: clonePointer(row.FirstTokenMS),
		}
		if key, ok := mapping.keys[row.KeyID]; ok {
			item.KeyID, item.KeyName = &key.id, &key.name
		}
		if account, ok := mapping.account(row.VMID, row.CreatedAt, row.CreatedAt.Add(time.Nanosecond)); ok {
			item.AccountID, item.AccountLabel = &account.id, &account.label
		}
		result.Items = append(result.Items, item)
	}
	return result, nil
}

func errorLabel(status int) *string {
	if status >= 200 && status < 400 {
		return nil
	}
	label := "请求失败"
	switch {
	case status == 400 || status == 422:
		label = "请求参数错误"
	case status == 401:
		label = "认证失败"
	case status == 403:
		label = "请求被拒绝"
	case status == 408 || status == 504:
		label = "请求超时"
	case status == 429:
		label = "请求受限，请稍后重试"
	case status >= 500:
		label = "上游服务暂不可用"
	}
	return &label
}
