package app

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/DueGin/seat-hub/internal/catalog"
	"github.com/DueGin/seat-hub/internal/vm2api"

	"github.com/DueGin/seat-hub/internal/api"
	"github.com/DueGin/seat-hub/internal/auth"
	"github.com/DueGin/seat-hub/internal/store"
)

// usersAdapter implements api.UserStore over the SQLite repositories.
type usersAdapter struct {
	users  *store.Users
	db     *sql.DB
	client vm2api.Client
	// entitle is injected by the wiring layer (provision.Service.Entitlements).
	// It is a function rather than a concrete service to keep this package's
	// dependency graph acyclic.
	entitle func(ctx context.Context, userID int64) (api.AdminUserDTO, error)
}

func toUserDTO(u store.User) api.UserDTO {
	return api.UserDTO{ID: u.ID, Email: u.Email, Role: u.Role, Status: u.Status, CreatedAt: u.CreatedAt}
}

func (a *usersAdapter) ByID(ctx context.Context, id int64) (api.UserDTO, error) {
	u, err := a.users.GetByID(ctx, id)
	if errors.Is(err, store.ErrNotFound) {
		return api.UserDTO{}, api.Err(store.ErrNotFound, 404, "user_not_found", "用户不存在")
	}
	return toUserDTO(u), err
}

func (a *usersAdapter) ByEmail(ctx context.Context, email string) (api.UserDTO, error) {
	u, err := a.users.GetByEmail(ctx, email)
	if errors.Is(err, store.ErrNotFound) {
		return api.UserDTO{}, api.Err(store.ErrNotFound, 404, "user_not_found", "用户不存在")
	}
	return toUserDTO(u), err
}

func (a *usersAdapter) Create(ctx context.Context, email, passwordHash, role string) (api.UserDTO, error) {
	u, err := a.users.Create(ctx, email, passwordHash, role)
	if errors.Is(err, store.ErrEmailTaken) {
		return api.UserDTO{}, api.Err(store.ErrEmailTaken, http.StatusConflict, "email_taken", "该邮箱已注册")
	}
	return toUserDTO(u), err
}

func (a *usersAdapter) Count(ctx context.Context) (int64, error) { return a.users.Count(ctx) }
func (a *usersAdapter) CountAdmins(ctx context.Context) (int64, error) {
	return a.users.CountAdmins(ctx)
}
func (a *usersAdapter) SetPassword(ctx context.Context, id int64, hash string) error {
	return a.users.SetPassword(ctx, id, hash)
}

// SetStatus flips the local account and mirrors the change upstream: a
// disabled portal user must not be able to spend through a live vm2api key.
func (a *usersAdapter) SetStatus(ctx context.Context, id int64, status string) error {
	u, err := a.users.GetByID(ctx, id)
	if err != nil {
		return err
	}
	if err := a.users.SetStatus(ctx, id, status); err != nil {
		return err
	}
	if u.VM2APIUserID == "" || a.client == nil {
		return nil
	}
	if err := a.client.SetUserEnabled(ctx, u.VM2APIUserID, status == "active"); err != nil {
		// The local state already changed; surface the upstream miss so the
		// admin console can retry instead of silently diverging.
		return fmt.Errorf("sync vm2api user state: %w", err)
	}
	if status != "active" {
		rows, err := a.db.QueryContext(ctx, `SELECT vm2api_key_id FROM api_keys WHERE user_id = ? AND status = 'active'`, id)
		if err != nil {
			return fmt.Errorf("list keys to disable: %w", err)
		}
		defer rows.Close()
		var keys []string
		for rows.Next() {
			var keyID string
			if err := rows.Scan(&keyID); err != nil {
				return err
			}
			keys = append(keys, keyID)
		}
		if err := rows.Err(); err != nil {
			return err
		}
		for _, keyID := range keys {
			active := false
			if err := a.client.UpdateKey(ctx, keyID, vm2api.UpdateKeyInput{Active: &active}); err != nil {
				return fmt.Errorf("disable vm2api key %s: %w", keyID, err)
			}
		}
	}
	return nil
}
func (a *usersAdapter) SetRole(ctx context.Context, id int64, role string) error {
	return a.users.SetRole(ctx, id, role)
}

func (a *usersAdapter) List(ctx context.Context, q string, page, pageSize int) ([]api.UserDTO, int64, error) {
	items, total, err := a.users.List(ctx, q, page, pageSize)
	if err != nil {
		return nil, 0, err
	}
	out := make([]api.UserDTO, 0, len(items))
	for _, u := range items {
		out = append(out, toUserDTO(u))
	}
	return out, total, nil
}

