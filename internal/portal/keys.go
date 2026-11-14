package portal

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
	"unicode/utf8"

	"github.com/DueGin/seat-hub/internal/api"
	"github.com/DueGin/seat-hub/internal/provision"
	"github.com/DueGin/seat-hub/internal/secret"
	"github.com/DueGin/seat-hub/internal/store"
	"github.com/DueGin/seat-hub/internal/vm2api"
)

var (
	ErrKeyNotFound      = api.Err(errors.New("key not found"), 404, "key_not_found", "密钥不存在")
	ErrKeyLimit         = api.Err(errors.New("active key limit reached"), 422, "key_limit", "最多启用 20 个密钥")
	ErrInvalidKeyName   = api.Err(errors.New("invalid key name"), 422, "invalid_name", "名称最多 40 个字符")
	ErrInvalidKeyStatus = api.Err(errors.New("invalid key status"), 422, "invalid_status", "状态只能是 active 或 disabled")
	ErrKeyMigrating     = api.Err(errors.New("legacy key scope pending migration"), 409, "key_migrating", "密钥正在升级，请稍后再试")
)

const keyColumns = `id,name,prefix,status,created_at,last_used_at,scope_mode,scope_accounts`

func scanKey(row interface{ Scan(...any) error }) (api.KeyDTO, error) {
	var item api.KeyDTO
	var accounts string
	if err := row.Scan(&item.ID, &item.Name, &item.Prefix, &item.Status, &item.CreatedAt, &item.LastUsedAt, &item.Scope.Mode, &accounts); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return item, ErrKeyNotFound
		}
		return item, fmt.Errorf("read key: %w", err)
	}
	item.Scope.Accounts = store.DecodeIDs(accounts)
	return item, nil
}

func (service *Service) List(ctx context.Context, userID int64) ([]api.KeyDTO, error) {
	vms, err := service.provisioning.SchedulableVMs(ctx, userID)
	if err != nil {
		return nil, fmt.Errorf("load schedulable accounts: %w", err)
	}
	rows, err := service.db.QueryContext(ctx, `SELECT `+keyColumns+` FROM api_keys WHERE user_id=? AND status<>'deleted' ORDER BY id DESC`, userID)
	if err != nil {
		return nil, fmt.Errorf("list keys: %w", err)
	}
	defer rows.Close()
	items := make([]api.KeyDTO, 0)
	for rows.Next() {
		item, err := scanKey(rows)
		if err != nil {
			return nil, err
		}
		item.Scope.Usable = scopeUsable(item.Scope, vms)
		items = append(items, item)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate keys: %w", err)
	}
	return items, nil
}

func scopeUsable(scope api.KeyScopeDTO, vms map[int64]string) bool {
	if scope.Mode == provision.ScopeAll {
		return true
	}
	for _, id := range scope.Accounts {
		if vms[id] != "" {
			return true
		}
	}
	return false
}

func scopeError(err error) error {
	switch {
	case errors.Is(err, provision.ErrScopeInvalid):
		return api.Err(err, 422, "key_scope_invalid", "所选账号无效")
	case errors.Is(err, provision.ErrScopeEmpty):
		return api.Err(err, 422, "key_scope_empty", "所选账号当前都不可用，请至少选择一个可用账号")
	default:
		return err
	}
}

func (service *Service) userActive(ctx context.Context, userID int64) (bool, error) {
	var status string
	err := service.db.QueryRowContext(ctx, `SELECT status FROM users WHERE id=?`, userID).Scan(&status)
	if errors.Is(err, sql.ErrNoRows) {
		return false, ErrUserNotFound
	}
	if err != nil {
		return false, fmt.Errorf("read user status: %w", err)
	}
	return status == "active", nil
}

