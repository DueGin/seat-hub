package provision

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/DueGin/seat-hub/internal/privacy"
	"github.com/DueGin/seat-hub/internal/vm2api"
)

// FulfilOrder applies a settled order. It is driven by the job engine, but it
// may also be called from a payment callback: the work and any failure record
// must therefore survive cancellation of the caller's request context, or the
// order would stay paid with no trace of what went wrong.
func (service *Service) FulfilOrder(ctx context.Context, orderID int64) error {
	workCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Minute)
	defer cancel()
	service.mu.Lock()
	defer service.mu.Unlock()
	err := service.fulfilOrder(workCtx, orderID)
	if err != nil {
		saveCtx, saveCancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
		defer saveCancel()
		if _, saveErr := service.db.ExecContext(saveCtx, `UPDATE orders SET status='fulfill_failed',fulfil_error=?,updated_at=? WHERE id=? AND status IN ('paid','fulfill_failed')`, err.Error(), timestamp(service.now()), orderID); saveErr != nil {
			return errors.Join(err, fmt.Errorf("record fulfilment error: %w", saveErr))
		}
	}
	return err
}

// EnqueueFulfilOrderTx makes Service an order.AsyncFulfiller: settlement and the
// durable job are written in the caller's transaction.
func (service *Service) EnqueueFulfilOrderTx(ctx context.Context, tx *sql.Tx, orderID int64) error {
	return enqueueTx(ctx, tx, "fulfil_order", jobPayload{OrderID: orderID}, fmt.Sprintf("fulfil_order:%d", orderID), service.now())
}

// AllocateRental tops a rental subscription up to the accounts it is entitled
// to. Grants and expiry extensions create or revive a subscription without an
// order, so without this the user would be entitled to N accounts while holding
// none.
func (service *Service) AllocateRental(ctx context.Context, subscriptionID int64) error {
	service.mu.Lock()
	defer service.mu.Unlock()
	return service.allocateRental(ctx, subscriptionID)
}

func (service *Service) allocateRental(ctx context.Context, subscriptionID int64) error {
	var userID int64
	var quantity int
	var status, expires string
	err := service.db.QueryRowContext(ctx, `SELECT user_id, quantity, status, expires_at FROM subscriptions WHERE id=?`, subscriptionID).Scan(&userID, &quantity, &status, &expires)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrNotFound
	}
	if err != nil {
		return fmt.Errorf("read rental subscription: %w", err)
	}
	if status != "active" {
		return nil
	}
	expiry, err := time.Parse(time.RFC3339, expires)
	if err != nil {
		return fmt.Errorf("parse subscription expiry: %w", err)
	}
	if !expiry.After(service.now()) {
		return nil
	}
	owner, err := service.EnsureUpstreamUser(ctx, userID)
	if err != nil {
		return err
	}
	// Lease first, then talk to vm2api: the upstream calls must not run inside
	// the write transaction, and a failed claim leaves nothing half-created.
	_, excluded, err := service.rentalAvailability(ctx)
	if err != nil {
		return err
	}
	vmIDs, err := service.claimRental(ctx, userID, subscriptionID, quantity, excluded)
	if err != nil || len(vmIDs) == 0 {
		return err
	}
	for _, vmID := range vmIDs {
		if err := service.client.SetVMOwner(ctx, vmID, &owner); err != nil {
			return service.quarantineRental(ctx, vmID, fmt.Errorf("assign rental owner: %w", err))
		}
		if err := service.client.SetSchedulable(ctx, vmID, true); err != nil {
			return service.quarantineRental(ctx, vmID, fmt.Errorf("enable rental scheduling: %w", err))
		}
		service.syncAccountIdentity(ctx, vmID)
	}
	return service.syncScopeAfterAdd(ctx, userID)
}

// syncAccountIdentity copies the signed-in account's masked email and plan
// from vm2api onto the live resource that holds the slot, so the user sees
// which account they have instead of "waiting for authorization". It is
// best-effort: the account works without it and the next sync fills it in.
func (service *Service) syncAccountIdentity(ctx context.Context, vmID string) {
	usage, err := service.client.GetAccountUsage(ctx, vmID)
	if err != nil || usage.VM.EmailMasked == "" && usage.VM.Tier == "" {
		return
	}
	usage.VM.EmailMasked = privacy.MaskEmail(usage.VM.EmailMasked)
	_, _ = service.db.ExecContext(ctx, `UPDATE resources SET
		email_masked=CASE WHEN ?<>'' THEN ? ELSE email_masked END,
		tier=CASE WHEN ?<>'' THEN ? ELSE tier END, updated_at=?
		WHERE vm_id=? AND status NOT IN ('released','releasing')`,
		usage.VM.EmailMasked, usage.VM.EmailMasked, usage.VM.Tier, usage.VM.Tier, timestamp(service.now()), vmID)
}

