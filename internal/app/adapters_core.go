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
	"github.com/DueGin/seat-hub/internal/mail"
	"github.com/DueGin/seat-hub/internal/signup"
	"github.com/DueGin/seat-hub/internal/store"
)

// usersAdapter implements api.UserStore over the SQLite repositories.
type usersAdapter struct {
	users      *store.Users
	db         *sql.DB
	client     vm2api.Client
	sessions   *store.Sessions
	setStatus  func(context.Context, int64, string) error
	lockScope  func(int64) func()
	deleteKeys func(context.Context, int64, []string) error
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
	if _, err := a.users.GetByID(ctx, id); err != nil {
		return err
	}
	return a.setStatus(ctx, id, status)
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

func (a *usersAdapter) AdminList(ctx context.Context, filter api.AdminUserFilter, page, pageSize int) ([]api.AdminUserDTO, int64, error) {
	items, total, err := a.users.Search(ctx, store.UserFilter{Q: filter.Q, Role: filter.Role, Status: filter.Status}, page, pageSize)
	if err != nil {
		return nil, 0, err
	}
	out := make([]api.AdminUserDTO, 0, len(items))
	for _, u := range items {
		out = append(out, a.toAdminDTO(ctx, u))
	}
	return out, total, nil
}

func (a *usersAdapter) toAdminDTO(ctx context.Context, u store.User) api.AdminUserDTO {
	dto := api.AdminUserDTO{UserDTO: toUserDTO(u), VM2APIUserID: u.VM2APIUserID, VM2APIUsername: u.VM2APIUsername, Note: u.Note, LastLoginAt: u.LastLoginAt, LastLoginIP: u.LastLoginIP}
	a.fillEntitlement(ctx, u.ID, &dto)
	if a.sessions != nil {
		dto.ActiveSessions, _ = a.sessions.CountActive(ctx, u.ID)
	}
	return dto
}

func (a *usersAdapter) SetEmail(ctx context.Context, id int64, email string) error {
	err := a.users.SetEmail(ctx, id, email)
	if errors.Is(err, store.ErrEmailTaken) {
		return api.Err(err, http.StatusConflict, "email_taken", "该邮箱已被其他用户使用")
	}
	return err
}

func (a *usersAdapter) SetNote(ctx context.Context, id int64, note string) error {
	return a.users.SetNote(ctx, id, note)
}

func (a *usersAdapter) Records(ctx context.Context, id int64) (api.UserRecordsDTO, error) {
	records, err := a.users.Records(ctx, id)
	return api.UserRecordsDTO(records), err
}

func (a *usersAdapter) Sessions(ctx context.Context, id int64) ([]api.SessionDTO, error) {
	items, err := a.sessions.ListActive(ctx, id)
	if err != nil {
		return nil, err
	}
	out := make([]api.SessionDTO, 0, len(items))
	for _, item := range items {
		out = append(out, api.SessionDTO(item))
	}
	return out, nil
}

func (a *usersAdapter) RevokeSessions(ctx context.Context, id int64) error {
	return a.sessions.DeleteAll(ctx, id)
}

// Delete removes a user that never bought anything. Upstream keys go first, so
// a key cannot outlive its owner and keep spending. Disable the upstream user.
func (a *usersAdapter) Delete(ctx context.Context, id int64) error {
	if a.lockScope != nil {
		unlock := a.lockScope(id)
		defer unlock()
	}
	u, err := a.users.GetByID(ctx, id)
	if errors.Is(err, store.ErrNotFound) {
		return api.Err(err, 404, "user_not_found", "用户不存在")
	}
	if err != nil {
		return err
	}
	records, err := a.users.Records(ctx, id)
	if err != nil {
		return err
	}
	if records.Any() {
		return userHasRecords(records)
	}
	rows, err := a.db.QueryContext(ctx, `SELECT vm2api_key_id,previous_vm2api_key_ids FROM api_keys WHERE user_id = ?`, id)
	if err != nil {
		return fmt.Errorf("list keys to delete: %w", err)
	}
	var keys []string
	for rows.Next() {
		var keyID, previous string
		if err := rows.Scan(&keyID, &previous); err != nil {
			rows.Close()
			return err
		}
		keys = append(keys, store.DecodeStrings(previous)...)
		if keyID != "" {
			keys = append(keys, keyID)
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	if a.client != nil {
		if err := a.deleteKeys(ctx, id, keys); err != nil {
			return fmt.Errorf("delete vm2api keys: %w", err)
		}
		if u.VM2APIUserID != "" {
			if err := a.client.SetUserEnabled(ctx, u.VM2APIUserID, false); err != nil && !vm2api.IsStatus(err, 404) {
				return fmt.Errorf("disable vm2api user: %w", err)
			}
		}
	}
	err = a.users.Delete(ctx, id)
	if errors.Is(err, store.ErrUserHasRecords) {
		// An order arrived between the check and the delete.
		latest, _ := a.users.Records(ctx, id)
		return userHasRecords(latest)
	}
	return err
}

func userHasRecords(records store.UserRecords) error {
	parts := make([]string, 0, 4)
	for _, item := range []struct {
		count int64
		unit  string
	}{{records.Orders, "笔订单"}, {records.Subscriptions, "个订阅"}, {records.Resources, "个账号资源"}, {records.Tickets, "条工单记录"}} {
		if item.count > 0 {
			parts = append(parts, fmt.Sprintf("%d %s", item.count, item.unit))
		}
	}
	return &api.UserError{Status: http.StatusConflict, Code: "user_has_records", Message: "该用户有 " + strings.Join(parts, "、") + "，不能删除，请改为停用"}
}

func (a *usersAdapter) AdminDetail(ctx context.Context, id int64) (api.AdminUserDTO, error) {
	u, err := a.users.GetByID(ctx, id)
	if errors.Is(err, store.ErrNotFound) {
		return api.AdminUserDTO{}, api.Err(store.ErrNotFound, 404, "user_not_found", "用户不存在")
	}
	if err != nil {
		return api.AdminUserDTO{}, err
	}
	return a.toAdminDTO(ctx, u), nil
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
	mail     interface {
		Configured(context.Context) (bool, error)
	}
}

func (a *settingsAdapter) Get(ctx context.Context) (api.SettingsPayload, error) {
	site, err := store.LoadSite(ctx, a.settings)
	if err != nil {
		return api.SettingsPayload{}, err
	}
	site.Payment, err = normalizePayment(site.Payment)
	if err != nil {
		return api.SettingsPayload{}, fmt.Errorf("normalize payment settings: %w", err)
	}
	return toSettingsPayload(site), nil
}

func (a *settingsAdapter) Put(ctx context.Context, p api.SettingsPayload, actorID int64) error {
	prev, err := store.LoadSite(ctx, a.settings)
	if err != nil {
		return err
	}
	whitelist, err := signup.NormalizeWhitelist(p.RegisterEmailWhitelist)
	if err != nil {
		return api.Err(err, 422, "invalid_settings", err.Error())
	}
	if p.RegisterEmailVerification {
		ready := false
		if a.mail != nil {
			ready, err = a.mail.Configured(ctx)
			if err != nil {
				return fmt.Errorf("check registration smtp: %w", err)
			}
		}
		if !ready {
			return &api.UserError{Status: 422, Code: "invalid_settings", Message: "请先在下方「邮件发送」里配置并启用 SMTP，再开启注册邮箱验证"}
		}
	}
	// Payment secrets: a masked or empty value keeps the stored one.
	merged, err := mergeSecrets(prev.Payment, p.Payment)
	if err != nil {
		return err
	}
	merged, err = normalizePayment(merged)
	if err != nil {
		return fmt.Errorf("normalize payment settings: %w", err)
	}
	if err := validatePayment(merged); err != nil {
		return err
	}
	site := store.Site{
		SiteName:                       p.SiteName,
		RegistrationOpen:               p.RegistrationOpen,
		RegisterEmailVerification:      p.RegisterEmailVerification,
		RegisterEmailWhitelist:         whitelist,
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
		RegisterEmailVerification:      site.RegisterEmailVerification,
		RegisterEmailWhitelist:         site.RegisterEmailWhitelist,
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
	if nextDoc == nil {
		return nil, api.Err(errors.New("invalid payment document"), 422, "invalid_settings", "收款方式配置必须是 JSON 对象")
	}
	mergeSecretMap(prevDoc, nextDoc)
	out, err := json.Marshal(nextDoc)
	if err != nil {
		return nil, err
	}
	return out, nil
}

func mergeSecretMap(prev, next map[string]any) {
	for k, v := range prev {
		if _, exists := next[k]; !exists {
			next[k] = v
		}
	}
	for k, v := range next {
		if str, ok := v.(string); ok && (k == "key" || k == "private_key") && (str == "***" || str == "") {
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

// registrationAdapter maps domain errors to the registration HTTP contract.
type registrationAdapter struct{ svc *signup.Service }

func registrationError(err error) error {
	var cooldown *signup.CooldownError
	switch {
	case errors.As(err, &cooldown):
		return api.Err(err, 429, "code_cooldown", cooldown.Error())
	case errors.Is(err, signup.ErrRateLimited):
		return api.Err(err, 429, "code_rate_limited", "验证码获取次数过多，请稍后再试")
	case errors.Is(err, signup.ErrInvalid):
		return api.Err(err, 422, "code_invalid", "验证码错误")
	case errors.Is(err, signup.ErrExpired):
		return api.Err(err, 422, "code_expired", "验证码已失效，请重新获取")
	case errors.Is(err, store.ErrEmailTaken):
		return api.ErrEmailTaken
	case errors.Is(err, mail.ErrNotConfigured):
		return api.Err(err, 422, "mail_not_configured", "请先启用并配置邮件发送")
	default:
		return err
	}
}
func (a *registrationAdapter) Issue(ctx context.Context, email, ip, siteName string) error {
	return registrationError(a.svc.Issue(ctx, email, ip, siteName))
}
func (a *registrationAdapter) Register(ctx context.Context, email, passwordHash, role, code string) (api.UserDTO, error) {
	u, err := a.svc.Register(ctx, email, passwordHash, role, code)
	return toUserDTO(u), registrationError(err)
}
