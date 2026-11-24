package provision

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"time"

	"github.com/DueGin/seat-hub/internal/secret"
	"github.com/DueGin/seat-hub/internal/store"
	"github.com/DueGin/seat-hub/internal/vm2api"
)

type identity struct {
	id, name, passwordEnc, status, pool string
}

func (service *Service) identity(ctx context.Context, userID int64) (identity, error) {
	var owner identity
	err := service.db.QueryRowContext(ctx, `SELECT COALESCE(vm2api_user_id,''), COALESCE(vm2api_username,''),
		COALESCE(vm2api_password_enc,''), status, COALESCE(vm2api_pool_id,'') FROM users WHERE id=?`, userID).
		Scan(&owner.id, &owner.name, &owner.passwordEnc, &owner.status, &owner.pool)
	if err != nil {
		return owner, fmt.Errorf("read upstream identity: %w", err)
	}
	return owner, nil
}

func (service *Service) EnsureUpstreamUser(ctx context.Context, userID int64) (string, error) {
	unlock := service.LockScope(userID)
	defer unlock()
	owner, err := service.ensureIdentity(ctx, userID)
	return owner.id, err
}

func (service *Service) ensureIdentity(ctx context.Context, userID int64) (identity, error) {
	owner, err := service.identity(ctx, userID)
	if err != nil || owner.id != "" {
		return owner, err
	}
	if owner.name != "" {
		users, err := service.client.ListUsers(ctx)
		if err != nil {
			return owner, fmt.Errorf("find recorded upstream user: %w", err)
		}
		for _, candidate := range users {
			if candidate.Username != owner.name || candidate.Role != "user" {
				continue
			}
			if candidate.Enabled != (owner.status == "active") {
				if err := service.client.SetUserEnabled(ctx, candidate.ID, owner.status == "active"); err != nil {
					return owner, fmt.Errorf("restore recorded upstream user: %w", err)
				}
			}
			owner.id = candidate.ID
			return owner, service.saveIdentity(ctx, userID, owner)
		}
	}
	password, sealed, err := service.newPassword()
	if err != nil {
		return owner, err
	}
	base := fmt.Sprintf("portal_%d", userID)
	name := base
	for attempts := 0; attempts < 8; attempts++ {
		upstream, err := service.client.CreateUser(ctx, name, password)
		if vm2api.IsStatus(err, 409) {
			var suffix [2]byte
			_, _ = rand.Read(suffix[:])
			name = base + "_" + hex.EncodeToString(suffix[:])
			continue
		}
		if err != nil {
			return owner, fmt.Errorf("create upstream user: %w", err)
		}
		if upstream.ID == "" {
			return owner, errors.New("create upstream user returned no id")
		}
		owner.id, owner.name, owner.passwordEnc = upstream.ID, name, sealed
		if err := service.saveIdentity(ctx, userID, owner); err != nil {
			return owner, err
		}
		if owner.status != "active" {
			if err := service.client.SetUserEnabled(ctx, owner.id, false); err != nil {
				return owner, fmt.Errorf("disable new upstream user: %w", err)
			}
		}
		return owner, nil
	}
	return owner, errors.New("could not allocate an upstream username")
}

func (service *Service) saveIdentity(ctx context.Context, userID int64, owner identity) error {
	work, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	_, err := service.db.ExecContext(work, `UPDATE users SET vm2api_user_id=?,vm2api_username=?,vm2api_password_enc=?,updated_at=? WHERE id=?`, owner.id, owner.name, owner.passwordEnc, timestamp(service.now()), userID)
	if err != nil {
		return fmt.Errorf("save upstream identity: %w", err)
	}
	return nil
}

func (service *Service) newPassword() (string, string, error) {
	if service.Box == nil {
		return "", "", errors.New("secret box is required for upstream credentials")
	}
	password, err := secret.RandomToken(32)
	if err != nil {
		return "", "", fmt.Errorf("generate upstream password: %w", err)
	}
	sealed, err := service.Box.Seal(password)
	if err != nil {
		return "", "", fmt.Errorf("seal upstream password: %w", err)
	}
	return password, sealed, nil
}

