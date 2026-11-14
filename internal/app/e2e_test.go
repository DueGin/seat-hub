package app

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/DueGin/seat-hub/internal/config"
	"github.com/DueGin/seat-hub/internal/store"
	"github.com/DueGin/seat-hub/internal/vm2api"
)

type harness struct {
	t      *testing.T
	app    *App
	fake   *vm2api.Fake
	portal *httptest.Server
	client *http.Client

	// What the last /v1 request actually handed to the upstream.
	upstreamMu   *sync.Mutex
	upstreamAuth string
	upstreamBody string
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	fake := vm2api.NewFake()
	fake.AddVM(vm2api.VM{
		ID: "vm-1", Label: "池内账号", Platform: "anthropic", Origin: "platform",
		Tier: "pro", CredStatus: "可用", HasCredential: true, ScheduleState: "on",
		ProxyConfigured: true, SeatsMax: 2,
		FiveHour: &vm2api.Window{Status: "ok"},
		SevenDay: &vm2api.Window{Status: "ok"},
	})
	h := &harness{t: t, fake: fake, upstreamMu: &sync.Mutex{}}
	// The upstream serves the panel API from the fake and records /v1 traffic so
	// the test can prove the gateway swapped credentials and forwarded the body.
	panel := fake.Handler("master")
	upstream := http.NewServeMux()
	upstream.HandleFunc("/v1/messages", func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		h.upstreamMu.Lock()
		h.upstreamAuth = r.Header.Get("Authorization")
		h.upstreamBody = string(b)
		h.upstreamMu.Unlock()
		var call vm2api.ProtocolCall
		var payload struct {
			Model string `json:"model"`
		}
		_ = json.Unmarshal(b, &payload)
		call.Secret = strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		call.Model, call.InputTokens, call.OutputTokens = payload.Model, 10, 5
		status, known := fake.RecordProtocolCall(call)
		if !known {
			status = http.StatusUnauthorized
		}
		if status != http.StatusOK {
			w.WriteHeader(status)
			_, _ = w.Write([]byte(`{"error":{"message":"upstream scope denied"}}`))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"type":"message","id":"msg_1"}`))
	})
	upstream.Handle("/", panel)
	fakeSrv := httptest.NewServer(upstream)
	t.Cleanup(fakeSrv.Close)

	cfg := config.Config{
		Listen:          ":0",
		DBPath:          filepath.Join(t.TempDir(), "portal.db"),
		SecretKey:       strings.Repeat("k", 40),
		AdminEmail:      "admin@example.com",
		AdminPassword:   "admin-pass-123",
		Dev:             true,
		MaxBodyMB:       8,
		VM2APIBaseURL:   fakeSrv.URL,
		VM2APIMasterKey: "master",
	}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	a, err := New(cfg, logger)
	if err != nil {
		t.Fatalf("app.New: %v", err)
	}
	t.Cleanup(func() { _ = a.Close(context.Background()) })
	if err := a.BootstrapAdmin(context.Background()); err != nil {
		t.Fatalf("bootstrap admin: %v", err)
	}
	srv := httptest.NewServer(a.Server.Handler)
	t.Cleanup(srv.Close)
	jar, _ := cookiejar.New(nil)
	h.app, h.portal = a, srv
	h.client = &http.Client{Jar: jar, Timeout: 20 * time.Second}
	return h
}

type envelope struct {
	OK    bool            `json:"ok"`
	Data  json.RawMessage `json:"data"`
	Error *struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

func (h *harness) call(method, path, body string, headers map[string]string) (int, envelope) {
	h.t.Helper()
	req, err := http.NewRequest(method, h.portal.URL+path, bytes.NewBufferString(body))
	if err != nil {
		h.t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := h.client.Do(req)
	if err != nil {
		h.t.Fatalf("%s %s: %v", method, path, err)
	}
	defer resp.Body.Close()
	var env envelope
	_ = json.NewDecoder(resp.Body).Decode(&env)
	return resp.StatusCode, env
}

func (h *harness) mustOK(method, path, body string, into any) {
	h.t.Helper()
	status, env := h.call(method, path, body, nil)
	if status < 200 || status >= 300 || !env.OK {
		h.t.Fatalf("%s %s -> %d %s", method, path, status, string(env.Data))
	}
	if into != nil {
		if err := json.Unmarshal(env.Data, into); err != nil {
			h.t.Fatalf("decode %s: %v", path, err)
		}
	}
}

func (h *harness) login(email, password string) {
	h.t.Helper()
	status, env := h.call(http.MethodPost, "/api/auth/login",
		`{"email":"`+email+`","password":"`+password+`"}`, nil)
	if status != 200 || !env.OK {
		h.t.Fatalf("login %s -> %d", email, status)
	}
}

func TestEndToEndPurchaseAndProxy(t *testing.T) {
	h := newHarness(t)
	h.login("admin@example.com", "admin-pass-123")

	// Admin publishes a concurrency product.
	var created struct {
		Product struct {
			ID int64 `json:"id"`
		} `json:"product"`
	}
	h.mustOK(http.MethodPost, "/api/admin/products", `{
		"kind":"concurrency","name":"5 路并发 · 30 天","unit_quantity":5,
		"duration_days":30,"price_cents":9900,"currency":"CNY","min_qty":1,"max_qty":10,
		"enabled":true,"sort_order":1}`, &created)
	if created.Product.ID == 0 {
		t.Fatal("product not created")
	}

	// A regular user registers, buys, and pays.
	h.mustOK(http.MethodPost, "/api/auth/logout", `{}`, nil)
	status, env := h.call(http.MethodPost, "/api/auth/register",
		`{"email":"buyer@example.com","password":"buyer-pass-1"}`, nil)
	if status != 201 || !env.OK {
		t.Fatalf("register -> %d %s", status, string(env.Data))
	}
	var orderResp struct {
		Order struct {
			OrderNo string `json:"order_no"`
			Status  string `json:"status"`
		} `json:"order"`
		Payment struct {
			Type string `json:"type"`
		} `json:"payment"`
	}
	h.mustOK(http.MethodPost, "/api/orders", `{"product_id":1,"quantity":1,"provider":"mock"}`, &orderResp)
	if orderResp.Order.OrderNo == "" || orderResp.Payment.Type != "mock" {
		t.Fatalf("order=%+v payment=%+v", orderResp.Order, orderResp.Payment)
	}
	h.mustOK(http.MethodPost, "/api/orders/"+orderResp.Order.OrderNo+"/mock-pay", `{}`, nil)

	// Fulfilment runs asynchronously through the job engine.
	deadline := time.Now().Add(10 * time.Second)
	for {
		var got struct {
			Order struct {
				Status string `json:"status"`
			} `json:"order"`
		}
		h.mustOK(http.MethodGet, "/api/orders/"+orderResp.Order.OrderNo, "", &got)
		if got.Order.Status == "fulfilled" {
			break
		}
		if got.Order.Status == "fulfill_failed" {
			t.Fatal("fulfilment failed")
		}
		if time.Now().After(deadline) {
			t.Fatalf("order stuck in %q", got.Order.Status)
		}
		time.Sleep(50 * time.Millisecond)
	}

	// Entitlement reaches the overview: base 1 + 5 purchased.
	var overview struct {
		Concurrency struct {
			Total int `json:"total"`
			Base  int `json:"base"`
		} `json:"concurrency"`
	}
	h.mustOK(http.MethodGet, "/api/me/overview", "", &overview)
	if overview.Concurrency.Total != 6 {
		t.Fatalf("concurrency total = %d, want 6", overview.Concurrency.Total)
	}

	// A gateway key maps to a scoped vm2api key owned by the service user.
	var keyResp struct {
		Key struct {
			Prefix string `json:"prefix"`
		} `json:"key"`
		Secret string `json:"secret"`
	}
	h.mustOK(http.MethodPost, "/api/keys", `{"name":"默认"}`, &keyResp)
	if !strings.HasPrefix(keyResp.Secret, "sk-pt-") {
		t.Fatalf("secret = %q", keyResp.Secret)
	}
	if keyResp.Secret[:10] != keyResp.Key.Prefix {
		t.Fatalf("prefix %q not in secret %q", keyResp.Key.Prefix, keyResp.Secret)
	}

	// Without an active account the gateway refuses with a helpful message.
	req, _ := http.NewRequest(http.MethodPost, h.portal.URL+"/v1/messages", strings.NewReader(`{"model":"x"}`))
	req.Header.Set("Authorization", "Bearer "+keyResp.Secret)
	resp, err := h.client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("gateway without account -> %d %s", resp.StatusCode, body)
	}

	// Attach an account to the user; now the gateway resolves and proxies.
	var userID int64
	if err := h.app.DB.QueryRow(`SELECT id FROM users WHERE email='buyer@example.com'`).Scan(&userID); err != nil {
		t.Fatal(err)
	}
	identity, err := store.LoadServiceIdentity(context.Background(), h.app.DB.DB)
	if err != nil || identity.UserID == "" {
		t.Fatalf("service identity missing: %+v, %v", identity, err)
	}
	upstream := identity.UserID
	if _, err := h.app.DB.Exec(`INSERT INTO resources (user_id,kind,vm_id,label,status,created_at,updated_at)
		VALUES (?,?,?,?,'active',?,?)`, userID, "rental", "vm-1", "池内账号", store.Now(), store.Now()); err != nil {
		t.Fatal(err)
	}
	if err := h.fake.SetVMOwner(context.Background(), "vm-1", &upstream); err != nil {
		t.Fatal(err)
	}
	var poolID string
	if err := h.app.DB.QueryRow(`SELECT vm2api_pool_id FROM users WHERE id=?`, userID).Scan(&poolID); err != nil {
		t.Fatal(err)
	}
	if _, err := h.fake.SetPoolMembers(context.Background(), poolID, []string{"vm-1"}); err != nil {
		t.Fatal(err)
	}
	// The resolver caches principals for a few seconds; a real deployment sees
	// the same staleness window after a purchase.
	h.app.resolver.Invalidate()

	req2, _ := http.NewRequest(http.MethodPost, h.portal.URL+"/v1/messages", strings.NewReader(`{"model":"x","messages":[]}`))
	req2.Header.Set("Authorization", "Bearer "+keyResp.Secret)
	req2.Header.Set("X-Kin-Debug", "1")
	resp2, err := h.client.Do(req2)
	if err != nil {
		t.Fatal(err)
	}
	body2, _ := io.ReadAll(resp2.Body)
	resp2.Body.Close()
	if resp2.StatusCode != http.StatusOK {
		t.Fatalf("gateway -> %d %s", resp2.StatusCode, body2)
	}
	h.upstreamMu.Lock()
	defer h.upstreamMu.Unlock()
	if !strings.HasPrefix(h.upstreamAuth, "Bearer sk-") || strings.HasPrefix(h.upstreamAuth, "Bearer sk-pt-") {
		t.Fatalf("upstream auth = %q, want the vm2api key", h.upstreamAuth)
	}
	if !strings.Contains(h.upstreamBody, `"model":"x"`) {
		t.Fatalf("body not forwarded: %q", h.upstreamBody)
	}
}

func TestHostedOnboardingFlow(t *testing.T) {
	h := newHarness(t)
	h.login("admin@example.com", "admin-pass-123")
	// Grant a hosted seat directly, as an admin would.
	h.mustOK(http.MethodGet, "/api/me/overview", "", nil) // warm session
	var adminID int64
	if err := h.app.DB.QueryRow(`SELECT id FROM users WHERE email='admin@example.com'`).Scan(&adminID); err != nil {
		t.Fatal(err)
	}
	h.mustOK(http.MethodPost, "/api/admin/users/1/grant",
		`{"kind":"hosted_seat","quantity":1,"duration_days":30,"note":"测试"}`, nil)

	var hosted struct {
		Account struct {
			ID     int64  `json:"id"`
			Status string `json:"status"`
		} `json:"account"`
	}
	h.mustOK(http.MethodPost, "/api/hosted", `{"label":"我的主号"}`, &hosted)
	if hosted.Account.ID == 0 {
		t.Fatal("hosted account not created")
	}

	// Provisioning runs in the background and lands on awaiting_auth.
	deadline := time.Now().Add(10 * time.Second)
	var onboarding struct {
		Status        string `json:"status"`
		CanRegenerate bool   `json:"can_regenerate"`
	}
	for {
		h.mustOK(http.MethodGet, "/api/hosted/"+itoa(hosted.Account.ID)+"/onboarding", "", &onboarding)
		if onboarding.Status == "awaiting_auth" {
			break
		}
		if onboarding.Status == "failed" {
			t.Fatal("provisioning failed")
		}
		if time.Now().After(deadline) {
			t.Fatalf("stuck in %q", onboarding.Status)
		}
		time.Sleep(50 * time.Millisecond)
	}

	var link struct {
		AuthURL string `json:"auth_url"`
	}
	h.mustOK(http.MethodPost, "/api/hosted/"+itoa(hosted.Account.ID)+"/auth-url", `{}`, &link)
	if !strings.Contains(link.AuthURL, "claude") {
		t.Fatalf("auth url = %q", link.AuthURL)
	}

	var after struct {
		Status string `json:"status"`
	}
	h.mustOK(http.MethodPost, "/api/hosted/"+itoa(hosted.Account.ID)+"/auth-code",
		`{"code":"abc123def456#state1"}`, &after)
	if after.Status == "awaiting_auth" {
		t.Fatalf("auth code not accepted, status still %q", after.Status)
	}
}

func itoa(n int64) string {
	return strings.TrimSpace(strings.ReplaceAll(jsonNumber(n), "\"", ""))
}

func jsonNumber(n int64) string {
	b, _ := json.Marshal(n)
	return string(b)
}
