package app

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/DueGin/seat-hub/internal/api"
	"github.com/DueGin/seat-hub/internal/vm2api"
)

func TestHTTPKeyScopeIsolationAndRentalReassignment(t *testing.T) {
	h := newHarness(t)
	h.login("admin@example.com", "admin-pass-123")
	for _, id := range []string{"vm-2", "vm-3"} {
		h.fake.AddVM(vm2api.VM{ID: id, Platform: "anthropic", Origin: "platform", HasCredential: true, ScheduleState: "on", ProxyConfigured: true, CredStatus: "可用"})
	}
	for _, id := range []string{"vm-1", "vm-2", "vm-3"} {
		if err := h.fake.AllocateProxy(context.Background(), id); err != nil {
			t.Fatal(err)
		}
	}
	h.mustOK(http.MethodPost, "/api/admin/rental-pool", `{"vm_ids":["vm-1","vm-2","vm-3"]}`, nil)
	a, aPassword := h.createUser(`{"email":"scope-a@example.com"}`)
	b, bPassword := h.createUser(`{"email":"scope-b@example.com"}`)
	h.mustOK(http.MethodPost, fmt.Sprintf("/api/admin/users/%d/grant", a.ID), `{"kind":"rental_account","quantity":2,"duration_days":30}`, nil)
	h.mustOK(http.MethodPost, fmt.Sprintf("/api/admin/users/%d/grant", b.ID), `{"kind":"rental_account","quantity":1,"duration_days":30}`, nil)
	alice, bob := h.session(), h.session()
	alice.login(a.Email, aPassword)
	bob.login(b.Email, bPassword)
	accounts := func(userID int64) map[int64]string {
		t.Helper()
		rows, err := h.app.DB.Query(`SELECT id,vm_id FROM resources WHERE user_id=? AND status='active' ORDER BY id`, userID)
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		out := make(map[int64]string)
		for rows.Next() {
			var id int64
			var vmID string
			if err := rows.Scan(&id, &vmID); err != nil {
				t.Fatal(err)
			}
			out[id] = vmID
		}
		if err := rows.Err(); err != nil {
			t.Fatal(err)
		}
		return out
	}
	aVMs, bVMs := accounts(a.ID), accounts(b.ID)
	if len(aVMs) != 2 || len(bVMs) != 1 {
		t.Fatalf("rental allocation = %v, %v", aVMs, bVMs)
	}
	var selectedID int64
	var selectedVM string
	for id, vm := range aVMs {
		selectedID, selectedVM = id, vm
		break
	}
	createKey := func(session *harness, body string) (int64, string, string) {
		t.Helper()
		var out struct {
			Key    api.KeyDTO `json:"key"`
			Secret string     `json:"secret"`
		}
		session.mustOK(http.MethodPost, "/api/keys", body, &out)
		var upstream string
		if err := h.app.DB.QueryRow(`SELECT vm2api_key_id FROM api_keys WHERE id=?`, out.Key.ID).Scan(&upstream); err != nil {
			t.Fatal(err)
		}
		return out.Key.ID, out.Secret, upstream
	}
	_, aSecret, aUpstream := createKey(alice, `{"name":"全部"}`)
	_, selectedSecret, selectedUpstream := createKey(alice, fmt.Sprintf(`{"name":"指定","scope":{"mode":"selected","accounts":[%d]}}`, selectedID))
	_, bSecret, bUpstream := createKey(bob, `{"name":"全部"}`)
	call := func(secret string) (int, string) {
		t.Helper()
		req, err := http.NewRequest(http.MethodPost, h.portal.URL+"/v1/messages", strings.NewReader(`{"model":"claude","messages":[]}`))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Authorization", "Bearer "+secret)
		resp, err := h.client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		body, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(body)
	}
	assertVMs := func(secret, key string, allowed map[string]bool) {
		t.Helper()
		for range 6 {
			if status, body := call(secret); status != 200 {
				t.Fatalf("protocol = %d %s", status, body)
			}
		}
		logs, err := h.fake.UserRequestLogs(context.Background(), "", vm2api.LogQuery{KeyID: key, Page: 1, PageSize: 100})
		if err != nil || len(logs.Items) == 0 {
			t.Fatalf("logs = %+v, %v", logs, err)
		}
		seen := make(map[string]bool)
		for _, row := range logs.Items {
			if row.StatusCode == 200 && !allowed[row.VMID] {
				t.Fatalf("key %s crossed scope onto %s (allowed %v)", key, row.VMID, allowed)
			}
			if row.StatusCode == 200 {
				seen[row.VMID] = true
			}
		}
		for vm := range allowed {
			if !seen[vm] {
				t.Fatalf("key %s never reached its own VM %s", key, vm)
			}
		}
	}
	allowedA, allowedB := make(map[string]bool), make(map[string]bool)
	for _, vm := range aVMs {
		allowedA[vm] = true
	}
	for _, vm := range bVMs {
		allowedB[vm] = true
	}
	assertVMs(aSecret, aUpstream, allowedA)
	assertVMs(bSecret, bUpstream, allowedB)
	assertVMs(selectedSecret, selectedUpstream, map[string]bool{selectedVM: true})
	// Release the selected account while A still has another usable account.
	h.mustOK(http.MethodPost, fmt.Sprintf("/api/admin/accounts/%d/release", selectedID), `{}`, nil)
	if status, _ := call(selectedSecret); status == 200 {
		t.Fatal("cached selected principal bypassed upstream revocation")
	}
	h.app.resolver.Invalidate()
	if status, body := call(selectedSecret); status != 403 || !strings.Contains(body, "重新选择账号") {
		t.Fatalf("expired selected key = %d %s", status, body)
	}
	for id := range aVMs {
		if id != selectedID {
			h.mustOK(http.MethodPost, fmt.Sprintf("/api/admin/accounts/%d/release", id), `{}`, nil)
		}
	}
	if status, _ := call(aSecret); status == 200 {
		t.Fatal("cached all-mode principal reached a released rental")
	}
	h.app.resolver.Invalidate()
	if status, body := call(aSecret); status != 403 || !strings.Contains(body, "没有可用的账号") {
		t.Fatalf("released user's key = %d %s", status, body)
	}
	h.mustOK(http.MethodPost, fmt.Sprintf("/api/admin/users/%d/grant", b.ID), `{"kind":"rental_account","quantity":2,"duration_days":30}`, nil)
	for _, vm := range accounts(b.ID) {
		allowedB[vm] = true
	}
	if len(allowedB) != 3 || !allowedB[selectedVM] {
		t.Fatalf("released VMs were not re-leased: %v", allowedB)
	}
	// A cached active principal is still unable to spend onto a re-leased VM.
	if status, _ := call(selectedSecret); status == 200 {
		t.Fatal("A's selected key reached a re-leased VM")
	}
	assertVMs(bSecret, bUpstream, allowedB)
	h.mustOK(http.MethodPatch, fmt.Sprintf("/api/admin/users/%d", a.ID), `{"status":"disabled"}`, nil)
	for _, key := range []string{aUpstream, selectedUpstream} {
		if exists, active := h.fake.KeyState(key); !exists || active {
			t.Fatalf("disabled user's upstream key %s = %v %v", key, exists, active)
		}
	}
	var detail struct {
		User api.AdminUserDTO `json:"user"`
	}
	h.mustOK(http.MethodGet, fmt.Sprintf("/api/admin/users/%d", b.ID), "", &detail)
	if detail.User.VM2APIPoolID == "" {
		raw, _ := json.Marshal(detail)
		t.Fatalf("admin detail omitted pool: %s", raw)
	}
}
