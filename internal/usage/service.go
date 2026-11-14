package usage

import (
	"container/heap"
	"context"
	"database/sql"
	"fmt"
	"math"
	"sort"
	"time"

	"github.com/DueGin/seat-hub/internal/store"
	"github.com/DueGin/seat-hub/internal/vm2api"
)

func (service *Service) Overview(ctx context.Context, userID int64) (Overview, error) {
	keys, err := service.keyMappings(ctx, userID)
	if err != nil {
		return Overview{}, err
	}
	spend := &Spend{}
	if len(keys) == 0 {
		return Overview{UsageToday: spend}, nil
	}
	if service.client == nil {
		return Overview{}, nil
	}
	until := service.now().UTC()
	from := time.Date(until.Year(), until.Month(), until.Day(), 0, 0, 0, 0, time.UTC)
	err = service.scanKeyLogs(ctx, keys, from, until, func(row vm2api.LogRow) { addSpend(spend, row) })
	if err != nil {
		if ctx.Err() != nil {
			return Overview{}, fmt.Errorf("fetch today's usage: %w", ctx.Err())
		}
		return Overview{}, nil
	}
	return Overview{UsageToday: spend}, nil
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

func (service *Service) keyMappings(ctx context.Context, userID int64) (map[string]keyMapping, error) {
	var exists bool
	if err := service.db.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM users WHERE id = ?)`, userID).Scan(&exists); err != nil {
		return nil, fmt.Errorf("read user: %w", err)
	}
	if !exists {
		return nil, ErrNotFound
	}
	result := make(map[string]keyMapping)
	rows, err := service.db.QueryContext(ctx, `SELECT id, name, vm2api_key_id, previous_vm2api_key_ids FROM api_keys WHERE user_id = ? ORDER BY id`, userID)
	if err != nil {
		return nil, fmt.Errorf("read key mappings: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var key keyMapping
		var upstreamID, previous string
		if err := rows.Scan(&key.id, &key.name, &upstreamID, &previous); err != nil {
			return nil, fmt.Errorf("scan key mapping: %w", err)
		}
		for _, id := range append(store.DecodeStrings(previous), upstreamID) {
			if id != "" {
				result[id] = key
			}
		}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read key mappings: %w", err)
	}
	return result, nil
}

func (service *Service) mappings(ctx context.Context, userID int64) (mappings, error) {
	result := mappings{accounts: make(map[string][]accountMapping)}
	var err error
	result.keys, err = service.keyMappings(ctx, userID)
	if err != nil {
		return result, err
	}
	rows, err := service.db.QueryContext(ctx, `SELECT id, label, vm_id, created_at, released_at
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
	mapping, err := service.mappings(ctx, userID)
	if err != nil {
		return Summary{}, err
	}
	result := emptySummary(from, until)
	if len(mapping.keys) == 0 {
		return result, nil
	}
	result.Stats.SuccessRate = 1
	if service.client == nil {
		return Summary{}, fmt.Errorf("fetch usage: %w", vm2api.ErrUnavailable)
	}
	daily := make(map[string]DailyUsage)
	models := make(map[string]ModelUsage)
	keys := make(map[int64]KeyUsage)
	accounts := make(map[int64]AccountSpend)
	latencies := make([]int64, 0)
	// Billing's per-key DTO omits token categories. Use the same scoped log
	// scan for totals and breakdowns so input/output/cache counts stay intact.
	err = service.scanKeyLogs(ctx, mapping.keys, from, until, func(row vm2api.LogRow) {
		addSpend(&result.Totals, row)
		tokens := logTokens(row)
		date := row.CreatedAt.UTC().Format("2006-01-02")
		day := daily[date]
		day.Date, day.Requests, day.Tokens, day.CostUSD = date, day.Requests+1, day.Tokens+tokens, day.CostUSD+row.CostUSD
		daily[date] = day
		model := models[row.Model]
		model.Model, model.Requests, model.Tokens, model.CostUSD = row.Model, model.Requests+1, model.Tokens+tokens, model.CostUSD+row.CostUSD
		models[row.Model] = model
		key := mapping.keys[row.KeyID]
		spend := keys[key.id]
		spend.KeyID, spend.KeyName = key.id, key.name
		spend.Requests, spend.Tokens, spend.CostUSD = spend.Requests+1, spend.Tokens+tokens, spend.CostUSD+row.CostUSD
		keys[key.id] = spend
		if account, ok := mapping.account(row.VMID, row.CreatedAt, row.CreatedAt.Add(time.Nanosecond)); ok {
			spend := accounts[account.id]
			spend.AccountID, spend.Label = account.id, account.label
			spend.Requests, spend.Tokens, spend.CostUSD = spend.Requests+1, spend.Tokens+tokens, spend.CostUSD+row.CostUSD
			accounts[account.id] = spend
		}
		if row.StatusCode < 200 || row.StatusCode >= 400 {
			result.Stats.ErrorCount++
		}
		if row.FirstTokenMS != nil {
			latencies = append(latencies, *row.FirstTokenMS)
			result.Stats.AvgTTFTMS += float64(*row.FirstTokenMS)
		}
	})
	if err != nil {
		return Summary{}, err
	}
	for _, day := range daily {
		result.Daily = append(result.Daily, day)
	}
	for _, model := range models {
		result.ByModel = append(result.ByModel, model)
	}
	for _, key := range keys {
		result.ByKey = append(result.ByKey, key)
	}
	for _, account := range accounts {
		result.ByAccount = append(result.ByAccount, account)
	}
	sort.Slice(result.Daily, func(left, right int) bool { return result.Daily[left].Date < result.Daily[right].Date })
	sort.Slice(result.ByModel, func(left, right int) bool { return result.ByModel[left].Model < result.ByModel[right].Model })
	sort.Slice(result.ByKey, func(left, right int) bool { return result.ByKey[left].KeyID < result.ByKey[right].KeyID })
	sort.Slice(result.ByAccount, func(left, right int) bool {
		return result.ByAccount[left].AccountID < result.ByAccount[right].AccountID
	})
	if result.Totals.Requests > 0 {
		result.Stats.SuccessRate = float64(result.Totals.Requests-result.Stats.ErrorCount) / float64(result.Totals.Requests)
	}
	if len(latencies) > 0 {
		sort.Slice(latencies, func(left, right int) bool { return latencies[left] < latencies[right] })
		result.Stats.AvgTTFTMS /= float64(len(latencies))
		result.Stats.P95TTFTMS = float64(latencies[int(math.Ceil(float64(len(latencies))*0.95))-1])
	}
	return result, nil
}

