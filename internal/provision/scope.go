package provision

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"slices"
	"sort"
	"sync"
	"time"

	"github.com/DueGin/seat-hub/internal/store"
	"github.com/DueGin/seat-hub/internal/vm2api"
)

const (
	ScopeAll      = "all"
	ScopeSelected = "selected"
)

var (
	ErrScopeInvalid = errors.New("invalid key scope")
	ErrScopeEmpty   = errors.New("key scope has no usable account")
)

// LockScope serializes writes and syncs for one portal user. Callers holding
// service.mu may take it, but code holding LockScope must not take service.mu.
func (service *Service) LockScope(userID int64) func() {
	value, _ := service.scopes.LoadOrStore(userID, &sync.Mutex{})
	lock := value.(*sync.Mutex)
	lock.Lock()
	return lock.Unlock
}

func (service *Service) SchedulableVMs(ctx context.Context, userID int64) (map[int64]string, error) {
	rows, err := service.db.QueryContext(ctx, `SELECT id,vm_id FROM resources WHERE user_id=? AND kind IN ('hosted','rental') AND status IN ('active','needs_reauth','suspended') AND COALESCE(vm_id,'')<>''`, userID)
	if err != nil {
		return nil, fmt.Errorf("list schedulable resources: %w", err)
	}
	defer rows.Close()
	accounts := make(map[int64]string)
	for rows.Next() {
		var id int64
		var vmID string
		if err := rows.Scan(&id, &vmID); err != nil {
			return nil, fmt.Errorf("read schedulable resource: %w", err)
		}
		accounts[id] = vmID
	}
	return accounts, rows.Err()
}

func (service *Service) ValidateKeyScope(ctx context.Context, userID int64, mode string, accountIDs []int64) (string, []int64, error) {
	if mode == "" || mode == ScopeAll {
		return ScopeAll, []int64{}, nil
	}
	if mode != ScopeSelected {
		return "", nil, ErrScopeInvalid
	}
	for _, id := range accountIDs {
		var exists bool
		if err := service.db.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM resources WHERE id=? AND user_id=? AND status<>'released')`, id, userID).Scan(&exists); err != nil {
			return "", nil, fmt.Errorf("validate scope account: %w", err)
		}
		if !exists {
			return "", nil, ErrScopeInvalid
		}
	}
	ids := store.NormalizeIDs(accountIDs)
	accounts, err := service.SchedulableVMs(ctx, userID)
	if err != nil {
		return "", nil, err
	}
	for _, id := range ids {
		if accounts[id] != "" {
			return ScopeSelected, ids, nil
		}
	}
	return "", nil, ErrScopeEmpty
}

// An unusable selected scope is nil: upstream rejects empty allowlists, so
// keep the existing allowlist and switch off the key instead.
func (service *Service) KeyScopeFor(ctx context.Context, userID int64, mode string, accountIDs []int64) (*vm2api.KeyScope, bool, error) {
	if mode == "" || mode == ScopeAll {
		return &vm2api.KeyScope{All: true}, true, nil
	}
	if mode != ScopeSelected {
		return nil, false, ErrScopeInvalid
	}
	accounts, err := service.SchedulableVMs(ctx, userID)
	if err != nil {
		return nil, false, err
	}
	owner, err := service.recordedOwner(ctx, userID)
	if err != nil || owner == "" || len(accounts) == 0 {
		return nil, false, err
	}
	vms, err := service.client.ListVMs(ctx, vm2api.VMFilter{OwnerUserID: &owner, ClaudeOnly: true})
	if err != nil {
		return nil, false, fmt.Errorf("list owned scope accounts: %w", err)
	}
	owned := make(map[string]bool, len(vms))
	for _, vm := range vms {
		owned[vm.ID] = true
	}
	ids := make([]string, 0, len(accountIDs))
	for _, id := range accountIDs {
		if vmID := accounts[id]; owned[vmID] {
			ids = append(ids, vmID)
		}
	}
	if len(ids) == 0 {
		return nil, false, nil
	}
	sort.Strings(ids)
	return &vm2api.KeyScope{AllowedVMs: slices.Compact(ids)}, true, nil
}

type scopeKey struct {
	id                                                      int64
	upstream, name, status, mode, accounts, owner, previous string
}

func (service *Service) userScopeKeys(ctx context.Context, userID int64) ([]scopeKey, error) {
	rows, err := service.db.QueryContext(ctx, `SELECT id,vm2api_key_id,name,status,scope_mode,scope_accounts,upstream_owner,previous_vm2api_key_ids FROM api_keys WHERE user_id=? ORDER BY id`, userID)
	if err != nil {
		return nil, fmt.Errorf("list scope keys: %w", err)
	}
	defer rows.Close()
	var keys []scopeKey
	for rows.Next() {
		var key scopeKey
		if err := rows.Scan(&key.id, &key.upstream, &key.name, &key.status, &key.mode, &key.accounts, &key.owner, &key.previous); err != nil {
			return nil, fmt.Errorf("read scope key: %w", err)
		}
		keys = append(keys, key)
	}
	return keys, rows.Err()
}

func (service *Service) SyncUserScope(ctx context.Context, userID int64) error {
	unlock := service.LockScope(userID)
	defer unlock()
	return service.syncKeyFlagsLocked(ctx, userID, true)
}

func (service *Service) syncKeyFlagsLocked(ctx context.Context, userID int64, enable bool) error {
	keys, err := service.userScopeKeys(ctx, userID)
	if err != nil || len(keys) == 0 {
		return err
	}
	owner, err := service.identity(ctx, userID)
	if errors.Is(err, sql.ErrNoRows) || err == nil && (owner.id == "" || owner.status != "active") {
		return nil
	}
	if err != nil {
		return err
	}
	_, client, err := service.UserKeysLocked(ctx, userID)
	if err != nil {
		return err
	}
	entitlements, err := service.Entitlements(ctx, userID)
	if err != nil {
		return err
	}
	limit := EffectiveConcurrency(entitlements.Concurrency.Total)
	for _, key := range keys {
		if key.status == "deleted" || key.owner != "" && key.owner != owner.id {
			continue
		}
		if err := service.syncKey(ctx, client, userID, key, limit, enable); err != nil && !vm2api.IsStatus(err, 404) {
			return err
		}
	}
	return nil
}

func (service *Service) syncKey(ctx context.Context, client vm2api.KeyClient, userID int64, key scopeKey, limit int, enable bool) error {
	scope, usable, err := service.KeyScopeFor(ctx, userID, key.mode, store.DecodeIDs(key.accounts))
	if err != nil {
		return err
	}
	active := enable && key.status == "active" && usable
	if err := client.UpdateKey(ctx, key.upstream, vm2api.UpdateKeyInput{Scope: scope, Name: &key.name, MaxConcurrency: &limit, Active: &active}); err != nil {
		return fmt.Errorf("sync upstream key: %w", err)
	}
	_, err = service.db.ExecContext(ctx, `UPDATE api_keys SET synced_concurrency=?,updated_at=? WHERE id=?`, limit, timestamp(service.now()), key.id)
	return err
}

func (service *Service) syncScopeAfterAdd(ctx context.Context, userID int64) error {
	if err := service.SyncUserScope(ctx, userID); err != nil {
		queueCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		_, enqueueErr := service.engine.Enqueue(queueCtx, "sync_scope", jobPayload{UserID: userID}, fmt.Sprintf("sync_scope:%d", userID), service.now(), 8)
		if enqueueErr != nil {
			return errors.Join(err, fmt.Errorf("queue scope sync: %w", enqueueErr))
		}
	}
	return nil
}
