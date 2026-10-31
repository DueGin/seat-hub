package usage

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/DueGin/vm2api-portal/internal/store/migrations"
	"github.com/DueGin/vm2api-portal/internal/vm2api"
	_ "modernc.org/sqlite"
)

type fixture struct {
	db      *sql.DB
	client  *vm2api.Fake
	service *Service
	owners  [2]string
	keys    [2]string
	now     time.Time
}

func setup(t *testing.T) *fixture {
	t.Helper()
	database, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	database.SetMaxOpenConns(1)
	t.Cleanup(func() { database.Close() })
	schema, err := migrations.FS.ReadFile("001_init.sql")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := database.Exec(string(schema)); err != nil {
		t.Fatal(err)
	}
	result := &fixture{db: database, client: vm2api.NewFake(), now: time.Date(2026, 10, 31, 12, 0, 0, 0, time.UTC)}
	result.service = New(database, result.client)
	result.service.now = func() time.Time { return result.now }
	for index := range result.owners {
		owner, err := result.client.CreateUser(context.Background(), fmt.Sprintf("owner-%d", index), "password")
		if err != nil {
			t.Fatal(err)
		}
		key, err := result.client.CreateKey(context.Background(), vm2api.CreateKeyInput{UserID: owner.ID, Name: "internal-key"})
		if err != nil {
			t.Fatal(err)
		}
		result.owners[index], result.keys[index] = owner.ID, key.ID
		result.exec(t, `INSERT INTO users (id, email, password_hash, vm2api_user_id, created_at, updated_at)
			VALUES (?, ?, 'password-hash', ?, ?, ?)`, index+1, fmt.Sprintf("%d@example.test", index), owner.ID, result.stamp(-24*time.Hour), result.stamp(0))
		result.exec(t, `INSERT INTO api_keys (id, user_id, name, key_hash, prefix, vm2api_key_id, vm2api_secret_enc, status, created_at, updated_at)
			VALUES (?, ?, ?, ?, 'sk-pt-test', ?, 'secret-ciphertext', 'active', ?, ?)`, index+11, index+1, fmt.Sprintf("密钥%d", index+1), fmt.Sprintf("hash-%d", index), key.ID, result.stamp(-24*time.Hour), result.stamp(0))
		result.exec(t, `INSERT INTO resources (id, user_id, kind, vm_id, label, status, created_at, updated_at)
			VALUES (?, ?, 'hosted', ?, ?, 'active', ?, ?)`, index+21, index+1, fmt.Sprintf("internal-vm-%d", index), fmt.Sprintf("账号%d", index+1), result.stamp(-24*time.Hour), result.stamp(0))
		result.client.AddVM(vm2api.VM{
			ID: fmt.Sprintf("internal-vm-%d", index), OwnerUserID: owner.ID, Label: "internal-slot-label",
			ScheduleState: "on", CredStatus: "internal-credential", ProxyConfigured: true,
			FiveHour: &vm2api.Window{Utilization: 12.5, Status: "ok"}, SeatsMax: 4, SeatsUsed: 1, Inflight: 2,
		})
	}
	return result
}

func (fixture *fixture) exec(t *testing.T, query string, args ...any) {
	t.Helper()
	if _, err := fixture.db.Exec(query, args...); err != nil {
		t.Fatal(err)
	}
}

func (fixture *fixture) stamp(offset time.Duration) string {
	return fixture.now.Add(offset).UTC().Format(time.RFC3339)
}

func (fixture *fixture) log(t *testing.T, owner int, row vm2api.LogRow) {
	t.Helper()
	if row.KeyID == "" {
		row.KeyID = fixture.keys[owner]
	}
	if row.VMID == "" {
		row.VMID = fmt.Sprintf("internal-vm-%d", owner)
	}
	if row.CreatedAt.IsZero() {
		row.CreatedAt = fixture.now.Add(-time.Hour)
	}
	if row.Model == "" {
		row.Model = "claude-sonnet"
	}
	if row.StatusCode == 0 {
		row.StatusCode = 200
	}
	if err := fixture.client.AddRequestLog(fixture.owners[owner], row); err != nil {
		t.Fatal(err)
	}
}

