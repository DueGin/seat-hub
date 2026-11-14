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

	"github.com/DueGin/vm2api-portal/internal/selfcheck"
	"github.com/DueGin/vm2api-portal/internal/vm2api"
)

type slotCheckStep struct {
	result selfcheck.Result
	err    error
}

type slotCheckerStub struct {
	db          *sql.DB
	steps       []slotCheckStep
	options     []selfcheck.Options
	attempts    []int
	resourceIDs []int64
}

func (stub *slotCheckerStub) Check(ctx context.Context, vmID string, options selfcheck.Options) (selfcheck.Result, error) {
	stub.options = append(stub.options, options)
	if len(stub.steps) == 0 {
		return selfcheck.Result{}, errors.New("unexpected self-check")
	}
	step := stub.steps[0]
	stub.steps = stub.steps[1:]
	step.result.VMID = vmID
	return step.result, step.err
}

func (stub *slotCheckerStub) Record(ctx context.Context, resourceID int64, attempt int, result selfcheck.Result) error {
	stub.resourceIDs = append(stub.resourceIDs, resourceID)
	stub.attempts = append(stub.attempts, attempt)
	checks, err := json.Marshal(result.Checks)
	if err != nil {
		return err
	}
	var resource any
	if resourceID != 0 {
		resource = resourceID
	}
	_, err = stub.db.ExecContext(ctx, `INSERT INTO slot_checks(resource_id,vm_id,attempt,passed,checks,created_at) VALUES (?,?,?,?,?,?)`, resource, result.VMID, attempt, result.Passed, string(checks), timestamp(result.CheckedAt))
	return err
}

func failedSlotCheck(code selfcheck.Code, remedy selfcheck.Remedy) slotCheckStep {
	return slotCheckStep{result: selfcheck.Result{Checks: []selfcheck.Check{{Code: code, Remedy: remedy, Message: "测试检查未通过"}}}}
}

func passedSlotCheck() slotCheckStep {
	return slotCheckStep{result: selfcheck.Result{Passed: true, Checks: []selfcheck.Check{{Code: selfcheck.FingerprintUnique, OK: true, Message: "检查通过"}}}}
}

func installSlotChecker(service *Service, steps ...slotCheckStep) *slotCheckerStub {
	stub := &slotCheckerStub{db: service.db, steps: steps}
	service.SetChecker(stub)
	return stub
}

