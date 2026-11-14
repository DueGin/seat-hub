package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"path/filepath"
	"strings"
	"testing"

	"github.com/DueGin/seat-hub/internal/api"
	"github.com/DueGin/seat-hub/internal/provision"
	"github.com/DueGin/seat-hub/internal/selfcheck"
	"github.com/DueGin/seat-hub/internal/store"
	"github.com/DueGin/seat-hub/internal/vm2api"
)

func TestSelfcheckErrorMapping(t *testing.T) {
	for _, test := range []struct {
		name    string
		failure error
		code    string
		message string
	}{
		{"failed", provision.ErrSelfcheckFailed, "selfcheck_failed", "槽位自检未通过，系统正在重建运行环境，请稍后再试"},
		{"exhausted", fmt.Errorf("recheck: %w", provision.ErrSelfcheckExhausted), "selfcheck_exhausted", "槽位多次自检未通过，已转人工处理"},
		{"duplicate", fmt.Errorf("exchange: %w", provision.ErrAccountDuplicate), "account_duplicate", "该 Claude 账号已在其它槽位托管，不能重复上号"},
	} {
		t.Run(test.name, func(t *testing.T) {
			mapped := api.AsUserError(mapProvisionErr(test.failure))
			if mapped == nil || mapped.Status != http.StatusConflict || mapped.Code != test.code || mapped.Message != test.message {
				t.Fatalf("mapped error = %+v", mapped)
			}
		})
	}
	wrapped := fmt.Errorf("check rental vm vm-1 [proxy_exclusive, exit_probe]: %w", provision.ErrSelfcheckFailed)
	mapped := api.AsUserError(mapProvisionErr(wrapped))
	if mapped == nil || mapped.Status != http.StatusConflict || mapped.Code != "selfcheck_failed" || !strings.Contains(mapped.Message, "proxy_exclusive, exit_probe") {
		t.Fatalf("rental admission error = %+v", mapped)
	}
	unknown := errors.New("unrecognized failure")
	if mapProvisionErr(unknown) != unknown || mapProvisionErr(nil) != nil {
		t.Fatal("unrecognized or nil error changed")
	}
}