func (service *Service) checkKeyLimit(ctx context.Context, userID int64) error {
	var count int
	if err := service.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM api_keys WHERE user_id=? AND status='active'`, userID).Scan(&count); err != nil {
		return fmt.Errorf("count active keys: %w", err)
	}
	if count >= 20 {
		return ErrKeyLimit
	}
	return nil
}

func (service *Service) Create(ctx context.Context, userID int64, in api.KeyInput) (api.KeyDTO, string, error) {
	name := in.Name
	if utf8.RuneCountInString(name) > 40 {
		return api.KeyDTO{}, "", ErrInvalidKeyName
	}
	if name == "" {
		name = "默认"
	}
	service.mu.Lock()
	defer service.mu.Unlock()
	if err := service.checkKeyLimit(ctx, userID); err != nil {
		return api.KeyDTO{}, "", err
	}
	userActive, err := service.userActive(ctx, userID)
	if err != nil {
		return api.KeyDTO{}, "", err
	}
	mode, accounts, err := service.provisioning.ValidateKeyScope(ctx, userID, in.Scope.Mode, in.Scope.Accounts)
	if err != nil {
		return api.KeyDTO{}, "", scopeError(err)
	}
	entitlements, err := service.provisioning.Entitlements(ctx, userID)
	if err != nil {
		return api.KeyDTO{}, "", fmt.Errorf("load entitlements: %w", err)
	}
	unlock := service.provisioning.LockScope(userID)
	defer unlock()
	userActive, err = service.userActive(ctx, userID)
	if err != nil {
		return api.KeyDTO{}, "", err
	}
	upstreamUser, err := service.provisioning.ServiceUser(ctx)
	if err != nil {
		return api.KeyDTO{}, "", fmt.Errorf("ensure service user: %w", err)
	}
	scope, usable, err := service.provisioning.KeyScopeFor(ctx, userID, mode, accounts)
	if err != nil {
		return api.KeyDTO{}, "", fmt.Errorf("compute key scope: %w", err)
	}
	token, err := secret.RandomToken(32)
	if err != nil {
		return api.KeyDTO{}, "", fmt.Errorf("generate portal key: %w", err)
	}
	plaintext := "sk-pt-" + token
	limit := provision.EffectiveConcurrency(entitlements.Concurrency.Total)
	upstream, err := service.client.CreateKey(ctx, vm2api.CreateKeyInput{Name: name, UserID: upstreamUser, MaxConcurrency: limit, Scope: scope})
	if err != nil {
		return api.KeyDTO{}, "", fmt.Errorf("create upstream key: %w", err)
	}
	if !usable || !userActive {
		active := false
		if err := service.client.UpdateKey(ctx, upstream.ID, vm2api.UpdateKeyInput{Active: &active}); err != nil {
			return api.KeyDTO{}, "", service.discardKey(ctx, upstream.ID, fmt.Errorf("disable unusable key: %w", err))
		}
	}
	encrypted, err := service.box.Seal(upstream.Secret)
	if err != nil {
		return api.KeyDTO{}, "", service.discardKey(ctx, upstream.ID, fmt.Errorf("encrypt upstream key: %w", err))
	}
	now := store.FmtTime(service.now())
	var id int64
	err = service.db.QueryRowContext(ctx, `INSERT INTO api_keys(user_id,name,key_hash,prefix,vm2api_key_id,vm2api_secret_enc,synced_concurrency,status,created_at,updated_at,scope_mode,scope_accounts,upstream_owner)
		SELECT ?,?,?,?,?,?,?,'active',?,?,?,?,? WHERE (SELECT COUNT(*) FROM api_keys WHERE user_id=? AND status='active')<20 RETURNING id`,
		userID, name, service.box.MAC(plaintext), plaintext[:10], upstream.ID, encrypted, limit, now, now, mode, store.EncodeIDs(accounts), upstreamUser, userID).Scan(&id)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			err = ErrKeyLimit
		}
		return api.KeyDTO{}, "", service.discardKey(ctx, upstream.ID, fmt.Errorf("save key: %w", err))
	}
	return api.KeyDTO{ID: id, Name: name, Prefix: plaintext[:10], Status: "active", CreatedAt: now,
		Scope: api.KeyScopeDTO{Mode: mode, Accounts: store.NormalizeIDs(accounts), Usable: usable}}, plaintext, nil
}

func (service *Service) discardKey(ctx context.Context, id string, cause error) error {
	cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	if err := service.client.DeleteKey(cleanupCtx, id); err != nil && !vm2api.IsStatus(err, 404) {
		return errors.Join(cause, fmt.Errorf("discard upstream key: %w", err))
	}
	return cause
}

func (service *Service) Update(ctx context.Context, userID, id int64, in api.KeyPatch) (api.KeyDTO, error) {
	name, status := "", ""
	if in.Name != nil {
		name = *in.Name
	}
	if in.Status != nil {
		status = *in.Status
	}
	if utf8.RuneCountInString(name) > 40 {
		return api.KeyDTO{}, ErrInvalidKeyName
	}
	if in.Status != nil && status != "active" && status != "disabled" {
		return api.KeyDTO{}, ErrInvalidKeyStatus
	}
	service.mu.Lock()
	defer service.mu.Unlock()
	unlock := service.provisioning.LockScope(userID)
	defer unlock()
	item, err := scanKey(service.db.QueryRowContext(ctx, `SELECT `+keyColumns+` FROM api_keys WHERE id=? AND user_id=? AND status<>'deleted'`, id, userID))
	if err != nil {
		return api.KeyDTO{}, err
	}
	var upstreamID, owner string
	var concurrency int
	if err := service.db.QueryRowContext(ctx, `SELECT vm2api_key_id,synced_concurrency,upstream_owner FROM api_keys WHERE id=? AND user_id=?`, id, userID).Scan(&upstreamID, &concurrency, &owner); err != nil {
		return api.KeyDTO{}, fmt.Errorf("read upstream key: %w", err)
	}
	if in.Scope != nil {
		if owner == "" {
			return api.KeyDTO{}, ErrKeyMigrating
		}
		mode, accounts, err := service.provisioning.ValidateKeyScope(ctx, userID, in.Scope.Mode, in.Scope.Accounts)
		if err != nil {
			return api.KeyDTO{}, scopeError(err)
		}
		item.Scope.Mode, item.Scope.Accounts = mode, store.NormalizeIDs(accounts)
	}
	vms, err := service.provisioning.SchedulableVMs(ctx, userID)
	if err != nil {
		return api.KeyDTO{}, fmt.Errorf("load schedulable accounts: %w", err)
	}
	item.Scope.Usable = scopeUsable(item.Scope, vms)
	if in.Name == nil && in.Status == nil && in.Scope == nil {
		return item, nil
	}
	original, err := service.client.GetKey(ctx, upstreamID)
	if err != nil {
		return api.KeyDTO{}, fmt.Errorf("read upstream key state: %w", err)
	}
	input := vm2api.UpdateKeyInput{}
	if name != "" {
		item.Name = name
		input.Name = &item.Name
	}
	enabling := status == "active" && item.Status != "active"
	if enabling {
		if err := service.checkKeyLimit(ctx, userID); err != nil {
			return api.KeyDTO{}, err
		}
		entitlements, err := service.provisioning.Entitlements(ctx, userID)
		if err != nil {
			return api.KeyDTO{}, fmt.Errorf("load entitlements: %w", err)
		}
		concurrency = provision.EffectiveConcurrency(entitlements.Concurrency.Total)
		input.MaxConcurrency = &concurrency
	}
	if status != "" {
		item.Status = status
	}
	if owner != "" {
		scope, usable, err := service.provisioning.KeyScopeFor(ctx, userID, item.Scope.Mode, item.Scope.Accounts)
		if err != nil {
			return api.KeyDTO{}, fmt.Errorf("compute updated key scope: %w", err)
		}
		input.Scope, item.Scope.Usable = &scope, usable
	}
	userActive, err := service.userActive(ctx, userID)
	if err != nil {
		return api.KeyDTO{}, err
	}
	active := item.Status == "active" && userActive && item.Scope.Usable
	input.Active = &active
	if err := service.client.UpdateKey(ctx, upstreamID, input); err != nil {
		return api.KeyDTO{}, fmt.Errorf("update upstream key: %w", err)
	}
	err = service.db.QueryRowContext(ctx, `UPDATE api_keys SET name=?,status=?,synced_concurrency=?,scope_mode=?,scope_accounts=?,updated_at=? WHERE id=? AND user_id=? AND status<>'deleted'
		AND (?=0 OR (SELECT COUNT(*) FROM api_keys WHERE user_id=? AND status='active')<20) RETURNING id`,
		item.Name, item.Status, concurrency, item.Scope.Mode, store.EncodeIDs(item.Scope.Accounts), store.FmtTime(service.now()), id, userID, enabling, userID).Scan(&id)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			if enabling {
				err = ErrKeyLimit
			} else {
				err = ErrKeyNotFound
			}
		}
		cause := fmt.Errorf("save updated key: %w", err)
		cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		restore := vm2api.UpdateKeyInput{Name: &original.Name, Active: &original.Active, MaxConcurrency: &original.MaxConcurrency}
		if scope, ok := original.Scope(); ok {
			restore.Scope = &scope
		}
		if cleanupErr := service.client.UpdateKey(cleanupCtx, upstreamID, restore); cleanupErr != nil {
			return api.KeyDTO{}, errors.Join(cause, fmt.Errorf("restore upstream key: %w", cleanupErr))
		}
		return api.KeyDTO{}, cause
	}
	return item, nil
}

func (service *Service) Delete(ctx context.Context, userID, id int64) error {
	service.mu.Lock()
	defer service.mu.Unlock()
	unlock := service.provisioning.LockScope(userID)
	defer unlock()
	var upstreamID string
	err := service.db.QueryRowContext(ctx, `SELECT vm2api_key_id FROM api_keys WHERE id=? AND user_id=? AND status<>'deleted'`, id, userID).Scan(&upstreamID)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrKeyNotFound
	}
	if err != nil {
		return fmt.Errorf("read upstream key: %w", err)
	}
	if err := service.client.DeleteKey(ctx, upstreamID); err != nil && !vm2api.IsStatus(err, 404) {
		return fmt.Errorf("delete upstream key: %w", err)
	}
	if _, err := service.db.ExecContext(ctx, `UPDATE api_keys SET status='deleted',updated_at=? WHERE id=? AND user_id=?`, store.FmtTime(service.now()), id, userID); err != nil {
		return fmt.Errorf("mark key deleted: %w", err)
	}
	return nil
}
