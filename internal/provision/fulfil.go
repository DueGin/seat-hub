package provision

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

func (service *Service) FulfilOrder(ctx context.Context, orderID int64) error {
	service.mu.Lock()
	defer service.mu.Unlock()
	err := service.fulfilOrder(ctx, orderID)
	if err != nil {
		if _, saveErr := service.db.ExecContext(ctx, `UPDATE orders SET status='fulfill_failed',fulfil_error=?,updated_at=? WHERE id=? AND status IN ('paid','fulfill_failed')`, err.Error(), timestamp(service.now()), orderID); saveErr != nil {
			return errors.Join(err, fmt.Errorf("record fulfilment error: %w", saveErr))
		}
	}
	return err
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
		var held int
		if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM resources WHERE subscription_id=? AND status NOT IN ('released','releasing')`, subscriptionID).Scan(&held); err != nil {
			return fmt.Errorf("count rented resources: %w", err)
		}
		needed := max(0, units-held)
		rows, err := tx.QueryContext(ctx, `SELECT vm_id,tier FROM rental_pool WHERE status='available' ORDER BY vm_id LIMIT ?`, needed)
		if err != nil {
			return fmt.Errorf("read rental pool: %w", err)
		}
		type availableVM struct{ id, tier string }
		available := make([]availableVM, 0)
		for rows.Next() {
			var item availableVM
			if err := rows.Scan(&item.id, &item.tier); err != nil {
				rows.Close()
				return fmt.Errorf("read rental vm: %w", err)
			}
			available = append(available, item)
		}
		readErr := rows.Err()
		rows.Close()
		if readErr != nil {
			return fmt.Errorf("read rental pool: %w", readErr)
		}
		if len(available) < needed {
			return ErrOutOfStock
		}
		for _, item := range available {
			result, err := tx.ExecContext(ctx, `INSERT INTO resources(user_id,kind,subscription_id,vm_id,label,tier,status,created_at,updated_at) VALUES (?,'rental',?,?,?,?,'active',?,?)`, userID, subscriptionID, item.id, "租赁账号", item.tier, timestamp(now), timestamp(now))
			if err != nil {
				return fmt.Errorf("create rental resource: %w", err)
			}
			resourceID, err := result.LastInsertId()
			if err != nil {
				return fmt.Errorf("read rental resource id: %w", err)
			}
			if _, err := tx.ExecContext(ctx, `UPDATE rental_pool SET status='leased',leased_resource_id=?,updated_at=? WHERE vm_id=? AND status='available'`, resourceID, timestamp(now), item.id); err != nil {
				return fmt.Errorf("lease rental vm: %w", err)
			}
			assigned = append(assigned, item.id)
			if err := service.client.SetVMOwner(ctx, item.id, &owner); err != nil {
				return fmt.Errorf("assign rental owner: %w", err)
			}
			if err := service.client.SetSchedulable(ctx, item.id, true); err != nil {
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
