package app

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"net/http"

	"github.com/DueGin/seat-hub/internal/api"
	"github.com/DueGin/seat-hub/internal/order"
	"github.com/DueGin/seat-hub/internal/portal"
	"github.com/DueGin/seat-hub/internal/privacy"
	"github.com/DueGin/seat-hub/internal/provision"
	"github.com/DueGin/seat-hub/internal/secret"
	"github.com/DueGin/seat-hub/internal/store"
	"github.com/DueGin/seat-hub/internal/usage"
	"github.com/DueGin/seat-hub/internal/vm2api"
)

// provisioner is the single object the HTTP layer talks to for accounts,
// usage, keys, and the admin console. It fans out to the three lower-level
// services and owns the background job loop.
type provisioner struct {
	*provisionSpan
	svc   *provision.Service
	keys  api.KeyService
	admin api.AdminCoreService
	usage *usage.Service
	log   *slog.Logger
}

func newProvisioner(
	db *store.DB,
	client vm2api.Client,
	box *secret.Box,
	orders *order.Service,
	settings *store.Settings,
	logger *slog.Logger,
) *provisioner {
	prov := provision.New(db.DB, client)
	prov.Box = box
	use := usage.New(db.DB, client)
	keysAndAdmin := portal.New(db, client, box, prov)
	p := &provisioner{
		provisionSpan: &provisionSpan{
			prov:     prov,
			usage:    use,
			db:       db.DB,
			settings: settings,
			keyStats: client.KeyStats,
			keyLogs:  client.UserRequestLogs,
		},
		svc:   prov,
		keys:  keysAndAdmin,
		admin: keysAndAdmin,
		usage: use,
		log:   logger,
	}
	return p
}

// --- self-check admin surface (implemented by the self-check integration) ---

func (p *provisioner) RunSelfcheck(ctx context.Context, resourceID int64, rebuild bool) (api.SelfcheckDTO, error) {
	state, err := p.svc.RecheckResource(ctx, resourceID, rebuild)
	return toSelfcheckDTO(state, true), mapProvisionErr(err)
}

func (p *provisioner) SelfcheckHistory(ctx context.Context, resourceID int64, limit int) ([]api.SelfcheckReportDTO, error) {
	records, err := p.svc.SelfcheckHistory(ctx, resourceID, limit)
	if err != nil {
		return nil, mapProvisionErr(err)
	}
	items := make([]api.SelfcheckReportDTO, 0, len(records))
	for _, record := range records {
		items = append(items, api.SelfcheckReportDTO{
			ID: record.ID, VMID: record.VMID, Attempt: record.Attempt,
			Passed: record.Passed, Checks: toSelfcheckChecks(record.Checks, true), CreatedAt: record.CreatedAt,
		})
	}
	return items, nil
}

func (p *provisioner) RunPoolSelfcheck(ctx context.Context, vmID string) (api.SelfcheckDTO, error) {
	state, err := p.svc.AdmitPoolVM(ctx, vmID)
	return toSelfcheckDTO(state, true), mapProvisionErr(err)
}

// keyAdapter adapts the key service. It is a separate type because
// api.AccountService and api.KeyService both declare List with different
// signatures, which one type cannot satisfy at once.
type keyAdapter struct {
	inner api.KeyService
}

func (a *keyAdapter) List(ctx context.Context, userID int64) ([]api.KeyDTO, error) {
	return a.inner.List(ctx, userID)
}

func (a *keyAdapter) Create(ctx context.Context, userID int64, in api.KeyInput) (api.KeyDTO, string, error) {
	return a.inner.Create(ctx, userID, in)
}

func (a *keyAdapter) Update(ctx context.Context, userID, id int64, in api.KeyPatch) (api.KeyDTO, error) {
	return a.inner.Update(ctx, userID, id, in)
}

func (a *keyAdapter) Delete(ctx context.Context, userID, id int64) error {
	return a.inner.Delete(ctx, userID, id)
}

// --- api.AdminService ---

