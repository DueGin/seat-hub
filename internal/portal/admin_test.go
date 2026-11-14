package portal

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/DueGin/seat-hub/internal/api"
	"github.com/DueGin/seat-hub/internal/provision"
	"github.com/DueGin/seat-hub/internal/secret"
	"github.com/DueGin/seat-hub/internal/selfcheck"
	"github.com/DueGin/seat-hub/internal/store"
	"github.com/DueGin/seat-hub/internal/vm2api"
)

type adminClient struct {
	*vm2api.Fake
	pingErr error
	listErr error
}

func (client *adminClient) Ping(ctx context.Context) (vm2api.Health, error) {
	if client.pingErr != nil {
		return vm2api.Health{}, client.pingErr
	}
	return client.Fake.Ping(ctx)
}

func (client *adminClient) ListVMs(ctx context.Context, filter vm2api.VMFilter) ([]vm2api.VM, error) {
	if client.listErr != nil {
		return nil, client.listErr
	}
	return client.Fake.ListVMs(ctx, filter)
}

type adminChecker struct {
	now     func() time.Time
	check   func(context.Context, string, selfcheck.Options) (selfcheck.Result, error)
	vmIDs   []string
	options []selfcheck.Options
	records []selfcheck.Result
}

func (checker *adminChecker) Check(ctx context.Context, vmID string, options selfcheck.Options) (selfcheck.Result, error) {
	checker.vmIDs = append(checker.vmIDs, vmID)
	checker.options = append(checker.options, options)
	if checker.check != nil {
		return checker.check(ctx, vmID, options)
	}
	return selfcheck.Result{VMID: vmID, Passed: true, CheckedAt: checker.now(),
		Checks: []selfcheck.Check{{Code: selfcheck.FingerprintUnique, OK: true, Message: "设备指纹未重复"}}}, nil
}

func (checker *adminChecker) Record(_ context.Context, _ int64, _ int, result selfcheck.Result) error {
	checker.records = append(checker.records, result)
	return nil
}

func adminFixture(t *testing.T) (*Service, *adminClient) {
	t.Helper()
	database, err := store.Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { database.Close() })
	box, err := secret.New(strings.Repeat("a", 32))
	if err != nil {
		t.Fatal(err)
	}
	client := &adminClient{Fake: vm2api.NewFake()}
	service := New(database, client, box)
	service.Now = func() time.Time { return time.Date(2026, 10, 31, 12, 0, 0, 0, time.UTC) }
	service.provisioning.Engine().Now = service.now
	service.provisioning.SetChecker(&adminChecker{now: service.now})
	stamp := store.FmtTime(service.now())
	adminExec(t, service, `INSERT INTO users(id,email,password_hash,role,created_at,updated_at)
		VALUES (1,'admin@example.com','hash','admin',?,?),
		(2,'tenant@example.com','hash','user',?,?),(3,'other@example.com','hash','user',?,?)`,
		stamp, stamp, stamp, stamp, stamp, stamp)
	return service, client
}

