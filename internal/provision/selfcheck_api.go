package provision

import (
	"context"
	"errors"

	"github.com/DueGin/vm2api-portal/internal/selfcheck"
)

// Self-check surface consumed by internal/app (admin endpoints, DTOs).
// PLACEHOLDER: the provision self-check worker replaces the bodies; the
// exported names and signatures are the contract.

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
	return SelfcheckState{State: "pending"}, nil
}

// RecheckResource runs a check now (admin); rebuild=true resets the budget and
// rebuilds before checking.
func (service *Service) RecheckResource(ctx context.Context, resourceID int64, rebuild bool) (SelfcheckState, error) {
	return SelfcheckState{}, ErrSelfcheckFailed
}

// SelfcheckHistory lists recorded runs for a resource, newest first.
func (service *Service) SelfcheckHistory(ctx context.Context, resourceID int64, limit int) ([]SelfcheckRecord, error) {
	return nil, nil
}

// AdmitPoolVM checks a rental-pool candidate and stores the verdict on its
// rental_pool row. A failing VM must not be leased.
func (service *Service) AdmitPoolVM(ctx context.Context, vmID string) (SelfcheckState, error) {
	return SelfcheckState{}, ErrSelfcheckFailed
}
