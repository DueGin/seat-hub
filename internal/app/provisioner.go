package app

import (
	"context"
	"database/sql"
	"log/slog"
	"net/http"

	"github.com/DueGin/vm2api-portal/internal/api"
	"github.com/DueGin/vm2api-portal/internal/order"
	"github.com/DueGin/vm2api-portal/internal/portal"
	"github.com/DueGin/vm2api-portal/internal/provision"
	"github.com/DueGin/vm2api-portal/internal/secret"
	"github.com/DueGin/vm2api-portal/internal/store"
	"github.com/DueGin/vm2api-portal/internal/usage"
	"github.com/DueGin/vm2api-portal/internal/vm2api"
)

// provisioner is the single object the HTTP layer talks to for accounts,
// usage, keys, and the admin console. It fans out to the three lower-level
// services and owns the background job loop.
type provisioner struct {
	*provisionSpan
	svc   *provision.Service
	keys  api.KeyService
	admin api.AdminService
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
	use := usage.New(db.DB, client)
	keysAndAdmin := portal.New(db, client, box, prov)
	if portalAdmin, ok := any(keysAndAdmin).(api.AdminService); ok {
		_ = portalAdmin
	}
	p := &provisioner{
		provisionSpan: &provisionSpan{prov: prov, usage: use, db: db.DB},
		svc:           prov,
		keys:          keysAndAdmin,
		admin:         keysAndAdmin,
		usage:         use,
		log:           logger,
	}
	return p
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

func (a *keyAdapter) Create(ctx context.Context, userID int64, name string) (api.KeyDTO, string, error) {
	return a.inner.Create(ctx, userID, name)
}

func (a *keyAdapter) Update(ctx context.Context, userID, id int64, name, status string) (api.KeyDTO, error) {
	return a.inner.Update(ctx, userID, id, name, status)
}

func (a *keyAdapter) Delete(ctx context.Context, userID, id int64) error {
	return a.inner.Delete(ctx, userID, id)
}

// --- api.AdminService ---

func (p *provisioner) Dashboard(ctx context.Context) (api.AdminDashboardDTO, error) {
	return p.admin.Dashboard(ctx)
}

func (p *provisioner) RentalPool(ctx context.Context) ([]api.RentalPoolRowDTO, error) {
	return p.admin.RentalPool(ctx)
}

func (p *provisioner) RentalCandidates(ctx context.Context) ([]api.RentalCandidateDTO, error) {
	return p.admin.RentalCandidates(ctx)
}

func (p *provisioner) RentalAdd(ctx context.Context, vmIDs []string, note string) error {
	return p.admin.RentalAdd(ctx, vmIDs, note)
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

func (p *provisioner) AdminList(ctx context.Context, kind, status string, userID int64, page, pageSize int) ([]api.AdminAccountDTO, int64, error) {
	return p.adminAccounts(ctx, kind, status, userID, page, pageSize)
}

func (p *provisioner) AdminRetry(ctx context.Context, id int64) error {
	return p.svc.RetryHosted(ctx, 0, id)
}
func (p *provisioner) AdminRelease(ctx context.Context, id int64) error {
	return p.svc.ReleaseResource(ctx, id)
}

// ApplyPaidOrder makes the provisioner the order fulfiller.
func (p *provisioner) ApplyPaidOrder(ctx context.Context, orderID int64) error {
	return p.svc.ApplyPaidOrder(ctx, orderID)
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
		COALESCE(s.expires_at,'')
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
		if err := rows.Scan(&row.ID, &row.UserID, &row.UserEmail, &row.Kind, &row.VMID, &row.Label,
			&row.EmailMasked, &row.Tier, &row.Status, &row.StatusReason, &row.LastError, &row.CreatedAt, &expires); err != nil {
			return nil, 0, err
		}
		if expires != "" {
			row.ExpiresAt = &expires
		}
		out = append(out, row)
	}
	return out, total, rows.Err()
}

var _ = sql.ErrNoRows