// claimRental leases the accounts a subscription is missing and commits.
func (service *Service) claimRental(ctx context.Context, userID, subscriptionID int64, quantity int, excluded []string) ([]string, error) {
	tx, err := service.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("begin rental allocation: %w", err)
	}
	defer tx.Rollback()
	vmIDs, err := claimRentalTx(ctx, tx, userID, subscriptionID, quantity, service.now(), excluded)
	if err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("commit rental allocation: %w", err)
	}
	return vmIDs, nil
}

// claimRental leases up to `quantity` minus the live accounts the subscription
// already holds. The upstream owner probe excludes unavailable VMs.
func claimRentalTx(ctx context.Context, tx *sql.Tx, userID, subscriptionID int64, quantity int, now time.Time, excluded []string) ([]string, error) {
	var held int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM resources WHERE subscription_id=? AND status NOT IN ('released','releasing')`, subscriptionID).Scan(&held); err != nil {
		return nil, fmt.Errorf("count rented resources: %w", err)
	}
	needed := max(0, quantity-held)
	if needed == 0 {
		return nil, nil
	}
	assigned := make([]string, 0, needed)
	for len(assigned) < needed {
		var vmID, tier string
		skip := make([]any, 0, len(assigned)+len(excluded))
		for _, item := range assigned {
			skip = append(skip, item)
		}
		for _, item := range excluded {
			skip = append(skip, item)
		}
		exclude := ""
		if len(skip) > 0 {
			exclude = ` AND vm_id NOT IN (` + strings.TrimSuffix(strings.Repeat("?,", len(skip)), ",") + `)`
		}
		err := tx.QueryRowContext(ctx, `SELECT vm_id,tier FROM rental_pool WHERE status='available' AND selfcheck_state='pass'`+exclude+` ORDER BY vm_id LIMIT 1`, skip...).Scan(&vmID, &tier)
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrOutOfStock
		}
		if err != nil {
			return nil, fmt.Errorf("read rental pool: %w", err)
		}
		result, err := tx.ExecContext(ctx, `INSERT INTO resources(user_id,kind,subscription_id,vm_id,label,tier,status,created_at,updated_at) VALUES (?,'rental',?,?,?,?,'active',?,?)`, userID, subscriptionID, vmID, "租赁账号", tier, timestamp(now), timestamp(now))
		if err != nil {
			return nil, fmt.Errorf("create rental resource: %w", err)
		}
		resourceID, err := result.LastInsertId()
		if err != nil {
			return nil, fmt.Errorf("read rental resource id: %w", err)
		}
		claimed, err := tx.ExecContext(ctx, `UPDATE rental_pool SET status='leased',leased_resource_id=?,updated_at=? WHERE vm_id=? AND status='available' AND selfcheck_state='pass'`, resourceID, timestamp(now), vmID)
		if err != nil {
			return nil, fmt.Errorf("lease rental vm: %w", err)
		}
		count, err := claimed.RowsAffected()
		if err != nil {
			return nil, fmt.Errorf("count leased rental vm: %w", err)
		}
		if count == 0 {
			if _, err := tx.ExecContext(ctx, `DELETE FROM resources WHERE id=?`, resourceID); err != nil {
				return nil, fmt.Errorf("discard unclaimed rental resource: %w", err)
			}
			continue
		}
		assigned = append(assigned, vmID)
	}
	return assigned, nil
}

// quarantineRental parks a VM whose upstream assignment failed so it is not
// handed to the next buyer in a half-configured state.
func (service *Service) quarantineRental(ctx context.Context, vmID string, failure error) error {
	if _, err := service.db.ExecContext(ctx, `UPDATE rental_pool SET status='maintenance',note=?,updated_at=? WHERE vm_id=?`, failure.Error(), timestamp(service.now()), vmID); err != nil {
		return errors.Join(failure, fmt.Errorf("quarantine rental vm: %w", err))
	}
	return failure
}

func (service *Service) fulfilOrder(ctx context.Context, orderID int64) (fulfilErr error) {
	var userID int64
	var kind, status string
	var units, duration int
	var renewal sql.NullInt64
	err := service.db.QueryRowContext(ctx, `SELECT user_id,kind,status,units,duration_days,renew_subscription_id FROM orders WHERE id=?`, orderID).Scan(&userID, &kind, &status, &units, &duration, &renewal)
	if err != nil {
		return fmt.Errorf("read fulfilment order: %w", err)
	}
	if status == "fulfilled" {
		return nil
	}
	if status != "paid" && status != "fulfill_failed" {
		return ErrInvalidState
	}
	if units <= 0 || duration <= 0 || (kind != "concurrency" && kind != "hosted_seat" && kind != "rental_account") {
		return ErrInvalidState
	}
	var owner string
	var excluded []string
	if kind == "rental_account" {
		owner, err = service.EnsureUpstreamUser(ctx, userID)
		if err != nil {
			return err
		}
		if _, excluded, err = service.rentalAvailability(ctx); err != nil {
			return err
		}
	}
	tx, err := service.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin fulfilment: %w", err)
	}
	defer tx.Rollback()
	result, err := tx.ExecContext(ctx, `UPDATE orders SET updated_at=? WHERE id=? AND status IN ('paid','fulfill_failed')`, timestamp(service.now()), orderID)
	if err != nil {
		return fmt.Errorf("lock fulfilment order: %w", err)
	}
	count, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("count fulfilment order: %w", err)
	}
	if count == 0 {
		return nil
	}
	now := service.now()
	expires := now.AddDate(0, 0, duration)
	var subscriptionID int64
	if renewal.Valid {
		var oldExpiry, oldKind, oldStatus string
		var quantity int
		if err := tx.QueryRowContext(ctx, `SELECT kind,quantity,status,expires_at FROM subscriptions WHERE id=? AND user_id=?`, renewal.Int64, userID).Scan(&oldKind, &quantity, &oldStatus, &oldExpiry); err != nil {
			return fmt.Errorf("read renewal subscription: %w", err)
		}
		if oldKind != kind || quantity != units || oldStatus == "cancelled" {
			return ErrInvalidState
		}
		previous, err := time.Parse(time.RFC3339, oldExpiry)
		if err != nil {
			return fmt.Errorf("parse subscription expiry: %w", err)
		}
		if previous.After(now) {
			expires = previous.AddDate(0, 0, duration)
		}
		if _, err := tx.ExecContext(ctx, `UPDATE subscriptions SET status='active',expires_at=?,updated_at=? WHERE id=?`, timestamp(expires), timestamp(now), renewal.Int64); err != nil {
			return fmt.Errorf("renew subscription: %w", err)
		}
		subscriptionID = renewal.Int64
	} else {
		result, err := tx.ExecContext(ctx, `INSERT INTO subscriptions(user_id,kind,quantity,status,starts_at,expires_at,source_order_id,created_at,updated_at) VALUES (?,?,?,'active',?,?,?,?,?)`, userID, kind, units, timestamp(now), timestamp(expires), orderID, timestamp(now), timestamp(now))
		if err != nil {
			return fmt.Errorf("create subscription: %w", err)
		}
		subscriptionID, err = result.LastInsertId()
		if err != nil {
			return fmt.Errorf("read subscription id: %w", err)
		}
	}
	assigned := make([]string, 0)
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback()
			cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
			defer cancel()
			fulfilErr = errors.Join(fulfilErr, service.undoRentalAssignments(cleanupCtx, userID, assigned))
		}
	}()
	if kind == "rental_account" {
		// Claim pool rows one at a time with a conditional UPDATE. Reading the
		// available list first and leasing afterwards would let two concurrent
		// fulfillments hand out the same VM; here the loser sees RowsAffected 0
		// and moves on to the next candidate.
		claimed, err := claimRentalTx(ctx, tx, userID, subscriptionID, units, now, excluded)
		if err != nil {
			return err
		}
		for _, vmID := range claimed {
			if err := service.client.SetVMOwner(ctx, vmID, &owner); err != nil {
				return fmt.Errorf("assign rental owner: %w", err)
			}
			assigned = append(assigned, vmID)
			if err := service.client.SetSchedulable(ctx, vmID, true); err != nil {
				return fmt.Errorf("enable rental scheduling: %w", err)
			}
		}
	}
	if err := enqueueTx(ctx, tx, "sync_concurrency", jobPayload{UserID: userID}, fmt.Sprintf("sync_concurrency:%d", userID), now); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE orders SET status='fulfilled',fulfil_error=NULL,fulfilled_at=?,updated_at=? WHERE id=?`, timestamp(now), timestamp(now), orderID); err != nil {
		return fmt.Errorf("complete fulfilment order: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit fulfilment: %w", err)
	}
	committed = true
	return service.syncScopeAfterAdd(ctx, userID)
}

