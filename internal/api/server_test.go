package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	authed "github.com/DueGin/seat-hub/internal/auth"
)

// --- fakes ---

type fakeAuth struct {
	user  *UserDTO
	token string

	loginErr error

	mu       sync.Mutex
	revoked  []string // "userID|keepHash" pairs passed to RevokeOthers
	tokenHsh string
}

func (f *fakeAuth) Login(ctx context.Context, email, password, ip, ua string) (string, UserDTO, error) {
	if f.loginErr != nil {
		return "", UserDTO{}, f.loginErr
	}
	if f.user == nil {
		return "", UserDTO{}, ErrInvalidCredentials
	}
	return "tok", *f.user, nil
}
func (f *fakeAuth) Logout(ctx context.Context, hash string) error { return nil }
func (f *fakeAuth) SetCookie(w http.ResponseWriter, token string) {}
func (f *fakeAuth) ClearCookie(w http.ResponseWriter)             {}
func (f *fakeAuth) Middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if f.user != nil {
			ctx := context.WithValue(r.Context(), fakeUserKey, *f.user)
			next.ServeHTTP(w, r.WithContext(ctx))
			return
		}
		next.ServeHTTP(w, r)
	})
}
func (f *fakeAuth) UserFrom(ctx context.Context) (UserDTO, bool) {
	u, ok := ctx.Value(fakeUserKey).(UserDTO)
	return u, ok
}
func (f *fakeAuth) TokenHashFrom(ctx context.Context) string   { return f.tokenHsh }
func (f *fakeAuth) Allowed(ip, email string) bool              { return true }
func (f *fakeAuth) HashPassword(p string) (string, error)      { return "h", nil }
func (f *fakeAuth) VerifyPassword(userID int64, p string) bool { return p == "right" }

// RevokeOthers records the call instead of touching sessions.
func (f *fakeAuth) RevokeOthers(ctx context.Context, userID int64, keepTokenHash string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.revoked = append(f.revoked, fmt.Sprintf("%d|%s", userID, keepTokenHash))
	return nil
}

type fakeUserKeyType struct{}

var fakeUserKey fakeUserKeyType

type fakeUsers struct{}

func (fakeUsers) ByID(ctx context.Context, id int64) (UserDTO, error) {
	return UserDTO{ID: id, Email: "u@example.com", Role: "user", Status: "active"}, nil
}
func (fakeUsers) ByEmail(ctx context.Context, email string) (UserDTO, error) {
	return UserDTO{}, errors.New("unexpected")
}
func (fakeUsers) Create(ctx context.Context, email, hash, role string) (UserDTO, error) {
	return UserDTO{ID: 2, Email: email, Role: role, Status: "active"}, nil
}
func (fakeUsers) Count(ctx context.Context) (int64, error)                     { return 1, nil }
func (fakeUsers) CountAdmins(ctx context.Context) (int64, error)               { return 1, nil }
func (fakeUsers) SetPassword(ctx context.Context, id int64, hash string) error { return nil }
func (fakeUsers) SetStatus(ctx context.Context, id int64, status string) error { return nil }
func (fakeUsers) SetRole(ctx context.Context, id int64, role string) error     { return nil }
func (fakeUsers) List(ctx context.Context, q string, page, size int) ([]UserDTO, int64, error) {
	return nil, 0, nil
}
func (fakeUsers) AdminList(ctx context.Context, q string, page, size int) ([]AdminUserDTO, int64, error) {
	return nil, 0, nil
}
func (fakeUsers) AdminDetail(ctx context.Context, id int64) (AdminUserDTO, error) {
	return AdminUserDTO{UserDTO: UserDTO{ID: id}}, nil
}

type fakeSettings struct{}

func (fakeSettings) Get(ctx context.Context) (SettingsPayload, error) {
	return SettingsPayload{SiteName: "测试站", RegistrationOpen: true, BaseConcurrency: 1}, nil
}
func (fakeSettings) Put(ctx context.Context, p SettingsPayload, actorID int64) error { return nil }

type fakeAudit struct{}

func (fakeAudit) Write(ctx context.Context, actorID int64, action, target string, detail any) {}

type fakeCatalog struct{}