func assertRedacted(t *testing.T, value any, secrets ...string) {
	t.Helper()
	encoded, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	for _, secret := range append([]string{`"vm"`, `"vm_id"`, `"owner_user_id"`, "secret-ciphertext", "internal-slot-label", "internal-credential"}, secrets...) {
		if strings.Contains(string(encoded), secret) {
			t.Fatalf("response contains %q: %s", secret, encoded)
		}
	}
}

func TestAccountUsageCacheAndOwnership(t *testing.T) {
	fixture := setup(t)
	ctx := context.Background()
	reset := fixture.now.In(time.FixedZone("local", 8*60*60))
	fixture.client.AddVM(vm2api.VM{
		ID: "internal-vm-0", OwnerUserID: fixture.owners[0], Label: "internal-slot-label", ScheduleState: "restricted",
		RestrictionText: "internal proxy 10.0.0.1 owner=" + fixture.owners[0],
		FiveHour:        &vm2api.Window{Utilization: 25, ResetsAt: &reset, Status: "ok"},
		SeatsMax:        5, SeatsUsed: 2, Inflight: 3,
	})
	first, err := fixture.service.AccountUsage(ctx, 1, 21)
	if err != nil || first == nil {
		t.Fatalf("account=%+v, err=%v", first, err)
	}
	if first.AccountID != 21 || first.Label != "账号1" || !first.Available || first.FiveHour.Utilization != 25 || first.FiveHour.ResetsAt.Location() != time.UTC || first.SeatsMax != 5 || first.SeatsUsed != 2 || first.Inflight != 3 || first.FetchedAt.Location() != time.UTC {
		t.Fatalf("account=%+v", first)
	}
	assertRedacted(t, first, fixture.owners[0], "10.0.0.1")
	if first.RestrictionText == nil || first.SevenDay != nil || first.Daily == nil || first.ByModel == nil {
		t.Fatalf("missing nullable fields: %+v", first)
	}
	first.FiveHour.Utilization = 999
	*first.FiveHour.ResetsAt = time.Time{}
	first.Estimate5h.Status = "changed"
	first.Today.Requests = 999
	fixture.exec(t, `UPDATE resources SET label = '新的备注' WHERE id = 21`)
	fixture.client.AddVM(vm2api.VM{ID: "internal-vm-0", OwnerUserID: fixture.owners[0], FiveHour: &vm2api.Window{Utilization: 75}})
	fixture.now = fixture.now.Add(29 * time.Second)
	cached, err := fixture.service.AccountUsage(ctx, 1, 21)
	if err != nil || cached == nil || cached.Label != "新的备注" || cached.FiveHour.Utilization != 25 || cached.FiveHour.ResetsAt.IsZero() || cached.Estimate5h.Status == "changed" || cached.Today.Requests == 999 {
		t.Fatalf("cached=%+v, err=%v", cached, err)
	}
	if _, err := fixture.service.AccountUsage(ctx, 2, 21); !errors.Is(err, ErrNotFound) {
		t.Fatalf("other tenant read: %v", err)
	}
	fixture.now = fixture.now.Add(time.Second)
	refreshed, err := fixture.service.AccountUsage(ctx, 1, 21)
	if err != nil || refreshed == nil || refreshed.FiveHour.Utilization != 75 {
		t.Fatalf("refreshed=%+v, err=%v", refreshed, err)
	}
	fixture.exec(t, `UPDATE resources SET user_id = 2 WHERE id = 21`)
	if _, err := fixture.service.AccountUsage(ctx, 1, 21); !errors.Is(err, ErrNotFound) {
		t.Fatalf("cached after owner change: %v", err)
	}
	if result, err := fixture.service.AccountUsage(ctx, 2, 21); err != nil || result != nil {
		t.Fatalf("mismatched upstream owner=%+v, err=%v", result, err)
	}
}

