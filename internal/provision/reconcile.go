package provision

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"time"

	"github.com/DueGin/seat-hub/internal/store"
	"github.com/DueGin/seat-hub/internal/vm2api"
)

const retainedPoolsKey = "owner_retained_pools"

type ownerPass struct {
	service    *Service
	legacy     store.ServiceIdentity
	legacyKeys vm2api.KeyClient
	legacyErr  error
	users      map[string]vm2api.User
	names      map[string]string
	vms        map[string]vm2api.VM
	retained   map[string]int64
	referenced map[string]int64
	items      []ScopeWarning
}

func (service *Service) ReconcileOwners(ctx context.Context) error {
	service.mu.Lock()
	defer service.mu.Unlock()
	legacy, err := store.LoadServiceIdentity(ctx, service.db)
	if err != nil {
		return err
	}
	users, err := service.client.ListUsers(ctx)
	if err != nil {
		return fmt.Errorf("list upstream users: %w", err)
	}
	vms, err := service.client.ListVMs(ctx, vm2api.VMFilter{})
	if err != nil {
		return fmt.Errorf("list upstream accounts: %w", err)
	}
	pass := ownerPass{service: service, legacy: legacy, users: map[string]vm2api.User{}, names: map[string]string{}, vms: map[string]vm2api.VM{}, retained: map[string]int64{}, referenced: map[string]int64{}, items: []ScopeWarning{}}
	for _, user := range users {
		pass.users[user.ID], pass.names[user.ID] = user, user.Username
	}
	for _, vm := range vms {
		pass.vms[vm.ID] = vm
	}
	var raw string
	if err := service.db.QueryRowContext(ctx, `SELECT value FROM settings WHERE key=?`, retainedPoolsKey).Scan(&raw); err == nil {
		if err := json.Unmarshal([]byte(raw), &pass.retained); err != nil {
			return fmt.Errorf("decode retained pools: %w", err)
		}
	} else if !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	ids, err := queryAll(ctx, service.db, `SELECT id FROM users WHERE COALESCE(vm2api_user_id,'')<>'' OR COALESCE(vm2api_pool_id,'')<>'' OR EXISTS (SELECT 1 FROM api_keys WHERE user_id=users.id AND status<>'deleted') OR EXISTS (SELECT 1 FROM resources WHERE user_id=users.id AND status<>'released') ORDER BY id`, func(rows *sql.Rows) (id int64, err error) {
		err = rows.Scan(&id)
		return
	})
	if err != nil {
		return err
	}
	type reference struct {
		userID            int64
		current, previous string
	}
	refs, err := queryAll(ctx, service.db, `SELECT user_id,vm2api_key_id,previous_vm2api_key_ids FROM api_keys`, func(rows *sql.Rows) (ref reference, err error) {
		err = rows.Scan(&ref.userID, &ref.current, &ref.previous)
		return
	})
	if err != nil {
		return err
	}
	for _, ref := range refs {
		for _, key := range append(store.DecodeStrings(ref.previous), ref.current) {
			if key != "" {
				pass.referenced[key] = ref.userID
			}
		}
	}
	pass.legacyKeys, pass.legacyErr = service.legacyKeySession(ctx, legacy, users)
	var failures []error
	for _, userID := range ids {
		unlock := service.LockScope(userID)
		err := pass.user(ctx, userID)
		unlock()
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			failures = append(failures, fmt.Errorf("reconcile user %d: %w", userID, err))
		}
	}
	poolIDs := make([]string, 0, len(pass.retained))
	for id := range pass.retained {
		poolIDs = append(poolIDs, id)
	}
	sort.Strings(poolIDs)
	for _, id := range poolIDs {
		if err := pass.deletePool(ctx, pass.retained[id], id); err != nil {
			failures = append(failures, err)
		}
	}
	if err := pass.retireService(ctx); err != nil {
		failures = append(failures, err)
	}
	stock, err := queryAll(ctx, service.db, `SELECT vm_id FROM rental_pool WHERE status='available' ORDER BY vm_id`, func(rows *sql.Rows) (id string, err error) {
		err = rows.Scan(&id)
		return
	})
	if err != nil {
		failures = append(failures, err)
	}
	for _, id := range stock {
		if vm := pass.vms[id]; vm.OwnerUserID != "" {
			pass.items = append(pass.items, rentalWarning(vm, pass.names))
		}
	}
	service.warningMu.Lock()
	defer service.warningMu.Unlock()
	return errors.Join(errors.Join(failures...), service.saveScopeWarnings(ctx, ScopeWarnings{CheckedAt: timestamp(service.now()), Items: pass.items}))
}

