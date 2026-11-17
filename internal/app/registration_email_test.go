package app

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/DueGin/seat-hub/internal/api"
	"github.com/DueGin/seat-hub/internal/mail"
	"github.com/DueGin/seat-hub/internal/store"
)

func enableRegistrationEmail(t *testing.T, h *harness) {
	t.Helper()
	h.login("admin@example.com", "admin-pass-123")
	h.mustOK(http.MethodPut, "/api/admin/settings/mail", `{"enabled":true,"host":"localhost","port":25,"security":"none","from_address":"sender@example.com","admin_emails":[]}`, nil)
	h.mustOK(http.MethodPut, "/api/admin/settings", `{"register_email_verification":true}`, nil)
}
func registrationCode(t *testing.T, h *harness, email string) string {
	t.Helper()
	// Read the queued mail, independent of SMTP availability and delivery timing.
	var raw string
	if err := h.app.DB.QueryRow(`SELECT message FROM mail_outbox WHERE json_extract(message,'$.to[0]')=? ORDER BY id DESC LIMIT 1`, email).Scan(&raw); err != nil {
		t.Fatal(err)
	}
	var msg mail.Message
	if err := json.Unmarshal([]byte(raw), &msg); err != nil {
		t.Fatal(err)
	}
	return strings.TrimPrefix(msg.Subject, "注册验证码：")
}
func TestRegistrationEmailSettingsHTTP(t *testing.T) {
	h := newHarness(t)
	h.login("admin@example.com", "admin-pass-123")
	var cfg api.SettingsPayload
	h.mustOK(http.MethodGet, "/api/admin/settings", "", &cfg)
	if cfg.RegisterEmailVerification || cfg.RegisterEmailWhitelist == nil || len(cfg.RegisterEmailWhitelist) != 0 {
		t.Fatalf("default=%+v", cfg)
	}
	h.expectError(http.MethodPut, "/api/admin/settings", `{"register_email_verification":true}`, 422, "invalid_settings")
	_, env := h.call(http.MethodPut, "/api/admin/settings", `{"register_email_verification":true}`, nil)
	if env.Error.Message != "请先在下方「邮件发送」里配置并启用 SMTP，再开启注册邮箱验证" {
		t.Fatal(env.Error.Message)
	}
	for _, bad := range []string{"localhost", "*qq.com", "a.*.com", "a..com", "Name <a@gmail.com>", "a@gmail.com,b@gmail.com"} {
		body, _ := json.Marshal(map[string]any{"register_email_whitelist": []string{bad}})
		h.expectError(http.MethodPut, "/api/admin/settings", string(body), 422, "invalid_settings")
	}
	entries := []string{}
	for i := 0; i < 201; i++ {
		entries = append(entries, fmt.Sprintf("d%d.example.com", i))
	}
	body, _ := json.Marshal(map[string]any{"register_email_whitelist": entries})
	h.expectError(http.MethodPut, "/api/admin/settings", string(body), 422, "invalid_settings")
	h.mustOK(http.MethodPut, "/api/admin/settings", `{"register_email_whitelist":[" QQ.COM ","qq.com","","*.edu.cn","Someone@Gmail.com"]}`, &cfg)
	if strings.Join(cfg.RegisterEmailWhitelist, ",") != "qq.com,*.edu.cn,someone@gmail.com" {
		t.Fatal(cfg.RegisterEmailWhitelist)
	}
	h.mustOK(http.MethodPut, "/api/admin/settings", `{"site_name":"邮箱注册站点"}`, &cfg)
	if len(cfg.RegisterEmailWhitelist) != 3 || cfg.RegisterEmailVerification {
		t.Fatal("partial update lost policy")
	}
	var public api.SiteConfig
	h.mustOK(http.MethodGet, "/api/public/config", "", &public)
	_, env = h.call(http.MethodGet, "/api/public/config", "", nil)
	if strings.Contains(string(env.Data), "someone") || strings.Contains(string(env.Data), "whitelist") || strings.Join(public.RegisterEmailDomains, ",") != "qq.com,*.edu.cn" {
		t.Fatalf("public=%s", env.Data)
	}
	h.mustOK(http.MethodPut, "/api/admin/settings", `{"register_email_whitelist":[]}`, &cfg)
	if len(cfg.RegisterEmailWhitelist) != 0 {
		t.Fatal("clear whitelist failed")
	}
	enableRegistrationEmail(t, h)
	h.mustOK(http.MethodPut, "/api/admin/settings", `{"site_name":"部分更新"}`, &cfg)
	if !cfg.RegisterEmailVerification {
		t.Fatal("partial update disabled verification")
	}
	h.mustOK(http.MethodGet, "/api/public/config", "", &public)
	if !public.RegisterEmailVerification || public.RegisterEmailDomains == nil {
		t.Fatal("public missing defaults")
	}
}
func TestRegistrationEmailHTTP(t *testing.T) {
	h := newHarness(t)
	guest := h.session()
	guest.expectError(http.MethodPost, "/api/auth/register/code", `{"email":"a@example.com"}`, 409, "verification_disabled")
	guest.mustOK(http.MethodPost, "/api/auth/register", `{"email":"old@example.com","password":"password-123"}`, nil)
	enableRegistrationEmail(t, h)
	guest.expectError(http.MethodPost, "/api/auth/register/code", `{"email":"Name <a@example.com>"}`, 422, "invalid_email")
	guest.expectError(http.MethodPost, "/api/auth/register/code", `{"email":"admin@example.com"}`, 409, "email_taken")
	guest.expectError(http.MethodPost, "/api/auth/register", `{"email":"a@example.com","password":"password-123"}`, 422, "code_required")
	var issued struct {
		CooldownSeconds int `json:"cooldown_seconds"`
	}
	guest.mustOK(http.MethodPost, "/api/auth/register/code", `{"email":" A@Example.COM "}`, &issued)
	if issued.CooldownSeconds != 60 {
		t.Fatalf("cooldown=%d", issued.CooldownSeconds)
	}
	code := registrationCode(t, h, "a@example.com")
	var hash string
	h.app.DB.QueryRow(`SELECT code_hash FROM registration_email_codes WHERE email='a@example.com'`).Scan(&hash)
	if len(code) != 6 || len(hash) != 64 || hash == code {
		t.Fatal("invalid persisted hash")
	}
	guest.expectError(http.MethodPost, "/api/auth/register/code", `{"email":"a@example.com"}`, 429, "code_cooldown")
	guest.expectError(http.MethodPost, "/api/auth/register", `{"email":"a@example.com","password":"password-123","code":"bad"}`, 422, "code_invalid")
	guest.mustOK(http.MethodPost, "/api/auth/register", fmt.Sprintf(`{"email":"a@example.com","password":"password-123","code":%q}`, code), nil)
	guest.mustOK(http.MethodGet, "/api/auth/me", "", nil)
	guest.expectError(http.MethodPost, "/api/auth/register", fmt.Sprintf(`{"email":"a@example.com","password":"password-123","code":%q}`, code), 422, "code_expired")
	guest.mustOK(http.MethodPost, "/api/auth/register/code", `{"email":"expired@example.com"}`, nil)
	expired := registrationCode(t, h, "expired@example.com")
	if _, err := h.app.DB.Exec(`UPDATE registration_email_codes SET expires_at=? WHERE email='expired@example.com'`, store.FmtTime(time.Now().Add(-time.Minute))); err != nil {
		t.Fatal(err)
	}
	guest.expectError(http.MethodPost, "/api/auth/register", fmt.Sprintf(`{"email":"expired@example.com","password":"password-123","code":%q}`, expired), 422, "code_expired")
	guest.mustOK(http.MethodPost, "/api/auth/register/code", `{"email":"locked@example.com"}`, nil)
	for i := 0; i < 5; i++ {
		want := "code_invalid"
		if i == 4 {
			want = "code_expired"
		}
		guest.expectError(http.MethodPost, "/api/auth/register", `{"email":"locked@example.com","password":"password-123","code":"bad"}`, 422, want)
	}
	locked := registrationCode(t, h, "locked@example.com")
	guest.expectError(http.MethodPost, "/api/auth/register", fmt.Sprintf(`{"email":"locked@example.com","password":"password-123","code":%q}`, locked), 422, "code_expired")
	h.mustOK(http.MethodPut, "/api/admin/settings", `{"registration_open":false}`, nil)
	guest.expectError(http.MethodPost, "/api/auth/register/code", `{"email":"new@example.com"}`, 403, "registration_closed")
	guest.expectError(http.MethodPost, "/api/auth/register", `{"email":"new@example.com","password":"password-123","code":"123456"}`, 403, "registration_closed")
	if guest.tryLogin("old@example.com", "password-123") != 200 {
		t.Fatal("existing login affected")
	}
}
func TestRegistrationWhitelistAndAdminBypassHTTP(t *testing.T) {
	h := newHarness(t)
	h.login("admin@example.com", "admin-pass-123")
	h.mustOK(http.MethodPut, "/api/admin/settings", `{"register_email_whitelist":["qq.com","*.edu.cn","one@gmail.com"]}`, nil)
	guest := h.session()
	for _, email := range []string{"no@gmail.com", "a@edu.cn", "a@mail.qq.com"} {
		guest.expectError(http.MethodPost, "/api/auth/register", fmt.Sprintf(`{"email":%q,"password":"password-123"}`, email), 422, "email_not_allowed")
	}
	for _, email := range []string{"a@qq.com", "a@school.edu.cn", "one@gmail.com"} {
		guest.mustOK(http.MethodPost, "/api/auth/register", fmt.Sprintf(`{"email":%q,"password":"password-123"}`, email), nil)
	}
	enableRegistrationEmail(t, h)
	guest.expectError(http.MethodPost, "/api/auth/register/code", `{"email":"no@gmail.com"}`, 422, "email_not_allowed")
	created, password := h.createUser(`{"email":"admin-created@blocked.com"}`)
	if h.session().tryLogin(created.Email, password) != 200 {
		t.Fatal("admin-created user checked by policy")
	}
	if h.session().tryLogin("admin@example.com", "admin-pass-123") != 200 {
		t.Fatal("login checked whitelist")
	}
}