func TestAccountUsageUnavailableAndResourceState(t *testing.T) {
	for _, status := range []string{"active", "needs_reauth", "suspended", "provisioning", "awaiting_auth", "bootstrapping", "failed", "releasing", "released", "allocating"} {
		t.Run(status, func(t *testing.T) {
			fixture := setup(t)
			fixture.exec(t, `UPDATE resources SET status = ? WHERE id = 21`, status)
			result, err := fixture.service.AccountUsage(context.Background(), 1, 21)
			wantUsage := status == "active" || status == "needs_reauth" || status == "suspended"
			if err != nil || (result != nil) != wantUsage {
				t.Fatalf("usage=%+v, err=%v", result, err)
			}
		})
	}
	fixture := setup(t)
	ctx := context.Background()
	if err := fixture.client.DeleteVM(ctx, "internal-vm-0"); err != nil {
		t.Fatal(err)
	}
	if result, err := fixture.service.AccountUsage(ctx, 1, 21); err != nil || result != nil {
		t.Fatalf("missing upstream=%+v, err=%v", result, err)
	}
	fixture.client.AddVM(vm2api.VM{ID: "internal-vm-0", OwnerUserID: fixture.owners[0]})
	if result, err := fixture.service.AccountUsage(ctx, 1, 21); err != nil || result == nil {
		t.Fatalf("failed response cached: result=%+v, err=%v", result, err)
	}
	fixture.exec(t, `UPDATE resources SET vm_id = NULL WHERE id = 21`)
	if result, err := fixture.service.AccountUsage(ctx, 1, 21); err != nil || result != nil {
		t.Fatalf("unallocated=%+v, err=%v", result, err)
	}
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := fixture.service.AccountUsage(canceled, 1, 21); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancel=%v", err)
	}
}

func TestAccountUsageConcurrentSnapshots(t *testing.T) {
	fixture := setup(t)
	var workers sync.WaitGroup
	for range 20 {
		workers.Go(func() {
			result, err := fixture.service.AccountUsage(context.Background(), 1, 21)
			if err != nil || result == nil || result.FiveHour.Utilization != 12.5 {
				t.Errorf("account=%+v, err=%v", result, err)
				return
			}
			result.FiveHour.Utilization = 90
		})
	}
	workers.Wait()
}

func TestAccountUsageScopesHistoryToTenantAndCurrentRental(t *testing.T) {
	fixture := setup(t)
	fixture.exec(t, `UPDATE resources SET kind = 'rental', created_at = ? WHERE id = 21`, fixture.stamp(-2*time.Hour))
	fixture.log(t, 1, vm2api.LogRow{VMID: "internal-vm-0", Model: "previous-tenant-model", CostUSD: 999})
	fixture.log(t, 0, vm2api.LogRow{CreatedAt: fixture.now.Add(-3 * time.Hour), Model: "previous-rental-model", CostUSD: 888})
	fixture.log(t, 0, vm2api.LogRow{VMID: "other-account", Model: "other-account-model", CostUSD: 777})
	for index := range 105 {
		fixture.log(t, 0, vm2api.LogRow{CreatedAt: fixture.now.Add(-time.Duration(index+1) * time.Minute),
			InputTokens: 1, OutputTokens: 2, CacheReadTokens: 3, CacheCreationTokens: 4, CostUSD: 0.125})
	}
	result, err := fixture.service.AccountUsage(context.Background(), 1, 21)
	if err != nil || result == nil {
		t.Fatalf("account=%+v, err=%v", result, err)
	}
	for _, spend := range []*Spend{result.Today, result.Window5h, result.Window7d} {
		if spend == nil || spend.Requests != 105 || spend.Tokens != 1050 || spend.InputTokens != 105 || spend.CacheCreationTokens != 420 || spend.CostUSD != 13.125 {
			t.Fatalf("scoped spend=%+v", spend)
		}
	}
	if len(result.Daily) != 1 || result.Daily[0].Requests != 105 || len(result.ByModel) != 1 || result.ByModel[0].Requests != 105 || result.Estimate5h != nil || result.Estimate7d != nil {
		t.Fatalf("scoped breakdown=%+v", result)
	}
	assertRedacted(t, result, "previous-tenant-model", "previous-rental-model", "other-account-model", fixture.owners[1])
}