func (pass *ownerPass) user(ctx context.Context, userID int64) error {
	service := pass.service
	owner, err := service.ensureIdentity(ctx, userID)
	if err != nil {
		return err
	}
	active := owner.status == "active"
	if before, ok := pass.users[owner.id]; ok && before.Enabled != active {
		if err := service.client.SetUserEnabled(ctx, owner.id, active); err != nil {
			return fmt.Errorf("reconcile upstream user status: %w", err)
		}
		before.Enabled = active
		pass.users[owner.id] = before
	}
	accounts, err := queryAll(ctx, service.db, `SELECT vm_id FROM resources WHERE user_id=? AND status NOT IN ('released','releasing') AND COALESCE(vm_id,'')<>'' ORDER BY id`, func(rows *sql.Rows) (id string, err error) {
		err = rows.Scan(&id)
		return
	}, userID)
	if err != nil {
		return err
	}
	for _, id := range accounts {
		vm, exists := pass.vms[id]
		if !exists || vm.OwnerUserID == owner.id {
			continue
		}
		if vm.OwnerUserID != "" && vm.OwnerUserID != pass.legacy.UserID {
			pass.items = append(pass.items, vmConflict(userID, vm, pass.names))
			continue
		}
		live, err := service.client.GetAccountUsage(ctx, id)
		if vm2api.IsStatus(err, 404) {
			delete(pass.vms, id)
			continue
		}
		if err != nil {
			return fmt.Errorf("verify vm owner before migration: %w", err)
		}
		vm = live.VM
		pass.vms[id] = vm
		if vm.OwnerUserID == owner.id {
			continue
		}
		if vm.OwnerUserID != "" && vm.OwnerUserID != pass.legacy.UserID {
			pass.items = append(pass.items, vmConflict(userID, vm, pass.names))
			continue
		}
		if err := service.client.SetVMOwner(ctx, id, &owner.id); err != nil && !vm2api.IsStatus(err, 404) {
			return fmt.Errorf("reconcile vm owner: %w", err)
		}
		vm.OwnerUserID = owner.id
		pass.vms[id] = vm
	}
	keys, err := service.userScopeKeys(ctx, userID)
	if err != nil {
		return err
	}
	if !active {
		return pass.cleanupDisabled(ctx, userID, keys, owner.pool)
	}
	_, client, err := service.UserKeysLocked(ctx, userID)
	if err != nil {
		return err
	}
	live, err := client.ListKeys(ctx, owner.id)
	if err != nil {
		return fmt.Errorf("list upstream member keys: %w", err)
	}
	owned := make(map[string]vm2api.KeyInfo, len(live))
	for _, key := range live {
		owned[key.ID] = key
	}
	entitlements, err := service.Entitlements(ctx, userID)
	if err != nil {
		return err
	}
	limit := EffectiveConcurrency(entitlements.Concurrency.Total)
	var failures []error
	current := map[string]bool{}
	for _, key := range keys {
		for _, id := range append(store.DecodeStrings(key.previous), key.upstream) {
			if id != "" {
				pass.referenced[id] = userID
			}
		}
		if key.status != "deleted" {
			current[key.upstream] = true
		}
	}
	for _, key := range keys {
		previous := store.DecodeStrings(key.previous)
		if key.status == "deleted" {
			previous = append(previous, key.upstream)
		} else if upstream, ok := owned[key.upstream]; !ok || upstream.PoolID != "" {
			if err := service.replaceKey(ctx, client, owner.id, userID, key, limit); err != nil {
				failures = append(failures, err)
				continue
			}
			delete(current, key.upstream)
			previous = append(previous, key.upstream)
		} else {
			if err := service.syncKey(ctx, client, userID, key, limit, true); err != nil {
				failures = append(failures, err)
			}
			if key.owner != owner.id {
				if _, err := service.db.ExecContext(ctx, `UPDATE api_keys SET upstream_owner=? WHERE id=?`, owner.id, key.id); err != nil {
					failures = append(failures, err)
				}
			}
		}
		for _, id := range previous {
			if id == "" || current[id] {
				continue
			}
			if _, exists := owned[id]; exists && pass.referenced[id] == userID {
				if err := deleteProvenKey(ctx, client, id); err != nil {
					failures = append(failures, err)
				}
			} else if err := pass.cleanupLegacyKey(ctx, id); err != nil {
				failures = append(failures, err)
			}
		}
	}
	for _, key := range live {
		ref, known := pass.referenced[key.ID]
		if known && ref != userID {
			pass.items = append(pass.items, ScopeWarning{Kind: KeyDrifted, UserID: ref, KeyID: key.ID, Message: fmt.Sprintf("旧密钥「%s」(%s) 已归属 vm2api 用户「%s」，门面未修改。", key.Name, key.ID, owner.name)})
		} else if !known && key.Active {
			disabled := false
			if err := client.UpdateKey(ctx, key.ID, vm2api.UpdateKeyInput{Active: &disabled}); err != nil && !vm2api.IsStatus(err, 404) {
				failures = append(failures, err)
			}
		}
	}
	if len(failures) == 0 && owner.pool != "" {
		failures = append(failures, pass.clearPool(ctx, userID, owner.pool))
	}
	return errors.Join(failures...)
}

