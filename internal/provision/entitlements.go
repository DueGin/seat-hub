package provision

import (
	"context"
	"fmt"

	"github.com/DueGin/vm2api-portal/internal/vm2api"
)

type Concurrency struct {
	Total        int `json:"total"`
	Base         int `json:"base"`
	Purchased    int `json:"purchased"`
	FromAccounts int `json:"from_accounts"`
}

type Capacity struct {
	Total int `json:"total"`
	Used  int `json:"used"`
}

type Entitlements struct {
	Concurrency    Concurrency `json:"concurrency"`
	HostedSeats    Capacity    `json:"hosted_seats"`
	RentalAccounts Capacity    `json:"rental_accounts"`
	AccountsActive int         `json:"accounts_active"`
}

func (service *Service) Entitlements(ctx context.Context, userID int64) (Entitlements, error) {
	var result Entitlements
	settings, err := service.site(ctx)
	if err != nil {
		return result, err
	}
	now := timestamp(service.now())
	err = service.db.QueryRowContext(ctx, `SELECT COALESCE(SUM(CASE WHEN kind='concurrency' THEN quantity ELSE 0 END),0), COALESCE(SUM(CASE WHEN kind='hosted_seat' THEN quantity ELSE 0 END),0), COALESCE(SUM(CASE WHEN kind='rental_account' THEN quantity ELSE 0 END),0) FROM subscriptions WHERE user_id=? AND status='active' AND julianday(starts_at)<=julianday(?) AND julianday(expires_at)>julianday(?)`, userID, now, now).Scan(&result.Concurrency.Purchased, &result.HostedSeats.Total, &result.RentalAccounts.Total)
	if err != nil {
		return result, fmt.Errorf("sum subscriptions: %w", err)
	}
	var activeHosted, activeRental int
	err = service.db.QueryRowContext(ctx, `SELECT COALESCE(SUM(CASE WHEN kind='hosted' AND status<>'released' THEN 1 ELSE 0 END),0), COALESCE(SUM(CASE WHEN kind='rental' AND status<>'released' THEN 1 ELSE 0 END),0), COALESCE(SUM(CASE WHEN kind='hosted' AND status='active' THEN 1 ELSE 0 END),0), COALESCE(SUM(CASE WHEN kind='rental' AND status='active' THEN 1 ELSE 0 END),0) FROM resources WHERE user_id=?`, userID).Scan(&result.HostedSeats.Used, &result.RentalAccounts.Used, &activeHosted, &activeRental)
	if err != nil {
		return result, fmt.Errorf("count resources: %w", err)
	}
	result.Concurrency.Base = settings.BaseConcurrency
	result.Concurrency.FromAccounts = settings.ConcurrencyPerHostedSeat*activeHosted + settings.ConcurrencyPerRental*activeRental
	result.Concurrency.Total = max(0, result.Concurrency.Base+result.Concurrency.Purchased+result.Concurrency.FromAccounts)
	result.AccountsActive = activeHosted + activeRental
	return result, nil
}

func (service *Service) SyncConcurrency(ctx context.Context, userID int64) error {
	service.mu.Lock()
	defer service.mu.Unlock()
	return service.syncConcurrency(ctx, userID)
}

func (service *Service) syncConcurrency(ctx context.Context, userID int64) error {
	entitlements, err := service.Entitlements(ctx, userID)
	if err != nil {
		return err
	}
	rows, err := service.db.QueryContext(ctx, `SELECT id,vm2api_key_id FROM api_keys WHERE user_id=? AND status='active' AND synced_concurrency<>?`, userID, entitlements.Concurrency.Total)
	if err != nil {
		return fmt.Errorf("list keys for sync: %w", err)
	}
	type key struct {
		id       int64
		upstream string
	}
	keys := make([]key, 0)
	for rows.Next() {
		var item key
		if err := rows.Scan(&item.id, &item.upstream); err != nil {
			rows.Close()
			return fmt.Errorf("read sync key: %w", err)
		}
		keys = append(keys, item)
	}
	readErr := rows.Err()
	rows.Close()
	if readErr != nil {
		return fmt.Errorf("read sync keys: %w", readErr)
	}
	for _, item := range keys {
		limit := entitlements.Concurrency.Total
		if err := service.client.UpdateKey(ctx, item.upstream, vm2api.UpdateKeyInput{MaxConcurrency: &limit}); err != nil {
			return fmt.Errorf("sync key concurrency: %w", err)
		}
		if _, err := service.db.ExecContext(ctx, `UPDATE api_keys SET synced_concurrency=?,updated_at=? WHERE id=? AND status='active'`, entitlements.Concurrency.Total, timestamp(service.now()), item.id); err != nil {
			return fmt.Errorf("save synced concurrency: %w", err)
		}
	}
	return nil
}