func TestAccountWindowStartsAtResetBoundary(t *testing.T) {
	until := time.Date(2026, 10, 31, 12, 0, 0, 0, time.UTC)
	reset := until.Add(time.Hour)
	if start := accountWindowStart(&Window{ResetsAt: &reset}, until, 5*time.Hour); !start.Equal(until.Add(-4 * time.Hour)) {
		t.Fatalf("window start=%s", start)
	}
	if start := accountWindowStart(nil, until, 5*time.Hour); !start.Equal(until.Add(-5 * time.Hour)) {
		t.Fatalf("missing reset start=%s", start)
	}
}

func TestOverviewUsesUTCDayAndNullOnFailure(t *testing.T) {
	fixture := setup(t)
	fixture.now = fixture.now.In(time.FixedZone("local", 8*60*60))
	fixture.log(t, 0, vm2api.LogRow{CreatedAt: time.Date(2026, 10, 30, 23, 59, 59, 0, time.UTC), InputTokens: 999})
	fixture.log(t, 0, vm2api.LogRow{CreatedAt: time.Date(2026, 10, 31, 0, 0, 0, 0, time.UTC), InputTokens: 10, OutputTokens: 2, CostUSD: 0.25})
	fixture.log(t, 0, vm2api.LogRow{CreatedAt: fixture.now.Add(time.Minute), InputTokens: 888})
	fixture.log(t, 1, vm2api.LogRow{InputTokens: 777})
	result, err := fixture.service.Overview(context.Background(), 1)
	if err != nil || result.UsageToday == nil || result.UsageToday.Requests != 1 || result.UsageToday.Tokens != 12 || result.UsageToday.CostUSD != 0.25 {
		t.Fatalf("overview=%+v, err=%v", result, err)
	}
	fixture.exec(t, `UPDATE users SET vm2api_user_id = 'missing-owner' WHERE id = 1`)
	result, err = fixture.service.Overview(context.Background(), 1)
	if err != nil || result.UsageToday != nil {
		t.Fatalf("unavailable overview=%+v, err=%v", result, err)
	}
	encoded, err := json.Marshal(result)
	if err != nil || string(encoded) != `{"usage_today":null}` {
		t.Fatalf("json=%s, err=%v", encoded, err)
	}
	fixture.exec(t, `UPDATE users SET vm2api_user_id = NULL WHERE id = 1`)
	if result, err = fixture.service.Overview(context.Background(), 1); err != nil || result.UsageToday != nil {
		t.Fatalf("unlinked overview=%+v, err=%v", result, err)
	}
	if _, err := fixture.service.Overview(context.Background(), 999); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown user=%v", err)
	}
}

