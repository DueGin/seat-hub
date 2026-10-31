package provision

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/DueGin/vm2api-portal/internal/selfcheck"
	"github.com/DueGin/vm2api-portal/internal/store"
	"github.com/DueGin/vm2api-portal/internal/vm2api"
)

func selfcheckRules(site store.Site) selfcheck.Rules {
	return selfcheck.Rules{RequireDedicatedProxy: site.SelfcheckRequireDedicatedProxy, RequireTimezoneMatch: site.SelfcheckRequireTimezoneMatch}
}

func selfcheckBudget(site store.Site) int {
	return min(10, max(0, site.SelfcheckMaxRebuilds))
}

func selfcheckInterval(site store.Site) time.Duration {
	minutes := site.SelfcheckRecheckMinutes
	if minutes <= 0 {
		minutes = 30
	}
	return time.Duration(minutes) * time.Minute
}

func selfcheckFailure(result selfcheck.Result, failure error) error {
	codes, messages := selfcheckFailures(result)
	return fmt.Errorf("%w: %s（%s）", failure, messages, codes)
}

func selfcheckFailures(result selfcheck.Result) (string, string) {
	var codes, messages []string
	for _, check := range result.Failed() {
		codes = append(codes, string(check.Code))
		if check.Message != "" {
			messages = append(messages, check.Message)
		}
	}
	if len(messages) == 0 {
		messages = append(messages, "槽位运行环境自检未通过")
	}
	return strings.Join(codes, ","), strings.Join(messages, "；")
}

func (service *Service) saveSelfcheck(ctx context.Context, resourceID int64, result selfcheck.Result, state string) error {
	checks, err := json.Marshal(result.Checks)
	if err != nil {
		return fmt.Errorf("encode self-check summary: %w", err)
	}
	if _, err := service.db.ExecContext(ctx, `UPDATE resources SET selfcheck_state=?,selfcheck_checked_at=?,selfcheck_summary=?,updated_at=? WHERE id=?`, state, timestamp(result.CheckedAt), string(checks), timestamp(service.now()), resourceID); err != nil {
		return fmt.Errorf("save self-check state: %w", err)
	}
	return nil
}

func (service *Service) checkSlot(ctx context.Context, resourceID int64, vmID string, attempts int, options selfcheck.Options) (selfcheck.Result, error) {
	result, err := service.checker.Check(ctx, vmID, options)
	if err != nil {
		return result, err
	}
	if result.VMID == "" {
		result.VMID = vmID
	}
	if result.CheckedAt.IsZero() {
		result.CheckedAt = service.now()
	}
	if err := service.checker.Record(ctx, resourceID, attempts, result); err != nil {
		return result, fmt.Errorf("record self-check: %w", err)
	}
	return result, nil
}

func (service *Service) expireAuthSessions(ctx context.Context, resourceID int64) error {
	if _, err := service.db.ExecContext(ctx, `UPDATE oauth_sessions SET status='expired' WHERE resource_id=? AND status='open'`, resourceID); err != nil {
		return fmt.Errorf("expire rebuilt auth sessions: %w", err)
	}
	return nil
}

func (service *Service) ensureSlotHealthy(ctx context.Context, account Account) error {
	if service.checker == nil {
		return nil
	}
	if account.SelfcheckState == "exhausted" {
		return ErrSelfcheckExhausted
	}
	site, err := service.site(ctx)
	if err != nil {
		return err
	}
	budget := selfcheckBudget(site)
	for {
		result, err := service.checkSlot(ctx, account.ID, account.VMID, account.SelfcheckAttempts, selfcheck.Options{Rules: selfcheckRules(site)})
		if err != nil {
			return err
		}
		state := "fail"
		if result.Passed {
			state = "pass"
		}
		if err := service.saveSelfcheck(ctx, account.ID, result, state); err != nil {
			return err
		}
		if result.Passed {
			return nil
		}
		if result.Remedy() == selfcheck.RemedyManual {
			return selfcheckFailure(result, ErrSelfcheckFailed)
		}
		if account.SelfcheckAttempts >= budget {
			codes, message := selfcheckFailures(result)
			if _, err := service.db.ExecContext(ctx, `UPDATE resources SET selfcheck_state='exhausted',status='failed',status_reason=?,last_error=?,updated_at=? WHERE id=?`, "selfcheck_exhausted:"+codes, message, timestamp(service.now()), account.ID); err != nil {
				return fmt.Errorf("save exhausted self-check: %w", err)
			}
			return selfcheckFailure(result, ErrSelfcheckExhausted)
		}
		account.SelfcheckAttempts++
		if _, err := service.db.ExecContext(ctx, `UPDATE resources SET selfcheck_attempts=?,selfcheck_state='running',updated_at=? WHERE id=?`, account.SelfcheckAttempts, timestamp(service.now()), account.ID); err != nil {
			return fmt.Errorf("save self-check attempt: %w", err)
		}
		if err := service.expireAuthSessions(ctx, account.ID); err != nil {
			return err
		}
		if result.Needs(selfcheck.RemedyReproxy) {
			if err := service.reproxySlot(ctx, account.VMID, result); err != nil {
				return err
			}
		}
		if result.Needs(selfcheck.RemedyReset) {
			if err := service.client.ResetVM(ctx, account.VMID); err != nil {
				return fmt.Errorf("reset self-check slot: %w", err)
			}
		}
	}
}

