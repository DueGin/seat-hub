package app

import (
	"fmt"
	"net/http"
	"testing"

	"github.com/DueGin/seat-hub/internal/store"
)

// adminSeedResource inserts a resource row for the admin user with the given
// state, returning its id.
func adminSeedResource(t *testing.T, h *harness, kind, status, vmID string) int64 {
	t.Helper()
	var userID int64
	if err := h.app.DB.QueryRow(`SELECT id FROM users WHERE email = 'admin@example.com'`).Scan(&userID); err != nil {
		t.Fatal(err)
	}
	stamp := store.Now()
	vm := any(nil)
	if vmID != "" {
		vm = vmID
	}
	var id int64
	if err := h.app.DB.QueryRow(`INSERT INTO resources(user_id,kind,vm_id,status,created_at,updated_at) VALUES (?,?,?,?,?,?) RETURNING id`,
		userID, kind, vm, status, stamp, stamp).Scan(&id); err != nil {
		t.Fatal(err)
	}
	return id
}

func TestAdminAccountRetryAndRelease(t *testing.T) {
	h := newHarness(t)
	h.login("admin@example.com", "admin-pass-123")

	// The retry endpoint has no owning user in its path. A hosted failure must
	// be repaired, a rental one rejected, and a missing row reported as 404.
	hosted := adminSeedResource(t, h, "hosted", "failed", "")
	if status, env := h.call(http.MethodPost, fmt.Sprintf("/api/admin/accounts/%d/retry", hosted), `{}`, nil); status != 200 || !env.OK {
		t.Fatalf("hosted retry -> %d %+v", status, env.Error)
	}
	rental := adminSeedResource(t, h, "rental", "failed", "")
	if status, env := h.call(http.MethodPost, fmt.Sprintf("/api/admin/accounts/%d/retry", rental), `{}`, nil); status != http.StatusConflict || env.Error == nil || env.Error.Code != "invalid_state" {
		t.Fatalf("rental retry -> %d %+v", status, env.Error)
	}
	if status, env := h.call(http.MethodPost, "/api/admin/accounts/99999/retry", `{}`, nil); status != http.StatusNotFound || env.Error == nil || env.Error.Code != "not_found" {
		t.Fatalf("missing retry -> %d %+v", status, env.Error)
	}

	// Force release must act on an active account instead of returning 200 with
	// nothing changed, and must stay idempotent once released.
	active := adminSeedResource(t, h, "hosted", "active", "vm-1")
	if status, env := h.call(http.MethodPost, fmt.Sprintf("/api/admin/accounts/%d/release", active), `{}`, nil); status != 200 || !env.OK {
		t.Fatalf("force release -> %d %+v", status, env.Error)
	}
	var resourceStatus string
	if err := h.app.DB.QueryRow(`SELECT status FROM resources WHERE id = ?`, active).Scan(&resourceStatus); err != nil {
		t.Fatal(err)
	}
	if resourceStatus != "released" {
		t.Fatalf("released account status=%s", resourceStatus)
	}
	if status, env := h.call(http.MethodPost, fmt.Sprintf("/api/admin/accounts/%d/release", active), `{}`, nil); status != 200 || !env.OK {
		t.Fatalf("repeated release -> %d %+v", status, env.Error)
	}
	if status, env := h.call(http.MethodPost, "/api/admin/accounts/99999/release", `{}`, nil); status != http.StatusNotFound || env.Error == nil || env.Error.Code != "not_found" {
		t.Fatalf("missing release -> %d %+v", status, env.Error)
	}
}