func (fakeCatalog) Public(ctx context.Context) ([]ProductDTO, error) {
	stock := 3
	return []ProductDTO{{ID: 1, Kind: "rental_account", Name: "Claude 账号 · 30 天", UnitQuantity: 1, DurationDays: 30, PriceCents: 9900, Currency: "CNY", MinQty: 1, MaxQty: 5, Enabled: true, Stock: &stock}}, nil
}
func (fakeCatalog) All(ctx context.Context) ([]ProductDTO, error) { return fakeCatalog{}.Public(ctx) }
func (fakeCatalog) Get(ctx context.Context, id int64) (ProductDTO, error) {
	items, _ := fakeCatalog{}.Public(ctx)
	return items[0], nil
}
func (fakeCatalog) Create(ctx context.Context, in ProductInput) (ProductDTO, error) {
	return ProductDTO{ID: 2, Kind: in.Kind, Name: in.Name}, nil
}
func (fakeCatalog) Update(ctx context.Context, id int64, in ProductInput) (ProductDTO, error) {
	return ProductDTO{ID: id, Kind: in.Kind, Name: in.Name}, nil
}
func (fakeCatalog) Delete(ctx context.Context, id int64) error              { return nil }
func (fakeCatalog) Stock(ctx context.Context, productID int64) (int, error) { return 3, nil }

type fakeOrders struct{}

func (fakeOrders) Create(ctx context.Context, userID, productID int64, quantity int, provider string, renew *int64) (OrderDTO, error) {
	return OrderDTO{OrderNo: "PT1", ProductID: productID, Quantity: quantity, Provider: provider, Status: "pending", UserID: userID}, nil
}
func (fakeOrders) Get(ctx context.Context, orderNo string) (OrderDTO, error) {
	if orderNo != "PT1" {
		return OrderDTO{}, apiErr(errors.New("nf"), 404, "order_not_found", "订单不存在")
	}
	return OrderDTO{OrderNo: "PT1", UserID: 1, Status: "pending"}, nil
}
func (fakeOrders) ListByUser(ctx context.Context, userID int64, page, size int) ([]OrderDTO, int64, error) {
	return []OrderDTO{{OrderNo: "PT1"}}, 1, nil
}
func (fakeOrders) AdminList(ctx context.Context, status string, userID int64, page, size int) ([]OrderDTO, int64, error) {
	return nil, 0, nil
}
func (fakeOrders) Cancel(ctx context.Context, orderNo string) error                   { return nil }
func (fakeOrders) MarkPaidByAdmin(ctx context.Context, orderNo, tradeNo string) error { return nil }
func (fakeOrders) Refulfill(ctx context.Context, orderNo string) error                { return nil }

type fakePayments struct{}

func (fakePayments) Providers(ctx context.Context) []PaymentProviderDTO {
	return []PaymentProviderDTO{{ID: "manual", Name: "人工转账"}}
}
func (fakePayments) CreatePayment(ctx context.Context, o OrderDTO) (PaymentDTO, error) {
	return PaymentDTO{Type: "manual", Instructions: "转账"}, nil
}
func (fakePayments) Notify(ctx context.Context, provider string, r *http.Request) (int, string, error) {
	return 200, "success", nil
}
func (fakePayments) MockPay(ctx context.Context, userID int64, orderNo string) error { return nil }
func (fakePayments) ReturnURL(ctx context.Context, provider string, r *http.Request) (string, error) {
	return "/orders/PT1", nil
}

type fakeAccounts struct{}

func (fakeAccounts) List(ctx context.Context, userID int64, includeReleased bool) ([]AccountDTO, error) {
	return []AccountDTO{{ID: 7, Kind: "hosted", Label: "主号", Status: "active"}}, nil
}
func (fakeAccounts) Get(ctx context.Context, userID, id int64) (AccountDTO, []DailyDTO, []ModelDTO, error) {
	return AccountDTO{ID: id}, nil, nil, nil
}
func (fakeAccounts) Rename(ctx context.Context, userID, id int64, label string) error { return nil }
func (fakeAccounts) CreateHosted(ctx context.Context, userID int64, label string) (AccountDTO, OnboardingDTO, error) {
	return AccountDTO{ID: 8}, OnboardingDTO{AccountID: 8, Status: "provisioning"}, nil
}
func (fakeAccounts) Onboarding(ctx context.Context, userID, id int64) (OnboardingDTO, error) {
	return OnboardingDTO{AccountID: id, Status: "awaiting_auth", AuthURL: "https://claude.ai/oauth"}, nil
}
func (fakeAccounts) RegenerateAuthURL(ctx context.Context, userID, id int64) (OnboardingDTO, error) {
	return OnboardingDTO{AccountID: id}, nil
}
func (fakeAccounts) SubmitAuthCode(ctx context.Context, userID, id int64, code string) (OnboardingDTO, error) {
	return OnboardingDTO{AccountID: id, Status: "bootstrapping"}, nil
}
func (fakeAccounts) RetryHosted(ctx context.Context, userID, id int64) (OnboardingDTO, error) {
	return OnboardingDTO{AccountID: id}, nil
}
func (fakeAccounts) DeleteHosted(ctx context.Context, userID, id int64) error { return nil }
func (fakeAccounts) AdminList(ctx context.Context, kind, status string, userID int64, page, size int) ([]AdminAccountDTO, int64, error) {
	return nil, 0, nil
}
func (fakeAccounts) AdminRetry(ctx context.Context, id int64) error   { return nil }
func (fakeAccounts) AdminRelease(ctx context.Context, id int64) error { return nil }