func (service *Service) reproxySlot(ctx context.Context, vmID string, result selfcheck.Result) error {
	vms, err := service.client.ListVMs(ctx, vm2api.VMFilter{})
	if err != nil {
		return fmt.Errorf("list proxy bindings: %w", err)
	}
	var proxyID string
	for _, vm := range vms {
		if vm.ID == vmID && vm.Proxy != nil {
			proxyID = vm.Proxy.ID
			break
		}
	}
	if proxyID != "" {
		shared, unhealthy := false, false
		for _, vm := range vms {
			if vm.ID != vmID && vm.Proxy != nil && vm.Proxy.ID == proxyID {
				shared = true
			}
		}
		for _, check := range result.Failed() {
			if check.Code == selfcheck.ProxyHealthy || check.Code == selfcheck.ExitIPUnique {
				unhealthy = true
			}
		}
		if unhealthy && !shared {
			if err := service.client.DisableProxy(ctx, proxyID); err != nil {
				return fmt.Errorf("disable failed proxy: %w", err)
			}
		}
		if err := service.client.UnbindProxy(ctx, proxyID, vmID); err != nil {
			return fmt.Errorf("unbind failed proxy: %w", err)
		}
	}
	if err := service.client.AllocateProxy(ctx, vmID); err != nil {
		return fmt.Errorf("replace failed proxy: %w", err)
	}
	return nil
}

func (service *Service) ensureFreshSelfcheck(ctx context.Context, account Account) error {
	if service.checker == nil {
		return nil
	}
	if account.SelfcheckState == "pass" && account.SelfcheckCheckedAt != nil {
		site, err := service.site(ctx)
		if err != nil {
			return err
		}
		checkedAt, err := time.Parse(time.RFC3339, *account.SelfcheckCheckedAt)
		if err == nil && service.now().Before(checkedAt.Add(selfcheckInterval(site))) {
			return nil
		}
	}
	return service.ensureSlotHealthy(ctx, account)
}

func (service *Service) checkAuthorizedAccount(ctx context.Context, account Account) error {
	if service.checker == nil {
		return nil
	}
	site, err := service.site(ctx)
	if err != nil {
		return err
	}
	account, err = service.resource(ctx, account.ID)
	if err != nil {
		return err
	}
	result, err := service.checkSlot(ctx, account.ID, account.VMID, account.SelfcheckAttempts, selfcheck.Options{Rules: selfcheckRules(site), CheckAccount: true})
	if err != nil {
		return err
	}
	state := "pass"
	if !result.Passed {
		state = "fail"
	}
	if err := service.saveSelfcheck(ctx, account.ID, result, state); err != nil {
		return err
	}
	for _, check := range result.Failed() {
		if check.Code == selfcheck.AccountUnique {
			resetErr := service.client.ResetVM(ctx, account.VMID)
			if resetErr != nil {
				resetErr = fmt.Errorf("clear duplicate account: %w", resetErr)
			}
			_, saveErr := service.db.ExecContext(ctx, `UPDATE resources SET status='awaiting_auth',status_reason='account_duplicate',last_error='该 Claude 账号已在其它槽位托管，不能重复上号',updated_at=? WHERE id=?`, timestamp(service.now()), account.ID)
			if saveErr != nil {
				saveErr = fmt.Errorf("save duplicate account failure: %w", saveErr)
			}
			return errors.Join(ErrAccountDuplicate, resetErr, saveErr, service.expireAuthSessions(ctx, account.ID))
		}
	}
	// Other failures are recorded without remediation to preserve the account just authorized.
	return nil
}
