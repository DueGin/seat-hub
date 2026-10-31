package vm2api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
)

func fakeTestUser(t *testing.T, client Client, name string) User {
	t.Helper()
	user, err := client.CreateUser(context.Background(), name, "password")
	if err != nil {
		t.Fatal(err)
	}
	return user
}

func fakeTestSeedLog(t *testing.T, fake *Fake, ownerID string, row LogRow) {
	t.Helper()
	if err := fake.AddRequestLog(ownerID, row); err != nil {
		t.Fatal(err)
	}
}

func fakeTestRaw(t *testing.T, handler http.Handler, method, path, key, body string, status int) map[string]any {
	t.Helper()
	request := httptest.NewRequest(method, path, strings.NewReader(body))
	if key != "" {
		request.Header.Set("Authorization", "Bearer "+key)
	}
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != status {
		t.Fatalf("%s %s: status %d, want %d: %s", method, path, response.Code, status, response.Body.String())
	}
	var payload map[string]any
	if err := json.Unmarshal(response.Body.Bytes(), &payload); err != nil {
		t.Fatal(err)
	}
	return payload
}

func TestFakeHTTPHostedRoundTrip(t *testing.T) {
	ctx := context.Background()
	fake := NewFake()
	server := httptest.NewServer(fake.Handler("master"))
	t.Cleanup(server.Close)
	client := NewHTTPClient(server.URL, "master", server.Client())
	health, err := client.Ping(ctx)
	if err != nil || !health.OK || health.Version != "mock-vm2api" {
		t.Fatalf("ping = %+v, %v", health, err)
	}
	owner := fakeTestUser(t, client, "owner")
	other := fakeTestUser(t, client, "other")
	if err := client.SetUserEnabled(ctx, owner.ID, false); err != nil {
		t.Fatal(err)
	}
	if err := client.SetUserEnabled(ctx, owner.ID, true); err != nil {
		t.Fatal(err)
	}
	key, err := client.CreateKey(ctx, CreateKeyInput{Name: "托管密钥", UserID: owner.ID, MaxConcurrency: 999})
	if err != nil || !strings.HasPrefix(key.Secret, "sk-vm-") || key.Prefix != key.Secret[:10] {
		t.Fatalf("create key = %+v, %v", key, err)
	}
	active, concurrency, name := false, -10, "新名称"
	if err := client.UpdateKey(ctx, key.ID, UpdateKeyInput{Name: &name, Active: &active, MaxConcurrency: &concurrency}); err != nil {
		t.Fatal(err)
	}
	statsWire := fakeTestRaw(t, fake.Handler("master"), "GET", "/api/panel/api-keys/"+key.ID+"/stats", "master", "", 200)
	item := statsWire["item"].(map[string]any)
	if item["status"] != "disabled" || item["max_concurrency"] != float64(0) || item["key"] == key.Secret {
		t.Fatalf("updated key wire = %+v", item)
	}
	vm, err := client.CreateHostedVM(ctx, "托管账号", json.RawMessage(`{"runtime_type":"docker"}`))
	if err != nil || vm.Label != "托管账号" || vm.ScheduleState != "unschedulable" || vm.UnschedulableCode != "no_credential" {
		t.Fatalf("create vm = %+v, %v", vm, err)
	}
	if err := client.SetVMOwner(ctx, vm.ID, &owner.ID); err != nil {
		t.Fatal(err)
	}
	if err := client.AllocateProxy(ctx, vm.ID); err != nil {
		t.Fatal(err)
	}
	auth, err := client.GenerateAuthURL(ctx, vm.ID)
	if err != nil || auth.URL == "" || auth.SessionID == "" || time.Until(auth.ExpiresAt) < 29*time.Minute || time.Until(auth.ExpiresAt) > 30*time.Minute {
		t.Fatalf("auth = %+v, %v", auth, err)
	}
	if err := client.ExchangeCode(ctx, vm.ID, auth.SessionID, "test-code#state"); err != nil {
		t.Fatal(err)
	}
	for _, expected := range []string{"pending", "running", "succeeded", "succeeded"} {
		bootstrap, err := client.BootstrapStatus(ctx, vm.ID)
		if err != nil || bootstrap.State != expected {
			t.Fatalf("bootstrap = %+v, %v; want %s", bootstrap, err, expected)
		}
	}
	if err := client.SetSchedulable(ctx, vm.ID, true); err != nil {
		t.Fatal(err)
	}
	owned, err := client.ListVMs(ctx, VMFilter{OwnerUserID: &owner.ID, ClaudeOnly: true})
	if err != nil || len(owned) != 1 || !owned[0].HasCredential || owned[0].ScheduleState != "on" {
		t.Fatalf("owned = %+v, %v", owned, err)
	}
	foreign, err := client.ListVMs(ctx, VMFilter{OwnerUserID: &other.ID})
	if err != nil || len(foreign) != 0 {
		t.Fatalf("foreign vms = %+v, %v", foreign, err)
	}
	stamp := time.Now().UTC().Add(-time.Minute).Truncate(time.Second)
	latency100, latency300 := int64(100), int64(300)
	first := LogRow{RequestID: "success", CreatedAt: stamp, Model: "claude", KeyID: key.ID, VMID: vm.ID,
		StatusCode: 200, InputTokens: 10, OutputTokens: 20, CacheReadTokens: 30, CacheCreationTokens: 40,
		CostUSD: 0.25, DurationMS: 400, FirstTokenMS: &latency100}
	second := LogRow{RequestID: "failure", CreatedAt: stamp.Add(time.Second), Model: "claude", KeyID: key.ID, VMID: vm.ID,
		StatusCode: 429, InputTokens: 5, OutputTokens: 5, CostUSD: 0.125, ErrorLabel: "rate_limit", FirstTokenMS: &latency300}
	fakeTestSeedLog(t, fake, owner.ID, first)
	fakeTestSeedLog(t, fake, owner.ID, second)
	fakeTestSeedLog(t, fake, other.ID, LogRow{RequestID: "foreign", CreatedAt: stamp, KeyID: "foreign-key", VMID: "foreign-vm", StatusCode: 200, CostUSD: 10})
	from, until := stamp.Add(-time.Second), stamp.Add(time.Minute)
	billing, err := client.UserBilling(ctx, owner.ID, from, until)
	if err != nil || billing.Totals.Requests != 2 || billing.Totals.Tokens != 110 || billing.Totals.CostUSD != 0.375 ||
		len(billing.ByKey) != 1 || billing.ByKey[0].KeyID != key.ID || billing.ByKey[0].Tokens != 110 || len(billing.ByVM) != 1 || billing.ByVM[0].VMID != vm.ID {
		t.Fatalf("billing = %+v, %v", billing, err)
	}
	directBilling, err := fake.UserBilling(ctx, owner.ID, from, until)
	if err != nil || !reflect.DeepEqual(billing, directBilling) {
		t.Fatalf("billing roundtrip = %+v, direct %+v, %v", billing, directBilling, err)
	}
	page, err := client.UserRequestLogs(ctx, owner.ID, LogQuery{From: from, Until: until, Page: 2, PageSize: 1, KeyID: key.ID, Model: "claude"})
	if err != nil || page.Total != 2 || len(page.Items) != 1 || !reflect.DeepEqual(page.Items[0], first) {
		t.Fatalf("logs = %+v, %v", page, err)
	}
	errorsPage, err := client.UserRequestLogs(ctx, owner.ID, LogQuery{Status: "error"})
	if err != nil || errorsPage.Total != 1 || !reflect.DeepEqual(errorsPage.Items[0], second) {
		t.Fatalf("error logs = %+v, %v", errorsPage, err)
	}
	stats, err := client.UserRequestStats(ctx, owner.ID, from, until)
	if err != nil || stats.Requests != 2 || stats.ErrorCount != 1 || stats.SuccessRate != 0.5 || stats.AvgTTFTMS != 200 || stats.P95TTFTMS != 300 {
		t.Fatalf("stats = %+v, %v", stats, err)
	}
	keyUsage, err := client.KeyStats(ctx, key.ID)
	if err != nil || len(keyUsage.Daily) != 1 || keyUsage.Daily[0].Tokens != 110 || len(keyUsage.ByModel) != 1 || keyUsage.ByModel[0].CostUSD != 0.375 {
		t.Fatalf("key stats = %+v, %v", keyUsage, err)
	}
	account, err := client.GetAccountUsage(ctx, vm.ID)
	if err != nil || account.Today == nil || account.Window5h.Tokens != 110 || account.Window7d.CostUSD != 0.375 ||
		account.Estimate5h.Status != "no_data" || len(account.Daily) != 1 || len(account.ByModel) != 1 || account.VM.CredStatus != "valid" {
		t.Fatalf("account = %+v, %v", account, err)
	}
	if err := client.SetVMOwner(ctx, vm.ID, nil); err != nil {
		t.Fatal(err)
	}
	pool, err := client.ListVMs(ctx, VMFilter{Unowned: true, ClaudeOnly: true})
	if err != nil || len(pool) != 1 || pool[0].Origin != "platform" {
		t.Fatalf("reclaimed pool = %+v, %v", pool, err)
	}
	if err := client.DeleteKey(ctx, key.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := client.KeyStats(ctx, key.ID); !IsStatus(err, 404) {
		t.Fatalf("deleted key = %v", err)
	}
	if err := client.DeleteVM(ctx, vm.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := client.GetAccountUsage(ctx, vm.ID); !IsStatus(err, 404) {
		t.Fatalf("deleted vm = %v", err)
	}
}

func TestFakeAuthExpiryAndSessionBinding(t *testing.T) {
	ctx := context.Background()
	fake := NewFake()
	now := time.Date(2026, 10, 31, 0, 0, 0, 0, time.UTC)
	fake.now = func() time.Time { return now }
	fake.AddVM(VM{ID: "first", ProxyConfigured: true})
	fake.AddVM(VM{ID: "second", ProxyConfigured: true})
	fake.AddVM(VM{ID: "no-proxy"})
	if _, err := fake.GenerateAuthURL(ctx, "no-proxy"); !IsCode(err, "proxy_required") {
		t.Fatalf("missing proxy = %v", err)
	}
	auth, err := fake.GenerateAuthURL(ctx, "first")
	if err != nil || !auth.ExpiresAt.Equal(now.Add(30*time.Minute)) {
		t.Fatalf("auth ttl = %+v, %v", auth, err)
	}
	if err := fake.ExchangeCode(ctx, "second", auth.SessionID, "code"); !IsCode(err, "session_vm_mismatch") {
		t.Fatalf("session binding = %v", err)
	}
	if err := fake.ExchangeCode(ctx, "first", auth.SessionID, " "); !IsCode(err, "code_required") {
		t.Fatalf("empty code = %v", err)
	}
	now = now.Add(30 * time.Minute)
	if err := fake.ExchangeCode(ctx, "first", auth.SessionID, "code"); !IsCode(err, "session_expired") {
		t.Fatalf("expiry boundary = %v", err)
	}
	auth, err = fake.GenerateAuthURL(ctx, "first")
	if err != nil {
		t.Fatal(err)
	}
	replacement, err := fake.GenerateAuthURL(ctx, "first")
	if err != nil {
		t.Fatal(err)
	}
	if err := fake.ExchangeCode(ctx, "first", auth.SessionID, "code"); !IsCode(err, "session_expired") {
		t.Fatalf("superseded session = %v", err)
	}
	if err := fake.ExchangeCode(ctx, "first", replacement.SessionID, "code"); err != nil {
		t.Fatal(err)
	}
	if err := fake.ExchangeCode(ctx, "first", replacement.SessionID, "code"); !IsCode(err, "session_expired") {
		t.Fatalf("replayed session = %v", err)
	}
	for _, expected := range []string{"pending", "running", "succeeded", "succeeded"} {
		bootstrap, err := fake.BootstrapStatus(ctx, "first")
		if err != nil || bootstrap.State != expected {
			t.Fatalf("bootstrap = %+v, %v", bootstrap, err)
		}
	}
}

func TestFakeOwnershipSnapshotsAndWire(t *testing.T) {
	ctx := context.Background()
	fake := NewFake()
	owner := fakeTestUser(t, fake, "owner")
	reset := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	seed := VM{ID: "seed", Label: "种子", Platform: "anthropic", OwnerUserID: owner.ID,
		CredStatus: "凭证失效", UnschedulableCode: "oauth_revoked", CredentialRejected: false,
		FiveHour: &Window{Utilization: 25, ResetsAt: &reset, Status: "allowed"},
		SevenDay: &Window{Utilization: 50, Status: "rejected"}, SevenDayFable: &Window{Utilization: 75, Status: "allowed"}}
	fake.AddVM(seed)
	seed.FiveHour.Utilization = 99
	reset = reset.Add(time.Hour)
	listed, err := fake.ListVMs(ctx, VMFilter{OwnerUserID: &owner.ID})
	if err != nil || len(listed) != 1 || !listed[0].CredentialRejected || listed[0].FiveHour.Utilization != 25 || listed[0].FiveHour.ResetsAt.Hour() != 0 {
		t.Fatalf("seed snapshot = %+v, %v", listed, err)
	}
	listed[0].FiveHour.Utilization = 88
	account, err := fake.GetAccountUsage(ctx, "seed")
	if err != nil || account.VM.FiveHour.Utilization != 25 || !account.VM.CredentialRejected {
		t.Fatalf("account snapshot = %+v, %v", account, err)
	}
	if err := fake.SetSchedulable(ctx, "seed", true); !IsCode(err, "credential_unavailable") {
		t.Fatalf("rejected credential = %v", err)
	}
	server := httptest.NewServer(fake.Handler("master"))
	t.Cleanup(server.Close)
	client := NewHTTPClient(server.URL, "master", server.Client())
	wireVMs, err := client.ListVMs(ctx, VMFilter{OwnerUserID: &owner.ID})
	if err != nil || len(wireVMs) != 1 || wireVMs[0].FiveHour.Utilization != 25 || wireVMs[0].FiveHour.Status != "allowed" ||
		wireVMs[0].SevenDay.Utilization != 50 || wireVMs[0].SevenDayFable.Utilization != 75 || wireVMs[0].CredStatus != seed.CredStatus || !wireVMs[0].CredentialRejected {
		t.Fatalf("wire windows = %+v, %v", wireVMs, err)
	}
	fake.AddVM(VM{ID: "platform"})
	fake.AddVM(VM{ID: "openai", Platform: "openai"})
	fake.AddVM(VM{ID: "invalid-origin", Origin: "user_created"})
	pool, err := fake.ListVMs(ctx, VMFilter{Unowned: true, ClaudeOnly: true})
	if err != nil || len(pool) != 1 || pool[0].ID != "platform" {
		t.Fatalf("pool filter = %+v, %v", pool, err)
	}
	if err := fake.SetVMOwner(ctx, "invalid-origin", nil); !IsCode(err, "cannot_unassign_user_created") {
		t.Fatalf("unassign user-created = %v", err)
	}
	fake.AddVM(VM{ID: "false-positive", CredentialRejected: true})
	account, err = fake.GetAccountUsage(ctx, "false-positive")
	if err != nil || account.VM.CredentialRejected {
		t.Fatalf("derived credential flag = %+v, %v", account.VM, err)
	}
}

func TestFakeKeySecrecyAndHandlerErrors(t *testing.T) {
	ctx := context.Background()
	fake := NewFake()
	owner := fakeTestUser(t, fake, "owner")
	other := fakeTestUser(t, fake, "other")
	key, err := fake.CreateKey(ctx, CreateKeyInput{UserID: owner.ID, MaxConcurrency: 999})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fake.CreateKey(ctx, CreateKeyInput{UserID: other.ID}); err != nil {
		t.Fatal(err)
	}
	if _, err := fake.CreateUser(ctx, "owner", "password"); !IsCode(err, "username_exists") || !IsStatus(err, 409) {
		t.Fatalf("duplicate user = %v", err)
	}
	if _, err := fake.CreateKey(ctx, CreateKeyInput{UserID: "missing"}); !IsStatus(err, 404) {
		t.Fatalf("missing owner = %v", err)
	}
	if fake.keys[key.ID].key.Secret != "" || fake.keys[key.ID].maxConcurrency != 128 {
		t.Fatalf("stored key = %+v", fake.keys[key.ID])
	}
	handler := fake.Handler("master")
	for _, path := range []string{"/api/panel/api-keys", "/api/panel/api-keys/" + key.ID + "/stats"} {
		payload := fakeTestRaw(t, handler, "GET", path, "master", "", 200)
		encoded, err := json.Marshal(payload)
		if err != nil || strings.Contains(string(encoded), key.Secret) {
			t.Fatalf("plaintext leaked: %s, %v", encoded, err)
		}
	}
	filtered := fakeTestRaw(t, handler, "GET", "/api/panel/api-keys?user_id="+owner.ID, "master", "", 200)
	if filtered["total"] != float64(1) {
		t.Fatalf("key owner filter = %+v", filtered)
	}
	for _, test := range []struct {
		method, path, key, body string
		status                  int
	}{
		{"GET", "/api/panel/version", "", "", 401},
		{"GET", "/api/panel/version", key.Secret, "", 401},
		{"POST", "/api/panel/users", "master", `{`, 400},
		{"POST", "/api/panel/users", "master", `{} {}`, 400},
		{"PATCH", "/api/panel/api-keys/" + key.ID, "master", `{"status":"paused"}`, 400},
		{"POST", "/api/panel/api-keys/" + key.ID + "/reveal", "master", `{}`, 409},
		{"GET", "/api/panel/api-keys/" + key.ID, "master", "", 404},
		{"GET", "/api/panel/billing", "master", "", 400},
		{"GET", "/api/panel/billing?user_id=" + owner.ID + "&from=bad", "master", "", 400},
		{"GET", "/api/panel/request-logs?user_id=" + owner.ID + "&limit=-1", "master", "", 400},
		{"DELETE", "/api/panel/users/" + owner.ID, "master", "", 404},
	} {
		fakeTestRaw(t, handler, test.method, test.path, test.key, test.body, test.status)
	}
	request := httptest.NewRequest("GET", "/api/panel/version", nil)
	request.Header.Set("x-api-key", "master")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != 200 {
		t.Fatalf("x-api-key status = %d", response.Code)
	}
	if _, err := fake.CreateHostedVM(ctx, "bad", json.RawMessage(`[]`)); !IsCode(err, "invalid_template") {
		t.Fatalf("invalid template = %v", err)
	}
	if _, err := fake.UserRequestStats(ctx, "missing", time.Time{}, time.Time{}); !IsStatus(err, 404) {
		t.Fatalf("missing stats user = %v", err)
	}
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := fake.CreateUser(canceled, "canceled", "password"); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled context = %v", err)
	}
}

func TestFakeUsageScopeAndPagination(t *testing.T) {
	ctx := context.Background()
	fake := NewFake()
	owner := fakeTestUser(t, fake, "owner")
	other := fakeTestUser(t, fake, "other")
	stamp := time.Date(2026, 10, 30, 16, 0, 0, 0, time.UTC)
	latency := int64(100)
	for index := range 520 {
		fakeTestSeedLog(t, fake, owner.ID, LogRow{RequestID: fmt.Sprintf("row-%03d", index), CreatedAt: stamp.Add(time.Duration(index) * time.Second),
			KeyID: "owner-key", VMID: "shared-vm", Model: "claude", StatusCode: 200, InputTokens: 1, FirstTokenMS: &latency})
	}
	latency = 999
	fakeTestSeedLog(t, fake, other.ID, LogRow{CreatedAt: stamp, KeyID: "other-key", VMID: "shared-vm", StatusCode: 429, CostUSD: 999})
	server := httptest.NewServer(fake.Handler("master"))
	t.Cleanup(server.Close)
	client := NewHTTPClient(server.URL, "master", server.Client())
	page, err := client.UserRequestLogs(ctx, owner.ID, LogQuery{Page: 2, PageSize: 300, KeyID: "owner-key"})
	if err != nil || page.Total != 520 || len(page.Items) != 220 || page.Items[0].RequestID != "row-219" || *page.Items[0].FirstTokenMS != 100 {
		t.Fatalf("cross-boundary page = %+v, %v", page, err)
	}
	page.Items[0].FirstTokenMS = &latency
	page, err = fake.UserRequestLogs(ctx, owner.ID, LogQuery{Page: math.MaxInt, PageSize: 500})
	if err != nil || page.Total != 520 || len(page.Items) != 0 {
		t.Fatalf("large page = %+v, %v", page, err)
	}
	page, err = fake.UserRequestLogs(ctx, owner.ID, LogQuery{KeyID: "other-key"})
	if err != nil || page.Total != 0 {
		t.Fatalf("foreign key logs = %+v, %v", page, err)
	}
	billing, err := client.UserBilling(ctx, owner.ID, stamp, stamp)
	if err != nil || billing.Totals.Requests != 1 || billing.Totals.CostUSD != 0 {
		t.Fatalf("inclusive range = %+v, %v", billing, err)
	}
	raw := fakeTestRaw(t, fake.Handler("master"), "GET", "/api/panel/request-logs?user_id="+url.QueryEscape(owner.ID)+"&limit=50&offset=490", "master", "", 200)
	if len(raw["items"].([]any)) != 30 || raw["total"] != float64(520) {
		t.Fatalf("wire offset = %+v", raw)
	}
}

func TestFakeConcurrentOperations(t *testing.T) {
	ctx := context.Background()
	fake := NewFake()
	owner := fakeTestUser(t, fake, "owner")
	server := httptest.NewServer(fake.Handler("master"))
	t.Cleanup(server.Close)
	client := NewHTTPClient(server.URL, "master", server.Client())
	var workers sync.WaitGroup
	for index := range 24 {
		workers.Go(func() {
			vm, err := client.CreateHostedVM(ctx, fmt.Sprintf("vm-%d", index), nil)
			if err != nil {
				t.Error(err)
				return
			}
			if err := client.SetVMOwner(ctx, vm.ID, &owner.ID); err != nil {
				t.Error(err)
			}
			if err := client.AllocateProxy(ctx, vm.ID); err != nil {
				t.Error(err)
			}
			auth, err := client.GenerateAuthURL(ctx, vm.ID)
			if err != nil {
				t.Error(err)
				return
			}
			if err := client.ExchangeCode(ctx, vm.ID, auth.SessionID, "code"); err != nil {
				t.Error(err)
			}
			key, err := client.CreateKey(ctx, CreateKeyInput{UserID: owner.ID})
			if err != nil {
				t.Error(err)
				return
			}
			if err := fake.AddRequestLog(owner.ID, LogRow{VMID: vm.ID, KeyID: key.ID, StatusCode: 200}); err != nil {
				t.Error(err)
			}
			if _, err := client.ListVMs(ctx, VMFilter{}); err != nil {
				t.Error(err)
			}
			if _, err := client.UserRequestStats(ctx, owner.ID, time.Time{}, time.Time{}); err != nil {
				t.Error(err)
			}
			if err := client.DeleteKey(ctx, key.ID); err != nil {
				t.Error(err)
			}
			if err := client.DeleteVM(ctx, vm.ID); err != nil {
				t.Error(err)
			}
		})
	}
	workers.Wait()
	vms, err := fake.ListVMs(ctx, VMFilter{})
	if err != nil || len(vms) != 0 {
		t.Fatalf("remaining vms = %+v, %v", vms, err)
	}
	stats, err := fake.UserRequestStats(ctx, owner.ID, time.Time{}, time.Time{})
	if err != nil || stats.Requests != 24 {
		t.Fatalf("concurrent stats = %+v, %v", stats, err)
	}
	response, err := server.Client().Get(server.URL + "/api/panel/version")
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	_, _ = io.Copy(io.Discard, response.Body)
	if response.StatusCode != 401 {
		t.Fatalf("unauthorized status = %d", response.StatusCode)
	}
}
