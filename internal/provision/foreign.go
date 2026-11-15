package provision

// Foreign pools (docs/KEY_SCOPE.md → 外部账号池).
//
// vm2api lets a VM sit in at most one account pool. A pool the portal did not
// create (someone made it by hand in the vm2api panel) that holds a VM the
// portal needs in a user's pool blocks that user's scope: the pool write fails
// with 409 vm_in_other_pool. Only an admin can resolve it, by taking the VM out
// of that pool in vm2api. Until then:
//
//   - the user's scope is left as it was (never widened, never half-applied);
//   - jobs record a scope warning instead of failing and retrying, and the
//     10-minute reconcile re-checks and clears the warning once it is fixed;
//   - foreign-pooled rental VMs are not offered for sale, not leased and not
//     admitted to the rental pool.

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"slices"

	"github.com/DueGin/seat-hub/internal/store"
	"github.com/DueGin/seat-hub/internal/vm2api"
)

// ForeignPoolError says a VM the portal needs is held by a vm2api pool the
// portal does not own.
type ForeignPoolError struct {
	VMID     string
	PoolID   string
	PoolName string
	// Cause is vm2api's 409 vm_in_other_pool, kept so vm2api.IsCode still
	// recognises the error.
	Cause error
}

func (e *ForeignPoolError) Error() string {
	return fmt.Sprintf("账号 %s 在 vm2api 账号池「%s」(%s) 里，门面无法把它放进用户的账号池；请在 vm2api 面板把它移出该账号池，门面会在 10 分钟内自动重试", e.VMID, e.PoolName, e.PoolID)
}

func (e *ForeignPoolError) Unwrap() error { return e.Cause }

// ForeignPoolVMs maps every VM held by a vm2api pool that is not some portal
// user's pool to that pool. It is a package function so wiring that runs
// before the provision service exists (the catalog stock probe) can use it.
func ForeignPoolVMs(ctx context.Context, db *sql.DB, client vm2api.Client) (map[string]vm2api.Pool, error) {
	pools, err := client.ListPools(ctx)
	if err != nil {
		return nil, fmt.Errorf("list vm2api pools: %w", err)
	}
	rows, err := db.QueryContext(ctx, `SELECT vm2api_pool_id FROM users WHERE COALESCE(vm2api_pool_id,'')<>''`)
	if err != nil {
		return nil, fmt.Errorf("list user pools: %w", err)
	}
	defer rows.Close()
	own := make(map[string]bool)
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("read user pool: %w", err)
		}
		own[id] = true
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read user pools: %w", err)
	}
	foreign := make(map[string]vm2api.Pool)
	for _, pool := range pools {
		if own[pool.ID] {
			continue
		}
		for _, vmID := range pool.VMIDs {
			foreign[vmID] = pool
		}
	}
	return foreign, nil
}

// ForeignPoolVMs is the service-bound form of the package function.
func (service *Service) ForeignPoolVMs(ctx context.Context) (map[string]vm2api.Pool, error) {
	return ForeignPoolVMs(ctx, service.db, service.client)
}

// foreignVMIDs lists the VMs ForeignPoolVMs reports, sorted.
func (service *Service) foreignVMIDs(ctx context.Context) ([]string, error) {
	foreign, err := service.ForeignPoolVMs(ctx)
	if err != nil {
		return nil, err
	}
	ids := make([]string, 0, len(foreign))
	for id := range foreign {
		ids = append(ids, id)
	}
	slices.Sort(ids)
	return ids, nil
}

// explainPoolConflict turns vm2api's terse 409 vm_in_other_pool into a
// ForeignPoolError naming the pool, when the conflicting VM can be found among
// the wanted members. Other errors pass through.
func (service *Service) explainPoolConflict(ctx context.Context, cause error, wanted []string, ownPoolID string) error {
	if !vm2api.IsCode(cause, "vm_in_other_pool") {
		return cause
	}
	pools, err := service.client.ListPools(ctx)
	if err != nil {
		return cause
	}
	for _, pool := range pools {
		if pool.ID == ownPoolID {
			continue
		}
		for _, vmID := range pool.VMIDs {
			if slices.Contains(wanted, vmID) {
				return &ForeignPoolError{VMID: vmID, PoolID: pool.ID, PoolName: pool.Name, Cause: cause}
			}
		}
	}
	return cause
}

