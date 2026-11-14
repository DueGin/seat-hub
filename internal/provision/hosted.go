package provision

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/DueGin/seat-hub/internal/vm2api"
)

func (service *Service) provisionJob(ctx context.Context, job Job) error {
	payload, err := decodePayload(job)
	if err != nil {
		return err
	}
	return service.ProvisionHosted(ctx, payload.ResourceID)
}

// Hosted provisioning stages recorded in resources.status_reason when a step
// fails. They keep "the VM is not ready" distinguishable from "the credential
// exchange failed": only the latter may continue to the OAuth flow.
const (
	stageSlot  = "provisioning_failed:slot"
	stageOwner = "provisioning_failed:owner"
	stageProxy = "provisioning_failed:proxy"
)

func (service *Service) ProvisionHosted(ctx context.Context, resourceID int64) error {
	service.mu.Lock()
	defer service.mu.Unlock()
	account, err := service.resource(ctx, resourceID)
	if err != nil {
		return err
	}
	if account.Kind != "hosted" {
		return ErrInvalidState
	}
	if account.Status != "provisioning" && account.Status != "failed" {
		return nil
	}
	if account.SelfcheckState == "exhausted" {
		return ErrSelfcheckExhausted
	}
	owner, err := service.upstreamUser(ctx, account.UserID)
	if err != nil {
		return service.failResource(ctx, resourceID, err)
	}
	if account.VMID == "" {
		settings, err := service.site(ctx)
		if err != nil {
			return service.failResource(ctx, resourceID, err)
		}
		vm, err := service.client.CreateHostedVM(ctx, account.Label, settings.HostedVMTemplate)
		if err != nil {
			return service.failResource(ctx, resourceID, fmt.Errorf("create hosted vm: %w", err))
		}
		account.VMID = vm.ID
		if _, err := service.db.ExecContext(ctx, `UPDATE resources SET vm_id=?,updated_at=? WHERE id=?`, vm.ID, timestamp(service.now()), resourceID); err != nil {
			cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
			defer cancel()
			cleanupErr := service.client.DeleteVM(cleanupCtx, vm.ID)
			return errors.Join(fmt.Errorf("save hosted vm: %w", err), cleanupErr)
		}
	}
	if err := service.prepareHosted(ctx, account, owner); err != nil {
		return err
	}
	if err := service.ensureSlotHealthy(ctx, account); err != nil {
		if errors.Is(err, ErrSelfcheckFailed) {
			return service.failStage(ctx, resourceID, "selfcheck_failed", err)
		}
		return err
	}
	return service.markAwaitingAuth(ctx, resourceID)
}

// prepareHosted runs the steps that make a hosted VM usable for onboarding:
// scheduling off, ownership assigned to the buyer, and an egress proxy in
// place. It is idempotent so a retry (or a link regeneration on a half-built
// resource) repairs whatever failed before, and it refuses to continue when a
// step is still broken.
func (service *Service) prepareHosted(ctx context.Context, account Account, owner string) error {
	fail := func(stage string, err error) error {
		return service.failStage(ctx, account.ID, stage, err)
	}
	if err := service.client.SetSchedulable(ctx, account.VMID, false); err != nil {
		return fail(stageSlot, fmt.Errorf("disable hosted scheduling: %w", err))
	}
	if err := service.client.SetVMOwner(ctx, account.VMID, &owner); err != nil {
		return fail(stageOwner, fmt.Errorf("assign hosted owner: %w", err))
	}
	if err := service.client.AllocateProxy(ctx, account.VMID); err != nil {
		return fail(stageProxy, fmt.Errorf("allocate hosted proxy: %w", err))
	}
	return nil
}

func (service *Service) markAwaitingAuth(ctx context.Context, resourceID int64) error {
	if _, err := service.db.ExecContext(ctx, `UPDATE resources SET status='awaiting_auth',status_reason=NULL,last_error=NULL,updated_at=? WHERE id=? AND status IN ('provisioning','failed','awaiting_auth','needs_reauth')`, timestamp(service.now()), resourceID); err != nil {
		return fmt.Errorf("mark hosted awaiting auth: %w", err)
	}
	return nil
}

