package provision

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/DueGin/seat-hub/internal/selfcheck"
)

// Self-check surface consumed by internal/app (admin endpoints, DTOs).

var (
	// ErrSelfcheckFailed blocks onboarding while the slot has not passed.
	ErrSelfcheckFailed = errors.New("slot self-check failed")
	// ErrSelfcheckExhausted means the rebuild budget is spent.
	ErrSelfcheckExhausted = errors.New("slot self-check rebuild budget exhausted")
	// ErrAccountDuplicate means the signed-in account already lives on another slot.
	ErrAccountDuplicate = errors.New("account already hosted on another slot")
)

// SelfcheckState is a resource's latest self-check, as shown to users/admins.
type SelfcheckState struct {
	State       string            // pending | pass | fail | exhausted
	Attempts    int               // rebuilds performed so far
	MaxAttempts int               // configured budget (selfcheck_max_rebuilds)
	CheckedAt   *string           // RFC3339
	Checks      []selfcheck.Check // latest run
}

// SelfcheckRecord is one row of slot_checks.
type SelfcheckRecord struct {
	ID        int64
	VMID      string
	Attempt   int
	Passed    bool
	Checks    []selfcheck.Check
	CreatedAt string
}

// SetChecker installs the self-check engine. Without one, onboarding is not
// gated (tests that predate self-check keep working).
func (service *Service) SetChecker(checker selfcheck.Checker) { service.checker = checker }

// Selfcheck returns the stored state without running a check.
func (service *Service) Selfcheck(ctx context.Context, resourceID int64) (SelfcheckState, error) {
	account, err := service.resource(ctx, resourceID)
	if err != nil {
		return SelfcheckState{}, err
	}
	site, err := service.site(ctx)
	if err != nil {
		return SelfcheckState{}, err
	}
	state := SelfcheckState{State: account.SelfcheckState, Attempts: account.SelfcheckAttempts, MaxAttempts: selfcheckBudget(site), CheckedAt: account.SelfcheckCheckedAt, Checks: []selfcheck.Check{}}
	if account.SelfcheckSummary != nil {
		if err := json.Unmarshal([]byte(*account.SelfcheckSummary), &state.Checks); err != nil {
			return state, fmt.Errorf("decode self-check summary: %w", err)
		}
	}
	return state, nil
}

// RecheckResource runs a check now (admin); rebuild=true resets the budget and
// rebuilds before checking.
func (service *Service) RecheckResource(ctx context.Context, resourceID int64, rebuild bool) (SelfcheckState, error) {
	service.mu.Lock()
	defer service.mu.Unlock()
	account, err := service.resource(ctx, resourceID)
	if err != nil {
		return SelfcheckState{}, err
	}
	if account.VMID == "" || account.Status == "released" || account.Status == "releasing" {
		return SelfcheckState{}, ErrInvalidState
	}
	if rebuild {
		if _, err := service.db.ExecContext(ctx, `UPDATE resources SET selfcheck_attempts=0,selfcheck_state='pending',updated_at=? WHERE id=?`, timestamp(service.now()), resourceID); err != nil {
			return SelfcheckState{}, fmt.Errorf("reset self-check budget: %w", err)
		}
		account.SelfcheckAttempts, account.SelfcheckState = 0, "pending"
		if err := service.expireAuthSessions(ctx, resourceID); err != nil {
			return SelfcheckState{}, err
		}
		if err := service.client.ResetVM(ctx, account.VMID); err != nil {
			return SelfcheckState{}, fmt.Errorf("rebuild resource: %w", err)
		}
		if err := service.ensureSlotHealthy(ctx, account); err != nil {
			state, readErr := service.Selfcheck(ctx, resourceID)
			return state, errors.Join(err, readErr)
		}
		if account.Kind == "hosted" {
			if _, err := service.db.ExecContext(ctx, `UPDATE resources SET status='awaiting_auth',status_reason=NULL,last_error=NULL,updated_at=? WHERE id=?`, timestamp(service.now()), resourceID); err != nil {
				return SelfcheckState{}, fmt.Errorf("prepare rebuilt authorization: %w", err)
			}
		}
		return service.Selfcheck(ctx, resourceID)
	}
	if service.checker == nil {
		return service.Selfcheck(ctx, resourceID)
	}
	site, err := service.site(ctx)
	if err != nil {
		return SelfcheckState{}, err
	}
	result, err := service.checkSlot(ctx, account.ID, account.VMID, account.SelfcheckAttempts, selfcheck.Options{Rules: selfcheckRules(site), CheckAccount: account.Status == "active" || account.Kind == "rental"})
	if err != nil {
		return SelfcheckState{}, err
	}
	state := "fail"
	if result.Passed {
		state = "pass"
	}
	if err := service.saveSelfcheck(ctx, resourceID, result, state); err != nil {
		return SelfcheckState{}, err
	}
	stored, err := service.Selfcheck(ctx, resourceID)
	if err == nil && !result.Passed {
		err = selfcheckFailure(result, ErrSelfcheckFailed)
	}
	return stored, err
}

