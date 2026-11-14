package provision

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/DueGin/seat-hub/internal/selfcheck"
	"github.com/DueGin/seat-hub/internal/store"
	"github.com/DueGin/seat-hub/internal/vm2api"
)

type sweepCheckCall struct {
	vmID string
	opts selfcheck.Options
}

type sweepRecordCall struct {
	resourceID int64
	attempt    int
	result     selfcheck.Result
}

type sweepStubChecker struct {
	db           *sql.DB
	results      map[string]selfcheck.Result
	checkErrors  map[string]error
	recordErrors map[string]error
	checks       []sweepCheckCall
	records      []sweepRecordCall
}

func (checker *sweepStubChecker) Check(ctx context.Context, vmID string, opts selfcheck.Options) (selfcheck.Result, error) {
	checker.checks = append(checker.checks, sweepCheckCall{vmID: vmID, opts: opts})
	// A database round trip detects rows left open on the single connection.
	var count int
	if err := checker.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM slot_checks`).Scan(&count); err != nil {
		return selfcheck.Result{}, fmt.Errorf("read check history in stub: %w", err)
	}
	if err := checker.checkErrors[vmID]; err != nil {
		return selfcheck.Result{}, err
	}
	result, ok := checker.results[vmID]
	if !ok {
		return selfcheck.Result{}, fmt.Errorf("unexpected self-check for vm %s", vmID)
	}
	return result, nil
}

func (checker *sweepStubChecker) Record(ctx context.Context, resourceID int64, attempt int, result selfcheck.Result) error {
	checker.records = append(checker.records, sweepRecordCall{resourceID: resourceID, attempt: attempt, result: result})
	if err := checker.recordErrors[result.VMID]; err != nil {
		return err
	}
	checks, err := json.Marshal(result.Checks)
	if err != nil {
		return fmt.Errorf("encode stub checks: %w", err)
	}
	var resource any
	if resourceID != 0 {
		resource = resourceID
	}
	_, err = checker.db.ExecContext(ctx, `INSERT INTO slot_checks(resource_id,vm_id,attempt,passed,checks,created_at) VALUES (?,?,?,?,?,?)`, resource, result.VMID, attempt, result.Passed, string(checks), timestamp(result.CheckedAt))
	if err != nil {
		return fmt.Errorf("save stub check: %w", err)
	}
	return nil
}

type sweepMutationClient struct {
	vm2api.Client
	resets, allocations, creations int
}

func (client *sweepMutationClient) ResetVM(context.Context, string) error {
	client.resets++
	return errors.New("unexpected vm reset")
}

func (client *sweepMutationClient) AllocateProxy(context.Context, string) error {
	client.allocations++
	return errors.New("unexpected proxy allocation")
}

func (client *sweepMutationClient) CreateHostedVM(context.Context, string, json.RawMessage) (vm2api.VM, error) {
	client.creations++
	return vm2api.VM{}, errors.New("unexpected vm creation")
}

func sweepFixture(t *testing.T) (*Service, *sweepStubChecker) {
	t.Helper()
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { db.Close() })
	if err := (&store.DB{DB: db}).Migrate(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`PRAGMA foreign_keys=ON`); err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, time.October, 31, 3, 4, 5, 0, time.UTC)
	if _, err := db.Exec(`INSERT INTO users(id,email,password_hash,created_at,updated_at) VALUES (1,'sweep@example.com','hash',?,?)`, timestamp(now), timestamp(now)); err != nil {
		t.Fatal(err)
	}
	client := &sweepMutationClient{}
	checker := &sweepStubChecker{
		db: db, results: make(map[string]selfcheck.Result),
		checkErrors: make(map[string]error), recordErrors: make(map[string]error),
	}
	// Avoid New and the real self-check Engine so this suite stays independent.
	service := &Service{db: db, client: client, checker: checker, Now: func() time.Time { return now }}
	t.Cleanup(func() {
		if client.resets != 0 || client.allocations != 0 || client.creations != 0 {
			t.Errorf("sweep changed vms: resets=%d allocations=%d creations=%d", client.resets, client.allocations, client.creations)
		}
	})
	return service, checker
}

func sweepAddResource(t *testing.T, service *Service, status string, vmID any, attempt int) int64 {
	t.Helper()
	result, err := service.db.Exec(`INSERT INTO resources(user_id,kind,vm_id,status,selfcheck_attempts,created_at,updated_at) VALUES (1,'hosted',?,?,?,?,?)`, vmID, status, attempt, timestamp(service.now()), timestamp(service.now()))
	if err != nil {
		t.Fatal(err)
	}
	id, err := result.LastInsertId()
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func sweepAddPool(t *testing.T, service *Service, vmID, status string) {
	t.Helper()
	if _, err := service.db.Exec(`INSERT INTO rental_pool(vm_id,status,created_at,updated_at) VALUES (?,?,?,?)`, vmID, status, timestamp(service.now()), timestamp(service.now())); err != nil {
		t.Fatal(err)
	}
}

func sweepResult(vmID string, passed bool, checkedAt time.Time) selfcheck.Result {
	check := selfcheck.Check{Code: selfcheck.ExitIPUnique, OK: passed}
	if !passed {
		check.Remedy = selfcheck.RemedyReproxy
	}
	return selfcheck.Result{VMID: vmID, Passed: passed, Checks: []selfcheck.Check{check}, CheckedAt: checkedAt}
}

func TestSweepSelfcheckWithoutChecker(t *testing.T) {
	service := &Service{}
	if err := service.SweepSelfcheck(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestSweepSelfcheckTargetsAndState(t *testing.T) {
	service, checker := sweepFixture(t)
	checkedAt := service.now().In(time.FixedZone("test", 8*60*60))
	type resourceCase struct {
		status, vmID string
		id           int64
		attempt      int
		included     bool
		passed       bool
	}
	resources := []resourceCase{
		{status: "active", vmID: "active", included: true, attempt: 3},
		{status: "awaiting_auth", vmID: "awaiting", included: true, passed: true, attempt: 1},
		{status: "needs_reauth", vmID: "reauth", included: true, attempt: 2},
		{status: "suspended", vmID: "suspended", included: true, passed: true, attempt: 4},
		{status: "bootstrapping", vmID: "bootstrap"},
		{status: "provisioning", vmID: "provision"},
		{status: "allocating", vmID: "allocating"},
		{status: "failed", vmID: "failed"},
		{status: "releasing", vmID: "releasing"},
		{status: "released", vmID: "released"},
		{status: "active", vmID: ""},
	}
	expectedRecords := make(map[string]sweepRecordCall)
	for index := range resources {
		item := &resources[index]
		item.id = sweepAddResource(t, service, item.status, item.vmID, item.attempt)
		if item.included {
			checker.results[item.vmID] = sweepResult(item.vmID, item.passed, checkedAt)
			expectedRecords[item.vmID] = sweepRecordCall{resourceID: item.id, attempt: item.attempt}
		}
	}
	nullVM := sweepAddResource(t, service, "active", nil, 0)
	pool := []struct {
		vmID, status string
		passed       bool
	}{
		{"pool-available-fail", "available", false},
		{"pool-available-pass", "available", true},
		{"pool-maintenance-fail", "maintenance", false},
		{"pool-maintenance-pass", "maintenance", true},
		{"pool-leased", "leased", false},
	}
	for _, item := range pool {
		sweepAddPool(t, service, item.vmID, item.status)
		if item.status != "leased" {
			checker.results[item.vmID] = sweepResult(item.vmID, item.passed, checkedAt)
			expectedRecords[item.vmID] = sweepRecordCall{}
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := service.SweepSelfcheck(ctx); err != nil {
		t.Fatal(err)
	}
	if len(checker.checks) != len(expectedRecords) || len(checker.records) != len(expectedRecords) {
		t.Fatalf("checks=%d records=%d want=%d", len(checker.checks), len(checker.records), len(expectedRecords))
	}
	for _, call := range checker.checks {
		want := selfcheck.Options{Rules: selfcheck.Rules{RequireDedicatedProxy: true, RequireTimezoneMatch: true}, CheckAccount: call.vmID == "active" || strings.HasPrefix(call.vmID, "pool-")}
		if call.opts != want {
			t.Errorf("vm %s options=%+v want=%+v", call.vmID, call.opts, want)
		}
	}
	for _, call := range checker.records {
		want, ok := expectedRecords[call.result.VMID]
		if !ok || call.resourceID != want.resourceID || call.attempt != want.attempt || !reflect.DeepEqual(call.result, checker.results[call.result.VMID]) {
			t.Errorf("unexpected record: %+v", call)
		}
		delete(expectedRecords, call.result.VMID)
	}
	if len(expectedRecords) != 0 {
		t.Errorf("missing records: %+v", expectedRecords)
	}
	for _, item := range resources {
		var status, state string
		var attempt int
		var savedAt, summary sql.NullString
		if err := service.db.QueryRow(`SELECT status,selfcheck_state,selfcheck_attempts,selfcheck_checked_at,selfcheck_summary FROM resources WHERE id=?`, item.id).Scan(&status, &state, &attempt, &savedAt, &summary); err != nil {
			t.Fatal(err)
		}
		wantState := "pending"
		if item.included {
			wantState = "fail"
			if item.passed {
				wantState = "pass"
			}
			var checks []selfcheck.Check
			if err := json.Unmarshal([]byte(summary.String), &checks); err != nil {
				t.Fatal(err)
			}
			if savedAt.String != timestamp(checkedAt) || !reflect.DeepEqual(checks, checker.results[item.vmID].Checks) {
				t.Errorf("vm %s checked_at=%v summary=%s", item.vmID, savedAt, summary.String)
			}
		} else if savedAt.Valid || summary.Valid {
			t.Errorf("excluded vm %s changed: checked_at=%v summary=%v", item.vmID, savedAt, summary)
		}
		if status != item.status || attempt != item.attempt || state != wantState {
			t.Errorf("vm %s status=%s attempts=%d state=%s; want status=%s attempts=%d state=%s", item.vmID, status, attempt, state, item.status, item.attempt, wantState)
		}
	}
	var nullState string
	if err := service.db.QueryRow(`SELECT selfcheck_state FROM resources WHERE id=?`, nullVM).Scan(&nullState); err != nil || nullState != "pending" {
		t.Fatalf("null vm state=%s err=%v", nullState, err)
	}
	for _, item := range pool {
		var status, state string
		var savedAt sql.NullString
		if err := service.db.QueryRow(`SELECT status,selfcheck_state,selfcheck_checked_at FROM rental_pool WHERE vm_id=?`, item.vmID).Scan(&status, &state, &savedAt); err != nil {
			t.Fatal(err)
		}
		wantState := "fail"
		if item.passed {
			wantState = "pass"
		}
		if item.status == "leased" {
			wantState = "pending"
			if savedAt.Valid {
				t.Errorf("leased pool vm changed: %+v", savedAt)
			}
		} else if savedAt.String != timestamp(checkedAt) {
			t.Errorf("pool vm %s checked_at=%v", item.vmID, savedAt)
		}
		if status != item.status || state != wantState {
			t.Errorf("pool vm %s status=%s state=%s; want status=%s state=%s", item.vmID, status, state, item.status, wantState)
		}
	}
	var jobs int
	if err := service.db.QueryRow(`SELECT COUNT(*) FROM jobs`).Scan(&jobs); err != nil || jobs != 0 {
		t.Errorf("sweep enqueued jobs=%d err=%v", jobs, err)
	}
}

func TestSweepSelfcheckContinuesAfterFailures(t *testing.T) {
	service, checker := sweepFixture(t)
	resourceCheckErr := errors.New("resource check unavailable")
	resourceRecordErr := errors.New("resource history unavailable")
	poolCheckErr := errors.New("pool check unavailable")
	poolRecordErr := errors.New("pool history unavailable")
	checker.checkErrors["resource-check"] = resourceCheckErr
	checker.recordErrors["resource-record"] = resourceRecordErr
	checker.checkErrors["pool-a-check"] = poolCheckErr
	checker.recordErrors["pool-b-record"] = poolRecordErr
	for _, suffix := range []string{"check", "record", "save", "success"} {
		vmID := "resource-" + suffix
		sweepAddResource(t, service, "active", vmID, 2)
		checker.results[vmID] = sweepResult(vmID, suffix == "success", service.now())
	}
	for _, suffix := range []string{"a-check", "b-record", "c-save", "z-success"} {
		vmID := "pool-" + suffix
		sweepAddPool(t, service, vmID, "available")
		checker.results[vmID] = sweepResult(vmID, suffix == "z-success", service.now())
	}
	for _, statement := range []string{
		`CREATE TRIGGER sweep_block_resource BEFORE UPDATE OF selfcheck_state ON resources WHEN NEW.vm_id='resource-save' BEGIN SELECT RAISE(FAIL,'resource sweep write blocked'); END`,
		`CREATE TRIGGER sweep_block_pool BEFORE UPDATE OF selfcheck_state ON rental_pool WHEN NEW.vm_id='pool-c-save' BEGIN SELECT RAISE(FAIL,'pool sweep write blocked'); END`,
	} {
		if _, err := service.db.Exec(statement); err != nil {
			t.Fatal(err)
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	err := service.SweepSelfcheck(ctx)
	for _, failure := range []error{resourceCheckErr, resourceRecordErr, poolCheckErr, poolRecordErr} {
		if !errors.Is(err, failure) {
			t.Errorf("sweep error=%v does not wrap %v", err, failure)
		}
	}
	for _, message := range []string{"resource sweep write blocked", "pool sweep write blocked"} {
		if err == nil || !strings.Contains(err.Error(), message) {
			t.Errorf("sweep error=%v does not include %s", err, message)
		}
	}
	if len(checker.checks) != 8 || len(checker.records) != 6 {
		t.Fatalf("checks=%d records=%d want checks=8 records=6", len(checker.checks), len(checker.records))
	}
	for _, target := range []struct{ table, vmID, state string }{
		{"resources", "resource-check", "pending"},
		{"resources", "resource-record", "fail"},
		{"resources", "resource-save", "pending"},
		{"resources", "resource-success", "pass"},
		{"rental_pool", "pool-a-check", "pending"},
		{"rental_pool", "pool-b-record", "fail"},
		{"rental_pool", "pool-c-save", "pending"},
		{"rental_pool", "pool-z-success", "pass"},
	} {
		var state string
		if err := service.db.QueryRow(`SELECT selfcheck_state FROM `+target.table+` WHERE vm_id=?`, target.vmID).Scan(&state); err != nil {
			t.Fatal(err)
		}
		if state != target.state {
			t.Errorf("vm %s state=%s want=%s", target.vmID, state, target.state)
		}
	}
	var history int
	if err := service.db.QueryRow(`SELECT COUNT(*) FROM slot_checks`).Scan(&history); err != nil || history != 4 {
		t.Errorf("history=%d want=4 err=%v", history, err)
	}
}

func TestSweepSelfcheckConfiguredRules(t *testing.T) {
	for _, rules := range []selfcheck.Rules{
		{},
		{RequireDedicatedProxy: true},
		{RequireTimezoneMatch: true},
	} {
		t.Run(fmt.Sprintf("proxy_%t_timezone_%t", rules.RequireDedicatedProxy, rules.RequireTimezoneMatch), func(t *testing.T) {
			service, checker := sweepFixture(t)
			raw, err := json.Marshal(map[string]bool{
				"selfcheck_require_dedicated_proxy": rules.RequireDedicatedProxy,
				"selfcheck_require_timezone_match":  rules.RequireTimezoneMatch,
			})
			if err != nil {
				t.Fatal(err)
			}
			if _, err := service.db.Exec(`INSERT INTO settings(key,value,updated_at) VALUES ('site',?,?)`, string(raw), timestamp(service.now())); err != nil {
				t.Fatal(err)
			}
			sweepAddResource(t, service, "awaiting_auth", "resource", 0)
			sweepAddPool(t, service, "pool", "maintenance")
			for _, vmID := range []string{"resource", "pool"} {
				checker.results[vmID] = sweepResult(vmID, true, service.now())
			}
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			if err := service.SweepSelfcheck(ctx); err != nil {
				t.Fatal(err)
			}
			if len(checker.checks) != 2 {
				t.Fatalf("checks=%d want=2", len(checker.checks))
			}
			for _, call := range checker.checks {
				if call.opts.Rules != rules {
					t.Errorf("vm %s rules=%+v want=%+v", call.vmID, call.opts.Rules, rules)
				}
			}
		})
	}
}

var _ selfcheck.Checker = (*sweepStubChecker)(nil)
