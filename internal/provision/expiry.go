package provision

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/DueGin/vm2api-portal/internal/store"
	"github.com/DueGin/vm2api-portal/internal/vm2api"
)

func (service *Service) SweepExpiry(ctx context.Context) error {
	service.mu.Lock()
	defer service.mu.Unlock()
	settings, err := service.site(ctx)
	if err != nil {
		return err
	}
	now := service.now()
	if _, err := service.db.ExecContext(ctx, `UPDATE subscriptions SET status='expired',updated_at=? WHERE status='active' AND julianday(expires_at)<=julianday(?)`, timestamp(now), timestamp(now)); err != nil {
		return fmt.Errorf("expire subscriptions: %w", err)
	}
	rows, err := service.db.QueryContext(ctx, `SELECT id FROM users`)
	if err != nil {
		return fmt.Errorf("list users for expiry: %w", err)
	}
	users := make([]int64, 0)
	for rows.Next() {
		var userID int64
		if err := rows.Scan(&userID); err != nil {
			rows.Close()
			return fmt.Errorf("read expiry user: %w", err)
		}
		users = append(users, userID)
	}
	readErr := rows.Err()
	rows.Close()
	if readErr != nil {
		return fmt.Errorf("read expiry users: %w", readErr)
	}
	// One failing user must not stop the sweep: a vm2api error for user 5 would
	// otherwise leave every later user unexpired on every run. Each failure is
	// recorded and the sweep continues; the user is retried next minute.
	failures := make([]error, 0)
	for _, userID := range users {
		if err := service.sweepUser(ctx, userID, settings, now); err != nil {
			failures = append(failures, fmt.Errorf("sweep user %d: %w", userID, err))
		}
	}
	if err := service.requeueStuckReleases(ctx, now); err != nil {
		failures = append(failures, err)
	}
	return errors.Join(failures...)
}