// ScopeWarning is a scope problem that needs an admin, shown on the console
// dashboard until the reconcile that follows the fix clears it.
type ScopeWarning struct {
	UserID   int64  `json:"user_id"`
	VMID     string `json:"vm_id"`
	PoolID   string `json:"pool_id"`
	PoolName string `json:"pool_name"`
	Message  string `json:"message"`
}

// ScopeWarnings is the warning list and when the reconcile last checked.
type ScopeWarnings struct {
	CheckedAt string         `json:"checked_at"`
	Items     []ScopeWarning `json:"items"`
}

const scopeWarningsKey = "scope_warnings"

// LoadScopeWarnings returns the current warnings (empty when none recorded).
func (service *Service) LoadScopeWarnings(ctx context.Context) (ScopeWarnings, error) {
	warnings := ScopeWarnings{Items: []ScopeWarning{}}
	var raw string
	err := service.db.QueryRowContext(ctx, `SELECT value FROM settings WHERE key=?`, scopeWarningsKey).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return warnings, nil
	}
	if err != nil {
		return warnings, fmt.Errorf("read scope warnings: %w", err)
	}
	if err := json.Unmarshal([]byte(raw), &warnings); err != nil {
		return ScopeWarnings{Items: []ScopeWarning{}}, nil
	}
	if warnings.Items == nil {
		warnings.Items = []ScopeWarning{}
	}
	return warnings, nil
}

func (service *Service) saveScopeWarnings(ctx context.Context, warnings ScopeWarnings) error {
	if warnings.Items == nil {
		warnings.Items = []ScopeWarning{}
	}
	raw, err := json.Marshal(warnings)
	if err != nil {
		return fmt.Errorf("encode scope warnings: %w", err)
	}
	_, err = service.db.ExecContext(ctx, `INSERT INTO settings (key, value, updated_at) VALUES (?, ?, ?)
		ON CONFLICT(key) DO UPDATE SET value=excluded.value, updated_at=excluded.updated_at`,
		scopeWarningsKey, string(raw), store.Now())
	if err != nil {
		return fmt.Errorf("save scope warnings: %w", err)
	}
	return nil
}

// warningFor builds the warning for an error that needs an admin; ok is false
// for every other error.
func warningFor(userID int64, err error) (ScopeWarning, bool) {
	var foreign *ForeignPoolError
	if !errors.As(err, &foreign) {
		return ScopeWarning{}, false
	}
	return ScopeWarning{UserID: userID, VMID: foreign.VMID, PoolID: foreign.PoolID, PoolName: foreign.PoolName, Message: foreign.Error()}, true
}

// addScopeWarning merges one warning into the stored list (event-driven jobs;
// the reconcile rewrites the whole list).
func (service *Service) addScopeWarning(ctx context.Context, warning ScopeWarning) error {
	warnings, err := service.LoadScopeWarnings(ctx)
	if err != nil {
		return err
	}
	for _, item := range warnings.Items {
		if item.UserID == warning.UserID && item.VMID == warning.VMID && item.PoolID == warning.PoolID {
			return nil
		}
	}
	warnings.Items = append(warnings.Items, warning)
	return service.saveScopeWarnings(ctx, warnings)
}

// absorbManual records an admin-only problem as a warning and reports it as
// handled, so a job does not burn its retries on something only a person can
// fix. Any other error is returned unchanged.
func (service *Service) absorbManual(ctx context.Context, userID int64, err error) error {
	warning, ok := warningFor(userID, err)
	if !ok {
		return err
	}
	if saveErr := service.addScopeWarning(ctx, warning); saveErr != nil {
		return errors.Join(err, saveErr)
	}
	return nil
}

// EnqueueScopeReconcile asks for a reconcile now (admin "re-check" button).
func (service *Service) EnqueueScopeReconcile(ctx context.Context) error {
	_, err := service.engine.Enqueue(ctx, "scope_reconcile", jobPayload{}, "scope_reconcile", service.now(), 8)
	return err
}