// UserKeysLocked requires LockScope. It keeps identity creation and credential
// recovery serialized with key changes and account-status transitions.
func (service *Service) UserKeysLocked(ctx context.Context, userID int64) (string, vm2api.KeyClient, error) {
	owner, err := service.ensureIdentity(ctx, userID)
	if err != nil {
		return "", nil, err
	}
	if owner.status != "active" {
		return owner.id, nil, ErrInvalidState
	}
	keys, err := service.loginManaged(ctx, owner, func(ctx context.Context, owner identity) error {
		return service.saveIdentity(ctx, userID, owner)
	})
	return owner.id, keys, err
}

func (service *Service) loginManaged(ctx context.Context, owner identity, save func(context.Context, identity) error) (vm2api.KeyClient, error) {
	if service.Box == nil {
		return nil, errors.New("secret box is required for upstream credentials")
	}
	if owner.id == "" || owner.name == "" {
		return nil, errors.New("missing recorded upstream identity")
	}
	if password, err := service.Box.Open(owner.passwordEnc); err == nil {
		keys, err := service.client.ForUser(ctx, owner.id, owner.name, password)
		if err == nil || !vm2api.IsStatus(err, 401) {
			return keys, err
		}
	}
	users, err := service.client.ListUsers(ctx)
	if err != nil {
		return nil, fmt.Errorf("verify upstream identity: %w", err)
	}
	for _, candidate := range users {
		if candidate.ID != owner.id || candidate.Username != owner.name || candidate.Role != "user" {
			continue
		}
		if !candidate.Enabled {
			return nil, &vm2api.Error{Status: 401, Code: "user_disabled", Message: "recorded upstream member is disabled"}
		}
		password, sealed, err := service.newPassword()
		if err != nil {
			return nil, err
		}
		if err := service.client.SetUserPassword(ctx, owner.id, password); err != nil {
			return nil, fmt.Errorf("reset upstream password: %w", err)
		}
		owner.passwordEnc = sealed
		if err := save(ctx, owner); err != nil {
			return nil, err
		}
		return service.client.ForUser(ctx, owner.id, owner.name, password)
	}
	return nil, &vm2api.Error{Status: 404, Code: "user_not_found", Message: "recorded upstream member was not found"}
}

func (service *Service) legacyKeySession(ctx context.Context, legacy store.ServiceIdentity, users []vm2api.User) (vm2api.KeyClient, error) {
	if legacy.UserID == "" {
		return nil, nil
	}
	if users == nil {
		var err error
		users, err = service.client.ListUsers(ctx)
		if err != nil {
			return nil, fmt.Errorf("list upstream users: %w", err)
		}
	}
	for _, candidate := range users {
		if candidate.ID != legacy.UserID || candidate.Username != legacy.Username || candidate.Role != "user" || !candidate.Enabled {
			continue
		}
		service.legacyMu.Lock()
		defer service.legacyMu.Unlock()
		latest, err := store.LoadServiceIdentity(ctx, service.db)
		if err != nil || latest.UserID != legacy.UserID || latest.Username != legacy.Username {
			return nil, err
		}
		return service.loginManaged(ctx, identity{id: latest.UserID, name: latest.Username, passwordEnc: latest.PasswordEnc}, func(ctx context.Context, owner identity) error {
			latest.PasswordEnc = owner.passwordEnc
			return store.SaveServiceIdentity(ctx, service.db, latest)
		})
	}
	return nil, nil
}

func (service *Service) legacyOwner(ctx context.Context) (string, error) {
	owner, err := store.LoadServiceIdentity(ctx, service.db)
	return owner.UserID, err
}

func (service *Service) recordedOwner(ctx context.Context, userID int64) (string, error) {
	owner, err := service.identity(ctx, userID)
	return owner.id, err
}