func (p *provisioner) Dashboard(ctx context.Context) (api.AdminDashboardDTO, error) {
	return p.admin.Dashboard(ctx)
}

func (p *provisioner) RentalPool(ctx context.Context) ([]api.RentalPoolRowDTO, error) {
	items, err := p.admin.RentalPool(ctx)
	if err != nil {
		return nil, mapProvisionErr(err)
	}
	site, err := store.LoadSite(ctx, p.settings)
	if err != nil {
		return nil, fmt.Errorf("load selfcheck settings: %w", err)
	}
	for index := range items {
		if items[index].Selfcheck == nil {
			items[index].Selfcheck = &api.SelfcheckDTO{State: "pending"}
		}
		items[index].Selfcheck.MaxAttempts = site.SelfcheckMaxRebuilds
		if items[index].Selfcheck.Checks == nil {
			items[index].Selfcheck.Checks = []api.SelfcheckCheckDTO{}
		}
	}
	return items, nil
}

func (p *provisioner) RentalCandidates(ctx context.Context) ([]api.RentalCandidateDTO, error) {
	return p.admin.RentalCandidates(ctx)
}

func (p *provisioner) RentalAdd(ctx context.Context, vmIDs []string, note string) error {
	return mapProvisionErr(p.admin.RentalAdd(ctx, vmIDs, note))
}

func (p *provisioner) RentalPatch(ctx context.Context, vmID, status, note string) error {
	return p.admin.RentalPatch(ctx, vmID, status, note)
}

func (p *provisioner) RentalRemove(ctx context.Context, vmID string) error {
	return p.admin.RentalRemove(ctx, vmID)
}

func (p *provisioner) Jobs(ctx context.Context, status string, page, pageSize int) ([]api.JobDTO, int64, error) {
	return p.admin.Jobs(ctx, status, page, pageSize)
}

func (p *provisioner) RetryJob(ctx context.Context, id int64) error {
	return p.admin.RetryJob(ctx, id)
}

func (p *provisioner) GrantSubscription(ctx context.Context, adminID, userID int64, kind string, quantity, durationDays int, note string) error {
	return p.admin.GrantSubscription(ctx, adminID, userID, kind, quantity, durationDays, note)
}

func (p *provisioner) PatchSubscription(ctx context.Context, id int64, expiresAt *string, status string) error {
	return p.admin.PatchSubscription(ctx, id, expiresAt, status)
}

func (p *provisioner) RecheckScopes(ctx context.Context) error {
	return p.admin.RecheckScopes(ctx)
}

func (p *provisioner) AdminList(ctx context.Context, kind, status string, userID int64, page, pageSize int) ([]api.AdminAccountDTO, int64, error) {
	return p.adminAccounts(ctx, kind, status, userID, page, pageSize)
}

// AdminRetry re-runs the failed provisioning step for a hosted account. The
// admin endpoint has no owning user in its path, so the owner is read from the
// resource row: passing userID 0 to RetryHosted would filter on user_id = 0 and
// never match.
func (p *provisioner) AdminRetry(ctx context.Context, id int64) error {
	owner, kind, err := p.resourceOwner(ctx, id)
	if err != nil {
		return err
	}
	if kind != "hosted" {
		return api.Err(provision.ErrInvalidState, 409, "invalid_state", "仅托管账号支持重跑失败步骤")
	}
	return p.svc.RetryHosted(ctx, owner, id)
}

// AdminRelease force-releases an account. Any state except released is pushed
// to releasing first (released stays an idempotent success), so the admin action
// can no longer return 200 while the slot and its VM stay in place.
func (p *provisioner) AdminRelease(ctx context.Context, id int64) error {
	if err := p.svc.MarkReleasing(ctx, id); err != nil {
		return err
	}
	return p.svc.ReleaseResource(ctx, id)
}