func TestSummaryScopesMapsAndPreservesUpstreamCosts(t *testing.T) {
	fixture := setup(t)
	ctx := context.Background()
	latency := int64(20)
	fixture.log(t, 0, vm2api.LogRow{InputTokens: 10, OutputTokens: 2, CacheReadTokens: 3, CacheCreationTokens: 4, CostUSD: 0.125, FirstTokenMS: &latency})
	fixture.log(t, 0, vm2api.LogRow{Model: "claude-opus", InputTokens: 1, CostUSD: 0.25, StatusCode: 500, FirstTokenMS: &latency})
	fixture.log(t, 1, vm2api.LogRow{InputTokens: 999, CostUSD: 999})
	fixture.log(t, 0, vm2api.LogRow{CreatedAt: fixture.now.Add(-48 * time.Hour), CostUSD: 999})
	fixture.exec(t, `UPDATE api_keys SET status = 'deleted', name = '历史密钥' WHERE id = 11`)
	fixture.exec(t, `UPDATE resources SET status = 'released', released_at = ?, label = '历史账号' WHERE id = 21`, fixture.stamp(-30*time.Minute))
	zone := time.FixedZone("local", 8*60*60)
	result, err := fixture.service.Summary(ctx, 1, fixture.now.Add(-24*time.Hour).In(zone), fixture.now.In(zone))
	if err != nil {
		t.Fatal(err)
	}
	if result.From.Location() != time.UTC || result.Until.Location() != time.UTC || result.Totals.Requests != 2 || result.Totals.Tokens != 20 || result.Totals.CostUSD != 0.375 {
		t.Fatalf("summary=%+v", result)
	}
	if len(result.ByKey) != 1 || result.ByKey[0].KeyID != 11 || result.ByKey[0].KeyName != "历史密钥" || len(result.ByAccount) != 1 || result.ByAccount[0].AccountID != 21 || result.ByAccount[0].Label != "历史账号" {
		t.Fatalf("mappings=%+v", result)
	}
	if len(result.Daily) != 1 || result.Daily[0].Requests != 2 || len(result.ByModel) != 2 || result.Stats.SuccessRate != 0.5 || result.Stats.ErrorCount != 1 || result.Stats.AvgTTFTMS != 20 || result.Stats.P95TTFTMS != 20 {
		t.Fatalf("breakdowns=%+v", result)
	}
	assertRedacted(t, result, fixture.owners[0], fixture.owners[1], fixture.keys[0], fixture.keys[1], "internal-vm-0", "internal-vm-1")
	for _, until := range []time.Time{fixture.now.Add(-time.Hour), fixture.now.Add(-2 * time.Hour)} {
		if _, err := fixture.service.Summary(ctx, 1, fixture.now.Add(-time.Hour), until); !errors.Is(err, ErrInvalidRange) {
			t.Fatalf("invalid range=%v", err)
		}
	}
	fixture.exec(t, `UPDATE users SET vm2api_user_id = 'missing-owner' WHERE id = 1`)
	if _, err := fixture.service.Summary(ctx, 1, fixture.now.Add(-time.Hour), fixture.now); !vm2api.IsStatus(err, 404) {
		t.Fatalf("unwrapped upstream error=%v", err)
	}
}

func TestSummaryBreakdownPaginatesAndUsesUTCDates(t *testing.T) {
	fixture := setup(t)
	for index := range 205 {
		fixture.log(t, 0, vm2api.LogRow{CreatedAt: fixture.now.Add(time.Duration(-index) * time.Minute), InputTokens: 1, CostUSD: 0.1})
	}
	fixture.log(t, 0, vm2api.LogRow{CreatedAt: time.Date(2026, 10, 30, 23, 30, 0, 0, time.UTC), Model: "claude-opus", InputTokens: 2})
	result, err := fixture.service.Summary(context.Background(), 1, fixture.now.Add(-24*time.Hour), fixture.now)
	if err != nil || len(result.Daily) != 2 || result.Daily[0].Date != "2026-10-30" || result.Daily[0].Requests != 1 || result.Daily[1].Requests != 205 || len(result.ByModel) != 2 || result.ByModel[1].Requests != 205 || math.Abs(result.Daily[1].CostUSD-20.5) > 1e-10 {
		t.Fatalf("summary=%+v, err=%v", result, err)
	}
}