func (a *usersAdapter) AdminList(ctx context.Context, q string, page, pageSize int) ([]api.AdminUserDTO, int64, error) {
	items, total, err := a.users.List(ctx, q, page, pageSize)
	if err != nil {
		return nil, 0, err
	}
	out := make([]api.AdminUserDTO, 0, len(items))
	for _, u := range items {
		dto := api.AdminUserDTO{UserDTO: toUserDTO(u), VM2APIUserID: u.VM2APIUserID}
		a.fillEntitlement(ctx, u.ID, &dto)
		out = append(out, dto)
	}
	return out, total, nil
}

func (a *usersAdapter) AdminDetail(ctx context.Context, id int64) (api.AdminUserDTO, error) {
	u, err := a.users.GetByID(ctx, id)
	if errors.Is(err, store.ErrNotFound) {
		return api.AdminUserDTO{}, api.Err(store.ErrNotFound, 404, "user_not_found", "用户不存在")
	}
	if err != nil {
		return api.AdminUserDTO{}, err
	}
	dto := api.AdminUserDTO{UserDTO: toUserDTO(u), VM2APIUserID: u.VM2APIUserID}
	a.fillEntitlement(ctx, u.ID, &dto)
	return dto, nil
}

// fillEntitlement attaches the concurrency and seat counters the admin console
// shows. A failure leaves the counters at zero rather than failing the list:
// the page is still usable without them.
func (a *usersAdapter) fillEntitlement(ctx context.Context, userID int64, dto *api.AdminUserDTO) {
	if a.entitle == nil {
		return
	}
	withEnt, err := a.entitle(ctx, userID)
	if err != nil {
		return
	}
	dto.ConcurrencyTotal = withEnt.ConcurrencyTotal
	dto.HostedUsed = withEnt.HostedUsed
	dto.HostedTotal = withEnt.HostedTotal
	dto.RentalUsed = withEnt.RentalUsed
	dto.RentalTotal = withEnt.RentalTotal
}

// settingsAdapter implements api.SettingsStore over settings + secret.Box.
type settingsAdapter struct {
	settings *store.Settings
	audit    *store.Audit
}

func (a *settingsAdapter) Get(ctx context.Context) (api.SettingsPayload, error) {
	site, err := store.LoadSite(ctx, a.settings)
	if err != nil {
		return api.SettingsPayload{}, err
	}
	return toSettingsPayload(site), nil
}

func (a *settingsAdapter) Put(ctx context.Context, p api.SettingsPayload, actorID int64) error {
	prev, err := store.LoadSite(ctx, a.settings)
	if err != nil {
		return err
	}
	// Payment secrets: a masked or empty value keeps the stored one.
	merged, err := mergeSecrets(prev.Payment, p.Payment)
	if err != nil {
		return err
	}
	site := store.Site{
		SiteName:                       p.SiteName,
		RegistrationOpen:               p.RegistrationOpen,
		BaseConcurrency:                p.BaseConcurrency,
		ConcurrencyPerHostedSeat:       p.ConcurrencyPerHostedSeat,
		ConcurrencyPerRental:           p.ConcurrencyPerRental,
		HostedGraceHours:               p.HostedGraceHours,
		RentalGraceHours:               p.RentalGraceHours,
		GatewayQueueTimeoutMS:          p.GatewayQueueTimeoutMS,
		HostedVMTemplate:               p.HostedVMTemplate,
		Payment:                        merged,
		SelfcheckRequireDedicatedProxy: p.SelfcheckRequireDedicatedProxy,
		SelfcheckRequireTimezoneMatch:  p.SelfcheckRequireTimezoneMatch,
		SelfcheckMaxRebuilds:           p.SelfcheckMaxRebuilds,
		SelfcheckRecheckMinutes:        p.SelfcheckRecheckMinutes,
	}
	if site.SiteName == "" {
		site.SiteName = prev.SiteName
	}
	if site.BaseConcurrency < 0 || site.ConcurrencyPerHostedSeat < 0 || site.ConcurrencyPerRental < 0 ||
		site.HostedGraceHours < 0 || site.RentalGraceHours < 0 || site.GatewayQueueTimeoutMS < 0 {
		return api.Err(errors.New("negative setting"), 422, "invalid_settings", "设置值不能为负数")
	}
	if len(site.HostedVMTemplate) == 0 {
		site.HostedVMTemplate = prev.HostedVMTemplate
	}
	if site.SelfcheckMaxRebuilds < 0 || site.SelfcheckMaxRebuilds > 10 ||
		site.SelfcheckRecheckMinutes < 5 || site.SelfcheckRecheckMinutes > 1440 {
		return &api.UserError{Status: 422, Code: "invalid_settings", Message: "自检重建次数须为 0 至 10，巡检间隔须为 5 至 1440 分钟"}
	}
	if err := a.settings.Put(ctx, "site", site); err != nil {
		return err
	}
	if a.audit != nil {
		a.audit.Write(ctx, actorID, "settings.update", "site", map[string]any{"base_concurrency": site.BaseConcurrency})
	}
	return nil
}