func (p *provisioner) resourceOwner(ctx context.Context, id int64) (int64, string, error) {
	var owner int64
	var kind string
	err := p.db.QueryRowContext(ctx, `SELECT user_id, kind FROM resources WHERE id = ?`, id).Scan(&owner, &kind)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, "", api.Err(provision.ErrNotFound, 404, "not_found", "账号不存在")
	}
	if err != nil {
		return 0, "", fmt.Errorf("read resource owner: %w", err)
	}
	return owner, kind, nil
}

// ApplyPaidOrder makes the provisioner the order fulfiller.
func (p *provisioner) ApplyPaidOrder(ctx context.Context, orderID int64) error {
	return p.svc.ApplyPaidOrder(ctx, orderID)
}

// SetInflightSource lets the wiring layer supply the gateway's live per-user
// request counter (the overview shows it as "in use").
func (p *provisioner) SetInflightSource(fn func(userID int64) int) {
	p.provisionSpan.inflight = fn
}

// Attach starts the expiry sweep/job loop once the process is about to serve.
func (p *provisioner) Attach(_ *http.Server) {
	go func() {
		if err := p.svc.Run(context.Background()); err != nil {
			p.log.Error("job engine stopped", "err", err)
		}
	}()
}

var (
	_ api.AccountService = (*provisioner)(nil)
	_ api.UsageService   = (*provisioner)(nil)
	_ api.KeyService     = (*keyAdapter)(nil)
	_ api.AdminService   = (*provisioner)(nil)
	_ order.Fulfiller    = (*provisioner)(nil)
)

// adminAccounts joins resources with their owner for the admin console.
func (p *provisioner) adminAccounts(ctx context.Context, kind, status string, userID int64, page, pageSize int) ([]api.AdminAccountDTO, int64, error) {
	query := `SELECT r.id, r.user_id, u.email, r.kind, COALESCE(r.vm_id,''), r.label, r.email_masked,
		r.tier, r.status, COALESCE(r.status_reason,''), COALESCE(r.last_error,''), r.created_at,
		COALESCE(s.expires_at,''), r.selfcheck_state, r.selfcheck_attempts, r.selfcheck_checked_at, r.selfcheck_summary
		FROM resources r JOIN users u ON u.id = r.user_id
		LEFT JOIN subscriptions s ON s.id = r.subscription_id WHERE 1=1`
	args := []any{}
	if kind != "" {
		query += ` AND r.kind = ?`
		args = append(args, kind)
	}
	if status != "" {
		query += ` AND r.status = ?`
		args = append(args, status)
	}
	if userID > 0 {
		query += ` AND r.user_id = ?`
		args = append(args, userID)
	}
	var total int64
	countQuery := `SELECT COUNT(1) FROM (` + query + `)`
	if err := p.db.QueryRowContext(ctx, countQuery, args...).Scan(&total); err != nil {
		return nil, 0, err
	}
	site, err := store.LoadSite(ctx, p.settings)
	if err != nil {
		return nil, 0, fmt.Errorf("load selfcheck settings: %w", err)
	}
	args = append(args, pageSize, (page-1)*pageSize)
	rows, err := p.db.QueryContext(ctx, query+` ORDER BY r.id DESC LIMIT ? OFFSET ?`, args...)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	out := make([]api.AdminAccountDTO, 0)
	for rows.Next() {
		var row api.AdminAccountDTO
		var expires string
		var account provision.Account
		if err := rows.Scan(&row.ID, &row.UserID, &row.UserEmail, &row.Kind, &row.VMID, &row.Label,
			&row.EmailMasked, &row.Tier, &row.Status, &row.StatusReason, &row.LastError, &row.CreatedAt, &expires,
			&account.SelfcheckState, &account.SelfcheckAttempts, &account.SelfcheckCheckedAt, &account.SelfcheckSummary); err != nil {
			return nil, 0, err
		}
		row.Selfcheck = accountSelfcheck(account, site.SelfcheckMaxRebuilds, true)
		row.EmailMasked = privacy.MaskEmail(row.EmailMasked)
		if expires != "" {
			row.ExpiresAt = &expires
		}
		out = append(out, row)
	}
	return out, total, rows.Err()
}

var _ = sql.ErrNoRows