func (service *Service) sweepUser(ctx context.Context, userID int64, settings store.Site, now time.Time) error {
	entitlements, err := service.Entitlements(ctx, userID)
	if err != nil {
		return err
	}
	accounts, err := service.ListAccounts(ctx, userID, false)
	if err != nil {
		return err
	}
	sort.Slice(accounts, func(first, second int) bool {
		if accounts[first].CreatedAt == accounts[second].CreatedAt {
			return accounts[first].ID < accounts[second].ID
		}
		return accounts[first].CreatedAt < accounts[second].CreatedAt
	})
	var failures []error
	hostedIndex := 0
	for _, account := range accounts {
		if account.Status == "releasing" {
			continue
		}
		covered := false
		grace := settings.HostedGraceHours
		if account.Kind == "hosted" {
			covered = hostedIndex < entitlements.HostedSeats.Total
			hostedIndex++
		} else {
			grace = settings.RentalGraceHours
			if account.SubscriptionID != nil {
				var count int
				if err := service.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM subscriptions WHERE id=? AND user_id=? AND kind='rental_account' AND status='active' AND julianday(starts_at)<=julianday(?) AND julianday(expires_at)>julianday(?)`, *account.SubscriptionID, userID, timestamp(now), timestamp(now)).Scan(&count); err != nil {
					failures = append(failures, fmt.Errorf("check rental subscription: %w", err))
					continue
				}
				covered = count > 0
			}
		}
		if covered {
			if err := service.resumeResource(ctx, account); err != nil {
				failures = append(failures, err)
			}
			continue
		}
		if err := service.suspendResource(ctx, account, time.Duration(max(0, grace))*time.Hour); err != nil {
			failures = append(failures, err)
		}
	}
	if err := service.syncConcurrency(ctx, userID); err != nil {
		failures = append(failures, err)
	}
	return errors.Join(failures...)
}

// requeueStuckReleases re-enqueues release_resource for resources left in
// releasing past their deadline with no live job (a job that exhausted its
// retries turns failed and never runs again, so the VM would leak forever).
// The dedupe key keeps a resource with a pending or running job from being
// queued twice.
func (service *Service) requeueStuckReleases(ctx context.Context, now time.Time) error {
	rows, err := service.db.QueryContext(ctx, `SELECT id FROM resources AS resource
		WHERE resource.status='releasing'
		AND (resource.release_after IS NULL OR julianday(resource.release_after)<=julianday(?))
		AND NOT EXISTS (SELECT 1 FROM jobs WHERE jobs.dedupe_key='release_resource:'||resource.id AND jobs.status IN ('pending','running'))
		ORDER BY resource.id`, timestamp(now))
	if err != nil {
		return fmt.Errorf("list stuck releases: %w", err)
	}
	ids := make([]int64, 0)
	for rows.Next() {
		var resourceID int64
		if err := rows.Scan(&resourceID); err != nil {
			rows.Close()
			return fmt.Errorf("read stuck release: %w", err)
		}
		ids = append(ids, resourceID)
	}
	readErr := rows.Err()
	rows.Close()
	if readErr != nil {
		return fmt.Errorf("read stuck releases: %w", readErr)
	}
	var failures []error
	for _, resourceID := range ids {
		if _, err := service.engine.Enqueue(ctx, "release_resource", jobPayload{ResourceID: resourceID}, fmt.Sprintf("release_resource:%d", resourceID), now, 8); err != nil {
			failures = append(failures, fmt.Errorf("requeue release for resource %d: %w", resourceID, err))
		}
	}
	return errors.Join(failures...)
}

func (service *Service) suspendResource(ctx context.Context, account Account, grace time.Duration) error {
	if account.VMID != "" {
		if err := service.client.SetSchedulable(ctx, account.VMID, false); err != nil && !vm2api.IsStatus(err, 404) {
			return fmt.Errorf("suspend resource scheduling: %w", err)
		}
	}
	now := service.now()
	releaseAt := now.Add(grace)
	if account.ReleaseAfter != nil {
		parsed, err := time.Parse(time.RFC3339, *account.ReleaseAfter)
		if err != nil {
			return fmt.Errorf("parse release deadline: %w", err)
		}
		releaseAt = parsed
	}
	tx, err := service.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin resource suspension: %w", err)
	}
	defer tx.Rollback()
	if account.Status != "suspended" {
		if _, err := tx.ExecContext(ctx, `UPDATE resources SET status='suspended',status_reason=?,suspended_at=?,release_after=?,updated_at=? WHERE id=? AND status NOT IN ('released','releasing')`, "subscription_expired:"+account.Status, timestamp(now), timestamp(releaseAt), timestamp(now), account.ID); err != nil {
			return fmt.Errorf("suspend resource: %w", err)
		}
	}
	if err := enqueueTx(ctx, tx, "release_resource", jobPayload{ResourceID: account.ID}, fmt.Sprintf("release_resource:%d", account.ID), releaseAt); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit resource suspension: %w", err)
	}
	return nil
}

func (service *Service) resumeResource(ctx context.Context, account Account) error {
	if account.Status != "suspended" || account.StatusReason == nil || !strings.HasPrefix(*account.StatusReason, "subscription_expired:") {
		return nil
	}
	status := strings.TrimPrefix(*account.StatusReason, "subscription_expired:")
	if status == "active" && account.VMID != "" {
		if err := service.client.SetSchedulable(ctx, account.VMID, true); err != nil && !vm2api.IsStatus(err, 404) {
			return fmt.Errorf("resume resource scheduling: %w", err)
		}
	}
	tx, err := service.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin resource resumption: %w", err)
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `UPDATE resources SET status=?,status_reason=NULL,suspended_at=NULL,release_after=NULL,updated_at=? WHERE id=? AND status='suspended'`, status, timestamp(service.now()), account.ID); err != nil {
		return fmt.Errorf("resume resource: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `UPDATE jobs SET status='succeeded',locked_until=NULL,updated_at=? WHERE dedupe_key=? AND status='pending'`, timestamp(service.now()), fmt.Sprintf("release_resource:%d", account.ID)); err != nil {
		return fmt.Errorf("cancel pending release: %w", err)
	}
	if status == "provisioning" || status == "bootstrapping" {
		kind := "provision_hosted"
		if status == "bootstrapping" {
			kind = "watch_bootstrap"
		}
		if err := enqueueTx(ctx, tx, kind, jobPayload{ResourceID: account.ID, Deadline: service.now().Add(15 * time.Minute)}, fmt.Sprintf("%s:%d", kind, account.ID), service.now()); err != nil {
			return err
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit resource resumption: %w", err)
	}
	return nil
}

func (service *Service) DeleteHosted(ctx context.Context, userID, resourceID int64) error {
	service.mu.Lock()
	defer service.mu.Unlock()
	account, err := service.Account(ctx, userID, resourceID)
	if err != nil {
		return err
	}
	if account.Kind != "hosted" {
		return ErrNotFound
	}
	if account.Status == "released" {
		return nil
	}
	if account.VMID != "" {
		if err := service.client.SetSchedulable(ctx, account.VMID, false); err != nil && !vm2api.IsStatus(err, 404) {
			return fmt.Errorf("disable deleted hosted resource: %w", err)
		}
	}
	tx, err := service.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin hosted removal: %w", err)
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `UPDATE resources SET status='releasing',release_after=?,updated_at=? WHERE id=?`, timestamp(service.now()), timestamp(service.now()), resourceID); err != nil {
		return fmt.Errorf("mark hosted releasing: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `UPDATE jobs SET run_after=? WHERE dedupe_key=? AND status='pending'`, timestamp(service.now()), fmt.Sprintf("release_resource:%d", resourceID)); err != nil {
		return fmt.Errorf("expedite hosted release: %w", err)
	}
	if err := enqueueTx(ctx, tx, "release_resource", jobPayload{ResourceID: resourceID}, fmt.Sprintf("release_resource:%d", resourceID), service.now()); err != nil {
		return err
	}
	if err := enqueueTx(ctx, tx, "sync_concurrency", jobPayload{UserID: userID}, fmt.Sprintf("sync_concurrency:%d", userID), service.now()); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit hosted removal: %w", err)
	}
	return nil
}

func (service *Service) releaseJob(ctx context.Context, job Job) error {
	payload, err := decodePayload(job)
	if err != nil {
		return err
	}
	return service.ReleaseResource(ctx, payload.ResourceID)
}

// MarkReleasing forces any not-yet-released resource into releasing with an
// immediate deadline, so the admin "force release" path and DeleteHosted share
// the same precondition and ReleaseResource is never a silent no-op.
func (service *Service) MarkReleasing(ctx context.Context, resourceID int64) error {
	result, err := service.db.ExecContext(ctx, `UPDATE resources SET status='releasing',release_after=?,updated_at=? WHERE id=? AND status<>'released'`, timestamp(service.now()), timestamp(service.now()), resourceID)
	if err != nil {
		return fmt.Errorf("mark resource releasing: %w", err)
	}
	count, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("count released resources: %w", err)
	}
	if count == 0 {
		var exists int
		if err := service.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM resources WHERE id=?`, resourceID).Scan(&exists); err != nil {
			return fmt.Errorf("check resource: %w", err)
		}
		if exists == 0 {
			return ErrNotFound
		}
	}
	return nil
}

func (service *Service) ReleaseResource(ctx context.Context, resourceID int64) error {
	service.mu.Lock()
	defer service.mu.Unlock()
	account, err := service.resource(ctx, resourceID)
	if err != nil {
		return err
	}
	if account.Status == "released" {
		return nil
	}
	if account.Status != "suspended" && account.Status != "releasing" {
		return nil
	}
	if account.Status == "suspended" && account.StatusReason != nil && strings.HasPrefix(*account.StatusReason, "subscription_expired:") {
		covered, err := service.resourceCovered(ctx, account)
		if err != nil {
			return err
		}
		if covered {
			if err := service.resumeResource(ctx, account); err != nil {
				return err
			}
			return service.syncConcurrency(ctx, account.UserID)
		}
	}
	if account.ReleaseAfter != nil && account.Status == "suspended" {
		deadline, err := time.Parse(time.RFC3339, *account.ReleaseAfter)
		if err != nil {
			return fmt.Errorf("parse release deadline: %w", err)
		}
		if service.now().Before(deadline) {
			return &RetryAfterError{After: deadline.Sub(service.now()), Err: errors.New("release grace period pending")}
		}
	}
	if _, err := service.db.ExecContext(ctx, `UPDATE resources SET status='releasing',updated_at=? WHERE id=?`, timestamp(service.now()), resourceID); err != nil {
		return fmt.Errorf("mark resource releasing: %w", err)
	}
	if account.VMID != "" {
		if err := service.client.SetSchedulable(ctx, account.VMID, false); err != nil && !vm2api.IsStatus(err, 404) {
			return service.releaseError(ctx, account.ID, fmt.Errorf("disable resource scheduling: %w", err))
		}
		var releaseErr error
		if account.Kind == "hosted" {
			releaseErr = service.client.DeleteVM(ctx, account.VMID)
		} else {
			releaseErr = service.client.SetVMOwner(ctx, account.VMID, nil)
		}
		if releaseErr != nil && !vm2api.IsStatus(releaseErr, 404) {
			return service.releaseError(ctx, account.ID, fmt.Errorf("release upstream resource: %w", releaseErr))
		}
	}
	tx, err := service.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin resource release: %w", err)
	}
	defer tx.Rollback()
	if account.Kind == "rental" {
		if _, err := tx.ExecContext(ctx, `UPDATE rental_pool SET status='available',leased_resource_id=NULL,updated_at=? WHERE vm_id=? AND leased_resource_id=?`, timestamp(service.now()), account.VMID, account.ID); err != nil {
			return fmt.Errorf("return rental to pool: %w", err)
		}
	}
	if _, err := tx.ExecContext(ctx, `UPDATE resources SET status='released',released_at=?,last_error=NULL,updated_at=? WHERE id=?`, timestamp(service.now()), timestamp(service.now()), account.ID); err != nil {
		return fmt.Errorf("mark resource released: %w", err)
	}
	if err := enqueueTx(ctx, tx, "sync_concurrency", jobPayload{UserID: account.UserID}, fmt.Sprintf("sync_concurrency:%d", account.UserID), service.now()); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit resource release: %w", err)
	}
	return nil
}