// SelfcheckHistory lists recorded runs for a resource, newest first.
func (service *Service) SelfcheckHistory(ctx context.Context, resourceID int64, limit int) ([]SelfcheckRecord, error) {
	if _, err := service.resource(ctx, resourceID); err != nil {
		return nil, err
	}
	if limit <= 0 {
		limit = 50
	}
	limit = min(limit, 200)
	rows, err := service.db.QueryContext(ctx, `SELECT id,vm_id,attempt,passed,checks,created_at FROM slot_checks WHERE resource_id=? ORDER BY id DESC LIMIT ?`, resourceID, limit)
	if err != nil {
		return nil, fmt.Errorf("list self-check history: %w", err)
	}
	defer rows.Close()
	records := make([]SelfcheckRecord, 0)
	for rows.Next() {
		var record SelfcheckRecord
		var checks string
		if err := rows.Scan(&record.ID, &record.VMID, &record.Attempt, &record.Passed, &checks, &record.CreatedAt); err != nil {
			return nil, fmt.Errorf("scan self-check history: %w", err)
		}
		if err := json.Unmarshal([]byte(checks), &record.Checks); err != nil {
			return nil, fmt.Errorf("decode self-check history: %w", err)
		}
		records = append(records, record)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read self-check history: %w", err)
	}
	return records, nil
}

// AdmitPoolVM checks a rental-pool candidate and stores the verdict on its
// rental_pool row. A failing VM must not be leased.
func (service *Service) AdmitPoolVM(ctx context.Context, vmID string) (SelfcheckState, error) {
	service.mu.Lock()
	defer service.mu.Unlock()
	site, err := service.site(ctx)
	if err != nil {
		return SelfcheckState{}, err
	}
	result := selfcheck.Result{VMID: vmID, Passed: true, CheckedAt: service.now(), Checks: []selfcheck.Check{}}
	if service.checker != nil {
		result, err = service.checkSlot(ctx, 0, vmID, 0, selfcheck.Options{Rules: selfcheckRules(site), CheckAccount: true})
		if err != nil {
			return SelfcheckState{}, err
		}
	}
	checkedAt := timestamp(result.CheckedAt)
	state := SelfcheckState{State: "pass", MaxAttempts: selfcheckBudget(site), CheckedAt: &checkedAt, Checks: result.Checks}
	if !result.Passed {
		state.State = "fail"
	}
	if _, err := service.db.ExecContext(ctx, `UPDATE rental_pool SET selfcheck_state=?,selfcheck_checked_at=?,updated_at=? WHERE vm_id=?`, state.State, checkedAt, timestamp(service.now()), vmID); err != nil {
		return state, fmt.Errorf("save rental self-check: %w", err)
	}
	if !result.Passed {
		return state, selfcheckFailure(result, ErrSelfcheckFailed)
	}
	return state, nil
}