func (pass *ownerPass) cleanupDisabled(ctx context.Context, userID int64, keys []scopeKey, pool string) error {
	for _, key := range keys {
		for _, id := range append(store.DecodeStrings(key.previous), key.upstream) {
			if err := pass.cleanupLegacyKey(ctx, id); err != nil {
				return err
			}
		}
	}
	if pool != "" {
		return pass.clearPool(ctx, userID, pool)
	}
	return nil
}

func (service *Service) replaceKey(ctx context.Context, client vm2api.KeyClient, owner string, userID int64, key scopeKey, limit int) error {
	scope, usable, err := service.KeyScopeFor(ctx, userID, key.mode, store.DecodeIDs(key.accounts))
	if err != nil {
		return err
	}
	if scope == nil {
		// A newly created key has no prior allowlist to retain. Store it only
		// after disabling it; a later selection installs its real allowlist.
		scope = &vm2api.KeyScope{All: true}
	}
	upstream, err := client.CreateKey(ctx, vm2api.CreateKeyInput{Name: key.name, UserID: owner, MaxConcurrency: limit, Scope: *scope})
	if err != nil {
		return fmt.Errorf("replace upstream key: %w", err)
	}
	work, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	defer cancel()
	if !usable || key.status != "active" {
		active := false
		if err := client.UpdateKey(work, upstream.ID, vm2api.UpdateKeyInput{Active: &active}); err != nil {
			return errors.Join(err, deleteProvenKey(work, client, upstream.ID))
		}
	}
	sealed, err := service.Box.Seal(upstream.Secret)
	if err == nil {
		previous := store.EncodeStrings(append(store.DecodeStrings(key.previous), key.upstream))
		_, err = service.db.ExecContext(work, `UPDATE api_keys SET vm2api_key_id=?,vm2api_secret_enc=?,upstream_owner=?,previous_vm2api_key_ids=?,synced_concurrency=?,updated_at=? WHERE id=?`, upstream.ID, sealed, owner, previous, limit, timestamp(service.now()), key.id)
	}
	if err != nil {
		return errors.Join(fmt.Errorf("save replacement key: %w", err), deleteProvenKey(work, client, upstream.ID))
	}
	return nil
}