type fakeUsage struct{}

func (fakeUsage) Overview(ctx context.Context, userID int64) (OverviewDTO, error) {
	return OverviewDTO{Concurrency: ConcurrencyDTO{Total: 3, Base: 1}}, nil
}
func (fakeUsage) Subscriptions(ctx context.Context, userID int64, status string) ([]SubscriptionDTO, error) {
	return []SubscriptionDTO{{ID: 1, Kind: "hosted_seat", Quantity: 1, Status: "active"}}, nil
}
func (fakeUsage) Summary(ctx context.Context, userID int64, from, until time.Time) (UsageSummaryDTO, error) {
	return UsageSummaryDTO{Daily: []DailyDTO{{Date: "2026-10-31", Requests: 2}}}, nil
}
func (fakeUsage) Logs(ctx context.Context, userID int64, q UsageLogQuery) (UsageLogPageDTO, error) {
	return UsageLogPageDTO{Total: 0, Page: q.Page, PageSize: q.PageSize}, nil
}
func (fakeUsage) KeyStats(ctx context.Context, userID, keyID int64) ([]DailyDTO, []ModelDTO, error) {
	return nil, nil, nil
}

type fakeKeys struct{}

func (fakeKeys) List(ctx context.Context, userID int64) ([]KeyDTO, error) { return nil, nil }
func (fakeKeys) Create(ctx context.Context, userID int64, name string) (KeyDTO, string, error) {
	return KeyDTO{ID: 1, Name: name, Prefix: "sk-pt-AbCd"}, "sk-pt-AbCdEfGh", nil
}
func (fakeKeys) Update(ctx context.Context, userID, id int64, name, status string) (KeyDTO, error) {
	return KeyDTO{ID: id, Name: name, Status: status}, nil
}
func (fakeKeys) Delete(ctx context.Context, userID, id int64) error { return nil }

type fakeAdmin struct{}

func (fakeAdmin) Dashboard(ctx context.Context) (AdminDashboardDTO, error) {
	return AdminDashboardDTO{Users: 6}, nil
}
func (fakeAdmin) RentalPool(ctx context.Context) ([]RentalPoolRowDTO, error)         { return nil, nil }
func (fakeAdmin) RentalCandidates(ctx context.Context) ([]RentalCandidateDTO, error) { return nil, nil }
func (fakeAdmin) RentalAdd(ctx context.Context, vmIDs []string, note string) error   { return nil }
func (fakeAdmin) RentalPatch(ctx context.Context, vmID, status, note string) error   { return nil }
func (fakeAdmin) RentalRemove(ctx context.Context, vmID string) error                { return nil }
func (fakeAdmin) Jobs(ctx context.Context, status string, page, size int) ([]JobDTO, int64, error) {
	return nil, 0, nil
}
func (fakeAdmin) RetryJob(ctx context.Context, id int64) error { return nil }
func (fakeAdmin) GrantSubscription(ctx context.Context, adminID, userID int64, kind string, quantity, days int, note string) error {
	return nil
}
func (fakeAdmin) PatchSubscription(ctx context.Context, id int64, expiresAt *string, status string) error {
	return nil
}

// apiErr builds a UserError the same way adapters do.
func apiErr(sentinel error, status int, code, msg string) error {
	return Err(sentinel, status, code, msg)
}

func newTestServer(t *testing.T, role string) *httptest.Server {
	t.Helper()
	srv, _ := newTestServerWithAuth(t, role)
	return srv
}

func newTestServerWithAuth(t *testing.T, role string) (*httptest.Server, *fakeAuth) {
	t.Helper()
	return newTestServerWithAdmin(t, role, fakeAdmin{})
}