func TestSelfcheckSettings(t *testing.T) {
	db, err := store.Open(filepath.Join(t.TempDir(), "settings.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	ctx := context.Background()
	adapter := &settingsAdapter{settings: store.NewSettings(db)}
	defaults, err := adapter.Get(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !defaults.SelfcheckRequireDedicatedProxy || !defaults.SelfcheckRequireTimezoneMatch || defaults.SelfcheckMaxRebuilds != 3 || defaults.SelfcheckRecheckMinutes != 30 {
		t.Fatalf("default selfcheck settings = %+v", defaults)
	}
	for _, invalid := range []struct{ rebuilds, minutes int }{{-1, 30}, {11, 30}, {3, 4}, {3, 1441}} {
		payload := defaults
		payload.SelfcheckMaxRebuilds, payload.SelfcheckRecheckMinutes = invalid.rebuilds, invalid.minutes
		mapped := api.AsUserError(adapter.Put(ctx, payload, 0))
		if mapped == nil || mapped.Status != http.StatusUnprocessableEntity || mapped.Code != "invalid_settings" {
			t.Fatalf("settings %+v mapped to %+v", invalid, mapped)
		}
	}
	for _, valid := range []struct{ rebuilds, minutes int }{{0, 5}, {10, 1440}} {
		payload := defaults
		payload.SelfcheckRequireDedicatedProxy, payload.SelfcheckRequireTimezoneMatch = false, false
		payload.SelfcheckMaxRebuilds, payload.SelfcheckRecheckMinutes = valid.rebuilds, valid.minutes
		if err := adapter.Put(ctx, payload, 0); err != nil {
			t.Fatal(err)
		}
		stored, err := adapter.Get(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if stored.SelfcheckRequireDedicatedProxy || stored.SelfcheckRequireTimezoneMatch || stored.SelfcheckMaxRebuilds != valid.rebuilds || stored.SelfcheckRecheckMinutes != valid.minutes {
			t.Fatalf("stored selfcheck settings = %+v", stored)
		}
	}
}

func TestSelfcheckWiringAndVisibility(t *testing.T) {
	h := newHarness(t)
	h.login("admin@example.com", "admin-pass-123")
	if err := h.fake.AllocateProxy(context.Background(), "vm-1"); err != nil {
		t.Fatal(err)
	}
	h.mustOK(http.MethodPut, "/api/admin/settings", `{"selfcheck_max_rebuilds":5}`, nil)
	id := adminSeedResource(t, h, "hosted", "awaiting_auth", "vm-1")
	var check api.SelfcheckDTO
	h.mustOK(http.MethodPost, fmt.Sprintf("/api/admin/accounts/%d/selfcheck", id), `{"rebuild":false}`, &check)
	if check.State != "pass" || check.MaxAttempts != 5 || check.CheckedAt == nil || len(check.Checks) != 9 {
		t.Fatalf("recheck result = %+v", check)
	}
	checks := []selfcheck.Check{{Code: selfcheck.ProxyExclusive, OK: true, Message: "独立出口检查通过", Detail: "private-proxy-and-ip"}}
	summary, err := json.Marshal(checks)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := h.app.DB.Exec(`UPDATE resources SET selfcheck_attempts=2,selfcheck_summary=? WHERE id=?`, string(summary), id); err != nil {
		t.Fatal(err)
	}
	if _, err := h.app.DB.Exec(`INSERT INTO slot_checks(resource_id,vm_id,attempt,passed,checks,created_at) VALUES (?,'vm-1',2,1,?,?)`, id, string(summary), store.Now()); err != nil {
		t.Fatal(err)
	}
	assertUserCheck := func(check *api.SelfcheckDTO) {
		t.Helper()
		if check == nil || check.State != "pass" || check.Attempts != 2 || check.MaxAttempts != 5 || len(check.Checks) != 1 || check.Checks[0].Detail != "" {
			t.Fatalf("user selfcheck = %+v", check)
		}
	}
	var accounts struct {
		Items []api.AccountDTO `json:"items"`
	}
	h.mustOK(http.MethodGet, "/api/accounts", "", &accounts)
	if len(accounts.Items) != 1 {
		t.Fatalf("account count = %d", len(accounts.Items))
	}
	assertUserCheck(accounts.Items[0].Selfcheck)
	var detail struct {
		Account api.AccountDTO `json:"account"`
	}
	h.mustOK(http.MethodGet, fmt.Sprintf("/api/accounts/%d", id), "", &detail)
	assertUserCheck(detail.Account.Selfcheck)
	var onboarding api.OnboardingDTO
	h.mustOK(http.MethodGet, fmt.Sprintf("/api/hosted/%d/onboarding", id), "", &onboarding)
	assertUserCheck(onboarding.Selfcheck)
	var adminAccounts struct {
		Items []api.AdminAccountDTO `json:"items"`
	}
	h.mustOK(http.MethodGet, "/api/admin/accounts", "", &adminAccounts)
	if len(adminAccounts.Items) != 1 || adminAccounts.Items[0].Selfcheck == nil || adminAccounts.Items[0].Selfcheck.Checks[0].Detail != checks[0].Detail {
		t.Fatalf("admin accounts = %+v", adminAccounts.Items)
	}
	var history struct {
		Items []api.SelfcheckReportDTO `json:"items"`
	}
	h.mustOK(http.MethodGet, fmt.Sprintf("/api/admin/accounts/%d/selfchecks?limit=1", id), "", &history)
	if len(history.Items) != 1 || len(history.Items[0].Checks) != 1 || history.Items[0].Checks[0].Detail != checks[0].Detail {
		t.Fatalf("selfcheck history = %+v", history.Items)
	}
	h.fake.AddVM(vm2api.VM{ID: "pool-unhealthy"})
	status, env := h.call(http.MethodPost, "/api/admin/rental-pool", `{"vm_ids":["pool-unhealthy"]}`, nil)
	if status != http.StatusConflict || env.Error == nil || env.Error.Code != "selfcheck_failed" || !strings.Contains(env.Error.Message, "proxy_bound") {
		t.Fatalf("failed pool admission = %d %+v", status, env.Error)
	}
	if err := h.fake.AllocateProxy(context.Background(), "pool-unhealthy"); err != nil {
		t.Fatal(err)
	}
	h.mustOK(http.MethodPost, "/api/admin/rental-pool", `{"vm_ids":["pool-unhealthy"]}`, nil)
	var pool struct {
		Items []api.RentalPoolRowDTO `json:"items"`
	}
	h.mustOK(http.MethodGet, "/api/admin/rental-pool", "", &pool)
	if len(pool.Items) != 1 || pool.Items[0].Selfcheck == nil || pool.Items[0].Selfcheck.MaxAttempts != 5 || pool.Items[0].Selfcheck.Checks == nil {
		t.Fatalf("pool selfcheck = %+v", pool.Items)
	}
}
