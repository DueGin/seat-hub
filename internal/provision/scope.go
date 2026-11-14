package provision

// Account scope for portal keys (docs/KEY_SCOPE.md).
//
// One vm2api service user owns every portal VM and every upstream key. Each
// portal user gets one vm2api account pool holding the VMs they currently
// hold; each portal key is limited either to that pool (mode "all") or to an
// explicit allowlist (mode "selected"). Isolation between portal users rests
// entirely on pool membership and allowlists, so removals are synchronous and
// ordered before a VM is handed back or deleted.
//
// Locking: lock order is service.mu → scope lock (per user) → service-user
// lock. Code in this file never takes service.mu, so callers holding a scope
// lock (internal/portal key writes) may call KeyScopeFor safely.

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/DueGin/seat-hub/internal/store"
	"github.com/DueGin/seat-hub/internal/vm2api"
)

// Key scope modes stored in api_keys.scope_mode.
const (
	ScopeAll      = "all"
	ScopeSelected = "selected"
)

var (
	// ErrScopeInvalid: unknown mode, or a selected account that is not the
	// user's (wrong owner, unknown id). Mapped to 422 key_scope_invalid.
	ErrScopeInvalid = errors.New("invalid key scope")
	// ErrScopeEmpty: selected mode with no schedulable account in the
	// selection. Mapped to 422 key_scope_empty.
	ErrScopeEmpty = errors.New("key scope has no usable account")
)

// scopeLocks serialises scope writes per portal user. The zero value is ready.
type scopeLocks struct {
	mu    sync.Mutex
	locks map[int64]*sync.Mutex
}

func (locks *scopeLocks) lock(userID int64) func() {
	locks.mu.Lock()
	if locks.locks == nil {
		locks.locks = map[int64]*sync.Mutex{}
	}
	lock := locks.locks[userID]
	if lock == nil {
		lock = &sync.Mutex{}
		locks.locks[userID] = lock
	}
	locks.mu.Unlock()
	lock.Lock()
	return lock.Unlock
}

// LockScope takes the per-user scope lock and returns its release. Key writes
// in internal/portal hold it from computing a scope until the key row is
// saved, so a concurrent SyncUserScope (which takes the same lock) always
// sees the finished key. Never call SyncUserScope while holding it.
func (service *Service) LockScope(userID int64) func() {
	service.scopeWrites.RLock()
	unlock := service.scopes.lock(userID)
	return func() {
		unlock()
		service.scopeWrites.RUnlock()
	}
}

// ServiceUser returns the vm2api user id that owns every portal VM and key,
// creating the user (role=user, vm_create_quota=0) on first use and saving it
// with store.SaveServiceIdentity. Safe for concurrent callers.
func (service *Service) ServiceUser(ctx context.Context) (string, error) {
	service.serviceUserMu.Lock()
	defer service.serviceUserMu.Unlock()
	identity, err := store.LoadServiceIdentity(ctx, service.db)
	if err != nil {
		return "", fmt.Errorf("load service user: %w", err)
	}
	if identity.UserID != "" {
		return identity.UserID, nil
	}
	var suffix [4]byte
	var password [32]byte
	if _, err := rand.Read(suffix[:]); err != nil {
		return "", fmt.Errorf("generate service username: %w", err)
	}
	if _, err := rand.Read(password[:]); err != nil {
		return "", fmt.Errorf("generate service password: %w", err)
	}
	username := "seathub_" + hex.EncodeToString(suffix[:])
	user, err := service.client.CreateUser(ctx, username, hex.EncodeToString(password[:]))
	if err != nil {
		return "", fmt.Errorf("create service user: %w", err)
	}
	if user.ID == "" {
		return "", errors.New("create service user returned an empty id")
	}
	saveCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	if err := store.SaveServiceIdentity(saveCtx, service.db, store.ServiceIdentity{UserID: user.ID, Username: username}); err != nil {
		return "", fmt.Errorf("save service user: %w", err)
	}
	return user.ID, nil
}