func newTestServerWithAdmin(t *testing.T, role string, admin AdminService) (*httptest.Server, *fakeAuth) {
	t.Helper()
	var user *UserDTO
	if role != "" {
		user = &UserDTO{ID: 1, Email: "u@example.com", Role: role, Status: "active"}
	}
	auth := &fakeAuth{user: user}
	s := &Server{
		Deps: Deps{
			Users: fakeUsers{}, Settings: fakeSettings{}, Audit: fakeAudit{},
			Auth:    auth,
			Catalog: fakeCatalog{}, Orders: fakeOrders{}, Payments: fakePayments{},
			Accounts: fakeAccounts{}, Usage: fakeUsage{}, Keys: fakeKeys{}, Admin: admin, System: &fakeSystem{},
			Site: func(r *http.Request) SiteConfig { return SiteConfig{SiteName: "测试站", BaseURL: "http://x"} },
			Dev:  true,
		},
		Log: slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	srv := httptest.NewServer(s.Deps.Auth.Middleware(s.Handler()))
	t.Cleanup(srv.Close)
	return srv, auth
}

func doJSON(t *testing.T, method, url, body string) (*http.Response, map[string]any) {
	t.Helper()
	req, err := http.NewRequest(method, url, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	var out map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&out)
	resp.Body.Close()
	return resp, out
}

func TestPublicProductsEnvelope(t *testing.T) {
	srv := newTestServer(t, "")
	resp, body := doJSON(t, http.MethodGet, srv.URL+"/api/public/products", "")
	if resp.StatusCode != 200 || body["ok"] != true {
		t.Fatalf("status=%d body=%v", resp.StatusCode, body)
	}
	data := body["data"].(map[string]any)
	items := data["items"].([]any)
	if len(items) != 1 {
		t.Fatalf("items=%v", items)
	}
}

func TestUnauthenticatedRejected(t *testing.T) {
	srv := newTestServer(t, "")
	resp, body := doJSON(t, http.MethodGet, srv.URL+"/api/me/overview", "")
	if resp.StatusCode != 401 {
		t.Fatalf("status=%d", resp.StatusCode)
	}
	if code := body["error"].(map[string]any)["code"]; code != "unauthorized" {
		t.Fatalf("code=%v", code)
	}
}

func TestAdminRequiresRole(t *testing.T) {
	srv := newTestServer(t, "user")
	resp, _ := doJSON(t, http.MethodGet, srv.URL+"/api/admin/dashboard", "")
	if resp.StatusCode != 403 {
		t.Fatalf("status=%d", resp.StatusCode)
	}
}

func TestFormsRejected(t *testing.T) {
	srv := newTestServer(t, "user")
	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/api/orders", strings.NewReader("{}"))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 415 {
		t.Fatalf("status=%d", resp.StatusCode)
	}
}

func TestCreateKeyReturnsSecretOnce(t *testing.T) {
	srv := newTestServer(t, "user")
	resp, body := doJSON(t, http.MethodPost, srv.URL+"/api/keys", `{"name":"默认"}`)
	if resp.StatusCode != 201 {
		t.Fatalf("status=%d body=%v", resp.StatusCode, body)
	}
	data := body["data"].(map[string]any)
	if data["secret"] != "sk-pt-AbCdEfGh" {
		t.Fatalf("secret=%v", data["secret"])
	}
}

func TestOnboardingEndpoint(t *testing.T) {
	srv := newTestServer(t, "user")
	resp, body := doJSON(t, http.MethodGet, srv.URL+"/api/hosted/8/onboarding", "")
	if resp.StatusCode != 200 {
		t.Fatalf("status=%d", resp.StatusCode)
	}
	data := body["data"].(map[string]any)
	if data["auth_url"] != "https://claude.ai/oauth" {
		t.Fatalf("payload=%v", data)
	}
}

func TestAuthCodeValidation(t *testing.T) {
	srv := newTestServer(t, "user")
	resp, _ := doJSON(t, http.MethodPost, srv.URL+"/api/hosted/8/auth-code", `{"code":"short"}`)
	if resp.StatusCode != 422 {
		t.Fatalf("status=%d", resp.StatusCode)
	}
}

func TestUnknownFieldsRejected(t *testing.T) {
	srv := newTestServer(t, "user")
	resp, _ := doJSON(t, http.MethodPost, srv.URL+"/api/orders", `{"product_id":1,"quantity":1,"provider":"manual","bogus":1}`)
	if resp.StatusCode != 422 {
		t.Fatalf("status=%d", resp.StatusCode)
	}
}

