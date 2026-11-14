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
)

const keyColumns = `id,name,prefix,status,created_at,last_used_at`

func scanKey(row interface{ Scan(...any) error }) (api.KeyDTO, error) {
	var item api.KeyDTO
	if err := row.Scan(&item.ID, &item.Name, &item.Prefix, &item.Status, &item.CreatedAt, &item.LastUsedAt); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return item, ErrKeyNotFound
		}
		return item, fmt.Errorf("read key: %w", err)
	}
	return item, nil
}

func (service *Service) List(ctx context.Context, userID int64) ([]api.KeyDTO, error) {
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
		items = append(items, item)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate keys: %w", err)
	}
	return items, nil
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

func (service *Service) Create(ctx context.Context, userID int64, name string) (api.KeyDTO, string, error) {
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
	upstreamUser, err := service.provisioning.EnsureUpstreamUser(ctx, userID)
	if err != nil {
		return api.KeyDTO{}, "", fmt.Errorf("ensure upstream user: %w", err)
	}
	entitlements, err := service.provisioning.Entitlements(ctx, userID)
	if err != nil {
		return api.KeyDTO{}, "", fmt.Errorf("load entitlements: %w", err)
	}
	token, err := secret.RandomToken(32)
	if err != nil {
		return api.KeyDTO{}, "", fmt.Errorf("generate portal key: %w", err)
	}
	plaintext := "sk-pt-" + token
	limit := provision.EffectiveConcurrency(entitlements.Concurrency.Total)
	upstream, err := service.client.CreateKey(ctx, vm2api.CreateKeyInput{Name: name, UserID: upstreamUser, MaxConcurrency: limit})
	if err != nil {
		return api.KeyDTO{}, "", fmt.Errorf("create upstream key: %w", err)
	}
	encrypted, err := service.box.Seal(upstream.Secret)
	if err != nil {
		return api.KeyDTO{}, "", service.discardKey(ctx, upstream.ID, fmt.Errorf("encrypt upstream key: %w", err))
	}
	now := store.FmtTime(service.now())
	var id int64
	err = service.db.QueryRowContext(ctx, `INSERT INTO api_keys(user_id,name,key_hash,prefix,vm2api_key_id,vm2api_secret_enc,synced_concurrency,status,created_at,updated_at)
		SELECT ?,?,?,?,?,?,?,'active',?,? WHERE (SELECT COUNT(*) FROM api_keys WHERE user_id=? AND status='active')<20 RETURNING id`,
		userID, name, service.box.MAC(plaintext), plaintext[:10], upstream.ID, encrypted, limit, now, now, userID).Scan(&id)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			err = ErrKeyLimit
		}
		return api.KeyDTO{}, "", service.discardKey(ctx, upstream.ID, fmt.Errorf("save key: %w", err))
	}
	return api.KeyDTO{ID: id, Name: name, Prefix: plaintext[:10], Status: "active", CreatedAt: now}, plaintext, nil
}

func (service *Service) discardKey(ctx context.Context, id string, cause error) error {
	cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	if err := service.client.DeleteKey(cleanupCtx, id); err != nil && !vm2api.IsStatus(err, 404) {
		return errors.Join(cause, fmt.Errorf("discard upstream key: %w", err))
	}
	return cause
}

func (service *Service) Update(ctx context.Context, userID, id int64, name, status string) (api.KeyDTO, error) {
	if utf8.RuneCountInString(name) > 40 {
		return api.KeyDTO{}, ErrInvalidKeyName
	}
	if status != "" && status != "active" && status != "disabled" {
		return api.KeyDTO{}, ErrInvalidKeyStatus
	}
	service.mu.Lock()
	defer service.mu.Unlock()
	item, err := scanKey(service.db.QueryRowContext(ctx, `SELECT `+keyColumns+` FROM api_keys WHERE id=? AND user_id=? AND status<>'deleted'`, id, userID))
	if err != nil {
		return api.KeyDTO{}, err
	}
	var upstreamID string
	var concurrency int
	if err := service.db.QueryRowContext(ctx, `SELECT vm2api_key_id,synced_concurrency FROM api_keys WHERE id=? AND user_id=?`, id, userID).Scan(&upstreamID, &concurrency); err != nil {
		return api.KeyDTO{}, fmt.Errorf("read upstream key: %w", err)
	}
	originalName, originalConcurrency := item.Name, concurrency
	originalActive := item.Status == "active"
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
		active := status == "active"
		input.Active = &active
		item.Status = status
	}
	if name == "" && status == "" {
		return item, nil
	}
	if err := service.client.UpdateKey(ctx, upstreamID, input); err != nil {
		return api.KeyDTO{}, fmt.Errorf("update upstream key: %w", err)
	}
	err = service.db.QueryRowContext(ctx, `UPDATE api_keys SET name=?,status=?,synced_concurrency=?,updated_at=? WHERE id=? AND user_id=? AND status<>'deleted'
		AND (?=0 OR (SELECT COUNT(*) FROM api_keys WHERE user_id=? AND status='active')<20) RETURNING id`,
		item.Name, item.Status, concurrency, store.FmtTime(service.now()), id, userID, enabling, userID).Scan(&id)
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
		if cleanupErr := service.client.UpdateKey(cleanupCtx, upstreamID, vm2api.UpdateKeyInput{Name: &originalName, Active: &originalActive, MaxConcurrency: &originalConcurrency}); cleanupErr != nil {
			return api.KeyDTO{}, errors.Join(cause, fmt.Errorf("restore upstream key: %w", cleanupErr))
		}
		return api.KeyDTO{}, cause
	}
	return item, nil
}

func (service *Service) Delete(ctx context.Context, userID, id int64) error {
	service.mu.Lock()
	defer service.mu.Unlock()
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