// SchedulableVMs returns resource id → vm id for the user's accounts that
// belong in the user's scope: kind hosted or rental, vm_id set, status in
// ('active','needs_reauth','suspended'). Database only; it is on the gateway
// hot path (through the resolver cache).
func (service *Service) SchedulableVMs(ctx context.Context, userID int64) (map[int64]string, error) {
	rows, err := service.db.QueryContext(ctx, `SELECT id,vm_id FROM resources WHERE user_id=? AND kind IN ('hosted','rental') AND status IN ('active','needs_reauth','suspended') AND vm_id IS NOT NULL AND vm_id<>'' ORDER BY id`, userID)
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
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read schedulable resources: %w", err)
	}
	return accounts, nil
}

// ValidateKeyScope normalises a requested scope before a key write. mode ""
// means ScopeAll. For ScopeSelected every account id must be a non-released
// resource of the user (else ErrScopeInvalid) and at least one must be
// schedulable (else ErrScopeEmpty). Returns the normalised mode and the
// sorted, de-duplicated ids (always empty for ScopeAll).
func (service *Service) ValidateKeyScope(ctx context.Context, userID int64, mode string, accountIDs []int64) (string, []int64, error) {
	if mode == "" || mode == ScopeAll {
		return ScopeAll, []int64{}, nil
	}
	if mode != ScopeSelected {
		return "", nil, ErrScopeInvalid
	}
	for _, id := range accountIDs {
		if id <= 0 {
			return "", nil, ErrScopeInvalid
		}
		var count int
		if err := service.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM resources WHERE id=? AND user_id=? AND status<>'released'`, id, userID).Scan(&count); err != nil {
			return "", nil, fmt.Errorf("validate scope account: %w", err)
		}
		if count == 0 {
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

// KeyScopeFor computes the upstream scope for one key of the user. It ensures
// the user's pool exists (creating it, possibly empty, named
// "sh-<service user id prefix>-u<user id>"). ScopeAll → the pool. ScopeSelected
// → AllowedVMs = selected ∩ schedulable; when that is empty it returns the
// pool scope and usable=false so the caller keeps the key disabled upstream
// (never a stale allowlist). usable is always true for ScopeAll.
func (service *Service) KeyScopeFor(ctx context.Context, userID int64, mode string, accountIDs []int64) (scope vm2api.KeyScope, usable bool, err error) {
	if mode == "" {
		mode = ScopeAll
	}
	if mode != ScopeAll && mode != ScopeSelected {
		return scope, false, ErrScopeInvalid
	}
	poolID, err := service.ensureUserPool(ctx, userID)
	if err != nil {
		return scope, false, err
	}
	scope.PoolID = poolID
	if mode == ScopeAll {
		return scope, true, nil
	}
	accounts, err := service.SchedulableVMs(ctx, userID)
	if err != nil {
		return scope, false, err
	}
	selected := make(map[int64]string)
	for _, id := range accountIDs {
		if vmID := accounts[id]; vmID != "" {
			selected[id] = vmID
		}
	}
	if len(selected) == 0 {
		return scope, false, nil
	}
	return vm2api.KeyScope{AllowedVMs: scopeVMIDs(selected)}, true, nil
}

// SyncUserScope makes vm2api match the portal for one user: pool members =
// SchedulableVMs, and for every non-deleted key its scope, max concurrency and
// Active = key active && user active && usable. It takes the user's scope
// lock. Removal paths (release, delete hosted) call it after marking the
// resource releasing and before deleting or returning the VM; an error there
// aborts the release so it is retried.
func (service *Service) SyncUserScope(ctx context.Context, userID int64) error {
	unlock := service.LockScope(userID)
	defer unlock()
	return service.syncUserScope(ctx, userID)
}

// DeleteUserScope removes the user's pool after their keys are gone (user
// deletion). Missing pools are not an error.
func (service *Service) DeleteUserScope(ctx context.Context, userID int64) error {
	unlock := service.LockScope(userID)
	defer unlock()
	var poolID string
	if err := service.db.QueryRowContext(ctx, `SELECT COALESCE(vm2api_pool_id,'') FROM users WHERE id=?`, userID).Scan(&poolID); err != nil {
		return fmt.Errorf("read user pool: %w", err)
	}
	if poolID == "" {
		return nil
	}
	if err := service.client.DeletePool(ctx, poolID); err != nil && !vm2api.IsStatus(err, 404) {
		return fmt.Errorf("delete user pool: %w", err)
	}
	if _, err := service.db.ExecContext(ctx, `UPDATE users SET vm2api_pool_id=NULL,updated_at=? WHERE id=?`, timestamp(service.now()), userID); err != nil {
		return fmt.Errorf("clear user pool: %w", err)
	}
	return nil
}

// ReconcileScopes is the scope_reconcile job (every 10 minutes):
// SyncUserScope for every user with keys or resources; disable service-user
// keys the portal does not know; move portal VMs whose owner drifted back to
// the service user.
func (service *Service) ReconcileScopes(ctx context.Context) error {
	users, err := service.scopeUsers(ctx, `SELECT id FROM users WHERE EXISTS (SELECT 1 FROM api_keys WHERE user_id=users.id) OR EXISTS (SELECT 1 FROM resources WHERE user_id=users.id) ORDER BY id`)
	if err != nil {
		return err
	}
	owner, err := service.ServiceUser(ctx)
	if err != nil {
		return err
	}
	var failures []error
	// Finish any migration that failed at start first. A user still on a
	// legacy vm2api user keeps their VMs there: moving those VMs to the
	// service user below would cut off their not-yet-migrated keys.
	if err := service.MigrateLegacyScopes(ctx); err != nil {
		failures = append(failures, err)
	}
	for _, userID := range users {
		if err := service.SyncUserScope(ctx, userID); err != nil {
			failures = append(failures, fmt.Errorf("sync user %d scope: %w", userID, err))
		}
	}
	// Wait for all in-flight portal key writes before identifying orphans.
	service.scopeWrites.Lock()
	keys, err := service.client.ListKeys(ctx, owner)
	if err != nil {
		failures = append(failures, fmt.Errorf("list service keys: %w", err))
	} else {
		for _, key := range keys {
			var known int
			err := service.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM api_keys WHERE vm2api_key_id=? AND status<>'deleted'`, key.ID).Scan(&known)
			if err == nil && known == 0 && key.Active {
				active := false
				err = service.client.UpdateKey(ctx, key.ID, vm2api.UpdateKeyInput{Active: &active})
			}
			if err != nil && !vm2api.IsStatus(err, 404) {
				failures = append(failures, fmt.Errorf("reconcile key %s: %w", key.ID, err))
			}
		}
	}
	service.scopeWrites.Unlock()
	vms, err := service.client.ListVMs(ctx, vm2api.VMFilter{})
	if err != nil {
		failures = append(failures, fmt.Errorf("list scope vms: %w", err))
		return errors.Join(failures...)
	}
	indexed := make(map[string]vm2api.VM, len(vms))
	for _, vm := range vms {
		indexed[vm.ID] = vm
	}
	for _, userID := range users {
		unlock := service.LockScope(userID)
		var legacy string
		err := service.db.QueryRowContext(ctx, `SELECT COALESCE(vm2api_user_id,'') FROM users WHERE id=?`, userID).Scan(&legacy)
		if err != nil {
			failures = append(failures, fmt.Errorf("read user %d legacy owner: %w", userID, err))
			unlock()
			continue
		}
		if legacy != "" {
			unlock()
			continue
		}
		accounts, err := service.SchedulableVMs(ctx, userID)
		if err == nil {
			for _, vmID := range scopeVMIDs(accounts) {
				if vm, exists := indexed[vmID]; exists && vm.OwnerUserID != owner {
					if err := service.client.SetVMOwner(ctx, vmID, &owner); err != nil {
						failures = append(failures, fmt.Errorf("repair vm %s owner: %w", vmID, err))
					}
				}
			}
		} else {
			failures = append(failures, err)
		}
		unlock()
	}
	return errors.Join(failures...)
}