func (service *Service) resourceCovered(ctx context.Context, account Account) (bool, error) {
	now := timestamp(service.now())
	if account.Kind == "rental" {
		if account.SubscriptionID == nil {
			return false, nil
		}
		var count int
		if err := service.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM subscriptions WHERE id=? AND user_id=? AND kind='rental_account' AND status='active' AND julianday(starts_at)<=julianday(?) AND julianday(expires_at)>julianday(?)`, *account.SubscriptionID, account.UserID, now, now).Scan(&count); err != nil {
			return false, fmt.Errorf("check rental coverage: %w", err)
		}
		return count > 0, nil
	}
	var capacity, rank int
	if err := service.db.QueryRowContext(ctx, `SELECT COALESCE(SUM(quantity),0) FROM subscriptions WHERE user_id=? AND kind='hosted_seat' AND status='active' AND julianday(starts_at)<=julianday(?) AND julianday(expires_at)>julianday(?)`, account.UserID, now, now).Scan(&capacity); err != nil {
		return false, fmt.Errorf("check hosted coverage: %w", err)
	}
	if err := service.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM resources WHERE user_id=? AND kind='hosted' AND status NOT IN ('released','releasing') AND (created_at<? OR (created_at=? AND id<=?))`, account.UserID, account.CreatedAt, account.CreatedAt, account.ID).Scan(&rank); err != nil {
		return false, fmt.Errorf("check hosted resource rank: %w", err)
	}
	return rank <= capacity, nil
}

func (service *Service) releaseError(ctx context.Context, resourceID int64, failure error) error {
	_, err := service.db.ExecContext(ctx, `UPDATE resources SET last_error=?,updated_at=? WHERE id=?`, failure.Error(), timestamp(service.now()), resourceID)
	if err != nil {
		return errors.Join(failure, fmt.Errorf("save release error: %w", err))
	}
	return failure
}