func TestRegistrationCodeLimitsHTTP(t *testing.T) {
	h := newHarness(t)
	enableRegistrationEmail(t, h)
	guest := h.session()
	insert := func(email, ip string, issued time.Time) {
		t.Helper()
		if _, err := h.app.DB.Exec(`INSERT INTO registration_email_codes(email,ip,code_hash,created_at,expires_at) VALUES (?,?,?,?,?)`, email, ip, strings.Repeat("a", 64), store.FmtTime(issued), store.FmtTime(issued.Add(10*time.Minute))); err != nil {
			t.Fatal(err)
		}
	}
	now := time.Now().UTC()
	for i := range 10 {
		insert("daily@example.com", "other-ip", now.Add(-time.Duration(i+2)*time.Minute))
	}
	guest.expectError(http.MethodPost, "/api/auth/register/code", `{"email":"daily@example.com"}`, 429, "code_rate_limited")
	for i := range 20 {
		insert(fmt.Sprintf("ip%d@example.com", i), "203.0.113.9", now.Add(-2*time.Minute))
	}
	status, env := guest.call(http.MethodPost, "/api/auth/register/code", `{"email":"ip-limit@example.com"}`, map[string]string{"X-Forwarded-For": "203.0.113.9"})
	if status != 429 || env.Error.Code != "code_rate_limited" {
		t.Fatalf("ip limit: status=%d error=%+v", status, env.Error)
	}
	insert("cooldown@example.com", "other-ip", now.Add(-25*time.Second))
	status, env = guest.call(http.MethodPost, "/api/auth/register/code", `{"email":"cooldown@example.com"}`, nil)
	if status != 429 || env.Error.Code != "code_cooldown" || !strings.Contains(env.Error.Message, "秒") {
		t.Fatalf("cooldown: status=%d error=%+v", status, env.Error)
	}
	// The limiter uses clientIP: an unrelated client still receives a code.
	status, env = guest.call(http.MethodPost, "/api/auth/register/code", `{"email":"other-ip@example.com"}`, map[string]string{"X-Forwarded-For": "203.0.113.10"})
	if status != 200 || !env.OK {
		t.Fatalf("separate ip: status=%d error=%+v", status, env.Error)
	}
}