// MigrateLegacyScopes is the scope_migrate job, run once at start and
// idempotent: re-create legacy upstream keys under the service user (the
// sk-pt- secret is unchanged), move the user's VMs to the service user,
// disable the old per-user vm2api user and clear users.vm2api_user_id.
func (service *Service) MigrateLegacyScopes(ctx context.Context) error {
	users, err := service.scopeUsers(ctx, `SELECT id FROM users WHERE COALESCE(vm2api_user_id,'')<>'' OR EXISTS (SELECT 1 FROM api_keys WHERE user_id=users.id AND (upstream_owner='' OR previous_vm2api_key_ids<>'[]')) ORDER BY id`)
	if err != nil {
		return err
	}
	var failures []error
	for _, userID := range users {
		unlock := service.LockScope(userID)
		err := service.migrateUserScope(ctx, userID)
		unlock()
		if err != nil {
			failures = append(failures, fmt.Errorf("migrate user %d scope: %w", userID, err))
		}
	}
	return errors.Join(failures...)
}

// Pool creation has its own lock because KeyScopeFor is also called while a
// caller already holds the user's scope lock.
// ensureUserPool returns the user's pool id, creating the pool with the user's
// current schedulable VMs as members (or adopting one left behind by a crash
// between create and save). A saved id is trusted: callers that write members
// recreate the pool when vm2api answers 404. Callers hold the user's scope lock.
func (service *Service) ensureUserPool(ctx context.Context, userID int64) (string, error) {
	unlock := service.pools.lock(userID)
	defer unlock()
	var saved string
	if err := service.db.QueryRowContext(ctx, `SELECT COALESCE(vm2api_pool_id,'') FROM users WHERE id=?`, userID).Scan(&saved); err != nil {
		return "", fmt.Errorf("read user pool: %w", err)
	}
	if saved != "" {
		return saved, nil
	}
	return service.createUserPool(ctx, userID)
}