func TestDisabledLoginReturns403(t *testing.T) {
	srv, auth := newTestServerWithAuth(t, "user")
	auth.loginErr = authed.ErrUserDisabled
	resp, body := doJSON(t, http.MethodPost, srv.URL+"/api/auth/login", `{"email":"u@example.com","password":"right"}`)
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", resp.StatusCode)
	}
	errObj, _ := body["error"].(map[string]any)
	if errObj["code"] != "user_disabled" {
		t.Fatalf("code = %v, want user_disabled", errObj["code"])
	}
}

func TestWrongPasswordStays401(t *testing.T) {
	srv, auth := newTestServerWithAuth(t, "user")
	auth.loginErr = authed.ErrInvalidCredentials
	resp, body := doJSON(t, http.MethodPost, srv.URL+"/api/auth/login", `{"email":"u@example.com","password":"nope"}`)
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", resp.StatusCode)
	}
	errObj, _ := body["error"].(map[string]any)
	if errObj["code"] != "invalid_credentials" {
		t.Fatalf("code = %v", errObj["code"])
	}
}

func TestChangePasswordRevokesOtherSessions(t *testing.T) {
	srv, auth := newTestServerWithAuth(t, "user")
	auth.tokenHsh = "hash-of-current-token"
	resp, body := doJSON(t, http.MethodPost, srv.URL+"/api/auth/password",
		`{"old_password":"right","new_password":"new-password"}`)
	if resp.StatusCode != 200 {
		t.Fatalf("status = %d body = %v", resp.StatusCode, body)
	}
	auth.mu.Lock()
	defer auth.mu.Unlock()
	if len(auth.revoked) != 1 {
		t.Fatalf("RevokeOthers calls = %v, want exactly one", auth.revoked)
	}
	if want := "1|hash-of-current-token"; auth.revoked[0] != want {
		t.Fatalf("RevokeOthers = %q, want %q (current session kept)", auth.revoked[0], want)
	}
}

func TestChangePasswordRejectsWrongOldPassword(t *testing.T) {
	srv, auth := newTestServerWithAuth(t, "user")
	resp, _ := doJSON(t, http.MethodPost, srv.URL+"/api/auth/password",
		`{"old_password":"wrong","new_password":"new-password"}`)
	if resp.StatusCode != 422 {
		t.Fatalf("status = %d, want 422", resp.StatusCode)
	}
	auth.mu.Lock()
	defer auth.mu.Unlock()
	if len(auth.revoked) != 0 {
		t.Fatalf("sessions revoked on a failed change: %v", auth.revoked)
	}
}

func TestJSONContentTypeIsStrict(t *testing.T) {
	cases := map[string]bool{
		"application/json":                  true,
		"application/json; charset=utf-8":   true,
		"APPLICATION/JSON":                  true,
		" application/json ":                true,
		"text/json":                         false,
		"application/jsonish":               false,
		"application/x-www-form-urlencoded": false,
		"multipart/form-data; boundary=x":   false,
		"":                                  false,
	}
	for contentType, want := range cases {
		if got := isJSONContentType(contentType); got != want {
			t.Fatalf("isJSONContentType(%q) = %v, want %v", contentType, got, want)
		}
	}
}

func TestClientIPHonoursForwardedForOnlyFromProxy(t *testing.T) {
	cases := []struct {
		name       string
		remoteAddr string
		xff        []string
		want       string
	}{
		{"direct client ignores spoofed xff", "203.0.113.9:5555", []string{"1.2.3.4"}, "203.0.113.9"},
		{"loopback proxy uses last hop", "127.0.0.1:5555", []string{"1.2.3.4"}, "1.2.3.4"},
		{"private proxy uses last hop", "10.0.0.5:5555", []string{"1.2.3.4"}, "1.2.3.4"},
		{"proxy keeps rightmost valid hop", "127.0.0.1:5555", []string{"9.9.9.9, 5.6.7.8"}, "5.6.7.8"},
		{"proxy with junk hop falls back", "127.0.0.1:5555", []string{"not-an-ip"}, "127.0.0.1"},
		{"proxy without xff", "127.0.0.1:5555", nil, "127.0.0.1"},
	}
	for _, tc := range cases {
		r := httptest.NewRequest(http.MethodPost, "/api/auth/login", nil)
		r.RemoteAddr = tc.remoteAddr
		for _, v := range tc.xff {
			r.Header.Add("X-Forwarded-For", v)
		}
		if got := clientIP(r); got != tc.want {
			t.Fatalf("%s: clientIP = %q, want %q", tc.name, got, tc.want)
		}
	}
}

