package api

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// --- fakes ---

type fakeAuth struct {
	user  *UserDTO
	token string
}

func (f *fakeAuth) Login(ctx context.Context, email, password, ip, ua string) (string, UserDTO, error) {
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
func (f *fakeAuth) TokenHashFrom(ctx context.Context) string   { return "" }
func (f *fakeAuth) Allowed(ip, email string) bool              { return true }
func (f *fakeAuth) HashPassword(p string) (string, error)      { return "h", nil }
func (f *fakeAuth) VerifyPassword(userID int64, p string) bool { return p == "right" }

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
	var user *UserDTO
	if role != "" {
		user = &UserDTO{ID: 1, Email: "u@example.com", Role: role, Status: "active"}
	}
	s := &Server{
		Deps: Deps{
			Users: fakeUsers{}, Settings: fakeSettings{}, Audit: fakeAudit{},
			Auth:    &fakeAuth{user: user},
			Catalog: fakeCatalog{}, Orders: fakeOrders{}, Payments: fakePayments{},
			Accounts: fakeAccounts{}, Usage: fakeUsage{}, Keys: fakeKeys{}, Admin: fakeAdmin{},
			Site: func(r *http.Request) SiteConfig { return SiteConfig{SiteName: "测试站", BaseURL: "http://x"} },
			Dev:  true,
		},
		Log: slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	srv := httptest.NewServer(s.Deps.Auth.Middleware(s.Handler()))
	t.Cleanup(srv.Close)
	return srv
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