func (service *Service) createUserPool(ctx context.Context, userID int64) (string, error) {
	name, err := service.poolName(ctx, userID)
	if err != nil {
		return "", err
	}
	accounts, err := service.SchedulableVMs(ctx, userID)
	if err != nil {
		return "", err
	}
	members := scopeVMIDs(accounts)
	pool, err := service.client.CreatePool(ctx, name, members)
	if vm2api.IsCode(err, "vm_pool_name_taken") {
		pools, listErr := service.client.ListPools(ctx)
		if listErr != nil {
			return "", fmt.Errorf("adopt user pool: %w", listErr)
		}
		for _, candidate := range pools {
			if candidate.Name == name {
				pool, err = service.client.SetPoolMembers(ctx, candidate.ID, members)
				break
			}
		}
	}
	if err != nil {
		return "", fmt.Errorf("create user pool: %w", err)
	}
	if pool.ID == "" {
		return "", errors.New("create user pool returned an empty id")
	}
	if _, err := service.db.ExecContext(ctx, `UPDATE users SET vm2api_pool_id=?,updated_at=? WHERE id=?`, pool.ID, timestamp(service.now()), userID); err != nil {
		return "", fmt.Errorf("save user pool: %w", err)
	}
	return pool.ID, nil
}

// poolName is "sh-<deployment tag>-u<user id>". The tag is the random suffix
// of the service username, so two seat-hub deployments sharing one vm2api
// never collide; vm2api caps pool names at 40 characters.
func (service *Service) poolName(ctx context.Context, userID int64) (string, error) {
	owner, err := service.ServiceUser(ctx)
	if err != nil {
		return "", err
	}
	identity, err := store.LoadServiceIdentity(ctx, service.db)
	if err != nil {
		return "", err
	}
	tag := strings.TrimPrefix(identity.Username, "seathub_")
	if tag == "" || tag == identity.Username {
		tag = owner
	}
	if len(tag) > 12 {
		tag = tag[len(tag)-12:]
	}
	return fmt.Sprintf("sh-%s-u%d", tag, userID), nil
}

