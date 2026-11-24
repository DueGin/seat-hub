package provision

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/DueGin/seat-hub/internal/vm2api"
)

const (
	VMOwnerConflict  = "vm_owner_conflict"
	RentalVMTaken    = "rental_vm_taken"
	KeyDrifted       = "key_drifted"
	PoolInUse        = "pool_in_use"
	scopeWarningsKey = "scope_warnings"
)

type ScopeWarning struct {
	Kind    string `json:"kind"`
	UserID  int64  `json:"user_id,omitempty"`
	VMID    string `json:"vm_id,omitempty"`
	KeyID   string `json:"key_id,omitempty"`
	Message string `json:"message"`
}

type ScopeWarnings struct {
	CheckedAt string         `json:"checked_at"`
	Items     []ScopeWarning `json:"items"`
}

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
	var saved ScopeWarnings
	if json.Unmarshal([]byte(raw), &saved) == nil {
		warnings.CheckedAt = saved.CheckedAt
		for _, item := range saved.Items {
			if item.Kind != "" {
				warnings.Items = append(warnings.Items, item)
			}
		}
	}
	return warnings, nil
}

func (service *Service) saveScopeWarnings(ctx context.Context, warnings ScopeWarnings) error {
	return service.saveOwnerSetting(ctx, scopeWarningsKey, warnings)
}

func (service *Service) saveOwnerSetting(ctx context.Context, key string, value any) error {
	raw, err := json.Marshal(value)
	if err != nil {
		return fmt.Errorf("encode owner setting: %w", err)
	}
	_, err = service.db.ExecContext(ctx, `INSERT INTO settings(key,value,updated_at) VALUES(?,?,?) ON CONFLICT(key) DO UPDATE SET value=excluded.value, updated_at=excluded.updated_at`, key, string(raw), timestamp(service.now()))
	return err
}

func (service *Service) addScopeWarning(ctx context.Context, warning ScopeWarning) error {
	service.warningMu.Lock()
	defer service.warningMu.Unlock()
	warnings, err := service.LoadScopeWarnings(ctx)
	if err != nil {
		return err
	}
	for _, item := range warnings.Items {
		if item == warning {
			return nil
		}
	}
	warnings.Items = append(warnings.Items, warning)
	return service.saveScopeWarnings(ctx, warnings)
}

func (service *Service) ownerNames(ctx context.Context) (map[string]string, error) {
	users, err := service.client.ListUsers(ctx)
	names := make(map[string]string, len(users))
	for _, user := range users {
		names[user.ID] = user.Username
	}
	return names, err
}

func ownerName(names map[string]string, id string) string {
	if name := names[id]; name != "" {
		return name
	}
	return id
}

func rentalWarning(vm vm2api.VM, names map[string]string) ScopeWarning {
	return ScopeWarning{Kind: RentalVMTaken, VMID: vm.ID, Message: fmt.Sprintf("租赁账号 %s 已被 vm2api 用户「%s」占用，不会分配。", vm.ID, ownerName(names, vm.OwnerUserID))}
}

func vmConflict(userID int64, vm vm2api.VM, names map[string]string) ScopeWarning {
	return ScopeWarning{Kind: VMOwnerConflict, UserID: userID, VMID: vm.ID, Message: fmt.Sprintf("账号 %s 已归属 vm2api 用户「%s」，门面未修改归属。", vm.ID, ownerName(names, vm.OwnerUserID))}
}

func (service *Service) EnqueueScopeReconcile(ctx context.Context) error {
	_, err := service.engine.Enqueue(ctx, "owner_reconcile", jobPayload{}, "owner_reconcile", service.now(), 8)
	return err
}