func sortedKeyIDs(keys map[string]keyMapping, portalKeyID int64) []string {
	ids := make([]string, 0, len(keys))
	for upstreamID, key := range keys {
		if portalKeyID == 0 || key.id == portalKeyID {
			ids = append(ids, upstreamID)
		}
	}
	sort.Strings(ids)
	return ids
}

func (service *Service) scanKeyLogs(ctx context.Context, keys map[string]keyMapping, from, until time.Time, visit func(vm2api.LogRow)) error {
	for _, keyID := range sortedKeyIDs(keys, 0) {
		for page := 1; ; page++ {
			result, err := service.client.UserRequestLogs(ctx, "", vm2api.LogQuery{
				KeyID: keyID, From: from, Until: until, Page: page, PageSize: 100,
			})
			if err != nil {
				return fmt.Errorf("fetch usage logs: %w", err)
			}
			for _, row := range result.Items {
				if row.KeyID == keyID && !row.CreatedAt.Before(from) && !row.CreatedAt.After(until) {
					visit(row)
				}
			}
			if len(result.Items) == 0 || int64(page)*100 >= result.Total {
				break
			}
		}
	}
	return nil
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
	mapping, err := service.mappings(ctx, userID)
	if err != nil {
		return LogPage{}, err
	}
	keyIDs := sortedKeyIDs(mapping.keys, query.KeyID)
	if query.KeyID != 0 && len(keyIDs) == 0 {
		return LogPage{}, ErrNotFound
	}
	if len(keyIDs) > 1 && query.Page > maxMergedLogRows/query.PageSize {
		return LogPage{}, ErrInvalidPagination
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
	if len(keyIDs) == 0 {
		return result, nil
	}
	if service.client == nil {
		return LogPage{}, fmt.Errorf("fetch request logs: %w", vm2api.ErrUnavailable)
	}
	page, err := service.keyLogPage(ctx, keyIDs, vm2api.LogQuery{
		From: query.From, Until: query.Until, Page: query.Page, PageSize: query.PageSize,
		Model: query.Model, VMID: upstreamVMID, Status: query.Status,
	})
	if err != nil {
		return LogPage{}, fmt.Errorf("fetch request logs: %w", err)
	}
	result.Total = page.Total
	for _, row := range page.Items {
		key, ok := mapping.keys[row.KeyID]
		if !ok {
			continue
		}
		item := LogRow{
			RequestID: row.RequestID, CreatedAt: row.CreatedAt.UTC(), Model: row.Model,
			Stream: row.Stream, StatusCode: row.StatusCode, ErrorLabel: errorLabel(row.StatusCode),
			InputTokens: row.InputTokens, OutputTokens: row.OutputTokens,
			CacheReadTokens: row.CacheReadTokens, CacheCreationTokens: row.CacheCreationTokens,
			CostUSD: row.CostUSD, DurationMS: row.DurationMS, FirstTokenMS: clonePointer(row.FirstTokenMS),
		}
		item.KeyID, item.KeyName = &key.id, &key.name
		if account, ok := mapping.account(row.VMID, row.CreatedAt, row.CreatedAt.Add(time.Nanosecond)); ok {
			item.AccountID, item.AccountLabel = &account.id, &account.label
		}
		result.Items = append(result.Items, item)
	}
	return result, nil
}

// Multi-key pagination is limited to the first 10,000 rows, bounding each
// key's prefix scan to page*page_size rows. Single-key pages pass through.
const maxMergedLogRows = 10_000

type keyLogCursor struct {
	keyID string
	items []vm2api.LogRow
	index int
	page  int
	total int64
}

type keyLogHeap []*keyLogCursor

func (items keyLogHeap) Len() int { return len(items) }
func (items keyLogHeap) Less(left, right int) bool {
	l, r := items[left], items[right]
	lTime, rTime := l.items[l.index].CreatedAt, r.items[r.index].CreatedAt
	if lTime.Equal(rTime) {
		return l.keyID < r.keyID
	}
	return lTime.After(rTime)
}
func (items keyLogHeap) Swap(left, right int) { items[left], items[right] = items[right], items[left] }
func (items *keyLogHeap) Push(value any)      { *items = append(*items, value.(*keyLogCursor)) }
func (items *keyLogHeap) Pop() any {
	last := len(*items) - 1
	value := (*items)[last]
	(*items)[last] = nil
	*items = (*items)[:last]
	return value
}

func (service *Service) keyLogPage(ctx context.Context, keyIDs []string, query vm2api.LogQuery) (vm2api.LogPage, error) {
	if len(keyIDs) == 1 {
		query.KeyID = keyIDs[0]
		return service.client.UserRequestLogs(ctx, "", query)
	}
	result := vm2api.LogPage{Items: []vm2api.LogRow{}, Page: query.Page, PageSize: query.PageSize}
	maxPage := query.Page
	query.Page = 1
	cursors := make(keyLogHeap, 0, len(keyIDs))
	for _, keyID := range keyIDs {
		query.KeyID = keyID
		page, err := service.client.UserRequestLogs(ctx, "", query)
		if err != nil {
			return vm2api.LogPage{}, err
		}
		result.Total += page.Total
		if len(page.Items) > 0 {
			cursors = append(cursors, &keyLogCursor{keyID: keyID, items: page.Items, page: 1, total: page.Total})
		}
	}
	heap.Init(&cursors)
	skip, limit := (maxPage-1)*query.PageSize, maxPage*query.PageSize
	for index := 0; index < limit && cursors.Len() > 0; index++ {
		cursor := heap.Pop(&cursors).(*keyLogCursor)
		if index >= skip {
			result.Items = append(result.Items, cursor.items[cursor.index])
		}
		if index+1 == limit {
			break
		}
		cursor.index++
		if cursor.index == len(cursor.items) {
			if cursor.page >= maxPage || int64(cursor.page)*int64(query.PageSize) >= cursor.total {
				continue
			}
			cursor.page++
			query.KeyID, query.Page = cursor.keyID, cursor.page
			page, err := service.client.UserRequestLogs(ctx, "", query)
			if err != nil {
				return vm2api.LogPage{}, err
			}
			cursor.items, cursor.index = page.Items, 0
		}
		if cursor.index < len(cursor.items) {
			heap.Push(&cursors, cursor)
		}
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