func scopeVMIDs(accounts map[int64]string) []string {
	ids := make([]string, 0, len(accounts))
	seen := make(map[string]bool)
	for _, vmID := range accounts {
		if !seen[vmID] {
			ids = append(ids, vmID)
			seen[vmID] = true
		}
	}
	sort.Strings(ids)
	return ids
}

func (service *Service) setUserPoolMembers(ctx context.Context, userID int64) error {
	accounts, err := service.SchedulableVMs(ctx, userID)
	if err != nil {
		return err
	}
	poolID, err := service.ensureUserPool(ctx, userID)
	if err != nil {
		return err
	}
	ids := scopeVMIDs(accounts)
	_, err = service.client.SetPoolMembers(ctx, poolID, ids)
	if vm2api.IsStatus(err, 404) {
		// The saved pool is gone upstream (deleted by hand): forget it and
		// create a fresh one with the right members.
		if _, clearErr := service.db.ExecContext(ctx, `UPDATE users SET vm2api_pool_id=NULL,updated_at=? WHERE id=? AND vm2api_pool_id=?`, timestamp(service.now()), userID, poolID); clearErr != nil {
			return fmt.Errorf("forget missing pool: %w", clearErr)
		}
		_, err = service.ensureUserPool(ctx, userID)
	}
	if err != nil {
		if vm2api.IsCode(err, "vm_in_other_pool") {
			// Some upstream responses omit vm_id from their error envelope.
			// Name the actual conflicting VM when the pool inventory is available.
			if pools, listErr := service.client.ListPools(ctx); listErr == nil {
				for _, pool := range pools {
					if pool.ID == poolID {
						continue
					}
					for _, vmID := range pool.VMIDs {
						for _, desired := range ids {
							if vmID == desired {
								return fmt.Errorf("vm %s belongs to foreign pool %s: %w", vmID, pool.ID, err)
							}
						}
					}
				}
			}
		}
		return fmt.Errorf("sync pool members %v: %w", ids, err)
	}
	return nil
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
	keys := make([]scopeKey, 0)
	for rows.Next() {
		var key scopeKey
		if err := rows.Scan(&key.id, &key.upstream, &key.name, &key.status, &key.mode, &key.accounts, &key.owner, &key.previous); err != nil {
			return nil, fmt.Errorf("read scope key: %w", err)
		}
		keys = append(keys, key)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read scope keys: %w", err)
	}
	return keys, nil
}