func TestLogsPaginationFiltersAndRedaction(t *testing.T) {
	fixture := setup(t)
	ctx := context.Background()
	latency := int64(40)
	for index := range 5 {
		fixture.log(t, 0, vm2api.LogRow{RequestID: fmt.Sprintf("request-%d", index), CreatedAt: fixture.now.Add(-time.Duration(index+1) * time.Minute),
			InputTokens: 1, OutputTokens: 2, CacheReadTokens: 3, CacheCreationTokens: 4, CostUSD: 0.125, Stream: true, DurationMS: 200, FirstTokenMS: &latency})
	}
	fixture.log(t, 0, vm2api.LogRow{RequestID: "failed", Model: "claude-opus", StatusCode: 503, ErrorLabel: "internal proxy 10.0.0.1 secret", CreatedAt: fixture.now.Add(-10 * time.Minute)})
	fixture.log(t, 1, vm2api.LogRow{RequestID: "other-user"})
	result, err := fixture.service.Logs(ctx, 1, LogQuery{Page: 2, PageSize: 2})
	if err != nil || result.Total != 6 || result.Page != 2 || result.PageSize != 2 || len(result.Items) != 2 || result.Items[0].RequestID != "request-2" {
		t.Fatalf("page=%+v, err=%v", result, err)
	}
	item := result.Items[0]
	if item.KeyID == nil || *item.KeyID != 11 || item.KeyName == nil || *item.KeyName != "密钥1" || item.AccountID == nil || *item.AccountID != 21 || item.AccountLabel == nil || *item.AccountLabel != "账号1" || item.CreatedAt.Location() != time.UTC || item.ErrorLabel != nil || !item.Stream || item.CacheReadTokens != 3 || item.CacheCreationTokens != 4 || item.CostUSD != 0.125 || item.DurationMS != 200 || item.FirstTokenMS == nil || *item.FirstTokenMS != 40 {
		t.Fatalf("item=%+v", item)
	}
	result, err = fixture.service.Logs(ctx, 1, LogQuery{KeyID: 11, Model: "claude-opus", Status: "error"})
	if err != nil || result.Total != 1 || len(result.Items) != 1 || result.Items[0].RequestID != "failed" || result.Items[0].ErrorLabel == nil {
		t.Fatalf("filtered=%+v, err=%v", result, err)
	}
	assertRedacted(t, result, "10.0.0.1", "secret", "other-user", fixture.keys[0], fixture.owners[0], "internal-vm-0")
	result, err = fixture.service.Logs(ctx, 1, LogQuery{Page: 9, PageSize: 2, Status: "success"})
	if err != nil || result.Total != 5 || result.Items == nil || len(result.Items) != 0 {
		t.Fatalf("empty page=%+v, err=%v", result, err)
	}
	result, err = fixture.service.Logs(ctx, 1, LogQuery{PageSize: 500})
	if err != nil || result.Page != 1 || result.PageSize != 100 {
		t.Fatalf("clamped page=%+v, err=%v", result, err)
	}
	result, err = fixture.service.Logs(ctx, 1, LogQuery{From: fixture.now.Add(-4 * time.Minute), Until: fixture.now.Add(-2 * time.Minute)})
	if err != nil || result.Total != 3 {
		t.Fatalf("time filter=%+v, err=%v", result, err)
	}
}

func TestLogsRejectForeignKeysAndInvalidQueries(t *testing.T) {
	fixture := setup(t)
	ctx := context.Background()
	for _, keyID := range []int64{12, 999} {
		if _, err := fixture.service.Logs(ctx, 1, LogQuery{KeyID: keyID}); !errors.Is(err, ErrNotFound) {
			t.Fatalf("key %d: %v", keyID, err)
		}
	}
	for _, testcase := range []struct {
		query LogQuery
		want  error
	}{
		{LogQuery{Status: "invalid"}, ErrInvalidQuery},
		{LogQuery{KeyID: -1}, ErrInvalidQuery},
		{LogQuery{Page: -1}, ErrInvalidPagination},
		{LogQuery{PageSize: -1}, ErrInvalidPagination},
		{LogQuery{Page: int(^uint(0) >> 1), PageSize: 100}, ErrInvalidPagination},
		{LogQuery{From: fixture.now, Until: fixture.now}, ErrInvalidRange},
	} {
		if _, err := fixture.service.Logs(ctx, 1, testcase.query); !errors.Is(err, testcase.want) {
			t.Fatalf("query=%+v, err=%v, want=%v", testcase.query, err, testcase.want)
		}
	}
	fixture.exec(t, `UPDATE api_keys SET status = 'deleted' WHERE id = 11`)
	fixture.log(t, 0, vm2api.LogRow{})
	if page, err := fixture.service.Logs(ctx, 1, LogQuery{KeyID: 11}); err != nil || page.Total != 1 {
		t.Fatalf("historical key=%+v, err=%v", page, err)
	}
	fixture.exec(t, `UPDATE users SET vm2api_user_id = 'missing-owner' WHERE id = 1`)
	if _, err := fixture.service.Logs(ctx, 1, LogQuery{}); !vm2api.IsStatus(err, 404) {
		t.Fatalf("unwrapped upstream error=%v", err)
	}
}