func (service *Service) DeleteReferencedKeysLocked(ctx context.Context, userID int64, keyIDs []string) error {
	if len(keyIDs) == 0 {
		return nil
	}
	owner, err := service.identity(ctx, userID)
	if err != nil {
		return err
	}
	if owner.id != "" && owner.status == "active" {
		_, keys, err := service.UserKeysLocked(ctx, userID)
		if err != nil && !vm2api.IsStatus(err, 404) && !vm2api.IsCode(err, "user_disabled") {
			return err
		}
		if keys != nil {
			for _, keyID := range keyIDs {
				if err := deleteProvenKey(ctx, keys, keyID); err != nil {
					return err
				}
			}
		}
	}
	return service.deleteLegacyKeys(ctx, keyIDs)
}

func (service *Service) deleteLegacyKeys(ctx context.Context, keyIDs []string) error {
	if len(keyIDs) == 0 {
		return nil
	}
	legacy, err := store.LoadServiceIdentity(ctx, service.db)
	if err != nil {
		return err
	}
	keys, sessionErr := service.legacyKeySession(ctx, legacy, nil)
	var failures []error
	for _, id := range keyIDs {
		if err := cleanupLegacyKey(ctx, service.client, keys, id); err != nil {
			failures = append(failures, err)
		}
	}
	return errors.Join(sessionErr, errors.Join(failures...))
}

func (service *Service) SetUserStatus(ctx context.Context, userID int64, status string) error {
	unlock := service.LockScope(userID)
	defer unlock()
	owner, err := service.identity(ctx, userID)
	if err != nil {
		return err
	}
	if owner.id != "" && status != "active" && owner.status == "active" {
		// Disable key flags while the owner's session still works. Regardless of
		// key-write failures the Master disable below revokes all access.
		_ = service.syncKeyFlagsLocked(ctx, userID, false)
	}
	if _, err := service.db.ExecContext(ctx, `UPDATE users SET status=?,updated_at=? WHERE id=?`, status, timestamp(service.now()), userID); err != nil {
		return fmt.Errorf("save user status: %w", err)
	}
	if owner.id != "" {
		if err := service.client.SetUserEnabled(ctx, owner.id, status == "active"); err != nil {
			return errors.Join(fmt.Errorf("sync upstream user status: %w", err), service.EnqueueScopeReconcile(ctx))
		}
	}
	if status == "active" {
		return errors.Join(service.syncKeyFlagsLocked(ctx, userID, true), service.EnqueueScopeReconcile(ctx))
	}
	keys, err := service.userScopeKeys(ctx, userID)
	if err != nil {
		return err
	}
	var legacyIDs []string
	for _, key := range keys {
		legacyIDs = append(legacyIDs, store.DecodeStrings(key.previous)...)
		if key.upstream != "" {
			legacyIDs = append(legacyIDs, key.upstream)
		}
	}
	if err := service.deleteLegacyKeys(ctx, legacyIDs); err != nil {
		return errors.Join(err, service.EnqueueScopeReconcile(ctx))
	}
	return nil
}

func (service *Service) mayMutateVM(ctx context.Context, account Account) (bool, error) {
	usage, err := service.client.GetAccountUsage(ctx, account.VMID)
	if err != nil {
		if vm2api.IsStatus(err, 404) {
			return false, nil
		}
		return false, fmt.Errorf("verify upstream vm owner: %w", err)
	}
	owner, err := service.recordedOwner(ctx, account.UserID)
	if err != nil {
		return false, err
	}
	if usage.VM.OwnerUserID == "" || usage.VM.OwnerUserID == owner {
		return true, nil
	}
	legacy, err := service.legacyOwner(ctx)
	if err != nil || usage.VM.OwnerUserID == legacy {
		return err == nil, err
	}
	names, _ := service.ownerNames(ctx)
	return false, service.addScopeWarning(ctx, vmConflict(account.UserID, usage.VM, names))
}