func (service *Service) syncUserScope(ctx context.Context, userID int64) error {
	// A user with no pool, no live key and no account has nothing upstream to
	// keep in step; do not create an empty pool for every registered user.
	var pool string
	var keyCount, accountCount int
	if err := service.db.QueryRowContext(ctx, `SELECT COALESCE(vm2api_pool_id,''),
		(SELECT COUNT(*) FROM api_keys WHERE user_id=users.id AND status<>'deleted'),
		(SELECT COUNT(*) FROM resources WHERE user_id=users.id AND status<>'released')
		FROM users WHERE id=?`, userID).Scan(&pool, &keyCount, &accountCount); err != nil {
		return fmt.Errorf("read scope user: %w", err)
	}
	if pool == "" && keyCount == 0 && accountCount == 0 {
		return nil
	}
	owner, err := service.ServiceUser(ctx)
	if err != nil {
		return err
	}
	if err := service.setUserPoolMembers(ctx, userID); err != nil {
		return err
	}
	var status string
	if err := service.db.QueryRowContext(ctx, `SELECT status FROM users WHERE id=?`, userID).Scan(&status); err != nil {
		return fmt.Errorf("read scope user: %w", err)
	}
	entitlements, err := service.Entitlements(ctx, userID)
	if err != nil {
		return err
	}
	limit := EffectiveConcurrency(entitlements.Concurrency.Total)
	keys, err := service.userScopeKeys(ctx, userID)
	if err != nil {
		return err
	}
	for _, key := range keys {
		if key.status == "deleted" || key.owner != owner {
			continue
		}
		scope, usable, err := service.KeyScopeFor(ctx, userID, key.mode, store.DecodeIDs(key.accounts))
		if err != nil {
			return err
		}
		active := key.status == "active" && status == "active" && usable
		if err := service.client.UpdateKey(ctx, key.upstream, vm2api.UpdateKeyInput{Scope: &scope, MaxConcurrency: &limit, Active: &active}); err != nil {
			if !vm2api.IsStatus(err, 404) {
				return fmt.Errorf("sync key %s scope: %w", key.upstream, err)
			}
		}
		if _, err := service.db.ExecContext(ctx, `UPDATE api_keys SET synced_concurrency=?,updated_at=? WHERE id=?`, limit, timestamp(service.now()), key.id); err != nil {
			return fmt.Errorf("save synced scope: %w", err)
		}
	}
	return nil
}

func (service *Service) scopeUsers(ctx context.Context, query string) ([]int64, error) {
	rows, err := service.db.QueryContext(ctx, query)
	if err != nil {
		return nil, fmt.Errorf("list scope users: %w", err)
	}
	defer rows.Close()
	users := make([]int64, 0)
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("read scope user: %w", err)
		}
		users = append(users, id)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read scope users: %w", err)
	}
	return users, nil
}