func (fakeAdmin) RunSelfcheck(ctx context.Context, resourceID int64, rebuild bool) (SelfcheckDTO, error) {
	return SelfcheckDTO{State: "pass", MaxAttempts: 3}, nil
}

func (fakeAdmin) SelfcheckHistory(ctx context.Context, resourceID int64, limit int) ([]SelfcheckReportDTO, error) {
	return nil, nil
}

func (fakeAdmin) RunPoolSelfcheck(ctx context.Context, vmID string) (SelfcheckDTO, error) {
	return SelfcheckDTO{State: "pass", MaxAttempts: 3}, nil
}

type fakeSelfcheckAdmin struct {
	fakeAdmin
	runSelfcheck     func(context.Context, int64, bool) (SelfcheckDTO, error)
	selfcheckHistory func(context.Context, int64, int) ([]SelfcheckReportDTO, error)
	runPoolSelfcheck func(context.Context, string) (SelfcheckDTO, error)
}

func (admin fakeSelfcheckAdmin) RunSelfcheck(ctx context.Context, resourceID int64, rebuild bool) (SelfcheckDTO, error) {
	return admin.runSelfcheck(ctx, resourceID, rebuild)
}

func (admin fakeSelfcheckAdmin) SelfcheckHistory(ctx context.Context, resourceID int64, limit int) ([]SelfcheckReportDTO, error) {
	return admin.selfcheckHistory(ctx, resourceID, limit)
}

func (admin fakeSelfcheckAdmin) RunPoolSelfcheck(ctx context.Context, vmID string) (SelfcheckDTO, error) {
	return admin.runPoolSelfcheck(ctx, vmID)
}

func TestAdminSelfcheckRequiresRole(t *testing.T) {
	for _, authCase := range []struct {
		role   string
		status int
		code   string
	}{
		{"", http.StatusUnauthorized, "unauthorized"},
		{"user", http.StatusForbidden, "forbidden"},
	} {
		t.Run(authCase.code, func(t *testing.T) {
			srv, _ := newTestServerWithAdmin(t, authCase.role, fakeSelfcheckAdmin{})
			for _, endpoint := range []struct {
				method string
				path   string
			}{
				{http.MethodPost, "/api/admin/accounts/7/selfcheck"},
				{http.MethodGet, "/api/admin/accounts/7/selfchecks"},
				{http.MethodPost, "/api/admin/rental-pool/vm-7/selfcheck"},
			} {
				t.Run(endpoint.path, func(t *testing.T) {
					resp, body := doJSON(t, endpoint.method, srv.URL+endpoint.path, "{}")
					if resp.StatusCode != authCase.status || body["ok"] != false {
						t.Fatalf("status=%d body=%v", resp.StatusCode, body)
					}
					if code := body["error"].(map[string]any)["code"]; code != authCase.code {
						t.Fatalf("code=%v want=%s", code, authCase.code)
					}
				})
			}
		})
	}
}

func TestAdminSelfcheckInvalidParameters(t *testing.T) {
	srv, _ := newTestServerWithAdmin(t, "admin", fakeSelfcheckAdmin{})
	assertInvalid := func(t *testing.T, method, path, requestBody string) {
		t.Helper()
		resp, body := doJSON(t, method, srv.URL+path, requestBody)
		if resp.StatusCode != http.StatusUnprocessableEntity || body["ok"] != false {
			t.Fatalf("status=%d body=%v", resp.StatusCode, body)
		}
		if code := body["error"].(map[string]any)["code"]; code != "invalid_request" {
			t.Fatalf("code=%v", code)
		}
	}
	for _, id := range []string{"abc", "0", "-1", "1.5", "9223372036854775808"} {
		t.Run("id="+id, func(t *testing.T) {
			assertInvalid(t, http.MethodPost, "/api/admin/accounts/"+id+"/selfcheck", "{}")
			assertInvalid(t, http.MethodGet, "/api/admin/accounts/"+id+"/selfchecks", "")
		})
	}
	for _, query := range []string{
		"limit=", "limit=abc", "limit=0", "limit=-1", "limit=101", "limit=1.5",
		"limit=999999999999999999999", "limit=%2020", "limit=20%20", "limit=1&limit=2",
	} {
		t.Run(query, func(t *testing.T) {
			assertInvalid(t, http.MethodGet, "/api/admin/accounts/7/selfchecks?"+query, "")
		})
	}
	for _, requestBody := range []string{
		`{"rebuild":"true"}`, `{"rebuild":1}`, `{"rebuild":null}`, `{"rebuild":[]}`,
		`{"rebuild":{}}`, `{"unknown":true}`, `{"rebuild":`, `null`, `[]`, `true`,
	} {
		t.Run(requestBody, func(t *testing.T) {
			assertInvalid(t, http.MethodPost, "/api/admin/accounts/7/selfcheck", requestBody)
		})
	}

	t.Run("blank_vm_id", func(t *testing.T) {
		assertInvalid(t, http.MethodPost, "/api/admin/rental-pool/%20%20/selfcheck", "{}")
	})
}