func adminExec(t *testing.T, service *Service, query string, args ...any) int64 {
	t.Helper()
	result, err := service.db.Exec(query, args...)
	if err != nil {
		t.Fatal(err)
	}
	id, err := result.LastInsertId()
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func adminCount(t *testing.T, service *Service, query string, args ...any) int64 {
	t.Helper()
	var count int64
	if err := service.db.QueryRow(query, args...).Scan(&count); err != nil {
		t.Fatal(err)
	}
	return count
}

func adminWantError(t *testing.T, err error, status int, code string) {
	t.Helper()
	mapped := api.AsUserError(fmt.Errorf("调用管理服务失败: %w", err))
	if mapped == nil || mapped.Status != status || mapped.Code != code {
		t.Fatalf("error = %v, mapped = %+v; want %d %s", err, mapped, status, code)
	}
}

// adminPoolVM makes one rental account available so a rental grant can be
// allocated. The VM is registered upstream too: allocation assigns the owner.
func adminPoolVM(t *testing.T, service *Service, client *adminClient, vmID string) int64 {
	t.Helper()
	client.AddVM(vm2api.VM{ID: vmID, Label: vmID, Platform: "anthropic", Origin: "platform", CredStatus: "valid"})
	stamp := store.FmtTime(service.now())
	return adminExec(t, service, `INSERT INTO rental_pool(vm_id,status,selfcheck_state,selfcheck_checked_at,created_at,updated_at)
		VALUES (?,'available','pass',?,?,?)`, vmID, stamp, stamp, stamp)
}

func adminProduct(t *testing.T, service *Service, kind string) int64 {
	t.Helper()
	stamp := store.FmtTime(service.now())
	return adminExec(t, service, `INSERT INTO products(kind,name,unit_quantity,duration_days,price_cents,created_at,updated_at)
		VALUES (?,'测试商品',1,30,100,?,?)`, kind, stamp, stamp)
}

func adminOrder(t *testing.T, service *Service, userID, productID int64, status, provider string, amount int64, paidAt any) int64 {
	t.Helper()
	stamp := store.FmtTime(service.now())
	return adminExec(t, service, `INSERT INTO orders(order_no,user_id,product_id,product_snapshot,kind,
		quantity,units,duration_days,amount_cents,provider,status,created_at,expires_at,paid_at,updated_at)
		SELECT lower(hex(randomblob(12))),?,id,'{}',kind,2,2,duration_days,?,?,?, ?,?,?,? FROM products WHERE id=?`,
		userID, amount, provider, status, store.FmtTime(service.now().AddDate(0, 0, -60)),
		stamp, paidAt, stamp, productID)
}

func adminDrainJobs(t *testing.T, service *Service) {
	t.Helper()
	for iteration := 0; iteration < 30; iteration++ {
		worked, err := service.provisioning.Engine().RunOnce(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if !worked {
			if count := adminCount(t, service, `SELECT COUNT(*) FROM jobs WHERE status IN ('pending','running','failed')`); count != 0 {
				t.Fatalf("unfinished jobs = %d", count)
			}
			return
		}
	}
	t.Fatal("job queue did not drain")
}

func TestAdminDashboard(t *testing.T) {
	service, client := adminFixture(t)
	ctx := context.Background()
	empty, err := service.Dashboard(ctx)
	if err != nil || empty.Users != 3 || empty.PayingUsers != 0 || empty.RevenueCents != 0 || !empty.VM2API.OK {
		t.Fatalf("empty dashboard = %+v, error = %v", empty, err)
	}
	productID := adminProduct(t, service, "concurrency")
	now := service.now()
	for _, item := range []struct {
		userID           int64
		status, provider string
		amount           int64
		paidAt           any
	}{
		{2, "paid", "manual", 100, store.FmtTime(now.Add(-time.Hour))},
		{2, "fulfilled", "mock", 200, store.FmtTime(now.Add(-time.Hour))},
		{3, "paid", "manual", 300, store.FmtTime(now.AddDate(0, 0, -30))},
		{2, "fulfilled", "manual", 400, store.FmtTime(now)},
		{2, "paid", "manual", 50, now.In(time.FixedZone("local", 8*3600)).Format(time.RFC3339)},
		{1, "fulfilled", "grant", 0, store.FmtTime(now)},
		{1, "fulfilled", "grant", 7, store.FmtTime(now)},
		{1, "paid", "manual", -25, store.FmtTime(now)},
		{1, "paid", "manual", 0, store.FmtTime(now)},
		{1, "paid", "manual", 1000, store.FmtTime(now.AddDate(0, 0, -30).Add(-time.Second))},
		{1, "paid", "manual", 1000, store.FmtTime(now.Add(time.Second))},
		{1, "paid", "manual", 1000, nil},
		{1, "fulfill_failed", "manual", 1000, store.FmtTime(now)},
		{1, "refunded", "manual", 1000, store.FmtTime(now)},
		{1, "cancelled", "manual", 1000, store.FmtTime(now)},
		{1, "pending", "manual", 1000, nil},
	} {
		adminOrder(t, service, item.userID, productID, item.status, item.provider, item.amount, item.paidAt)
	}
	stamp := store.FmtTime(now)
	for _, item := range []struct{ kind, status string }{
		{"hosted", "active"}, {"rental", "active"}, {"hosted", "failed"}, {"rental", "failed"}, {"hosted", "released"},
	} {
		adminExec(t, service, `INSERT INTO resources(user_id,kind,status,created_at,updated_at) VALUES (2,?,?,?,?)`, item.kind, item.status, stamp, stamp)
	}
	for _, status := range []string{"available", "available", "leased", "maintenance"} {
		adminExec(t, service, `INSERT INTO rental_pool(vm_id,status,created_at,updated_at) VALUES (lower(hex(randomblob(8))),?,?,?)`, status, stamp, stamp)
	}
	result, err := service.Dashboard(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if result.Users != 3 || result.PayingUsers != 2 || result.RevenueCents != 1032 || result.OrdersPending != 1 ||
		result.Accounts != (api.AccountsCounts{HostedActive: 1, RentalActive: 1, Failed: 2}) ||
		result.RentalPool != (api.RentalCounts{Total: 4, Available: 2, Leased: 1, Maintenance: 1}) {
		t.Fatalf("dashboard = %+v", result)
	}
	client.pingErr = fmt.Errorf("connection failed: %w", vm2api.ErrUnavailable)
	result, err = service.Dashboard(ctx)
	if err != nil || result.VM2API.OK || result.VM2API.Error != "连接上游服务失败" || result.RevenueCents != 1032 {
		t.Fatalf("degraded dashboard = %+v, error = %v", result, err)
	}
}

func TestAdminRentalPoolAllocationAndConflicts(t *testing.T) {
	service, client := adminFixture(t)
	ctx := context.Background()
	reset := service.now().In(time.FixedZone("local", 8*3600))
	for _, vm := range []vm2api.VM{
		{ID: "first", Label: "平台账号", Tier: "max", EmailMasked: "a***@example.com", CredStatus: "valid", FiveHour: &vm2api.Window{Utilization: 42, ResetsAt: &reset, Status: "ok"}},
		{ID: "second", Tier: "pro"}, {ID: "third"},
		{ID: "owned", OwnerUserID: "someone"}, {ID: "openai", Platform: "openai"},
		{ID: "user-created", Origin: "user_created"}, {ID: "local-owned"},
	} {
		client.AddVM(vm)
	}
	stamp := store.FmtTime(service.now())
	adminExec(t, service, `INSERT INTO resources(user_id,kind,vm_id,status,created_at,updated_at)
		VALUES (2,'hosted','local-owned','awaiting_auth',?,?)`, stamp, stamp)
	candidates, err := service.RentalCandidates(ctx)
	if err != nil || len(candidates) != 3 || candidates[0].VMID != "first" || candidates[1].VMID != "second" || candidates[2].VMID != "third" {
		t.Fatalf("candidates = %+v, error = %v", candidates, err)
	}
	for _, vmID := range []string{"owned", "openai", "user-created", "local-owned"} {
		adminWantError(t, service.RentalAdd(ctx, []string{"first", vmID}, "拒绝"), 409, "vm_not_available")
		if count := adminCount(t, service, `SELECT COUNT(*) FROM rental_pool`); count != 0 {
			t.Fatalf("partial batch persisted for %s", vmID)
		}
	}
	adminWantError(t, service.RentalAdd(ctx, []string{"first", "missing"}, ""), 404, "vm_not_found")
	adminWantError(t, service.RentalAdd(ctx, nil, ""), 422, "invalid_vm_ids")
	adminWantError(t, service.RentalAdd(ctx, []string{"first", "first"}, ""), 422, "invalid_vm_ids")
	if err := service.RentalAdd(ctx, []string{"first", "second"}, "平台库存"); err != nil {
		t.Fatal(err)
	}
	adminWantError(t, service.RentalAdd(ctx, []string{"third", "first"}, "重复"), 409, "vm_not_available")
	if count := adminCount(t, service, `SELECT COUNT(*) FROM rental_pool WHERE vm_id='third'`); count != 0 {
		t.Fatal("batch insert was not rolled back")
	}
	if err := service.RentalPatch(ctx, "second", "maintenance", "维护"); err != nil {
		t.Fatal(err)
	}
	if err := service.RentalPatch(ctx, "second", "available", "恢复"); err != nil {
		t.Fatal(err)
	}
	adminWantError(t, service.RentalPatch(ctx, "second", "leased", ""), 422, "invalid_status")
	productID := adminProduct(t, service, "rental_account")
	orderID := adminOrder(t, service, 2, productID, "paid", "manual", 200, stamp)
	if err := service.provisioning.FulfilOrder(ctx, orderID); err != nil {
		t.Fatal(err)
	}
	pool, err := service.RentalPool(ctx)
	if err != nil || len(pool) != 2 {
		t.Fatalf("pool = %+v, error = %v", pool, err)
	}
	for _, item := range pool {
		if item.Status != "leased" || item.LeasedToUserID == nil || *item.LeasedToUserID != 2 || item.LeasedToEmail != "tenant@example.com" || item.VM == nil {
			t.Fatalf("leased pool row = %+v", item)
		}
		adminWantError(t, service.RentalPatch(ctx, item.VMID, "maintenance", ""), 409, "vm_leased")
		adminWantError(t, service.RentalRemove(ctx, item.VMID), 409, "vm_leased")
	}
	if pool[0].VM.FiveHour == nil || pool[0].VM.FiveHour.Utilization != 42 || pool[0].VM.FiveHour.ResetsAt != stamp || pool[0].Note != "平台库存" {
		t.Fatalf("VM metadata = %+v", pool[0])
	}
	if err := service.RentalAdd(ctx, []string{"third"}, "移除测试"); err != nil {
		t.Fatal(err)
	}
	if err := service.RentalRemove(ctx, "third"); err != nil {
		t.Fatal(err)
	}
	adminWantError(t, service.RentalRemove(ctx, "third"), 404, "pool_vm_not_found")
	adminWantError(t, service.RentalPatch(ctx, "missing", "available", ""), 404, "pool_vm_not_found")
}

func TestAdminRentalAddRequiresPassedSelfchecks(t *testing.T) {
	for _, failedVM := range []string{"first", "second"} {
		t.Run(failedVM, func(t *testing.T) {
			service, client := adminFixture(t)
			ctx := context.Background()
			for _, vmID := range []string{"first", "second"} {
				client.AddVM(vm2api.VM{ID: vmID, Tier: "pro"})
			}
			checker := &adminChecker{now: service.now}
			checker.check = func(_ context.Context, vmID string, _ selfcheck.Options) (selfcheck.Result, error) {
				if count := adminCount(t, service, `SELECT COUNT(*) FROM rental_pool`); count != 0 {
					t.Fatalf("admission exposed %d unchecked pool rows", count)
				}
				result := selfcheck.Result{VMID: vmID, Passed: vmID != failedVM, CheckedAt: service.now()}
				if !result.Passed {
					result.Checks = []selfcheck.Check{
						{Code: selfcheck.ProxyExclusive, OK: true},
						{Code: selfcheck.ExitIPUnique, Remedy: selfcheck.RemedyReproxy, Message: "出口 IP 已被其它槽位使用"},
						{Code: selfcheck.FingerprintUnique, Remedy: selfcheck.RemedyReset, Message: "设备指纹重复"},
						{Code: selfcheck.ExitGeoTimezone, Skipped: true},
					}
				}
				return result, nil
			}
			service.provisioning.SetChecker(checker)
			err := service.RentalAdd(ctx, []string{"first", "second"}, "禁止入池")
			if !errors.Is(err, provision.ErrSelfcheckFailed) || !strings.Contains(err.Error(), failedVM) ||
				!strings.Contains(err.Error(), string(selfcheck.ExitIPUnique)) || !strings.Contains(err.Error(), string(selfcheck.FingerprintUnique)) {
				t.Fatalf("admission lost self-check failure or codes: %v", err)
			}
			if strings.Contains(err.Error(), string(selfcheck.ProxyExclusive)) || strings.Contains(err.Error(), string(selfcheck.ExitGeoTimezone)) {
				t.Fatalf("passed or skipped check reported as a failure: %v", err)
			}
			if count := adminCount(t, service, `SELECT COUNT(*) FROM rental_pool`); count != 0 {
				t.Fatalf("failed admission persisted %d pool rows", count)
			}
			wantCalls := []string{"first"}
			if failedVM == "second" {
				wantCalls = append(wantCalls, "second")
			}
			if !slices.Equal(checker.vmIDs, wantCalls) || len(checker.records) != len(wantCalls) {
				t.Fatalf("checks=%v records=%d; want checks %v", checker.vmIDs, len(checker.records), wantCalls)
			}
		})
	}
}

func TestAdminRentalAddPersistsEachPassedVerdict(t *testing.T) {
	service, client := adminFixture(t)
	ctx := context.Background()
	checked := map[string]time.Time{
		"first":  service.now().Add(-2 * time.Minute),
		"second": service.now().Add(-time.Minute),
	}
	for vmID := range checked {
		client.AddVM(vm2api.VM{ID: vmID, Tier: "max"})
	}
	checker := &adminChecker{now: service.now}
	checker.check = func(_ context.Context, vmID string, _ selfcheck.Options) (selfcheck.Result, error) {
		if count := adminCount(t, service, `SELECT COUNT(*) FROM rental_pool`); count != 0 {
			t.Fatalf("admission inserted %d rows before all checks passed", count)
		}
		return selfcheck.Result{VMID: vmID, Passed: true, CheckedAt: checked[vmID].In(time.FixedZone("local", 8*3600))}, nil
	}
	service.provisioning.SetChecker(checker)
	if err := service.RentalAdd(ctx, []string{"first", "second"}, "已通过自检"); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(checker.vmIDs, []string{"first", "second"}) || len(checker.records) != 2 {
		t.Fatalf("checks=%v records=%d", checker.vmIDs, len(checker.records))
	}
	for _, options := range checker.options {
		if !options.CheckAccount {
			t.Fatal("rental admission did not check for duplicate accounts")
		}
	}
	for vmID, when := range checked {
		if count := adminCount(t, service, `SELECT COUNT(*) FROM rental_pool WHERE vm_id=? AND status='available'
			AND tier='max' AND selfcheck_state='pass' AND selfcheck_checked_at=? AND note='已通过自检'`, vmID, store.FmtTime(when)); count != 1 {
			t.Fatalf("passed verdict not stored for %s", vmID)
		}
	}
	pool, err := service.RentalPool(ctx)
	if err != nil || len(pool) != 2 {
		t.Fatalf("pool=%+v err=%v", pool, err)
	}
	for _, item := range pool {
		if item.Selfcheck == nil || item.Selfcheck.State != "pass" || item.Selfcheck.CheckedAt == nil ||
			*item.Selfcheck.CheckedAt != store.FmtTime(checked[item.VMID]) {
			t.Fatalf("pool self-check=%+v for %s", item.Selfcheck, item.VMID)
		}
	}
}

func TestAdminRentalAddPreservesCheckerErrors(t *testing.T) {
	for _, cause := range []error{vm2api.ErrUnavailable, context.Canceled} {
		t.Run(cause.Error(), func(t *testing.T) {
			service, client := adminFixture(t)
			client.AddVM(vm2api.VM{ID: "first"})
			client.AddVM(vm2api.VM{ID: "second"})
			checker := &adminChecker{now: service.now}
			checker.check = func(_ context.Context, vmID string, _ selfcheck.Options) (selfcheck.Result, error) {
				if vmID == "second" {
					return selfcheck.Result{}, cause
				}
				return selfcheck.Result{VMID: vmID, Passed: true, CheckedAt: service.now()}, nil
			}
			service.provisioning.SetChecker(checker)
			err := service.RentalAdd(context.Background(), []string{"first", "second"}, "")
			if !errors.Is(err, cause) || !errors.Is(err, provision.ErrSelfcheckFailed) {
				t.Fatalf("admission lost checker error: %v", err)
			}
			if count := adminCount(t, service, `SELECT COUNT(*) FROM rental_pool`); count != 0 {
				t.Fatalf("checker error persisted %d pool rows", count)
			}
		})
	}
}

func TestAdminRentalPoolMapsStoredSelfcheck(t *testing.T) {
	service, _ := adminFixture(t)
	ctx := context.Background()
	pool, err := service.RentalPool(ctx)
	if err != nil || pool == nil || len(pool) != 0 {
		t.Fatalf("empty pool=%+v err=%v", pool, err)
	}
	stamp := store.FmtTime(service.now())
	for _, state := range []string{"pending", "pass", "fail", "exhausted"} {
		var checkedAt any
		if state != "pending" {
			checkedAt = stamp
		}
		adminExec(t, service, `INSERT INTO rental_pool(vm_id,status,selfcheck_state,selfcheck_checked_at,created_at,updated_at)
			VALUES (?,'available',?,?,?,?)`, state, state, checkedAt, stamp, stamp)
	}
	pool, err = service.RentalPool(ctx)
	if err != nil || len(pool) != 4 {
		t.Fatalf("pool=%+v err=%v", pool, err)
	}
	for _, item := range pool {
		if item.Selfcheck == nil || item.Selfcheck.State != item.VMID || item.VM != nil {
			t.Fatalf("stored self-check not mapped without upstream VM: %+v", item)
		}
		if item.VMID == "pending" {
			if item.Selfcheck.CheckedAt != nil {
				t.Fatalf("pending checked_at=%v", item.Selfcheck.CheckedAt)
			}
		} else if item.Selfcheck.CheckedAt == nil || *item.Selfcheck.CheckedAt != stamp {
			t.Fatalf("checked_at not mapped: %+v", item.Selfcheck)
		}
	}
}

func TestAdminRentalGrantStockRequiresPassedSelfcheck(t *testing.T) {
	for _, state := range []string{"pending", "fail", "exhausted", "running", ""} {
		t.Run(state, func(t *testing.T) {
			service, client := adminFixture(t)
			ctx := context.Background()
			adminPoolVM(t, service, client, "unchecked")
			adminExec(t, service, `UPDATE rental_pool SET selfcheck_state=? WHERE vm_id='unchecked'`, state)
			adminWantError(t, service.GrantSubscription(ctx, 1, 2, "rental_account", 1, 30, "自检未通过"), 409, "out_of_stock")
			for _, table := range []string{"products", "orders", "subscriptions", "resources", "jobs", "audit_log"} {
				if count := adminCount(t, service, `SELECT COUNT(*) FROM `+table); count != 0 {
					t.Fatalf("grant with unchecked stock persisted %d rows in %s", count, table)
				}
			}
			adminPoolVM(t, service, client, "passed")
			adminWantError(t, service.GrantSubscription(ctx, 1, 2, "rental_account", 2, 30, "库存不足"), 409, "out_of_stock")
			adminExec(t, service, `UPDATE rental_pool SET selfcheck_state='pass',status='maintenance' WHERE vm_id='unchecked'`)
			if stock, err := service.rentalStock(ctx, "rental_account", 1); err != nil || !stock {
				t.Fatalf("passed stock=%t err=%v", stock, err)
			}
			if err := service.GrantSubscription(ctx, 1, 2, "rental_account", 1, 30, "自检通过"); err != nil {
				t.Fatal(err)
			}
			if count := adminCount(t, service, `SELECT COUNT(*) FROM resources WHERE vm_id='passed' AND status='active'`); count != 1 {
				t.Fatal("grant did not allocate the passed VM")
			}
		})
	}
}

func TestAdminJobsPaginationAndRetry(t *testing.T) {
	service, _ := adminFixture(t)
	ctx := context.Background()
	stamp := store.FmtTime(service.now())
	ids := make([]int64, 0)
	for _, status := range []string{"failed", "failed", "pending", "running", "succeeded"} {
		ids = append(ids, adminExec(t, service, `INSERT INTO jobs(type,payload,status,attempts,run_after,locked_until,last_error,created_at,updated_at)
			VALUES ('sync_concurrency','{"user_id":2}',?,8,?,?,'旧错误',?,?)`, status, stamp, stamp, stamp, stamp))
	}
	items, total, err := service.Jobs(ctx, "failed", 2, 1)
	if err != nil || total != 2 || len(items) != 1 || items[0].ID != ids[0] || items[0].Attempts != 8 || items[0].Payload != `{"user_id":2}` {
		t.Fatalf("jobs = %+v, total = %d, error = %v", items, total, err)
	}
	items, total, err = service.Jobs(ctx, "", 0, 0)
	if err != nil || total != 5 || len(items) != 5 || items[0].ID != ids[4] {
		t.Fatalf("all jobs = %+v, total = %d, error = %v", items, total, err)
	}
	if err := service.RetryJob(ctx, ids[0]); err != nil {
		t.Fatal(err)
	}
	if count := adminCount(t, service, `SELECT COUNT(*) FROM jobs WHERE id=? AND status='pending' AND attempts=0
		AND locked_until IS NULL AND last_error IS NULL AND run_after=? AND updated_at=?`, ids[0], stamp, stamp); count != 1 {
		t.Fatal("retry did not reset job state")
	}
	for _, id := range ids[2:] {
		adminWantError(t, service.RetryJob(ctx, id), 409, "job_not_retryable")
	}
	adminExec(t, service, `UPDATE jobs SET dedupe_key='same' WHERE id IN (?,?)`, ids[1], ids[2])
	adminWantError(t, service.RetryJob(ctx, ids[1]), 409, "job_not_retryable")
	adminWantError(t, service.RetryJob(ctx, 999), 404, "job_not_found")
	_, _, err = service.Jobs(ctx, "invalid", 1, 20)
	adminWantError(t, err, 422, "invalid_status")
}

func TestAdminGrantAndDeferredEntitlementSync(t *testing.T) {
	service, _ := adminFixture(t)
	ctx := context.Background()
	key, _, err := service.Create(ctx, 2, "赠送同步")
	if err != nil {
		t.Fatal(err)
	}
	before := adminCount(t, service, `SELECT synced_concurrency FROM api_keys WHERE id=?`, key.ID)
	if err := service.GrantSubscription(ctx, 1, 2, "concurrency", 3, 30, "人工补偿"); err != nil {
		t.Fatal(err)
	}
	var subscriptionID, productID, actorID int64
	var note, starts, expires, detailRaw string
	err = service.db.QueryRow(`SELECT sub.id,product.id,sub.note,sub.starts_at,sub.expires_at,audit.actor_id,audit.detail
		FROM subscriptions AS sub JOIN orders AS purchase ON purchase.id=sub.source_order_id
		JOIN products AS product ON product.id=purchase.product_id
		JOIN audit_log AS audit ON audit.target=CAST(sub.id AS TEXT) AND audit.action='subscription.grant'
		WHERE purchase.provider='grant' AND purchase.status='fulfilled' AND purchase.amount_cents=0
		AND purchase.quantity=3 AND purchase.units=3 AND purchase.paid_at IS NOT NULL AND purchase.fulfilled_at IS NOT NULL
		AND sub.status='active' AND sub.quantity=3 AND product.enabled=0`).Scan(
		&subscriptionID, &productID, &note, &starts, &expires, &actorID, &detailRaw)
	if err != nil {
		t.Fatal(err)
	}
	var detail map[string]any
	if err := json.Unmarshal([]byte(detailRaw), &detail); err != nil {
		t.Fatal(err)
	}
	if productID <= 0 || actorID != 1 || note != "人工补偿" || detail["note"] != note || detail["user_id"] != float64(2) ||
		starts != store.FmtTime(service.now()) || expires != store.FmtTime(service.now().AddDate(0, 0, 30)) {
		t.Fatalf("grant = product %d, actor %d, note %q, starts %s, expires %s, audit %v", productID, actorID, note, starts, expires, detail)
	}
	if count := adminCount(t, service, `SELECT COUNT(*) FROM jobs WHERE type='fulfil_order'`); count != 0 {
		t.Fatal("grant scheduled duplicate fulfilment")
	}
	adminDrainJobs(t, service)
	if got := adminCount(t, service, `SELECT synced_concurrency FROM api_keys WHERE id=?`, key.ID); got != before+3 {
		t.Fatalf("synced concurrency = %d; want %d", got, before+3)
	}
	if err := service.GrantSubscription(ctx, 1, 2, "concurrency", 2, 30, "再次赠送"); err != nil {
		t.Fatal(err)
	}
	if count := adminCount(t, service, `SELECT COUNT(*) FROM products`); count != 1 {
		t.Fatalf("grant products = %d; want reusable product", count)
	}
	adminDrainJobs(t, service)
	if count := adminCount(t, service, `SELECT COUNT(*) FROM subscriptions`); count != 2 {
		t.Fatal("grant was fulfilled twice")
	}
	if err := service.PatchSubscription(ctx, subscriptionID, nil, "cancelled"); err != nil {
		t.Fatal(err)
	}
	adminDrainJobs(t, service)
	if got := adminCount(t, service, `SELECT synced_concurrency FROM api_keys WHERE id=?`, key.ID); got != before+2 {
		t.Fatalf("cancelled concurrency = %d; want %d", got, before+2)
	}
}

func TestAdminPatchSubscriptionExpiryAndValidation(t *testing.T) {
	service, _ := adminFixture(t)
	ctx := context.Background()
	if err := service.GrantSubscription(ctx, 1, 2, "hosted_seat", 1, 30, "赠送席位"); err != nil {
		t.Fatal(err)
	}
	id := adminCount(t, service, `SELECT id FROM subscriptions`)
	for _, expiry := range []string{"", "2026-10-31", "不是时间", "2026-02-30T00:00:00Z"} {
		adminWantError(t, service.PatchSubscription(ctx, id, &expiry, ""), 422, "invalid_expires_at")
	}
	adminWantError(t, service.PatchSubscription(ctx, id, nil, "active"), 422, "invalid_status")
	adminWantError(t, service.PatchSubscription(ctx, id, nil, ""), 422, "invalid_body")
	adminWantError(t, service.PatchSubscription(ctx, 999, nil, "cancelled"), 404, "subscription_not_found")
	boundary := store.FmtTime(service.now())
	if err := service.PatchSubscription(ctx, id, &boundary, ""); err != nil {
		t.Fatal(err)
	}
	if count := adminCount(t, service, `SELECT COUNT(*) FROM subscriptions WHERE id=? AND status='expired'`, id); count != 1 {
		t.Fatal("subscription did not expire at boundary")
	}
	future := service.now().AddDate(0, 0, 60)
	offset := future.In(time.FixedZone("local", 8*3600)).Format(time.RFC3339)
	if err := service.PatchSubscription(ctx, id, &offset, ""); err != nil {
		t.Fatal(err)
	}
	if count := adminCount(t, service, `SELECT COUNT(*) FROM subscriptions WHERE id=? AND status='active' AND expires_at=?`, id, store.FmtTime(future)); count != 1 {
		t.Fatal("extended subscription was not normalized and reactivated")
	}
	if err := service.PatchSubscription(ctx, id, nil, "cancelled"); err != nil {
		t.Fatal(err)
	}
	if err := service.PatchSubscription(ctx, id, &offset, ""); err != nil {
		t.Fatal(err)
	}
	if count := adminCount(t, service, `SELECT COUNT(*) FROM subscriptions WHERE id=? AND status='cancelled'`, id); count != 1 {
		t.Fatal("cancelled subscription was reactivated")
	}
	adminWantError(t, service.GrantSubscription(ctx, 1, 999, "concurrency", 1, 30, ""), 404, "user_not_found")
	if err := service.GrantSubscription(ctx, 1, 999, "concurrency", 1, 30, ""); !errors.Is(err, ErrUserNotFound) {
		t.Fatalf("missing user error = %v", err)
	}
	adminWantError(t, service.GrantSubscription(ctx, 1, 2, "invalid", 1, 30, ""), 422, "invalid_kind")
	for _, quantity := range []int{0, -1} {
		adminWantError(t, service.GrantSubscription(ctx, 1, 2, "concurrency", quantity, 30, ""), 422, "invalid_quantity")
	}
	for _, duration := range []int{0, -1, int(^uint(0) >> 1)} {
		adminWantError(t, service.GrantSubscription(ctx, 1, 2, "concurrency", 1, duration, ""), 422, "invalid_quantity")
	}
}

func TestAdminTransactionsRollback(t *testing.T) {
	service, client := adminFixture(t)
	ctx := context.Background()
	adminExec(t, service, `CREATE TRIGGER admin_fail_audit BEFORE INSERT ON audit_log BEGIN SELECT RAISE(ABORT,'audit unavailable'); END`)
	if err := service.GrantSubscription(ctx, 1, 2, "concurrency", 1, 30, "回滚"); err == nil {
		t.Fatal("grant unexpectedly succeeded")
	}
	for _, table := range []string{"products", "orders", "subscriptions", "jobs", "audit_log"} {
		if count := adminCount(t, service, `SELECT COUNT(*) FROM `+table); count != 0 {
			t.Fatalf("grant rollback left %d rows in %s", count, table)
		}
	}
	adminExec(t, service, `DROP TRIGGER admin_fail_audit`)
	adminPoolVM(t, service, client, "grant-rental")
	if err := service.GrantSubscription(ctx, 1, 2, "rental_account", 1, 30, "原始赠送"); err != nil {
		t.Fatal(err)
	}
	id := adminCount(t, service, `SELECT id FROM subscriptions`)
	jobs := adminCount(t, service, `SELECT COUNT(*) FROM jobs`)
	adminExec(t, service, `CREATE TRIGGER admin_fail_audit BEFORE INSERT ON audit_log BEGIN SELECT RAISE(ABORT,'audit unavailable'); END`)
	if err := service.PatchSubscription(ctx, id, nil, "cancelled"); err == nil {
		t.Fatal("patch unexpectedly succeeded")
	}
	if count := adminCount(t, service, `SELECT COUNT(*) FROM subscriptions WHERE id=? AND status='active'`, id); count != 1 {
		t.Fatal("subscription change was not rolled back")
	}
	if count := adminCount(t, service, `SELECT COUNT(*) FROM jobs`); count != jobs {
		t.Fatal("subscription rollback left synchronization jobs")
	}
}

func TestAdminPreservesUpstreamAndDatabaseErrors(t *testing.T) {
	service, client := adminFixture(t)
	ctx := context.Background()
	cause := &vm2api.Error{Status: 503, Code: "offline", Message: "private upstream detail"}
	client.listErr = cause
	_, err := service.RentalCandidates(ctx)
	if !errors.Is(err, cause) {
		t.Fatalf("candidate error lost cause: %v", err)
	}
	if err := service.RentalAdd(ctx, []string{"first"}, ""); !errors.Is(err, cause) {
		t.Fatalf("add error lost cause: %v", err)
	}
	stamp := store.FmtTime(service.now())
	adminExec(t, service, `INSERT INTO rental_pool(vm_id,status,created_at,updated_at) VALUES ('first','available',?,?)`, stamp, stamp)
	_, err = service.RentalPool(ctx)
	var upstream *vm2api.Error
	if !errors.As(err, &upstream) || upstream != cause {
		t.Fatalf("pool error lost cause: %v", err)
	}
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	_, err = service.Dashboard(cancelled)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("database error lost context cancellation: %v", err)
	}
	if err := service.db.Close(); err != nil {
		t.Fatal(err)
	}
	_, _, err = service.Jobs(ctx, "", 1, 20)
	if err == nil || errors.Unwrap(err) == nil || errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("closed database error was not wrapped: %v", err)
	}
}

func TestAdminGrantRentalAllocatesAccount(t *testing.T) {
	service, client := adminFixture(t)
	ctx := context.Background()
	adminWantError(t, service.GrantSubscription(ctx, 1, 2, "rental_account", 1, 30, "无库存"), 409, "out_of_stock")
	if count := adminCount(t, service, `SELECT COUNT(*) FROM subscriptions`); count != 0 {
		t.Fatalf("grant without stock created %d subscriptions", count)
	}
	adminPoolVM(t, service, client, "grant-vm")
	if err := service.GrantSubscription(ctx, 1, 2, "rental_account", 1, 30, "赠送一个号"); err != nil {
		t.Fatal(err)
	}
	var subscriptionID, resourceID int64
	var status string
	if err := service.db.QueryRow(`SELECT subscription_id, id, status FROM resources WHERE user_id=2 AND kind='rental'`).Scan(&subscriptionID, &resourceID, &status); err != nil {
		t.Fatalf("grant allocated no account: %v", err)
	}
	if status != "active" || subscriptionID == 0 {
		t.Fatalf("resource=%d status=%s subscription=%d", resourceID, status, subscriptionID)
	}
	if count := adminCount(t, service, `SELECT COUNT(*) FROM rental_pool WHERE status='leased' AND leased_resource_id=?`, resourceID); count != 1 {
		t.Fatal("rental pool row was not leased")
	}
	usage, err := client.GetAccountUsage(ctx, "grant-vm")
	if err != nil {
		t.Fatal(err)
	}
	if usage.VM.OwnerUserID == "" || usage.VM.ScheduleState != "on" {
		t.Fatalf("upstream owner=%q schedule=%s", usage.VM.OwnerUserID, usage.VM.ScheduleState)
	}
}

func TestAdminPatchRentalSubscriptionReallocates(t *testing.T) {
	service, client := adminFixture(t)
	ctx := context.Background()
	adminPoolVM(t, service, client, "patch-rental")
	if err := service.GrantSubscription(ctx, 1, 2, "rental_account", 1, 30, "赠送"); err != nil {
		t.Fatal(err)
	}
	var subscriptionID, resourceID int64
	if err := service.db.QueryRow(`SELECT id FROM subscriptions`).Scan(&subscriptionID); err != nil {
		t.Fatal(err)
	}
	if err := service.db.QueryRow(`SELECT id FROM resources WHERE subscription_id=?`, subscriptionID).Scan(&resourceID); err != nil {
		t.Fatal(err)
	}
	// Releasing the account while the subscription stays alive models the "号
	// 已释放" state; extending the subscription must bring an account back.
	if err := service.provisioning.ReleaseResource(ctx, resourceID); err != nil {
		t.Fatal(err)
	}
	adminPoolVM(t, service, client, "patch-rental-2")
	future := store.FmtTime(service.now().AddDate(0, 0, 60))
	if err := service.PatchSubscription(ctx, subscriptionID, &future, ""); err != nil {
		t.Fatal(err)
	}
	if count := adminCount(t, service, `SELECT COUNT(*) FROM resources WHERE subscription_id=? AND status='active'`, subscriptionID); count != 1 {
		t.Fatalf("active rental accounts=%d after extension", count)
	}
}