// ensureOnboardable verifies, right before an OAuth link is issued or a code is
// exchanged, that the VM belongs to the buyer and has an egress proxy. A
// resource whose owner assignment never succeeded must not be onboarded: the VM
// would stay shared and the user's credential would sit on someone else's slot.
func (service *Service) ensureOnboardable(ctx context.Context, account Account) error {
	account, err := service.resource(ctx, account.ID)
	if err != nil {
		return err
	}
	owner, err := service.upstreamUser(ctx, account.UserID)
	if err != nil {
		return err
	}
	if provisioningIncomplete(account) {
		if err := service.prepareHosted(ctx, account, owner); err != nil {
			return err
		}
		if err := service.ensureSlotHealthy(ctx, account); err != nil {
			return err
		}
		return service.markAwaitingAuth(ctx, account.ID)
	}
	usage, err := service.client.GetAccountUsage(ctx, account.VMID)
	if err != nil {
		if vm2api.IsStatus(err, 404) {
			return service.failResource(ctx, account.ID, fmt.Errorf("hosted vm %s is missing upstream", account.VMID))
		}
		return fmt.Errorf("verify hosted resource: %w", err)
	}
	if usage.VM.OwnerUserID == owner && usage.VM.ProxyConfigured {
		return nil
	}
	if err := service.prepareHosted(ctx, account, owner); err != nil {
		return err
	}
	if err := service.ensureSlotHealthy(ctx, account); err != nil {
		return err
	}
	return service.markAwaitingAuth(ctx, account.ID)
}

// failStage records which provisioning step failed, so a failed resource whose
// owner or proxy was never assigned can be told apart from one whose
// authorization code exchange failed.
func (service *Service) failStage(ctx context.Context, resourceID int64, stage string, failure error) error {
	if errors.Is(failure, context.Canceled) || errors.Is(failure, context.DeadlineExceeded) {
		return failure
	}
	_, err := service.db.ExecContext(ctx, `UPDATE resources SET status='failed',status_reason=?,last_error=?,updated_at=? WHERE id=? AND status NOT IN ('released','releasing','suspended')`, stage, failure.Error(), timestamp(service.now()), resourceID)
	if err != nil {
		return errors.Join(failure, fmt.Errorf("save resource failure: %w", err))
	}
	return failure
}

// provisioningIncomplete reports whether the resource never finished the steps
// that precede onboarding (create slot, assign owner, allocate proxy).
func provisioningIncomplete(account Account) bool {
	return account.StatusReason != nil && strings.HasPrefix(*account.StatusReason, "provisioning_failed:")
}

type OnboardingError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
	// Reason is the resource's status_reason (stage code). Message is the raw
	// internal error, so callers that face users must translate Reason instead.
	Reason string `json:"-"`
}

type Onboarding struct {
	AccountID        int64             `json:"account_id"`
	Status           string            `json:"status"`
	AuthURL          string            `json:"auth_url,omitempty"`
	AuthURLExpiresAt *string           `json:"auth_url_expires_at,omitempty"`
	Bootstrap        *vm2api.Bootstrap `json:"bootstrap"`
	Error            *OnboardingError  `json:"error"`
	CanRegenerate    bool              `json:"can_regenerate"`
}

func (service *Service) Onboarding(ctx context.Context, userID, resourceID int64) (Onboarding, error) {
	account, err := service.Account(ctx, userID, resourceID)
	if err != nil {
		return Onboarding{}, err
	}
	if account.Kind != "hosted" {
		return Onboarding{}, ErrNotFound
	}
	result := Onboarding{AccountID: resourceID, Status: account.Status, CanRegenerate: account.VMID != "" && (account.Status == "awaiting_auth" || account.Status == "needs_reauth" || account.Status == "failed")}
	if account.LastError != nil {
		result.Error = &OnboardingError{Code: "onboarding_failed", Message: *account.LastError}
		if account.StatusReason != nil {
			result.Error.Reason = *account.StatusReason
		}
	}
	var url, expires string
	err = service.db.QueryRowContext(ctx, `SELECT auth_url,expires_at FROM oauth_sessions WHERE resource_id=? AND status='open' AND julianday(expires_at)>julianday(?) ORDER BY id DESC LIMIT 1`, resourceID, timestamp(service.now())).Scan(&url, &expires)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return result, fmt.Errorf("read onboarding session: %w", err)
	}
	if err == nil {
		result.AuthURL = url
		result.AuthURLExpiresAt = &expires
	}
	if account.Status == "bootstrapping" {
		result.Bootstrap = &vm2api.Bootstrap{State: "running"}
	}
	if account.Status == "active" {
		result.Bootstrap = &vm2api.Bootstrap{State: "succeeded"}
	}
	return result, nil
}