func TestUnknownMappingsNeverUseOtherTenantNames(t *testing.T) {
	fixture := setup(t)
	fixture.log(t, 0, vm2api.LogRow{KeyID: fixture.keys[1], VMID: "internal-vm-1", InputTokens: 3})
	fixture.log(t, 0, vm2api.LogRow{KeyID: "removed-internal-key", VMID: "removed-internal-vm", InputTokens: 5})
	page, err := fixture.service.Logs(context.Background(), 1, LogQuery{})
	if err != nil || page.Total != 2 || len(page.Items) != 2 {
		t.Fatalf("logs=%+v, err=%v", page, err)
	}
	for _, row := range page.Items {
		if row.KeyID != nil || row.KeyName != nil || row.AccountID != nil || row.AccountLabel != nil {
			t.Fatalf("unknown mapping=%+v", row)
		}
	}
	result, err := fixture.service.Summary(context.Background(), 1, fixture.now.Add(-24*time.Hour), fixture.now)
	if err != nil || result.Totals.Requests != 2 || len(result.ByKey) != 0 || len(result.ByAccount) != 0 {
		t.Fatalf("summary=%+v, err=%v", result, err)
	}
	assertRedacted(t, result, fixture.keys[1], "internal-vm-1", "removed-internal-key", "removed-internal-vm", "密钥2", "账号2")
	assertRedacted(t, page, fixture.keys[1], "internal-vm-1", "removed-internal-key", "removed-internal-vm", "密钥2", "账号2")
}

func TestLogsResolveReusedRentalByRequestTime(t *testing.T) {
	fixture := setup(t)
	fixture.exec(t, `UPDATE resources SET kind = 'rental', status = 'released', released_at = ? WHERE id = 21`, fixture.stamp(-2*time.Hour))
	fixture.exec(t, `INSERT INTO resources (id, user_id, kind, vm_id, label, status, created_at, updated_at)
		VALUES (23, 1, 'rental', 'internal-vm-0', '再次租用', 'active', ?, ?)`, fixture.stamp(-time.Hour), fixture.stamp(0))
	fixture.log(t, 0, vm2api.LogRow{RequestID: "old-rental", CreatedAt: fixture.now.Add(-3 * time.Hour)})
	fixture.log(t, 0, vm2api.LogRow{RequestID: "new-rental", CreatedAt: fixture.now.Add(-30 * time.Minute)})
	fixture.log(t, 0, vm2api.LogRow{RequestID: "between-rentals", CreatedAt: fixture.now.Add(-90 * time.Minute)})
	page, err := fixture.service.Logs(context.Background(), 1, LogQuery{})
	if err != nil || len(page.Items) != 3 {
		t.Fatalf("page=%+v, err=%v", page, err)
	}
	if page.Items[0].AccountID == nil || *page.Items[0].AccountID != 23 || page.Items[1].AccountID != nil || page.Items[2].AccountID == nil || *page.Items[2].AccountID != 21 {
		t.Fatalf("rental mapping=%+v", page.Items)
	}
	result, err := fixture.service.Summary(context.Background(), 1, fixture.now.Add(-24*time.Hour), fixture.now)
	if err != nil || result.Totals.Requests != 3 || len(result.ByAccount) != 2 || result.ByAccount[0].AccountID != 21 || result.ByAccount[0].Requests != 1 || result.ByAccount[1].AccountID != 23 || result.ByAccount[1].Requests != 1 {
		t.Fatalf("rental summary=%+v, err=%v", result, err)
	}
}