func deleteProvenKey(ctx context.Context, client vm2api.KeyClient, id string) error {
	if err := client.DeleteKey(ctx, id); err != nil && !vm2api.IsStatus(err, 404) {
		return fmt.Errorf("delete upstream key: %w", err)
	}
	return nil
}

func (pass *ownerPass) cleanupLegacyKey(ctx context.Context, id string) error {
	return errors.Join(cleanupLegacyKey(ctx, pass.service.client, pass.legacyKeys, id), pass.legacyErr)
}

func cleanupLegacyKey(ctx context.Context, master vm2api.Client, legacy vm2api.KeyClient, id string) error {
	if id == "" {
		return nil
	}
	if legacy != nil {
		if err := deleteProvenKey(ctx, legacy, id); err != nil {
			return err
		}
	}
	return serviceDeleteOwnerless(ctx, master, id)
}

func serviceDeleteOwnerless(ctx context.Context, client vm2api.Client, id string) error {
	key, err := client.GetKey(ctx, id)
	if vm2api.IsStatus(err, 404) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("verify ownerless key: %w", err)
	}
	if key.UserID != "" {
		return nil
	}
	return deleteProvenKey(ctx, client, id)
}

func (pass *ownerPass) clearPool(ctx context.Context, userID int64, id string) error {
	if err := pass.deletePool(ctx, userID, id); err != nil {
		return err
	}
	_, err := pass.service.db.ExecContext(ctx, `UPDATE users SET vm2api_pool_id=NULL WHERE id=? AND vm2api_pool_id=?`, userID, id)
	return err
}

func (pass *ownerPass) deletePool(ctx context.Context, userID int64, id string) error {
	service := pass.service
	err := service.client.DeletePool(ctx, id)
	if err == nil || vm2api.IsStatus(err, 404) {
		if _, retained := pass.retained[id]; retained {
			delete(pass.retained, id)
			return service.saveOwnerSetting(ctx, retainedPoolsKey, pass.retained)
		}
		return nil
	}
	if !vm2api.IsCode(err, "vm_pool_in_use") {
		return fmt.Errorf("delete legacy pool: %w", err)
	}
	if _, retained := pass.retained[id]; !retained {
		pass.retained[id] = userID
		if err := service.saveOwnerSetting(ctx, retainedPoolsKey, pass.retained); err != nil {
			return err
		}
	}
	name := id
	if pools, err := service.client.ListPools(ctx); err == nil {
		for _, pool := range pools {
			if pool.ID == id {
				name = pool.Name
				break
			}
		}
	}
	for _, warning := range pass.items {
		if warning.Kind == PoolInUse && warning.UserID == userID && warning.Message == fmt.Sprintf("旧账号池「%s」(%s) 仍被密钥引用，门面未删除。", name, id) {
			return nil
		}
	}
	pass.items = append(pass.items, ScopeWarning{Kind: PoolInUse, UserID: userID, Message: fmt.Sprintf("旧账号池「%s」(%s) 仍被密钥引用，门面未删除。", name, id)})
	return nil
}

func (pass *ownerPass) retireService(ctx context.Context) error {
	if pass.legacyKeys == nil {
		return pass.legacyErr
	}
	keys, err := pass.legacyKeys.ListKeys(ctx, pass.legacy.UserID)
	if err != nil || len(keys) != 0 {
		return err
	}
	for _, vm := range pass.vms {
		if vm.OwnerUserID == pass.legacy.UserID {
			return nil
		}
	}
	err = pass.service.client.SetUserEnabled(ctx, pass.legacy.UserID, false)
	if vm2api.IsStatus(err, 404) {
		return nil
	}
	return err
}

func queryAll[T any](ctx context.Context, db *sql.DB, query string, scan func(*sql.Rows) (T, error), args ...any) ([]T, error) {
	rows, err := db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var result []T
	for rows.Next() {
		value, err := scan(rows)
		if err != nil {
			return nil, err
		}
		result = append(result, value)
	}
	return result, rows.Err()
}