func TestAdminAccountSelfcheck(t *testing.T) {
	checkedAt := "2026-10-31T00:00:00Z"
	for _, testCase := range []struct {
		name    string
		body    string
		rebuild bool
		state   string
	}{
		{"default", "{}", false, "pass"},
		{"empty_body", "", false, "pass"},
		{"no_rebuild", `{"rebuild":false}`, false, "fail"},
		{"rebuild", `{"rebuild": true}`, true, "exhausted"},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			admin := fakeSelfcheckAdmin{
				runSelfcheck: func(ctx context.Context, resourceID int64, rebuild bool) (SelfcheckDTO, error) {
					if resourceID != 7 || rebuild != testCase.rebuild {
						return SelfcheckDTO{}, fmt.Errorf("unexpected selfcheck arguments: id=%d rebuild=%t", resourceID, rebuild)
					}
					return SelfcheckDTO{
						State: testCase.state, Attempts: 2, MaxAttempts: 3, CheckedAt: &checkedAt,
						Checks: []SelfcheckCheckDTO{{
							Code: "proxy_exclusive", OK: false, Remedy: "rebuild", Message: "代理不独占", Detail: "vm-8",
						}},
					}, nil
				},
			}
			srv, _ := newTestServerWithAdmin(t, "admin", admin)
			resp, body := doJSON(t, http.MethodPost, srv.URL+"/api/admin/accounts/7/selfcheck", testCase.body)
			if resp.StatusCode != http.StatusOK || body["ok"] != true {
				t.Fatalf("status=%d body=%v", resp.StatusCode, body)
			}
			data := body["data"].(map[string]any)
			if data["state"] != testCase.state || data["attempts"] != float64(2) ||
				data["max_attempts"] != float64(3) || data["checked_at"] != checkedAt {
				t.Fatalf("data=%v", data)
			}
			checks := data["checks"].([]any)
			if len(checks) != 1 {
				t.Fatalf("checks=%v", checks)
			}
			check := checks[0].(map[string]any)
			if check["code"] != "proxy_exclusive" || check["ok"] != false || check["remedy"] != "rebuild" ||
				check["message"] != "代理不独占" || check["detail"] != "vm-8" {
				t.Fatalf("check=%v", check)
			}
		})
	}
}

func TestAdminSelfcheckHistory(t *testing.T) {
	for _, testCase := range []struct {
		name  string
		query string
		limit int
		empty bool
	}{
		{"default", "", 20, false},
		{"minimum", "?limit=1", 1, false},
		{"maximum", "?limit=100", 100, false},
		{"empty", "", 20, true},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			admin := fakeSelfcheckAdmin{
				selfcheckHistory: func(ctx context.Context, resourceID int64, limit int) ([]SelfcheckReportDTO, error) {
					if resourceID != 7 || limit != testCase.limit {
						return nil, fmt.Errorf("unexpected history arguments: id=%d limit=%d", resourceID, limit)
					}
					if testCase.empty {
						return nil, nil
					}
					return []SelfcheckReportDTO{{
						ID: 12, VMID: "vm-7", Attempt: 1, Passed: true, CreatedAt: "2026-10-31T00:00:00Z",
						Checks: []SelfcheckCheckDTO{{Code: "proxy_exclusive", OK: true, Message: "检查通过"}},
					}}, nil
				},
			}
			srv, _ := newTestServerWithAdmin(t, "admin", admin)
			resp, body := doJSON(t, http.MethodGet, srv.URL+"/api/admin/accounts/7/selfchecks"+testCase.query, "")
			if resp.StatusCode != http.StatusOK || body["ok"] != true {
				t.Fatalf("status=%d body=%v", resp.StatusCode, body)
			}
			items, ok := body["data"].(map[string]any)["items"].([]any)
			if !ok {
				t.Fatalf("history must be an array: body=%v", body)
			}
			if testCase.empty {
				if len(items) != 0 {
					t.Fatalf("items=%v", items)
				}
				return
			}
			if len(items) != 1 {
				t.Fatalf("items=%v", items)
			}
			item := items[0].(map[string]any)
			if item["id"] != float64(12) || item["vm_id"] != "vm-7" || item["attempt"] != float64(1) ||
				item["passed"] != true || item["created_at"] != "2026-10-31T00:00:00Z" {
				t.Fatalf("item=%v", item)
			}
			checks := item["checks"].([]any)
			if len(checks) != 1 || checks[0].(map[string]any)["code"] != "proxy_exclusive" {
				t.Fatalf("checks=%v", checks)
			}
		})
	}
}

