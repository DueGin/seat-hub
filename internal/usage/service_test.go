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

	"github.com/DueGin/seat-hub/internal/store"
	"github.com/DueGin/seat-hub/internal/store/migrations"
	"github.com/DueGin/seat-hub/internal/vm2api"
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

type logsErrorClient struct {
	vm2api.Client
	err error
}

func (client logsErrorClient) UserRequestLogs(context.Context, string, vm2api.LogQuery) (vm2api.LogPage, error) {
	return vm2api.LogPage{}, client.err
}

type recordedLogQuery struct {
	userID string
	query  vm2api.LogQuery
}

type recordingLogsClient struct {
	vm2api.Client
	queries []recordedLogQuery
}

func (client *recordingLogsClient) UserRequestLogs(ctx context.Context, userID string, query vm2api.LogQuery) (vm2api.LogPage, error) {
	client.queries = append(client.queries, recordedLogQuery{userID: userID, query: query})
	return client.Client.UserRequestLogs(ctx, userID, query)
}

func setup(t *testing.T) *fixture {
	t.Helper()
	database, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	database.SetMaxOpenConns(1)
	t.Cleanup(func() { database.Close() })
	for _, name := range []string{"001_init.sql", "004_key_scope.sql"} {
		schema, err := migrations.FS.ReadFile(name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := database.Exec(string(schema)); err != nil {
			t.Fatal(err)
		}
	}
	result := &fixture{db: database, client: vm2api.NewFake(), now: time.Date(2026, 10, 31, 12, 0, 0, 0, time.UTC)}
	result.service = New(database, result.client)
	result.service.now = func() time.Time { return result.now }
	for index := range result.owners {
		owner, err := result.client.CreateUser(context.Background(), fmt.Sprintf("owner-%d", index), "password")
		if err != nil {
			t.Fatal(err)
		}
		pool, err := result.client.CreatePool(context.Background(), fmt.Sprintf("legacy-pool-%d", index), nil)
		if err != nil {
			t.Fatal(err)
		}
		key, err := result.client.CreateKey(context.Background(), vm2api.CreateKeyInput{UserID: owner.ID, Name: "internal-key", Scope: vm2api.KeyScope{PoolID: pool.ID}})
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

func (fixture *fixture) shareServiceOwner(t *testing.T) {
	t.Helper()
	identity, err := fixture.client.CreateUser(context.Background(), "seat-hub-service", "password")
	if err != nil {
		t.Fatal(err)
	}
	if err := store.SaveServiceIdentity(context.Background(), fixture.db, store.ServiceIdentity{UserID: identity.ID, Username: identity.Username}); err != nil {
		t.Fatal(err)
	}
	for index := range fixture.owners {
		pool, err := fixture.client.CreatePool(context.Background(), fmt.Sprintf("service-pool-%d", index), nil)
		if err != nil {
			t.Fatal(err)
		}
		key, err := fixture.client.CreateKey(context.Background(), vm2api.CreateKeyInput{UserID: identity.ID, Name: "service-key", Scope: vm2api.KeyScope{PoolID: pool.ID}})
		if err != nil {
			t.Fatal(err)
		}
		fixture.owners[index], fixture.keys[index] = identity.ID, key.ID
		fixture.exec(t, `UPDATE users SET vm2api_user_id = NULL WHERE id = ?`, index+1)
		fixture.exec(t, `UPDATE api_keys SET vm2api_key_id = ?, upstream_owner = ? WHERE id = ?`, key.ID, identity.ID, index+11)
		if err := fixture.client.SetVMOwner(context.Background(), fmt.Sprintf("internal-vm-%d", index), &identity.ID); err != nil {
			t.Fatal(err)
		}
	}
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
	fixture.service.client = logsErrorClient{Client: fixture.client, err: vm2api.ErrUnavailable}
	result, err = fixture.service.Overview(context.Background(), 1)
	if err != nil || result.UsageToday != nil {
		t.Fatalf("unavailable overview=%+v, err=%v", result, err)
	}
	encoded, err := json.Marshal(result)
	if err != nil || string(encoded) != `{"usage_today":null}` {
		t.Fatalf("json=%s, err=%v", encoded, err)
	}
	fixture.exec(t, `UPDATE users SET vm2api_user_id = NULL WHERE id = 1`)
	fixture.service.client = fixture.client
	if result, err = fixture.service.Overview(context.Background(), 1); err != nil || result.UsageToday == nil || result.UsageToday.Requests != 1 {
		t.Fatalf("migrated overview=%+v, err=%v", result, err)
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
	fixture.service.client = logsErrorClient{Client: fixture.client, err: &vm2api.Error{Status: 404}}
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
	fixture.service.client = logsErrorClient{Client: fixture.client, err: &vm2api.Error{Status: 404}}
	if _, err := fixture.service.Logs(ctx, 1, LogQuery{}); !vm2api.IsStatus(err, 404) {
		t.Fatalf("unwrapped upstream error=%v", err)
	}
}

func TestUnknownMappingsNeverUseOtherTenantNames(t *testing.T) {
	fixture := setup(t)
	fixture.log(t, 0, vm2api.LogRow{KeyID: fixture.keys[1], VMID: "internal-vm-1", InputTokens: 3})
	fixture.log(t, 0, vm2api.LogRow{KeyID: "removed-internal-key", VMID: "removed-internal-vm", InputTokens: 5})
	page, err := fixture.service.Logs(context.Background(), 1, LogQuery{})
	if err != nil || page.Total != 0 || len(page.Items) != 0 {
		t.Fatalf("logs=%+v, err=%v", page, err)
	}
	result, err := fixture.service.Summary(context.Background(), 1, fixture.now.Add(-24*time.Hour), fixture.now)
	if err != nil || result.Totals.Requests != 0 || len(result.ByKey) != 0 || len(result.ByAccount) != 0 {
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

func TestUserWithNoKeysReturnsZerosAndEmptyCollections(t *testing.T) {
	fixture := setup(t)
	fixture.shareServiceOwner(t)
	fixture.log(t, 0, vm2api.LogRow{InputTokens: 999, CostUSD: 999})
	fixture.exec(t, `DELETE FROM api_keys WHERE user_id = 1`)
	fixture.exec(t, `UPDATE users SET vm2api_user_id = NULL WHERE id = 1`)
	fixture.service.client = logsErrorClient{Client: fixture.client, err: vm2api.ErrUnavailable}
	overview, err := fixture.service.Overview(context.Background(), 1)
	if err != nil || overview.UsageToday == nil || *overview.UsageToday != (Spend{}) {
		t.Fatalf("overview=%+v, err=%v", overview, err)
	}
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
	account, err := fixture.service.AccountUsage(context.Background(), 1, 21)
	if err != nil || account == nil || account.Today == nil || *account.Today != (Spend{}) || account.Window5h == nil || *account.Window5h != (Spend{}) || account.Window7d == nil || *account.Window7d != (Spend{}) || len(account.Daily) != 0 || len(account.ByModel) != 0 {
		t.Fatalf("account=%+v, err=%v", account, err)
	}
}

func TestLogsFilterByAccount(t *testing.T) {
	fixture := setup(t)
	ctx := context.Background()
	fixture.exec(t, `INSERT INTO resources (id, user_id, kind, vm_id, label, status, created_at, updated_at)
		VALUES (25, 1, 'hosted', 'internal-vm-extra', '副号', 'active', ?, ?)`, fixture.stamp(-24*time.Hour), fixture.stamp(0))
	fixture.exec(t, `INSERT INTO resources (id, user_id, kind, label, status, created_at, updated_at)
		VALUES (26, 1, 'hosted', '未分配', 'provisioning', ?, ?)`, fixture.stamp(-time.Hour), fixture.stamp(0))
	fixture.client.AddVM(vm2api.VM{ID: "internal-vm-extra", OwnerUserID: fixture.owners[0]})
	fixture.log(t, 0, vm2api.LogRow{RequestID: "main-1", CreatedAt: fixture.now.Add(-time.Minute)})
	fixture.log(t, 0, vm2api.LogRow{RequestID: "main-2", CreatedAt: fixture.now.Add(-2 * time.Minute)})
	fixture.log(t, 0, vm2api.LogRow{RequestID: "extra-1", VMID: "internal-vm-extra", CreatedAt: fixture.now.Add(-3 * time.Minute)})
	fixture.log(t, 1, vm2api.LogRow{RequestID: "other-user"})

	page, err := fixture.service.Logs(ctx, 1, LogQuery{AccountID: 21})
	if err != nil || page.Total != 2 || len(page.Items) != 2 {
		t.Fatalf("main account=%+v, err=%v", page, err)
	}
	for _, item := range page.Items {
		if item.AccountID == nil || *item.AccountID != 21 {
			t.Fatalf("main account item=%+v", item)
		}
	}
	page, err = fixture.service.Logs(ctx, 1, LogQuery{AccountID: 25})
	if err != nil || page.Total != 1 || page.Items[0].RequestID != "extra-1" || *page.Items[0].AccountLabel != "副号" {
		t.Fatalf("extra account=%+v, err=%v", page, err)
	}
	assertRedacted(t, page, "internal-vm-extra", "other-user")
	// An account that has no slot yet has no calls, not an error.
	page, err = fixture.service.Logs(ctx, 1, LogQuery{AccountID: 26})
	if err != nil || page.Total != 0 || page.Items == nil {
		t.Fatalf("slotless account=%+v, err=%v", page, err)
	}
	// Another user's account and a missing one are indistinguishable.
	for _, id := range []int64{22, 999} {
		if _, err := fixture.service.Logs(ctx, 1, LogQuery{AccountID: id}); !errors.Is(err, ErrNotFound) {
			t.Fatalf("account %d: %v", id, err)
		}
	}
	if _, err := fixture.service.Logs(ctx, 1, LogQuery{AccountID: -1}); !errors.Is(err, ErrInvalidQuery) {
		t.Fatalf("negative account: %v", err)
	}
}

func TestLogsAccountFilterKeepsToItsLease(t *testing.T) {
	fixture := setup(t)
	ctx := context.Background()
	fixture.exec(t, `UPDATE resources SET kind = 'rental', status = 'released', released_at = ? WHERE id = 21`, fixture.stamp(-2*time.Hour))
	fixture.exec(t, `INSERT INTO resources (id, user_id, kind, vm_id, label, status, created_at, updated_at)
		VALUES (23, 1, 'rental', 'internal-vm-0', '再次租用', 'active', ?, ?)`, fixture.stamp(-time.Hour), fixture.stamp(0))
	fixture.log(t, 0, vm2api.LogRow{RequestID: "old-rental", CreatedAt: fixture.now.Add(-3 * time.Hour)})
	fixture.log(t, 0, vm2api.LogRow{RequestID: "between-rentals", CreatedAt: fixture.now.Add(-90 * time.Minute)})
	fixture.log(t, 0, vm2api.LogRow{RequestID: "new-rental", CreatedAt: fixture.now.Add(-30 * time.Minute)})
	from := fixture.now.Add(-24 * time.Hour)
	for _, testcase := range []struct {
		account int64
		want    string
	}{{21, "old-rental"}, {23, "new-rental"}} {
		page, err := fixture.service.Logs(ctx, 1, LogQuery{AccountID: testcase.account, From: from})
		if err != nil || page.Total != 1 || page.Items[0].RequestID != testcase.want {
			t.Fatalf("account %d=%+v, err=%v", testcase.account, page, err)
		}
	}
	// A range that ends before the lease began is empty without asking upstream.
	page, err := fixture.service.Logs(ctx, 1, LogQuery{AccountID: 23, From: from, Until: fixture.now.Add(-2 * time.Hour)})
	if err != nil || page.Total != 0 || page.Items == nil {
		t.Fatalf("out of lease=%+v, err=%v", page, err)
	}
}

func TestSharedServiceUsageIsIsolatedByCurrentAndPreviousKeys(t *testing.T) {
	fixture := setup(t)
	previous := fixture.keys[0]
	fixture.log(t, 0, vm2api.LogRow{RequestID: "before-migration", InputTokens: 1, OutputTokens: 2, CacheReadTokens: 3, CacheCreationTokens: 4, CostUSD: 0.125})
	fixture.shareServiceOwner(t)
	fixture.exec(t, `UPDATE api_keys SET status = 'deleted', previous_vm2api_key_ids = ? WHERE id = 11`, store.EncodeStrings([]string{previous, previous, fixture.keys[0]}))
	if err := fixture.client.DeleteKey(context.Background(), previous); err != nil {
		t.Fatal(err)
	}
	latency := int64(30)
	fixture.log(t, 0, vm2api.LogRow{RequestID: "after-migration", InputTokens: 5, OutputTokens: 6, CacheReadTokens: 7, CacheCreationTokens: 8, CostUSD: 0.25, FirstTokenMS: &latency})
	fixture.log(t, 1, vm2api.LogRow{RequestID: "other-user", InputTokens: 999, CostUSD: 999, StatusCode: 500})
	ctx := context.Background()
	for _, testcase := range []struct {
		userID, requests, tokens int64
		cost                     float64
	}{{1, 2, 36, 0.375}, {2, 1, 999, 999}} {
		overview, err := fixture.service.Overview(ctx, testcase.userID)
		if err != nil || overview.UsageToday == nil || overview.UsageToday.Requests != testcase.requests || overview.UsageToday.Tokens != testcase.tokens || overview.UsageToday.CostUSD != testcase.cost {
			t.Fatalf("user %d overview=%+v, err=%v", testcase.userID, overview, err)
		}
		summary, err := fixture.service.Summary(ctx, testcase.userID, fixture.now.Add(-24*time.Hour), fixture.now)
		if err != nil || summary.Totals.Requests != testcase.requests || summary.Totals.Tokens != testcase.tokens || summary.Totals.CostUSD != testcase.cost || len(summary.ByKey) != 1 || summary.ByKey[0].Requests != testcase.requests {
			t.Fatalf("user %d summary=%+v, err=%v", testcase.userID, summary, err)
		}
		if testcase.userID == 1 {
			if summary.Totals.InputTokens != 6 || summary.Totals.OutputTokens != 8 || summary.Totals.CacheReadTokens != 10 || summary.Totals.CacheCreationTokens != 12 || summary.ByKey[0].KeyID != 11 || summary.Stats.SuccessRate != 1 || summary.Stats.AvgTTFTMS != 30 {
				t.Fatalf("detailed totals=%+v", summary)
			}
			page, err := fixture.service.Logs(ctx, 1, LogQuery{KeyID: 11})
			if err != nil || page.Total != 2 || len(page.Items) != 2 {
				t.Fatalf("historical logs=%+v, err=%v", page, err)
			}
			for _, row := range page.Items {
				if row.KeyID == nil || *row.KeyID != 11 {
					t.Fatalf("historical mapping=%+v", row)
				}
			}
			assertRedacted(t, page, "other-user", previous, fixture.keys[0], fixture.keys[1])
		}
	}
}

func TestSharedServiceLogsMergeAcrossKeysAndPages(t *testing.T) {
	fixture := setup(t)
	fixture.shareServiceOwner(t)
	pool, err := fixture.client.CreatePool(context.Background(), "extra-pool", nil)
	if err != nil {
		t.Fatal(err)
	}
	otherKey, err := fixture.client.CreateKey(context.Background(), vm2api.CreateKeyInput{UserID: fixture.owners[0], Name: "second-key", Scope: vm2api.KeyScope{PoolID: pool.ID}})
	if err != nil {
		t.Fatal(err)
	}
	fixture.exec(t, `INSERT INTO api_keys (id, user_id, name, key_hash, prefix, vm2api_key_id, vm2api_secret_enc, status, created_at, updated_at)
		VALUES (13, 1, '副密钥', 'hash-extra', 'sk-pt-extra', ?, 'secret-ciphertext', 'disabled', ?, ?)`, otherKey.ID, fixture.stamp(-time.Hour), fixture.stamp(0))
	client := &recordingLogsClient{Client: fixture.client}
	fixture.service.client = client
	for index := range 220 {
		keyID := fixture.keys[0]
		if index%2 == 1 {
			keyID = otherKey.ID
		}
		fixture.log(t, 0, vm2api.LogRow{RequestID: fmt.Sprintf("merged-%03d", index), KeyID: keyID, CreatedAt: fixture.now.Add(-time.Duration(index+1) * time.Minute)})
		fixture.log(t, 1, vm2api.LogRow{RequestID: fmt.Sprintf("foreign-%03d", index), CreatedAt: fixture.now.Add(-time.Duration(index+1) * time.Minute)})
	}
	for _, pageNumber := range []int{1, 2, 3, 4} {
		client.queries = nil
		page, err := fixture.service.Logs(context.Background(), 1, LogQuery{Page: pageNumber, PageSize: 100})
		wantCount := min(100, max(0, 220-(pageNumber-1)*100))
		if err != nil || page.Total != 220 || len(page.Items) != wantCount {
			t.Fatalf("page %d=%+v, err=%v", pageNumber, page, err)
		}
		for index, row := range page.Items {
			if want := fmt.Sprintf("merged-%03d", (pageNumber-1)*100+index); row.RequestID != want {
				t.Fatalf("page %d item %d=%s, want %s", pageNumber, index, row.RequestID, want)
			}
		}
		requested := make(map[string]int)
		for _, call := range client.queries {
			requested[call.query.KeyID] += call.query.PageSize
			if call.userID != "" || call.query.KeyID != fixture.keys[0] && call.query.KeyID != otherKey.ID || call.query.PageSize != 100 || call.query.Page > pageNumber {
				t.Fatalf("unscoped or excessive query=%+v", call)
			}
		}
		for keyID, count := range requested {
			if count > pageNumber*100 {
				t.Fatalf("page %d requested %d rows for key %s", pageNumber, count, keyID)
			}
		}
	}
	client.queries = nil
	if _, err := fixture.service.Logs(context.Background(), 1, LogQuery{Page: 101, PageSize: 100}); !errors.Is(err, ErrInvalidPagination) {
		t.Fatalf("unbounded merged page: %v", err)
	}
	if len(client.queries) != 0 {
		t.Fatalf("deep page queried upstream: %+v", client.queries)
	}
	page, err := fixture.service.Logs(context.Background(), 1, LogQuery{KeyID: 11, Page: 101, PageSize: 100})
	if err != nil || page.Total != 110 || len(page.Items) != 0 {
		t.Fatalf("single-key deep page=%+v, err=%v", page, err)
	}
	if len(client.queries) != 1 || client.queries[0].userID != "" || client.queries[0].query.KeyID != fixture.keys[0] || client.queries[0].query.Page != 101 || client.queries[0].query.PageSize != 100 {
		t.Fatalf("single-key page not passed through: %+v", client.queries)
	}
	fixture.log(t, 0, vm2api.LogRow{RequestID: "filtered-first", Model: "claude-opus", StatusCode: 503, CreatedAt: fixture.now.Add(-30 * time.Second)})
	fixture.log(t, 0, vm2api.LogRow{RequestID: "filtered-second", KeyID: otherKey.ID, Model: "claude-opus", StatusCode: 503, CreatedAt: fixture.now.Add(-time.Minute)})
	fixture.log(t, 1, vm2api.LogRow{RequestID: "filtered-foreign", VMID: "internal-vm-0", Model: "claude-opus", StatusCode: 503})
	page, err = fixture.service.Logs(context.Background(), 1, LogQuery{AccountID: 21, Model: "claude-opus", Status: "error", Page: 2, PageSize: 1})
	if err != nil || page.Total != 2 || len(page.Items) != 1 || page.Items[0].RequestID != "filtered-second" || page.Items[0].KeyID == nil || *page.Items[0].KeyID != 13 {
		t.Fatalf("filtered merge=%+v, err=%v", page, err)
	}
}

func TestSummaryStatsUseOnlyOwnedKeyLogSamples(t *testing.T) {
	fixture := setup(t)
	fixture.shareServiceOwner(t)
	for index := range 20 {
		latency := int64(index * 10)
		status := 200
		if index%4 == 0 {
			status = 503
		}
		fixture.log(t, 0, vm2api.LogRow{FirstTokenMS: &latency, StatusCode: status})
	}
	fixture.log(t, 0, vm2api.LogRow{})
	foreignLatency := int64(9999)
	fixture.log(t, 1, vm2api.LogRow{StatusCode: 500, FirstTokenMS: &foreignLatency})
	summary, err := fixture.service.Summary(context.Background(), 1, fixture.now.Add(-24*time.Hour), fixture.now)
	if err != nil || summary.Stats.ErrorCount != 5 || summary.Stats.SuccessRate != float64(16)/21 || summary.Stats.AvgTTFTMS != 95 || summary.Stats.P95TTFTMS != 180 {
		t.Fatalf("stats=%+v, err=%v", summary.Stats, err)
	}
}

func TestAccountUsageWithServiceOwnerDropsOtherUsersKeys(t *testing.T) {
	fixture := setup(t)
	previous := fixture.keys[0]
	fixture.log(t, 0, vm2api.LogRow{CreatedAt: fixture.now.Add(-30 * time.Minute), InputTokens: 2, CostUSD: 0.125})
	fixture.shareServiceOwner(t)
	fixture.exec(t, `UPDATE api_keys SET previous_vm2api_key_ids = ? WHERE id = 11`, store.EncodeStrings([]string{previous}))
	fixture.exec(t, `UPDATE resources SET kind = 'rental', created_at = ? WHERE id = 21`, fixture.stamp(-2*time.Hour))
	fixture.log(t, 0, vm2api.LogRow{InputTokens: 3, CostUSD: 0.25})
	fixture.log(t, 1, vm2api.LogRow{VMID: "internal-vm-0", Model: "foreign-model", InputTokens: 999, CostUSD: 999})
	fixture.log(t, 0, vm2api.LogRow{CreatedAt: fixture.now.Add(-3 * time.Hour), InputTokens: 888, CostUSD: 888})
	client := &recordingLogsClient{Client: fixture.client}
	fixture.service.client = client
	result, err := fixture.service.AccountUsage(context.Background(), 1, 21)
	if err != nil || result == nil || result.Today == nil || result.Today.Requests != 2 || result.Today.Tokens != 5 || result.Today.CostUSD != 0.375 || len(result.ByModel) != 1 {
		t.Fatalf("account=%+v, err=%v", result, err)
	}
	assertRedacted(t, result, "foreign-model")
	for _, call := range client.queries {
		if call.userID != "" || call.query.VMID != "internal-vm-0" || call.query.KeyID != "" || !call.query.From.Equal(fixture.now.Add(-2*time.Hour)) {
			t.Fatalf("account history not scoped to VM and lease: %+v", call)
		}
	}
	page, err := fixture.service.Logs(context.Background(), 1, LogQuery{AccountID: 21})
	if err != nil || page.Total != 2 || len(page.Items) != 2 {
		t.Fatalf("account logs=%+v, err=%v", page, err)
	}
}

func TestSummaryWithKeysButNoRequestsKeepsEmptySuccessRate(t *testing.T) {
	fixture := setup(t)
	result, err := fixture.service.Summary(context.Background(), 1, fixture.now.Add(-24*time.Hour), fixture.now)
	if err != nil || result.Totals != (Spend{}) || result.Stats != (Stats{SuccessRate: 1}) || len(result.ByKey) != 0 {
		t.Fatalf("empty summary=%+v, err=%v", result, err)
	}
}

func TestAccountUsageAcceptsLegacyOwnerWhileServiceIdentityExists(t *testing.T) {
	fixture := setup(t)
	identity, err := fixture.client.CreateUser(context.Background(), "seat-hub-service", "password")
	if err != nil {
		t.Fatal(err)
	}
	if err := store.SaveServiceIdentity(context.Background(), fixture.db, store.ServiceIdentity{UserID: identity.ID, Username: identity.Username}); err != nil {
		t.Fatal(err)
	}
	fixture.log(t, 0, vm2api.LogRow{InputTokens: 3, CostUSD: 0.125})
	result, err := fixture.service.AccountUsage(context.Background(), 1, 21)
	if err != nil || result == nil || result.Today == nil || result.Today.Requests != 1 || result.Today.Tokens != 3 {
		t.Fatalf("legacy account=%+v, err=%v", result, err)
	}
	fixture.exec(t, `UPDATE users SET vm2api_user_id = NULL WHERE id = 1`)
	result, err = fixture.service.AccountUsage(context.Background(), 1, 21)
	if err != nil || result != nil {
		t.Fatalf("migrated user accepted stale legacy owner=%+v, err=%v", result, err)
	}
}
