package provision

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
)

type Account struct {
	ID             int64   `json:"id"`
	UserID         int64   `json:"-"`
	Kind           string  `json:"kind"`
	VMID           string  `json:"-"`
	SubscriptionID *int64  `json:"-"`
	Label          string  `json:"label"`
	EmailMasked    string  `json:"email_masked"`
	Tier           string  `json:"tier"`
	Status         string  `json:"status"`
	StatusReason   *string `json:"status_reason"`
	LastError      *string `json:"-"`
	CreatedAt      string  `json:"created_at"`
	ExpiresAt      *string `json:"expires_at"`
	ReleaseAfter   *string `json:"-"`
}

const accountColumns = `r.id,r.user_id,r.kind,COALESCE(r.vm_id,''),r.subscription_id,r.label,r.email_masked,r.tier,r.status,r.status_reason,r.last_error,r.created_at,s.expires_at,r.release_after`

func scanAccount(scanner interface{ Scan(...any) error }) (Account, error) {
	var account Account
	if err := scanner.Scan(&account.ID, &account.UserID, &account.Kind, &account.VMID, &account.SubscriptionID, &account.Label, &account.EmailMasked, &account.Tier, &account.Status, &account.StatusReason, &account.LastError, &account.CreatedAt, &account.ExpiresAt, &account.ReleaseAfter); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return account, ErrNotFound
		}
		return account, fmt.Errorf("read resource: %w", err)
	}
	return account, nil
}

func (service *Service) Account(ctx context.Context, userID, resourceID int64) (Account, error) {
	return scanAccount(service.db.QueryRowContext(ctx, `SELECT `+accountColumns+` FROM resources r LEFT JOIN subscriptions s ON s.id=r.subscription_id WHERE r.id=? AND r.user_id=?`, resourceID, userID))
}

func (service *Service) resource(ctx context.Context, resourceID int64) (Account, error) {
	return scanAccount(service.db.QueryRowContext(ctx, `SELECT `+accountColumns+` FROM resources r LEFT JOIN subscriptions s ON s.id=r.subscription_id WHERE r.id=?`, resourceID))
}

func (service *Service) ListAccounts(ctx context.Context, userID int64, includeReleased bool) ([]Account, error) {
	query := `SELECT ` + accountColumns + ` FROM resources r LEFT JOIN subscriptions s ON s.id=r.subscription_id WHERE r.user_id=?`
	if !includeReleased {
		query += ` AND r.status<>'released'`
	}
	rows, err := service.db.QueryContext(ctx, query+` ORDER BY r.id DESC`, userID)
	if err != nil {
		return nil, fmt.Errorf("list resources: %w", err)
	}
	defer rows.Close()
	accounts := make([]Account, 0)
	for rows.Next() {
		account, err := scanAccount(rows)
		if err != nil {
			return nil, err
		}
		accounts = append(accounts, account)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read resources: %w", err)
	}
	return accounts, nil
}

func (service *Service) RenameAccount(ctx context.Context, userID, resourceID int64, label string) error {
	result, err := service.db.ExecContext(ctx, `UPDATE resources SET label=?,updated_at=? WHERE id=? AND user_id=?`, strings.TrimSpace(label), timestamp(service.now()), resourceID, userID)
	if err != nil {
		return fmt.Errorf("rename resource: %w", err)
	}
	count, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("count renamed resource: %w", err)
	}
	if count == 0 {
		return ErrNotFound
	}
	return nil
}

func (service *Service) CreateHosted(ctx context.Context, userID int64, label string) (Account, error) {
	service.mu.Lock()
	defer service.mu.Unlock()
	now := timestamp(service.now())
	tx, err := service.db.BeginTx(ctx, nil)
	if err != nil {
		return Account{}, fmt.Errorf("begin hosted creation: %w", err)
	}
	defer tx.Rollback()
	result, err := tx.ExecContext(ctx, `INSERT INTO resources(user_id,kind,label,status,created_at,updated_at) SELECT ?,'hosted',?,'provisioning',?,? WHERE (SELECT COUNT(*) FROM resources WHERE user_id=? AND kind='hosted' AND status<>'released') < (SELECT COALESCE(SUM(quantity),0) FROM subscriptions WHERE user_id=? AND kind='hosted_seat' AND status='active' AND julianday(starts_at)<=julianday(?) AND julianday(expires_at)>julianday(?))`, userID, strings.TrimSpace(label), now, now, userID, userID, now, now)
	if err != nil {
		return Account{}, fmt.Errorf("create hosted resource: %w", err)
	}
	count, err := result.RowsAffected()
	if err != nil {
		return Account{}, fmt.Errorf("count hosted resource: %w", err)
	}
	if count == 0 {
		return Account{}, ErrNoFreeSeat
	}
	resourceID, err := result.LastInsertId()
	if err != nil {
		return Account{}, fmt.Errorf("read hosted resource id: %w", err)
	}
	if err := enqueueTx(ctx, tx, "provision_hosted", jobPayload{ResourceID: resourceID}, fmt.Sprintf("provision_hosted:%d", resourceID), service.now()); err != nil {
		return Account{}, err
	}
	if err := tx.Commit(); err != nil {
		return Account{}, fmt.Errorf("commit hosted creation: %w", err)
	}
	return service.Account(ctx, userID, resourceID)
}

func (service *Service) failResource(ctx context.Context, resourceID int64, failure error) error {
	if errors.Is(failure, context.Canceled) || errors.Is(failure, context.DeadlineExceeded) {
		return failure
	}
	_, err := service.db.ExecContext(ctx, `UPDATE resources SET status='failed',last_error=?,updated_at=? WHERE id=? AND status NOT IN ('released','releasing','suspended')`, failure.Error(), timestamp(service.now()), resourceID)
	if err != nil {
		return errors.Join(failure, fmt.Errorf("save resource failure: %w", err))
	}
	return failure
}
