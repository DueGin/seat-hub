package provision

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
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
	// Lease first, then talk to vm2api: the upstream calls must not run inside
	// the write transaction, and a failed claim leaves nothing half-created.
	vmIDs, err := service.claimRental(ctx, userID, subscriptionID, quantity)
	if err != nil || len(vmIDs) == 0 {
		return err
	}
	owner, err := service.upstreamUser(ctx, userID)
	if err != nil {
		return err
	}
	for _, vmID := range vmIDs {
		if err := service.client.SetVMOwner(ctx, vmID, &owner); err != nil {
			return service.quarantineRental(ctx, vmID, fmt.Errorf("assign rental owner: %w", err))
		}
		if err := service.client.SetSchedulable(ctx, vmID, true); err != nil {
			return service.quarantineRental(ctx, vmID, fmt.Errorf("enable rental scheduling: %w", err))
		}
	}
	return nil
}

// claimRental leases the accounts a subscription is missing and commits.
func (service *Service) claimRental(ctx context.Context, userID, subscriptionID int64, quantity int) ([]string, error) {
	tx, err := service.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("begin rental allocation: %w", err)
	}
	defer tx.Rollback()
	vmIDs, err := claimRentalTx(ctx, tx, userID, subscriptionID, quantity, service.now())
	if err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("commit rental allocation: %w", err)
	}
	return vmIDs, nil
}

// claimRental leases up to `quantity` minus the live accounts the subscription
// already holds.
func claimRentalTx(ctx context.Context, tx *sql.Tx, userID, subscriptionID int64, quantity int, now time.Time) ([]string, error) {
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
		skip := make([]any, 0, len(assigned))
		exclude := ""
		if len(assigned) > 0 {
			exclude = ` AND vm_id NOT IN (` + strings.TrimSuffix(strings.Repeat("?,", len(assigned)), ",") + `)`
			for _, item := range assigned {
				skip = append(skip, item)
			}
		}
		err := tx.QueryRowContext(ctx, `SELECT vm_id,tier FROM rental_pool WHERE status='available'`+exclude+` ORDER BY vm_id LIMIT 1`, skip...).Scan(&vmID, &tier)
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
		claimed, err := tx.ExecContext(ctx, `UPDATE rental_pool SET status='leased',leased_resource_id=?,updated_at=? WHERE vm_id=? AND status='available'`, resourceID, timestamp(now), vmID)
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
	if kind == "rental_account" {
		owner, err = service.upstreamUser(ctx, userID)
		if err != nil {
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
			for _, vmID := range assigned {
				if cleanupErr := service.client.SetVMOwner(cleanupCtx, vmID, nil); cleanupErr != nil {
					fulfilErr = errors.Join(fulfilErr, fmt.Errorf("undo rental owner assignment: %w", cleanupErr))
					if _, saveErr := service.db.ExecContext(cleanupCtx, `UPDATE rental_pool SET status='maintenance',note=?,updated_at=? WHERE vm_id=?`, cleanupErr.Error(), timestamp(service.now()), vmID); saveErr != nil {
						fulfilErr = errors.Join(fulfilErr, fmt.Errorf("quarantine rental vm: %w", saveErr))
					}
				}
			}
		}
	}()
	if kind == "rental_account" {
		// Claim pool rows one at a time with a conditional UPDATE. Reading the
		// available list first and leasing afterwards would let two concurrent
		// fulfillments hand out the same VM; here the loser sees RowsAffected 0
		// and moves on to the next candidate.
		claimed, err := claimRentalTx(ctx, tx, userID, subscriptionID, units, now)
		if err != nil {
			return err
		}
		for _, vmID := range claimed {
			assigned = append(assigned, vmID)
			if err := service.client.SetVMOwner(ctx, vmID, &owner); err != nil {
				return fmt.Errorf("assign rental owner: %w", err)
			}
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
	return nil
}