// A rolled-back allocation has no resource rows left. Re-create releasing
// tombstones before scope removal so interrupted compensation has a retryable
// release job and the stock cannot be sold while a stale allowlist exists.
func (service *Service) undoRentalAssignments(ctx context.Context, userID int64, vmIDs []string) error {
	var failures []error
	for _, vmID := range vmIDs {
		var resourceID int64
		now := timestamp(service.now())
		err := service.db.QueryRowContext(ctx, `INSERT INTO resources(user_id,kind,vm_id,status,release_after,created_at,updated_at) VALUES (?,'rental',?,'releasing',?,?,?) RETURNING id`, userID, vmID, now, now, now).Scan(&resourceID)
		if err != nil {
			failures = append(failures, service.quarantineRental(ctx, vmID, fmt.Errorf("record rental compensation: %w", err)))
			continue
		}
		if _, err := service.db.ExecContext(ctx, `UPDATE rental_pool SET status='maintenance',leased_resource_id=?,updated_at=? WHERE vm_id=?`, resourceID, now, vmID); err != nil {
			failures = append(failures, fmt.Errorf("park compensating rental: %w", err))
			continue
		}
		_, queueErr := service.engine.Enqueue(ctx, "release_resource", jobPayload{ResourceID: resourceID}, fmt.Sprintf("release_resource:%d", resourceID), service.now(), 8)
		if queueErr != nil {
			failures = append(failures, fmt.Errorf("queue rental compensation: %w", queueErr))
		}
		cleanupErr := service.releaseVM(ctx, Account{UserID: userID, Kind: "rental", VMID: vmID})
		if cleanupErr != nil {
			failures = append(failures, service.releaseError(ctx, resourceID, fmt.Errorf("undo rental owner assignment: %w", cleanupErr)))
			continue
		}
		tx, err := service.db.BeginTx(ctx, nil)
		if err != nil {
			failures = append(failures, fmt.Errorf("begin rental compensation: %w", err))
			continue
		}
		_, err = tx.ExecContext(ctx, `UPDATE rental_pool SET status='available',leased_resource_id=NULL,updated_at=? WHERE vm_id=? AND leased_resource_id=?`, now, vmID, resourceID)
		if err == nil {
			_, err = tx.ExecContext(ctx, `DELETE FROM resources WHERE id=?`, resourceID)
		}
		if err == nil {
			_, err = tx.ExecContext(ctx, `UPDATE jobs SET status='succeeded',updated_at=? WHERE dedupe_key=? AND status='pending'`, now, fmt.Sprintf("release_resource:%d", resourceID))
		}
		if err == nil {
			err = tx.Commit()
		}
		_ = tx.Rollback()
		if err != nil {
			failures = append(failures, fmt.Errorf("finish rental compensation: %w", err))
		}
	}
	return errors.Join(failures...)
}