func TestAdminRentalPoolSelfcheck(t *testing.T) {
	admin := fakeSelfcheckAdmin{
		runPoolSelfcheck: func(ctx context.Context, vmID string) (SelfcheckDTO, error) {
			if vmID != "vm-7" {
				return SelfcheckDTO{}, fmt.Errorf("unexpected vm id: %s", vmID)
			}
			return SelfcheckDTO{
				State: "pass", MaxAttempts: 3,
				Checks: []SelfcheckCheckDTO{{Code: "exit_geo_timezone", OK: true, Skipped: true, Message: "已跳过"}},
			}, nil
		},
	}
	srv, _ := newTestServerWithAdmin(t, "admin", admin)
	resp, body := doJSON(t, http.MethodPost, srv.URL+"/api/admin/rental-pool/vm-7/selfcheck", "{}")
	if resp.StatusCode != http.StatusOK || body["ok"] != true {
		t.Fatalf("status=%d body=%v", resp.StatusCode, body)
	}
	data := body["data"].(map[string]any)
	if data["state"] != "pass" || data["attempts"] != float64(0) || data["max_attempts"] != float64(3) || data["checked_at"] != nil {
		t.Fatalf("data=%v", data)
	}
	checks := data["checks"].([]any)
	if len(checks) != 1 {
		t.Fatalf("checks=%v", checks)
	}
	check := checks[0].(map[string]any)
	if check["code"] != "exit_geo_timezone" || check["ok"] != true || check["skipped"] != true || check["message"] != "已跳过" {
		t.Fatalf("check=%v", check)
	}
}

func TestAdminSelfcheckServiceErrors(t *testing.T) {
	for _, serviceError := range []*UserError{
		{Status: http.StatusNotFound, Code: "not_found", Message: "资源不存在"},
		{Status: http.StatusConflict, Code: "selfcheck_failed", Message: "自检未通过"},
		{Status: http.StatusConflict, Code: "selfcheck_exhausted", Message: "重建次数已耗尽"},
		{Status: http.StatusConflict, Code: "account_duplicate", Message: "账号已在其它槽位托管"},
	} {
		t.Run(serviceError.Code, func(t *testing.T) {
			admin := fakeSelfcheckAdmin{
				runSelfcheck: func(context.Context, int64, bool) (SelfcheckDTO, error) {
					return SelfcheckDTO{}, fmt.Errorf("run selfcheck: %w", serviceError)
				},
				selfcheckHistory: func(context.Context, int64, int) ([]SelfcheckReportDTO, error) {
					return nil, fmt.Errorf("load history: %w", serviceError)
				},
				runPoolSelfcheck: func(context.Context, string) (SelfcheckDTO, error) {
					return SelfcheckDTO{}, fmt.Errorf("run pool selfcheck: %w", serviceError)
				},
			}
			srv, _ := newTestServerWithAdmin(t, "admin", admin)
			for _, endpoint := range []struct {
				method string
				path   string
			}{
				{http.MethodPost, "/api/admin/accounts/7/selfcheck"},
				{http.MethodGet, "/api/admin/accounts/7/selfchecks"},
				{http.MethodPost, "/api/admin/rental-pool/vm-7/selfcheck"},
			} {
				t.Run(endpoint.path, func(t *testing.T) {
					resp, body := doJSON(t, endpoint.method, srv.URL+endpoint.path, "{}")
					if resp.StatusCode != serviceError.Status || body["ok"] != false {
						t.Fatalf("status=%d body=%v", resp.StatusCode, body)
					}
					failure := body["error"].(map[string]any)
					if failure["code"] != serviceError.Code || failure["message"] != serviceError.Message {
						t.Fatalf("error=%v", failure)
					}
				})
			}
		})
	}
}