func hostedForSelfcheck(t *testing.T, service *Service, fake *vm2api.Fake, userID int64) Account {
	t.Helper()
	vm, err := fake.CreateHostedVM(context.Background(), "测试槽位", nil)
	if err != nil {
		t.Fatal(err)
	}
	id := addResource(t, service, userID, "hosted", "provisioning", vm.ID, nil)
	account, err := service.resource(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	return account
}

func TestSlotSelfcheckResetsThenPasses(t *testing.T) {
	service, fake, userID := provisionFixture(t)
	account := hostedForSelfcheck(t, service, fake, userID)
	stub := installSlotChecker(service, failedSlotCheck(selfcheck.FingerprintUnique, selfcheck.RemedyReset), passedSlotCheck())
	if err := service.ProvisionHosted(context.Background(), account.ID); err != nil {
		t.Fatal(err)
	}
	stored, err := service.resource(context.Background(), account.ID)
	if err != nil {
		t.Fatal(err)
	}
	if fake.ResetCalls(account.VMID) != 1 || stored.Status != "awaiting_auth" || stored.SelfcheckState != "pass" || stored.SelfcheckAttempts != 1 || stored.SelfcheckSummary == nil || stored.SelfcheckCheckedAt == nil {
		t.Fatalf("reset=%d account=%+v", fake.ResetCalls(account.VMID), stored)
	}
	if !reflect.DeepEqual(stub.attempts, []int{0, 1}) || stub.options[0].CheckAccount || !stub.options[0].Rules.RequireDedicatedProxy || !stub.options[0].Rules.RequireTimezoneMatch {
		t.Fatalf("attempts=%v options=%+v", stub.attempts, stub.options)
	}
	history, err := service.SelfcheckHistory(context.Background(), account.ID, 10)
	if err != nil || len(history) != 2 || history[0].Attempt != 1 || !history[0].Passed || history[1].Passed {
		t.Fatalf("history=%+v err=%v", history, err)
	}
}

func TestSlotSelfcheckExhaustionPersistsAcrossRetries(t *testing.T) {
	service, fake, userID := provisionFixture(t)
	account := hostedForSelfcheck(t, service, fake, userID)
	failure := failedSlotCheck(selfcheck.FingerprintUnique, selfcheck.RemedyReset)
	stub := installSlotChecker(service, failure, failure, failure, failure)
	ctx := context.Background()
	if err := service.ProvisionHosted(ctx, account.ID); !errors.Is(err, ErrSelfcheckExhausted) {
		t.Fatalf("error=%v", err)
	}
	stored, err := service.resource(ctx, account.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.Status != "failed" || stored.SelfcheckState != "exhausted" || stored.SelfcheckAttempts != 3 || stored.StatusReason == nil || *stored.StatusReason != "selfcheck_exhausted:fingerprint_unique" || stored.LastError == nil || !strings.Contains(*stored.LastError, "检查未通过") {
		t.Fatalf("account=%+v", stored)
	}
	for index := 0; index < 4; index++ {
		if err := service.ProvisionHosted(ctx, account.ID); !errors.Is(err, ErrSelfcheckExhausted) {
			t.Fatalf("repeat error=%v", err)
		}
		if err := service.RetryHosted(ctx, userID, account.ID); !errors.Is(err, ErrSelfcheckExhausted) {
			t.Fatalf("retry error=%v", err)
		}
	}
	if fake.ResetCalls(account.VMID) != 3 || len(stub.options) != 4 || !reflect.DeepEqual(stub.attempts, []int{0, 1, 2, 3}) {
		t.Fatalf("resets=%d options=%v attempts=%v", fake.ResetCalls(account.VMID), stub.options, stub.attempts)
	}
}

func TestSlotSelfcheckReproxy(t *testing.T) {
	for _, shared := range []bool{false, true} {
		t.Run(fmt.Sprint(shared), func(t *testing.T) {
			service, fake, userID := provisionFixture(t)
			account := hostedForSelfcheck(t, service, fake, userID)
			bad := vm2api.ProxyView{ID: "bad-proxy", Scheme: "socks5", Enabled: true, Status: "dead"}
			fake.SetProxy(account.VMID, &bad)
			if shared {
				peer, err := fake.CreateHostedVM(context.Background(), "peer", nil)
				if err != nil {
					t.Fatal(err)
				}
				fake.SetProxy(peer.ID, &bad)
			}
			fake.QueueProxies(vm2api.ProxyView{ID: "new-proxy", Scheme: "socks5", Status: "ok", Enabled: true})
			installSlotChecker(service, failedSlotCheck(selfcheck.ProxyHealthy, selfcheck.RemedyReproxy), passedSlotCheck())
			if err := service.ProvisionHosted(context.Background(), account.ID); err != nil {
				t.Fatal(err)
			}
			vms, err := fake.ListVMs(context.Background(), vm2api.VMFilter{})
			if err != nil {
				t.Fatal(err)
			}
			for _, vm := range vms {
				if vm.ID == account.VMID && (vm.Proxy == nil || vm.Proxy.ID != "new-proxy") {
					t.Fatalf("proxy=%+v", vm.Proxy)
				}
			}
			if fake.ProxyDisabled(bad.ID) == shared || fake.ResetCalls(account.VMID) != 0 {
				t.Fatalf("disabled=%v shared=%v resets=%d", fake.ProxyDisabled(bad.ID), shared, fake.ResetCalls(account.VMID))
			}
		})
	}
}

func TestSlotSelfcheckManualFailureBlocksOAuth(t *testing.T) {
	service, fake, userID := provisionFixture(t)
	account := hostedForSelfcheck(t, service, fake, userID)
	manual := failedSlotCheck(selfcheck.AccountUnique, selfcheck.RemedyManual)
	installSlotChecker(service, manual, manual, manual)
	ctx := context.Background()
	if err := service.ProvisionHosted(ctx, account.ID); !errors.Is(err, ErrSelfcheckFailed) {
		t.Fatalf("provision error=%v", err)
	}
	if _, err := service.GenerateAuthURL(ctx, userID, account.ID); !errors.Is(err, ErrSelfcheckFailed) {
		t.Fatalf("generate error=%v", err)
	}
	if err := service.SubmitAuthCode(ctx, userID, account.ID, "", "valid-code"); !errors.Is(err, ErrSelfcheckFailed) {
		t.Fatalf("submit error=%v", err)
	}
	var sessions int
	if err := service.db.QueryRow(`SELECT COUNT(*) FROM oauth_sessions`).Scan(&sessions); err != nil {
		t.Fatal(err)
	}
	stored, err := service.resource(ctx, account.ID)
	if err != nil || sessions != 0 || fake.ResetCalls(account.VMID) != 0 || stored.SelfcheckState != "fail" || stored.Status != "failed" || stored.SelfcheckAttempts != 0 {
		t.Fatalf("account=%+v sessions=%d resets=%d error=%v", stored, sessions, fake.ResetCalls(account.VMID), err)
	}
}

func TestSlotSelfcheckSnapshotErrorDoesNotSpendBudget(t *testing.T) {
	service, fake, userID := provisionFixture(t)
	account := hostedForSelfcheck(t, service, fake, userID)
	failure := errors.New("snapshot unavailable")
	stub := installSlotChecker(service, slotCheckStep{err: failure})
	if err := service.ProvisionHosted(context.Background(), account.ID); err != failure {
		t.Fatalf("error=%v", err)
	}
	state, err := service.Selfcheck(context.Background(), account.ID)
	if err != nil || state.Attempts != 0 || len(stub.attempts) != 0 || fake.ResetCalls(account.VMID) != 0 {
		t.Fatalf("state=%+v records=%v err=%v", state, stub.attempts, err)
	}
}

func TestSlotSelfcheckPostExchange(t *testing.T) {
	for _, duplicate := range []bool{true, false} {
		t.Run(fmt.Sprint(duplicate), func(t *testing.T) {
			service, fake, userID := provisionFixture(t)
			account := hostedForSelfcheck(t, service, fake, userID)
			failure := failedSlotCheck(selfcheck.FingerprintUnique, selfcheck.RemedyReset)
			if duplicate {
				failure = failedSlotCheck(selfcheck.AccountUnique, selfcheck.RemedyManual)
			}
			stub := installSlotChecker(service, passedSlotCheck(), failure)
			ctx := context.Background()
			if err := service.ProvisionHosted(ctx, account.ID); err != nil {
				t.Fatal(err)
			}
			auth, err := service.GenerateAuthURL(ctx, userID, account.ID)
			if err != nil {
				t.Fatal(err)
			}
			err = service.SubmitAuthCode(ctx, userID, account.ID, auth.SessionID, "valid-code")
			if duplicate && !errors.Is(err, ErrAccountDuplicate) || !duplicate && err != nil {
				t.Fatalf("exchange error=%v", err)
			}
			stored, readErr := service.resource(ctx, account.ID)
			if readErr != nil {
				t.Fatal(readErr)
			}
			if len(stub.options) != 2 || !stub.options[1].CheckAccount || stored.SelfcheckState != "fail" {
				t.Fatalf("options=%+v account=%+v", stub.options, stored)
			}
			if duplicate {
				if fake.ResetCalls(account.VMID) != 1 || stored.Status != "awaiting_auth" || stored.LastError == nil || *stored.LastError != "该 Claude 账号已在其它槽位托管，不能重复上号" {
					t.Fatalf("resets=%d account=%+v", fake.ResetCalls(account.VMID), stored)
				}
			} else if fake.ResetCalls(account.VMID) != 0 || stored.Status != "bootstrapping" {
				t.Fatalf("resets=%d account=%+v", fake.ResetCalls(account.VMID), stored)
			}
		})
	}
}

func TestSlotSelfcheckRebuildExpiresOldOAuth(t *testing.T) {
	service, fake, userID := provisionFixture(t)
	account := hostedForSelfcheck(t, service, fake, userID)
	stub := installSlotChecker(service, passedSlotCheck(), failedSlotCheck(selfcheck.FingerprintUnique, selfcheck.RemedyReset), passedSlotCheck())
	ctx := context.Background()
	if err := service.ProvisionHosted(ctx, account.ID); err != nil {
		t.Fatal(err)
	}
	auth, err := service.GenerateAuthURL(ctx, userID, account.ID)
	if err != nil {
		t.Fatal(err)
	}
	old := timestamp(service.now().Add(-time.Hour))
	if _, err := service.db.Exec(`UPDATE resources SET selfcheck_checked_at=? WHERE id=?`, old, account.ID); err != nil {
		t.Fatal(err)
	}
	if err := service.SubmitAuthCode(ctx, userID, account.ID, auth.SessionID, "valid-code"); !errors.Is(err, ErrAuthExpired) {
		t.Fatalf("submit error=%v", err)
	}
	var status string
	if err := service.db.QueryRow(`SELECT status FROM oauth_sessions WHERE resource_id=?`, account.ID).Scan(&status); err != nil {
		t.Fatal(err)
	}
	if status != "expired" || fake.ResetCalls(account.VMID) != 1 || len(stub.options) != 3 {
		t.Fatalf("status=%s resets=%d options=%v", status, fake.ResetCalls(account.VMID), stub.options)
	}
}

func TestSlotSelfcheckAdminCanResetBudget(t *testing.T) {
	service, fake, userID := provisionFixture(t)
	account := hostedForSelfcheck(t, service, fake, userID)
	if _, err := service.db.Exec(`UPDATE resources SET selfcheck_state='exhausted',selfcheck_attempts=3,status='failed' WHERE id=?`, account.ID); err != nil {
		t.Fatal(err)
	}
	stub := installSlotChecker(service, failedSlotCheck(selfcheck.FingerprintUnique, selfcheck.RemedyReset), passedSlotCheck())
	ctx := context.Background()
	state, err := service.RecheckResource(ctx, account.ID, false)
	if !errors.Is(err, ErrSelfcheckFailed) || state.Attempts != 3 || fake.ResetCalls(account.VMID) != 0 {
		t.Fatalf("state=%+v resets=%d error=%v", state, fake.ResetCalls(account.VMID), err)
	}
	state, err = service.RecheckResource(ctx, account.ID, true)
	if err != nil || state.State != "pass" || state.Attempts != 0 || state.MaxAttempts != 3 || fake.ResetCalls(account.VMID) != 1 || !reflect.DeepEqual(stub.attempts, []int{3, 0}) {
		t.Fatalf("state=%+v attempts=%v resets=%d error=%v", state, stub.attempts, fake.ResetCalls(account.VMID), err)
	}
}

func TestSlotSelfcheckConfiguredBudget(t *testing.T) {
	for _, configured := range []int{-3, 0, 1, 15} {
		t.Run(fmt.Sprint(configured), func(t *testing.T) {
			service, fake, userID := provisionFixture(t)
			account := hostedForSelfcheck(t, service, fake, userID)
			if _, err := service.db.Exec(`INSERT INTO settings(key,value,updated_at) VALUES ('site',?,?)`, fmt.Sprintf(`{"selfcheck_max_rebuilds":%d}`, configured), timestamp(service.now())); err != nil {
				t.Fatal(err)
			}
			budget := min(10, max(0, configured))
			steps := make([]slotCheckStep, budget+1)
			for index := range steps {
				steps[index] = failedSlotCheck(selfcheck.FingerprintUnique, selfcheck.RemedyReset)
			}
			installSlotChecker(service, steps...)
			if err := service.ProvisionHosted(context.Background(), account.ID); !errors.Is(err, ErrSelfcheckExhausted) {
				t.Fatalf("error=%v", err)
			}
			state, err := service.Selfcheck(context.Background(), account.ID)
			if err != nil || state.Attempts != budget || state.MaxAttempts != budget || fake.ResetCalls(account.VMID) != budget {
				t.Fatalf("state=%+v resets=%d error=%v", state, fake.ResetCalls(account.VMID), err)
			}
		})
	}
}

type selfcheckClientSpy struct {
	vm2api.Client
	calls []string
}

func (client *selfcheckClientSpy) DisableProxy(ctx context.Context, proxyID string) error {
	client.calls = append(client.calls, "disable")
	return client.Client.DisableProxy(ctx, proxyID)
}

func (client *selfcheckClientSpy) UnbindProxy(ctx context.Context, proxyID, vmID string) error {
	client.calls = append(client.calls, "unbind")
	return client.Client.UnbindProxy(ctx, proxyID, vmID)
}

func (client *selfcheckClientSpy) AllocateProxy(ctx context.Context, vmID string) error {
	client.calls = append(client.calls, "allocate")
	return client.Client.AllocateProxy(ctx, vmID)
}

func (client *selfcheckClientSpy) ResetVM(ctx context.Context, vmID string) error {
	client.calls = append(client.calls, "reset")
	return client.Client.ResetVM(ctx, vmID)
}

func (client *selfcheckClientSpy) GenerateAuthURL(ctx context.Context, vmID string) (vm2api.AuthURL, error) {
	client.calls = append(client.calls, "auth")
	return client.Client.GenerateAuthURL(ctx, vmID)
}

func (client *selfcheckClientSpy) ExchangeCode(ctx context.Context, vmID, sessionID, code string) error {
	client.calls = append(client.calls, "exchange")
	return client.Client.ExchangeCode(ctx, vmID, sessionID, code)
}

func TestSlotSelfcheckCombinedRemedies(t *testing.T) {
	service, fake, userID := provisionFixture(t)
	account := hostedForSelfcheck(t, service, fake, userID)
	fake.SetProxy(account.VMID, &vm2api.ProxyView{ID: "bad", Enabled: true, Status: "dead", Scheme: "socks5"})
	client := &selfcheckClientSpy{Client: fake}
	service.client = client
	failure := failedSlotCheck(selfcheck.ProxyHealthy, selfcheck.RemedyReproxy)
	failure.result.Checks = append(failure.result.Checks, selfcheck.Check{Code: selfcheck.FingerprintUnique, Remedy: selfcheck.RemedyReset, Message: "指纹重复"})
	installSlotChecker(service, failure, passedSlotCheck())
	if err := service.ensureSlotHealthy(context.Background(), account); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(client.calls, []string{"disable", "unbind", "allocate", "reset"}) {
		t.Fatalf("calls=%v", client.calls)
	}
}

func TestSlotSelfcheckOAuthRejectsBeforeUpstream(t *testing.T) {
	service, fake, userID := provisionFixture(t)
	account := hostedForSelfcheck(t, service, fake, userID)
	if err := service.ProvisionHosted(context.Background(), account.ID); err != nil {
		t.Fatal(err)
	}
	client := &selfcheckClientSpy{Client: fake}
	service.client = client
	failure := failedSlotCheck(selfcheck.AccountUnique, selfcheck.RemedyManual)
	installSlotChecker(service, failure, failure)
	if _, err := service.GenerateAuthURL(context.Background(), userID, account.ID); !errors.Is(err, ErrSelfcheckFailed) {
		t.Fatalf("generate error=%v", err)
	}
	if err := service.SubmitAuthCode(context.Background(), userID, account.ID, "old-session", "valid-code"); !errors.Is(err, ErrSelfcheckFailed) {
		t.Fatalf("submit error=%v", err)
	}
	if len(client.calls) != 0 {
		t.Fatalf("upstream calls=%v", client.calls)
	}
}

func TestSlotSelfcheckRechecksAfterOnboardingRepair(t *testing.T) {
	service, fake, userID := provisionFixture(t)
	account := hostedForSelfcheck(t, service, fake, userID)
	installSlotChecker(service, passedSlotCheck())
	ctx := context.Background()
	if err := service.ProvisionHosted(ctx, account.ID); err != nil {
		t.Fatal(err)
	}
	fake.SetProxy(account.VMID, nil)
	client := &selfcheckClientSpy{Client: fake}
	service.client = client
	installSlotChecker(service, failedSlotCheck(selfcheck.AccountUnique, selfcheck.RemedyManual))
	if _, err := service.GenerateAuthURL(ctx, userID, account.ID); !errors.Is(err, ErrSelfcheckFailed) {
		t.Fatalf("generate error=%v", err)
	}
	if !reflect.DeepEqual(client.calls, []string{"allocate"}) {
		t.Fatalf("calls=%v", client.calls)
	}
}

func TestSlotSelfcheckRetryPreservesBudget(t *testing.T) {
	service, fake, userID := provisionFixture(t)
	account := hostedForSelfcheck(t, service, fake, userID)
	if _, err := service.db.Exec(`UPDATE resources SET status='failed',selfcheck_attempts=2,selfcheck_state='fail' WHERE id=?`, account.ID); err != nil {
		t.Fatal(err)
	}
	if err := service.RetryHosted(context.Background(), userID, account.ID); err != nil {
		t.Fatal(err)
	}
	state, err := service.Selfcheck(context.Background(), account.ID)
	if err != nil || state.Attempts != 2 || state.State != "fail" {
		t.Fatalf("state=%+v error=%v", state, err)
	}
}

func TestRentalAllocationRequiresSelfcheckPass(t *testing.T) {
	service, fake, userID := provisionFixture(t)
	ctx := context.Background()
	for index, state := range []string{"pending", "fail", "pass"} {
		vmID := fmt.Sprintf("rental-%d", index)
		fake.AddVM(vm2api.VM{ID: vmID, Platform: "anthropic", HasCredential: true, EmailMasked: fmt.Sprintf("ren***%d@example.com", index), Tier: "max"})
		if _, err := service.db.Exec(`INSERT INTO rental_pool(vm_id,status,selfcheck_state,created_at,updated_at) VALUES (?,'available',?,?,?)`, vmID, state, timestamp(service.now()), timestamp(service.now())); err != nil {
			t.Fatal(err)
		}
	}
	subscriptionID := addSubscription(t, service, userID, "rental_account", 1, service.now().Add(time.Hour))
	if err := service.AllocateRental(ctx, subscriptionID); err != nil {
		t.Fatal(err)
	}
	var vmID string
	if err := service.db.QueryRow(`SELECT vm_id FROM resources WHERE subscription_id=?`, subscriptionID).Scan(&vmID); err != nil {
		t.Fatal(err)
	}
	if vmID != "rental-2" {
		t.Fatalf("allocated=%s", vmID)
	}
	var email, tier string
	if err := service.db.QueryRow(`SELECT email_masked,tier FROM resources WHERE subscription_id=?`, subscriptionID).Scan(&email, &tier); err != nil {
		t.Fatal(err)
	}
	if email != "ren***2@example.com" || tier != "max" {
		t.Fatalf("rental identity=%q/%q", email, tier)
	}
	subscriptionID = addSubscription(t, service, userID, "rental_account", 1, service.now().Add(time.Hour))
	if err := service.AllocateRental(ctx, subscriptionID); !errors.Is(err, ErrOutOfStock) {
		t.Fatalf("unhealthy stock error=%v", err)
	}
}

func TestSlotSelfcheckSweepScheduledAtStartup(t *testing.T) {
	service, fake, _ := provisionFixture(t)
	vm, err := fake.CreateHostedVM(context.Background(), "巡检槽位", nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.db.Exec(`INSERT INTO rental_pool(vm_id,status,created_at,updated_at) VALUES (?,'available',?,?)`, vm.ID, timestamp(service.now()), timestamp(service.now())); err != nil {
		t.Fatal(err)
	}
	stub := installSlotChecker(service, passedSlotCheck())
	service.engine.Workers = 1
	service.engine.PollInterval = time.Millisecond
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	done := make(chan error, 1)
	go func() { done <- service.Run(ctx) }()
	t.Cleanup(func() {
		cancel()
	})
	ticker := time.NewTicker(time.Millisecond)
	defer ticker.Stop()
	for {
		var passed int
		if err := service.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM rental_pool WHERE vm_id=? AND selfcheck_state='pass'`, vm.ID).Scan(&passed); err != nil {
			cancel()
			<-done
			t.Fatal(err)
		}
		if passed == 1 {
			break
		}
		select {
		case <-ctx.Done():
			<-done
			t.Fatal("startup sweep was not performed")
		case err := <-done:
			t.Fatalf("service stopped before startup sweep: %v", err)
		case <-ticker.C:
		}
	}
	cancel()
	if err := <-done; err != nil && !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	var jobs int
	if err := service.db.QueryRow(`SELECT COUNT(*) FROM jobs WHERE type='selfcheck_sweep'`).Scan(&jobs); err != nil {
		t.Fatal(err)
	}
	if jobs != 1 || len(stub.options) != 1 || !stub.options[0].CheckAccount || fake.ResetCalls(vm.ID) != 0 {
		t.Fatalf("jobs=%d options=%+v resets=%d", jobs, stub.options, fake.ResetCalls(vm.ID))
	}
}
