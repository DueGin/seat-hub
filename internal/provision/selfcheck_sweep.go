package provision

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/DueGin/vm2api-portal/internal/selfcheck"
)

// SweepSelfcheck records fresh verdicts without disrupting running slots.
func (service *Service) SweepSelfcheck(ctx context.Context) error {
	service.mu.Lock()
	defer service.mu.Unlock()
	if service.checker == nil {
		return nil
	}
	settings, err := service.site(ctx)
	if err != nil {
		return err
	}
	rules := selfcheck.Rules{
		RequireDedicatedProxy: settings.SelfcheckRequireDedicatedProxy,
		RequireTimezoneMatch:  settings.SelfcheckRequireTimezoneMatch,
	}
	type target struct {
		resourceID   int64
		vmID         string
		attempt      int
		checkAccount bool
	}
	var targets []target
	var failures []error
	// Check and Record may use this same database with only one connection.
	// Finish and close both result sets before invoking either method.
	for _, source := range []struct {
		name  string
		query string
	}{
		{
			name: "resources",
			query: `SELECT id,vm_id,selfcheck_attempts,status='active' FROM resources
				WHERE status IN ('active','awaiting_auth','needs_reauth','suspended')
				AND vm_id IS NOT NULL AND vm_id<>'' ORDER BY id`,
		},
		{
			name: "rental pool",
			query: `SELECT 0,vm_id,0,1 FROM rental_pool
				WHERE status IN ('available','maintenance') ORDER BY vm_id`,
		},
	} {
		rows, err := service.db.QueryContext(ctx, source.query)
		if err != nil {
			failures = append(failures, fmt.Errorf("list %s for self-check sweep: %w", source.name, err))
			continue
		}
		for rows.Next() {
			var item target
			if err := rows.Scan(&item.resourceID, &item.vmID, &item.attempt, &item.checkAccount); err != nil {
				failures = append(failures, fmt.Errorf("read %s self-check target: %w", source.name, err))
				continue
			}
			targets = append(targets, item)
		}
		if err := rows.Err(); err != nil {
			failures = append(failures, fmt.Errorf("read %s for self-check sweep: %w", source.name, err))
		}
		if err := rows.Close(); err != nil {
			failures = append(failures, fmt.Errorf("close %s self-check targets: %w", source.name, err))
		}
	}
	for _, item := range targets {
		result, err := service.checker.Check(ctx, item.vmID, selfcheck.Options{Rules: rules, CheckAccount: item.checkAccount})
		if err != nil {
			failures = append(failures, fmt.Errorf("self-check vm %s: %w", item.vmID, err))
			continue
		}
		if result.CheckedAt.IsZero() {
			result.CheckedAt = service.now()
		}
		if err := service.checker.Record(ctx, item.resourceID, item.attempt, result); err != nil {
			failures = append(failures, fmt.Errorf("record self-check for vm %s: %w", item.vmID, err))
		}
		state := "fail"
		if result.Passed {
			state = "pass"
		}
		checkedAt := timestamp(result.CheckedAt)
		if item.resourceID == 0 {
			if _, err := service.db.ExecContext(ctx, `UPDATE rental_pool SET selfcheck_state=?,selfcheck_checked_at=?,updated_at=? WHERE vm_id=?`, state, checkedAt, timestamp(service.now()), item.vmID); err != nil {
				failures = append(failures, fmt.Errorf("save rental pool self-check for vm %s: %w", item.vmID, err))
			}
			continue
		}
		summary, err := json.Marshal(result.Checks)
		if err != nil {
			failures = append(failures, fmt.Errorf("encode self-check summary for resource %d: %w", item.resourceID, err))
			continue
		}
		if _, err := service.db.ExecContext(ctx, `UPDATE resources SET selfcheck_state=?,selfcheck_checked_at=?,selfcheck_summary=?,updated_at=? WHERE id=?`, state, checkedAt, string(summary), timestamp(service.now()), item.resourceID); err != nil {
			failures = append(failures, fmt.Errorf("save self-check for resource %d: %w", item.resourceID, err))
		}
	}
	return errors.Join(failures...)
}