func (service *Service) migrateUserScope(ctx context.Context, userID int64) error {
	owner, err := service.ServiceUser(ctx)
	if err != nil {
		return err
	}
	if _, err := service.ensureUserPool(ctx, userID); err != nil {
		return err
	}
	var legacy, status string
	if err := service.db.QueryRowContext(ctx, `SELECT COALESCE(vm2api_user_id,''),status FROM users WHERE id=?`, userID).Scan(&legacy, &status); err != nil {
		return fmt.Errorf("read legacy user: %w", err)
	}
	// Check preconditions before touching anything upstream: moving the VMs
	// and then failing to re-key would strand the legacy keys.
	if service.Box == nil {
		var pending int
		if err := service.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM api_keys WHERE user_id=? AND upstream_owner='' AND status<>'deleted'`, userID).Scan(&pending); err != nil {
			return fmt.Errorf("count legacy keys: %w", err)
		}
		if pending > 0 {
			return errors.New("secret box is required for scope migration")
		}
	}
	// Include slots still onboarding: they are live but not yet schedulable.
	rows, err := service.db.QueryContext(ctx, `SELECT vm_id FROM resources WHERE user_id=? AND status NOT IN ('released','releasing') AND vm_id IS NOT NULL AND vm_id<>''`, userID)
	if err != nil {
		return fmt.Errorf("list legacy vms: %w", err)
	}
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return fmt.Errorf("read legacy vm: %w", err)
		}
		ids = append(ids, id)
	}
	readErr := rows.Err()
	rows.Close()
	if readErr != nil {
		return fmt.Errorf("read legacy vms: %w", readErr)
	}
	for _, id := range ids {
		if err := service.client.SetVMOwner(ctx, id, &owner); err != nil && !vm2api.IsStatus(err, 404) {
			return fmt.Errorf("migrate vm %s owner: %w", id, err)
		}
	}
	if err := service.setUserPoolMembers(ctx, userID); err != nil {
		return err
	}
	entitlements, err := service.Entitlements(ctx, userID)
	if err != nil {
		return err
	}
	limit := EffectiveConcurrency(entitlements.Concurrency.Total)
	keys, err := service.userScopeKeys(ctx, userID)
	if err != nil {
		return err
	}
	for _, key := range keys {
		if key.owner != "" || key.status == "deleted" {
			continue
		}
		if service.Box == nil {
			return errors.New("secret box is required for scope migration")
		}
		scope, usable, err := service.KeyScopeFor(ctx, userID, key.mode, store.DecodeIDs(key.accounts))
		if err != nil {
			return err
		}
		upstream, err := service.client.CreateKey(ctx, vm2api.CreateKeyInput{Name: key.name, UserID: owner, MaxConcurrency: limit, Scope: scope})
		if err != nil {
			return fmt.Errorf("create migrated key: %w", err)
		}
		sealed, sealErr := service.Box.Seal(upstream.Secret)
		active := key.status == "active" && status == "active" && usable
		previous := store.EncodeStrings(append(store.DecodeStrings(key.previous), key.upstream))
		if sealErr == nil {
			saveCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
			_, sealErr = service.db.ExecContext(saveCtx, `UPDATE api_keys SET vm2api_key_id=?,vm2api_secret_enc=?,upstream_owner=?,previous_vm2api_key_ids=?,synced_concurrency=?,updated_at=? WHERE id=?`, upstream.ID, sealed, owner, previous, limit, timestamp(service.now()), key.id)
			cancel()
		}
		if sealErr != nil {
			cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
			cleanupErr := service.client.DeleteKey(cleanupCtx, upstream.ID)
			cancel()
			if vm2api.IsStatus(cleanupErr, 404) {
				cleanupErr = nil
			}
			return errors.Join(fmt.Errorf("save migrated key: %w", sealErr), cleanupErr)
		}
		if err := service.client.UpdateKey(ctx, upstream.ID, vm2api.UpdateKeyInput{Active: &active}); err != nil {
			return fmt.Errorf("set migrated key active state: %w", err)
		}
		if err := service.client.DeleteKey(ctx, key.upstream); err != nil && !vm2api.IsStatus(err, 404) {
			return fmt.Errorf("delete legacy key %s: %w", key.upstream, err)
		}
	}
	// Replay after a failure between replacing the portal row and deleting the
	// old key. The previous-id ledger survives that boundary and process crashes.
	if err := service.syncUserScope(ctx, userID); err != nil {
		return err
	}
	keys, err = service.userScopeKeys(ctx, userID)
	if err != nil {
		return err
	}
	for _, key := range keys {
		old := store.DecodeStrings(key.previous)
		if key.owner == "" && key.status == "deleted" {
			old = append(old, key.upstream)
		}
		for _, id := range old {
			if id == key.upstream && key.status != "deleted" {
				continue
			}
			if err := service.client.DeleteKey(ctx, id); err != nil && !vm2api.IsStatus(err, 404) {
				return fmt.Errorf("delete legacy key %s: %w", id, err)
			}
		}
		if key.owner == "" && key.status == "deleted" {
			if _, err := service.db.ExecContext(ctx, `UPDATE api_keys SET upstream_owner=?,updated_at=? WHERE id=?`, owner, timestamp(service.now()), key.id); err != nil {
				return fmt.Errorf("mark deleted legacy key migrated: %w", err)
			}
		}
	}
	if legacy != "" && legacy != owner {
		if err := service.client.SetUserEnabled(ctx, legacy, false); err != nil && !vm2api.IsStatus(err, 404) {
			return fmt.Errorf("disable legacy user: %w", err)
		}
	}
	if _, err := service.db.ExecContext(ctx, `UPDATE users SET vm2api_user_id=NULL,updated_at=? WHERE id=?`, timestamp(service.now()), userID); err != nil {
		return fmt.Errorf("clear legacy user: %w", err)
	}
	return nil
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