func TestAccountCacheDoesNotServeStaleOrRemappedData(t *testing.T) {
	fixture := setup(t)
	ctx := context.Background()
	if _, err := fixture.service.AccountUsage(ctx, 1, 21); err != nil {
		t.Fatal(err)
	}
	fixture.client.AddVM(vm2api.VM{ID: "another-internal-vm", OwnerUserID: fixture.owners[0], SeatsMax: 7})
	fixture.exec(t, `UPDATE resources SET vm_id = 'another-internal-vm' WHERE id = 21`)
	result, err := fixture.service.AccountUsage(ctx, 1, 21)
	if err != nil || result == nil || result.SeatsMax != 7 {
		t.Fatalf("remapped=%+v, err=%v", result, err)
	}
	if err := fixture.client.DeleteVM(ctx, "another-internal-vm"); err != nil {
		t.Fatal(err)
	}
	fixture.now = fixture.now.Add(30 * time.Second)
	result, err = fixture.service.AccountUsage(ctx, 1, 21)
	if err != nil || result != nil {
		t.Fatalf("stale account=%+v, err=%v", result, err)
	}
}

func TestCacheDeepCopiesNestedEstimatesAndBreakdowns(t *testing.T) {
	reset := time.Now().In(time.FixedZone("local", 8*60*60))
	amount, ratio := 4.5, 0.5
	value := &AccountUsage{
		FiveHour: &Window{ResetsAt: &reset}, SevenDay: &Window{ResetsAt: &reset}, SevenDayFable: &Window{ResetsAt: &reset},
		Estimate5h: &Estimate{TotalUSD: &amount, RemainingUSD: &amount, UsedRatio: &ratio},
		Estimate7d: &Estimate{TotalUSD: &amount},
		Today:      &Spend{}, Window5h: &Spend{}, Window7d: &Spend{},
		Daily: []DailyUsage{{Requests: 2}}, ByModel: []ModelUsage{{Requests: 3}},
	}
	cloned := cloneAccountUsage(value)
	*cloned.Estimate5h.TotalUSD = 999
	*cloned.Estimate5h.RemainingUSD = 999
	*cloned.Estimate5h.UsedRatio = 999
	*cloned.Estimate7d.TotalUSD = 999
	*cloned.SevenDayFable.ResetsAt = time.Time{}
	cloned.Daily[0].Requests, cloned.ByModel[0].Requests = 999, 999
	if amount != 4.5 || ratio != 0.5 || reset.IsZero() || value.Daily[0].Requests != 2 || value.ByModel[0].Requests != 3 || cloned.FiveHour.ResetsAt.Location() != time.UTC {
		t.Fatalf("cache snapshot aliases source: %+v", cloned)
	}
}

func TestUnlinkedUserReturnsEmptyCollections(t *testing.T) {
	fixture := setup(t)
	fixture.exec(t, `UPDATE users SET vm2api_user_id = NULL WHERE id = 1`)
	result, err := fixture.service.Summary(context.Background(), 1, fixture.now.Add(-time.Hour), fixture.now)
	if err != nil || result.Daily == nil || result.ByModel == nil || result.ByKey == nil || result.ByAccount == nil || result.Totals.Requests != 0 {
		t.Fatalf("summary=%+v, err=%v", result, err)
	}
	page, err := fixture.service.Logs(context.Background(), 1, LogQuery{})
	if err != nil || page.Items == nil || page.Page != 1 || page.PageSize != 20 || page.Total != 0 {
		t.Fatalf("logs=%+v, err=%v", page, err)
	}
	if _, err := fixture.service.Logs(context.Background(), 1, LogQuery{KeyID: 12}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unlinked user foreign key: %v", err)
	}
}