func (service *Service) GenerateAuthURL(ctx context.Context, userID, resourceID int64) (vm2api.AuthURL, error) {
	service.mu.Lock()
	defer service.mu.Unlock()
	account, err := service.Account(ctx, userID, resourceID)
	if err != nil {
		return vm2api.AuthURL{}, err
	}
	if account.Kind != "hosted" || account.VMID == "" || (account.Status != "awaiting_auth" && account.Status != "needs_reauth" && account.Status != "failed") {
		return vm2api.AuthURL{}, ErrInvalidState
	}
	if err := service.ensureFreshSelfcheck(ctx, account); err != nil {
		return vm2api.AuthURL{}, err
	}
	if err := service.ensureOnboardable(ctx, account); err != nil {
		return vm2api.AuthURL{}, err
	}
	auth, err := service.client.GenerateAuthURL(ctx, account.VMID)
	if err != nil {
		return vm2api.AuthURL{}, service.failResource(ctx, resourceID, fmt.Errorf("generate auth url: %w", err))
	}
	auth.ExpiresAt = auth.ExpiresAt.UTC()
	tx, err := service.db.BeginTx(ctx, nil)
	if err != nil {
		return vm2api.AuthURL{}, fmt.Errorf("begin auth session: %w", err)
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `UPDATE oauth_sessions SET status='expired' WHERE resource_id=? AND status='open'`, resourceID); err != nil {
		return vm2api.AuthURL{}, fmt.Errorf("expire old auth sessions: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO oauth_sessions(resource_id,vm_id,auth_url,session_id,expires_at,status,created_at) VALUES (?,?,?,?,?,'open',?)`, resourceID, account.VMID, auth.URL, auth.SessionID, timestamp(auth.ExpiresAt), timestamp(service.now())); err != nil {
		return vm2api.AuthURL{}, fmt.Errorf("save auth session: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `UPDATE resources SET status='awaiting_auth',last_error=NULL,updated_at=? WHERE id=?`, timestamp(service.now()), resourceID); err != nil {
		return vm2api.AuthURL{}, fmt.Errorf("set awaiting auth: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return vm2api.AuthURL{}, fmt.Errorf("commit auth session: %w", err)
	}
	return auth, nil
}

func (service *Service) SubmitAuthCode(ctx context.Context, userID, resourceID int64, sessionID, code string) error {
	service.mu.Lock()
	defer service.mu.Unlock()
	code = strings.TrimSpace(code)
	if code == "" || len(code) > 8192 || strings.ContainsAny(code, "\r\n\x00") {
		return ErrInvalidCode
	}
	account, err := service.Account(ctx, userID, resourceID)
	if err != nil {
		return err
	}
	if account.Kind != "hosted" || account.VMID == "" || (account.Status != "awaiting_auth" && account.Status != "needs_reauth" && account.Status != "failed") {
		return ErrInvalidState
	}
	if err := service.ensureFreshSelfcheck(ctx, account); err != nil {
		return err
	}
	if err := service.ensureOnboardable(ctx, account); err != nil {
		return err
	}
	var id int64
	var storedSession, expires string
	query := `SELECT id,session_id,expires_at FROM oauth_sessions WHERE resource_id=? AND vm_id=? AND status='open'`
	args := []any{resourceID, account.VMID}
	if sessionID != "" {
		query += ` AND session_id=?`
		args = append(args, sessionID)
	}
	err = service.db.QueryRowContext(ctx, query+` ORDER BY id DESC LIMIT 1`, args...).Scan(&id, &storedSession, &expires)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrAuthExpired
	}
	if err != nil {
		return fmt.Errorf("read auth session: %w", err)
	}
	expiry, err := time.Parse(time.RFC3339, expires)
	if err != nil {
		return fmt.Errorf("parse auth expiry: %w", err)
	}
	if !expiry.After(service.now()) {
		if _, err := service.db.ExecContext(ctx, `UPDATE oauth_sessions SET status='expired' WHERE id=?`, id); err != nil {
			return fmt.Errorf("expire auth session: %w", err)
		}
		return ErrAuthExpired
	}
	if err := service.client.ExchangeCode(ctx, account.VMID, storedSession, code); err != nil {
		failure := fmt.Errorf("exchange authorization code: %w", err)
		if _, saveErr := service.db.ExecContext(ctx, `UPDATE oauth_sessions SET status='failed',error=? WHERE id=?`, failure.Error(), id); saveErr != nil {
			return errors.Join(failure, fmt.Errorf("save auth failure: %w", saveErr))
		}
		return service.failResource(ctx, resourceID, failure)
	}
	if err := service.checkAuthorizedAccount(ctx, account); err != nil {
		return err
	}
	tx, err := service.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin bootstrap transition: %w", err)
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `UPDATE oauth_sessions SET status='submitted' WHERE id=?`, id); err != nil {
		return fmt.Errorf("submit auth session: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `UPDATE resources SET status='bootstrapping',last_error=NULL,updated_at=? WHERE id=?`, timestamp(service.now()), resourceID); err != nil {
		return fmt.Errorf("set bootstrapping: %w", err)
	}
	if err := enqueueTx(ctx, tx, "watch_bootstrap", jobPayload{ResourceID: resourceID, Deadline: service.now().Add(15 * time.Minute)}, fmt.Sprintf("watch_bootstrap:%d", resourceID), service.now()); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit bootstrap transition: %w", err)
	}
	return nil
}

func (service *Service) bootstrapJob(ctx context.Context, job Job) error {
	payload, err := decodePayload(job)
	if err != nil {
		return err
	}
	return service.watchBootstrap(ctx, payload.ResourceID, payload.Deadline)
}

func (service *Service) watchBootstrap(ctx context.Context, resourceID int64, deadline time.Time) error {
	service.mu.Lock()
	defer service.mu.Unlock()
	account, err := service.resource(ctx, resourceID)
	if err != nil {
		return err
	}
	if account.Status == "failed" && account.LastError != nil {
		return errors.New(*account.LastError)
	}
	if account.Status != "bootstrapping" {
		return nil
	}
	if !deadline.IsZero() && !service.now().Before(deadline) {
		return service.failResource(ctx, resourceID, errors.New("bootstrap timed out"))
	}
	bootstrap, err := service.client.BootstrapStatus(ctx, account.VMID)
	if err != nil {
		return service.failResource(ctx, resourceID, fmt.Errorf("read bootstrap status: %w", err))
	}
	switch bootstrap.State {
	case "pending", "running":
		return &RetryAfterError{After: 2 * time.Second, Err: errors.New("bootstrap pending")}
	case "failed":
		return service.failResource(ctx, resourceID, fmt.Errorf("bootstrap failed: %s", bootstrap.Message))
	case "succeeded":
		if err := service.client.SetSchedulable(ctx, account.VMID, true); err != nil {
			return service.failResource(ctx, resourceID, fmt.Errorf("enable hosted scheduling: %w", err))
		}
		tx, err := service.db.BeginTx(ctx, nil)
		if err != nil {
			return fmt.Errorf("begin bootstrap completion: %w", err)
		}
		defer tx.Rollback()
		if _, err := tx.ExecContext(ctx, `UPDATE resources SET status='active',last_error=NULL,updated_at=? WHERE id=? AND status='bootstrapping'`, timestamp(service.now()), resourceID); err != nil {
			return fmt.Errorf("activate hosted resource: %w", err)
		}
		if err := enqueueTx(ctx, tx, "sync_concurrency", jobPayload{UserID: account.UserID}, fmt.Sprintf("sync_concurrency:%d", account.UserID), service.now()); err != nil {
			return err
		}
		if err := tx.Commit(); err != nil {
			return fmt.Errorf("commit bootstrap completion: %w", err)
		}
		service.syncAccountIdentity(ctx, account.VMID)
		return nil
	default:
		return service.failResource(ctx, resourceID, fmt.Errorf("unknown bootstrap state: %s", bootstrap.State))
	}
}

func (service *Service) RetryHosted(ctx context.Context, userID, resourceID int64) error {
	service.mu.Lock()
	defer service.mu.Unlock()
	account, err := service.Account(ctx, userID, resourceID)
	if err != nil {
		return err
	}
	if account.Kind != "hosted" || account.Status != "failed" {
		return ErrInvalidState
	}
	if account.SelfcheckState == "exhausted" {
		return ErrSelfcheckExhausted
	}
	status, kind := "provisioning", "provision_hosted"
	if account.VMID != "" {
		var submitted int
		if err := service.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM oauth_sessions WHERE resource_id=? AND status='submitted'`, resourceID).Scan(&submitted); err != nil {
			return fmt.Errorf("read bootstrap retry state: %w", err)
		}
		if submitted > 0 {
			status, kind = "bootstrapping", "watch_bootstrap"
		}
	}
	tx, err := service.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin hosted retry: %w", err)
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `UPDATE resources SET status=?,last_error=NULL,updated_at=? WHERE id=?`, status, timestamp(service.now()), resourceID); err != nil {
		return fmt.Errorf("reset hosted provisioning: %w", err)
	}
	if err := enqueueTx(ctx, tx, kind, jobPayload{ResourceID: resourceID, Deadline: service.now().Add(15 * time.Minute)}, fmt.Sprintf("%s:%d", kind, resourceID), service.now()); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit hosted retry: %w", err)
	}
	return nil
}