func toSettingsPayload(site store.Site) api.SettingsPayload {
	return api.SettingsPayload{
		SiteName:                       site.SiteName,
		RegistrationOpen:               site.RegistrationOpen,
		BaseConcurrency:                site.BaseConcurrency,
		ConcurrencyPerHostedSeat:       site.ConcurrencyPerHostedSeat,
		ConcurrencyPerRental:           site.ConcurrencyPerRental,
		HostedGraceHours:               site.HostedGraceHours,
		RentalGraceHours:               site.RentalGraceHours,
		GatewayQueueTimeoutMS:          site.GatewayQueueTimeoutMS,
		HostedVMTemplate:               site.HostedVMTemplate,
		Payment:                        site.Payment,
		SelfcheckRequireDedicatedProxy: site.SelfcheckRequireDedicatedProxy,
		SelfcheckRequireTimezoneMatch:  site.SelfcheckRequireTimezoneMatch,
		SelfcheckMaxRebuilds:           site.SelfcheckMaxRebuilds,
		SelfcheckRecheckMinutes:        site.SelfcheckRecheckMinutes,
	}
}

// authAdapter bridges internal/auth.Service to api.AuthService, which speaks
// api.UserDTO instead of store.User.
type authAdapter struct {
	svc      *auth.Service
	users    *store.Users
	limiter  *auth.Limiter
	apiAllow func(ip, email string) bool
}

func (a *authAdapter) Login(ctx context.Context, email, password, ip, ua string) (string, api.UserDTO, error) {
	res, err := a.svc.Login(ctx, email, password, ip, ua)
	if err != nil {
		return "", api.UserDTO{}, err
	}
	return res.Token, toUserDTO(res.User), nil
}

func (a *authAdapter) Logout(ctx context.Context, tokenHash string) error {
	return a.svc.Sessions.Delete(ctx, tokenHash)
}

func (a *authAdapter) SetCookie(w http.ResponseWriter, token string) { a.svc.SetCookie(w, token) }
func (a *authAdapter) ClearCookie(w http.ResponseWriter)             { a.svc.ClearCookie(w) }
func (a *authAdapter) Middleware(next http.Handler) http.Handler     { return a.svc.Middleware(next) }

func (a *authAdapter) UserFrom(ctx context.Context) (api.UserDTO, bool) {
	u, ok := auth.UserFrom(ctx)
	if !ok {
		return api.UserDTO{}, false
	}
	return toUserDTO(*u), true
}

func (a *authAdapter) TokenHashFrom(ctx context.Context) string { return auth.TokenHashFrom(ctx) }

func (a *authAdapter) Allowed(ip, email string) bool {
	if a.limiter == nil {
		return true
	}
	key := strings.ToLower(ip + "|" + email)
	return a.limiter.Allow(key)
}

func (a *authAdapter) HashPassword(password string) (string, error) {
	return auth.HashPassword(password)
}

func (a *authAdapter) VerifyPassword(userID int64, password string) bool {
	return a.svc.VerifyPassword(userID, password)
}

// vmHealthFunc adapts the vm2api client into catalog.HealthFunc: a VM only
// counts toward rental stock when vm2api reports a usable schedule state and a
// credential that is neither missing nor rejected.
func vmHealthFunc(getUsage func(ctx context.Context, vmID string) (vm2api.AccountUsage, error)) catalog.HealthFunc {
	return func(vmID string) (bool, bool) {
		if getUsage == nil {
			return false, false
		}
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		usage, err := getUsage(ctx, vmID)
		if err != nil {
			return false, false
		}
		vm := usage.VM
		if vm.CredentialRejected || !vm.HasCredential {
			return false, true
		}
		switch vm.ScheduleState {
		case "on", "restricted":
			return true, true
		default:
			return false, true
		}
	}
}

// mergeSecrets keeps the stored payment document's secrets when the incoming
// payload carries the mask or an empty value.
func mergeSecrets(prev, next json.RawMessage) (json.RawMessage, error) {
	if len(next) == 0 {
		return prev, nil
	}
	var prevDoc, nextDoc map[string]any
	if len(prev) > 0 {
		if err := json.Unmarshal(prev, &prevDoc); err != nil {
			return nil, err
		}
	}
	if err := json.Unmarshal(next, &nextDoc); err != nil {
		return nil, err
	}
	mergeSecretMap(prevDoc, nextDoc)
	out, err := json.Marshal(nextDoc)
	if err != nil {
		return nil, err
	}
	return out, nil
}

func mergeSecretMap(prev, next map[string]any) {
	for k, v := range next {
		if str, ok := v.(string); ok && (str == "***" || str == "") {
			if old, ok := prev[k]; ok {
				next[k] = old
			}
			continue
		}
		nextMap, okNext := v.(map[string]any)
		prevMap, okPrev := prev[k].(map[string]any)
		if okNext && okPrev {
			mergeSecretMap(prevMap, nextMap)
		}
	}
}