func (service *Service) RentalStock(ctx context.Context) (int, error) {
	service.mu.Lock()
	defer service.mu.Unlock()
	count, _, err := service.rentalAvailability(ctx)
	return count, err
}

func (service *Service) rentalAvailability(ctx context.Context) (int, []string, error) {
	vms, err := service.client.ListVMs(ctx, vm2api.VMFilter{})
	if err != nil {
		return 0, nil, fmt.Errorf("check rental vm owners: %w", err)
	}
	stock, err := queryAll(ctx, service.db, `SELECT vm_id FROM rental_pool WHERE status='available' AND selfcheck_state='pass' ORDER BY vm_id`, func(rows *sql.Rows) (id string, err error) {
		err = rows.Scan(&id)
		return
	})
	if err != nil {
		return 0, nil, err
	}
	indexed := make(map[string]vm2api.VM, len(vms))
	for _, vm := range vms {
		indexed[vm.ID] = vm
	}
	var excluded []string
	var names map[string]string
	for _, id := range stock {
		vm, exists := indexed[id]
		if exists && vm.OwnerUserID == "" && vm.Origin == "platform" && vm.Platform == "anthropic" {
			continue
		}
		excluded = append(excluded, id)
		if vm.OwnerUserID != "" {
			if names == nil {
				names, _ = service.ownerNames(ctx)
			}
			if err := service.addScopeWarning(ctx, rentalWarning(vm, names)); err != nil {
				return 0, nil, err
			}
		}
	}
	return len(stock) - len(excluded), excluded, nil
}
