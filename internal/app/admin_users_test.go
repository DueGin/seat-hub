package app

import (
	"encoding/json"
	"net/http"
	"net/http/cookiejar"
	"strconv"
	"testing"
)

// session is a second browser on the same portal, with its own cookies.
func (h *harness) session() *harness {
	h.t.Helper()
	jar, err := cookiejar.New(nil)
	if err != nil {
		h.t.Fatal(err)
	}
	other := *h
	other.client = &http.Client{Jar: jar}
	return &other
}

type adminUser struct {
	ID             int64  `json:"id"`
	Email          string `json:"email"`
	Role           string `json:"role"`
	Status         string `json:"status"`
	Note           string `json:"note"`
	LastLoginAt    string `json:"last_login_at"`
	LastLoginIP    string `json:"last_login_ip"`
	ActiveSessions int64  `json:"active_sessions"`
	VM2APIUserID   string `json:"vm2api_user_id"`
}

func (h *harness) createUser(body string) (adminUser, string) {
	h.t.Helper()
	var out struct {
		User     adminUser `json:"user"`
		Password string    `json:"password"`
	}
	status, env := h.call(http.MethodPost, "/api/admin/users", body, nil)
	if status != http.StatusCreated || !env.OK {
		h.t.Fatalf("create user %s -> %d %s", body, status, env.Data)
	}
	if err := json.Unmarshal(env.Data, &out); err != nil {
		h.t.Fatal(err)
	}
	return out.User, out.Password
}

func (h *harness) expectError(method, path, body string, status int, code string) {
	h.t.Helper()
	got, env := h.call(method, path, body, nil)
	if got != status || env.Error == nil || code != "" && env.Error.Code != code {
		h.t.Fatalf("%s %s %s -> %d %+v, want %d %s", method, path, body, got, env.Error, status, code)
	}
}

func (h *harness) tryLogin(email, password string) int {
	h.t.Helper()
	status, _ := h.call(http.MethodPost, "/api/auth/login", `{"email":"`+email+`","password":"`+password+`"}`, nil)
	return status
}

func adminID(t *testing.T, h *harness) int64 {
	t.Helper()
	var id int64
	if err := h.app.DB.QueryRow(`SELECT id FROM users WHERE email='admin@example.com'`).Scan(&id); err != nil {
		t.Fatal(err)
	}
	return id
}

func TestAdminCreatesAndEditsUsers(t *testing.T) {
	h := newHarness(t)
	h.login("admin@example.com", "admin-pass-123")

	created, password := h.createUser(`{"email":"  Alice@Example.com ","note":"渠道：内测"}`)
	if created.Email != "alice@example.com" || created.Role != "user" || created.Status != "active" || created.Note != "渠道：内测" || len(password) != 16 {
		t.Fatalf("created=%+v password=%q", created, password)
	}
	alice := h.session()
	if status := alice.tryLogin("alice@example.com", password); status != 200 {
		t.Fatalf("generated password does not log in: %d", status)
	}
	// An explicit password is used as given and not echoed back.
	bob, echoed := h.createUser(`{"email":"bob@example.com","password":"bob-pass-123","role":"admin"}`)
	if echoed != "" || bob.Role != "admin" || h.session().tryLogin("bob@example.com", "bob-pass-123") != 200 {
		t.Fatalf("explicit password: user=%+v echoed=%q", bob, echoed)
	}
	h.expectError(http.MethodPost, "/api/admin/users", `{"email":"ALICE@example.com"}`, http.StatusConflict, "email_taken")
	h.expectError(http.MethodPost, "/api/admin/users", `{"email":"not-an-email"}`, http.StatusUnprocessableEntity, "invalid_email")
	h.expectError(http.MethodPost, "/api/admin/users", `{"email":"c@example.com","password":"short"}`, http.StatusUnprocessableEntity, "invalid_password")
	h.expectError(http.MethodPost, "/api/admin/users", `{"email":"c@example.com","role":"root"}`, http.StatusUnprocessableEntity, "invalid_role")

	// The list shows the last login and the signed-in device.
	var page struct {
		Items []adminUser `json:"items"`
		Total int64       `json:"total"`
	}
	h.mustOK(http.MethodGet, "/api/admin/users?q=alice", "", &page)
	if page.Total != 1 || page.Items[0].LastLoginAt == "" || page.Items[0].LastLoginIP == "" || page.Items[0].ActiveSessions != 1 {
		t.Fatalf("alice in list=%+v", page)
	}
	h.mustOK(http.MethodGet, "/api/admin/users?role=admin", "", &page)
	if page.Total != 2 {
		t.Fatalf("admins=%d", page.Total)
	}
	h.mustOK(http.MethodGet, "/api/admin/users?q=%23"+strconv.FormatInt(created.ID, 10), "", &page)
	if page.Total != 1 || page.Items[0].ID != created.ID {
		t.Fatalf("search by id=%+v", page)
	}
	h.mustOK(http.MethodGet, "/api/admin/users?q=%E5%86%85%E6%B5%8B", "", &page) // note text
	if page.Total != 1 {
		t.Fatalf("search by note=%d", page.Total)
	}
	h.expectError(http.MethodGet, "/api/admin/users?status=gone", "", http.StatusUnprocessableEntity, "invalid_status")

	// Edit email and note together; the old email stops working.
	var patched struct {
		User adminUser `json:"user"`
	}
	h.mustOK(http.MethodPatch, "/api/admin/users/"+strconv.FormatInt(created.ID, 10), `{"email":"alice2@example.com","note":"VIP"}`, &patched)
	if patched.User.Email != "alice2@example.com" || patched.User.Note != "VIP" {
		t.Fatalf("patched=%+v", patched.User)
	}
	if h.session().tryLogin("alice@example.com", password) == 200 || h.session().tryLogin("alice2@example.com", password) != 200 {
		t.Fatal("email change did not move the sign-in")
	}
	h.expectError(http.MethodPatch, "/api/admin/users/"+strconv.FormatInt(created.ID, 10), `{"email":"bob@example.com"}`, http.StatusConflict, "email_taken")
	h.expectError(http.MethodPatch, "/api/admin/users/"+strconv.FormatInt(created.ID, 10), `{}`, http.StatusUnprocessableEntity, "invalid_body")

	// The acting admin cannot lock themselves out.
	self := strconv.FormatInt(adminID(t, h), 10)
	h.expectError(http.MethodPatch, "/api/admin/users/"+self, `{"role":"user"}`, http.StatusConflict, "self_lockout")
	h.expectError(http.MethodPatch, "/api/admin/users/"+self, `{"status":"disabled"}`, http.StatusConflict, "self_lockout")
	h.expectError(http.MethodPost, "/api/admin/users/"+self+"/password", `{}`, http.StatusConflict, "self_lockout")
	h.expectError(http.MethodPost, "/api/admin/users/"+self+"/sessions/revoke", `{}`, http.StatusConflict, "self_lockout")
	h.expectError(http.MethodDelete, "/api/admin/users/"+self, "", http.StatusConflict, "self_lockout")
	// But may still edit their own note.
	h.mustOK(http.MethodPatch, "/api/admin/users/"+self, `{"note":"站长"}`, nil)

	// Non-admins get nothing.
	alice.expectError(http.MethodPost, "/api/admin/users", `{"email":"x@example.com"}`, http.StatusForbidden, "")
}

func TestAdminResetsPasswordAndSignsUserOut(t *testing.T) {
	h := newHarness(t)
	h.login("admin@example.com", "admin-pass-123")
	user, password := h.createUser(`{"email":"carol@example.com"}`)
	carol := h.session()
	carol.login("carol@example.com", password)
	carol.mustOK(http.MethodGet, "/api/me/overview", "", nil)
	id := strconv.FormatInt(user.ID, 10)

	h.mustOK(http.MethodPost, "/api/admin/users/"+id+"/sessions/revoke", `{}`, nil)
	if status, _ := carol.call(http.MethodGet, "/api/me/overview", "", nil); status != http.StatusUnauthorized {
		t.Fatalf("revoked session still works: %d", status)
	}
	carol.login("carol@example.com", password)

	var reset struct {
		Password string `json:"password"`
	}
	h.mustOK(http.MethodPost, "/api/admin/users/"+id+"/password", `{}`, &reset)
	if len(reset.Password) != 16 || reset.Password == password {
		t.Fatalf("reset password=%q", reset.Password)
	}
	if status, _ := carol.call(http.MethodGet, "/api/me/overview", "", nil); status != http.StatusUnauthorized {
		t.Fatal("reset did not end the existing session")
	}
	if carol.tryLogin("carol@example.com", password) == 200 || carol.tryLogin("carol@example.com", reset.Password) != 200 {
		t.Fatal("reset did not replace the password")
	}
	var explicit struct {
		Password string `json:"password"`
	}
	h.mustOK(http.MethodPost, "/api/admin/users/"+id+"/password", `{"password":"chosen-pass-1"}`, &explicit)
	if explicit.Password != "" || h.session().tryLogin("carol@example.com", "chosen-pass-1") != 200 {
		t.Fatalf("explicit reset echoed password=%q", explicit.Password)
	}
	h.expectError(http.MethodPost, "/api/admin/users/"+id+"/password", `{"password":"short"}`, http.StatusUnprocessableEntity, "invalid_password")
	h.expectError(http.MethodPost, "/api/admin/users/99999/password", `{}`, http.StatusNotFound, "user_not_found")

	var detail struct {
		Sessions []struct {
			IP string `json:"ip"`
		} `json:"sessions"`
		Records struct {
			Orders int64 `json:"orders"`
		} `json:"records"`
	}
	h.mustOK(http.MethodGet, "/api/admin/users/"+id, "", &detail)
	if len(detail.Sessions) != 1 || detail.Sessions[0].IP == "" {
		t.Fatalf("sessions=%+v", detail.Sessions)
	}
}

func TestAdminDisableEnableSyncsUpstreamKeys(t *testing.T) {
	h := newHarness(t)
	h.login("admin@example.com", "admin-pass-123")
	user, password := h.createUser(`{"email":"dave@example.com"}`)
	dave := h.session()
	dave.login("dave@example.com", password)
	dave.mustOK(http.MethodPost, "/api/keys", `{"name":"笔记本"}`, nil)
	var upstreamKey, upstreamUser string
	if err := h.app.DB.QueryRow(`SELECT k.vm2api_key_id, u.vm2api_user_id FROM api_keys k JOIN users u ON u.id=k.user_id WHERE u.id=?`, user.ID).Scan(&upstreamKey, &upstreamUser); err != nil {
		t.Fatal(err)
	}
	id := strconv.FormatInt(user.ID, 10)
	h.mustOK(http.MethodPatch, "/api/admin/users/"+id, `{"status":"disabled"}`, nil)
	if _, active := h.fake.KeyState(upstreamKey); active {
		t.Fatal("disabling did not switch the upstream key off")
	}
	if status, _ := dave.call(http.MethodGet, "/api/me/overview", "", nil); status != http.StatusUnauthorized {
		t.Fatal("disabled user still signed in")
	}
	h.mustOK(http.MethodPatch, "/api/admin/users/"+id, `{"status":"active"}`, nil)
	if _, active := h.fake.KeyState(upstreamKey); !active {
		t.Fatal("enabling did not switch the upstream key back on")
	}
	if _, enabled := h.fake.UserEnabled(upstreamUser); !enabled {
		t.Fatal("upstream user still disabled")
	}

	// The admin can disable and delete one key (e.g. after a leak).
	var keyID int64
	if err := h.app.DB.QueryRow(`SELECT id FROM api_keys WHERE user_id=?`, user.ID).Scan(&keyID); err != nil {
		t.Fatal(err)
	}
	keyPath := "/api/admin/users/" + id + "/keys/" + strconv.FormatInt(keyID, 10)
	h.mustOK(http.MethodPatch, keyPath, `{"status":"disabled"}`, nil)
	if _, active := h.fake.KeyState(upstreamKey); active {
		t.Fatal("admin key disable did not reach upstream")
	}
	h.expectError(http.MethodPatch, "/api/admin/users/"+id+"/keys/99999", `{"status":"active"}`, http.StatusNotFound, "key_not_found")
	h.mustOK(http.MethodDelete, keyPath, "", nil)
	if exists, _ := h.fake.KeyState(upstreamKey); exists {
		t.Fatal("admin key delete did not reach upstream")
	}
}

func TestAdminDeletesOnlyUsersWithoutRecords(t *testing.T) {
	h := newHarness(t)
	h.login("admin@example.com", "admin-pass-123")

	// A user with a key but no purchases can be deleted, key and all.
	user, password := h.createUser(`{"email":"erin@example.com"}`)
	erin := h.session()
	erin.login("erin@example.com", password)
	erin.mustOK(http.MethodPost, "/api/keys", `{"name":"k"}`, nil)
	var upstreamKey, upstreamUser string
	if err := h.app.DB.QueryRow(`SELECT k.vm2api_key_id, u.vm2api_user_id FROM api_keys k JOIN users u ON u.id=k.user_id WHERE u.id=?`, user.ID).Scan(&upstreamKey, &upstreamUser); err != nil {
		t.Fatal(err)
	}
	h.mustOK(http.MethodDelete, "/api/admin/users/"+strconv.FormatInt(user.ID, 10), "", nil)
	if exists, _ := h.fake.KeyState(upstreamKey); exists {
		t.Fatal("upstream key survived the user")
	}
	if exists, enabled := h.fake.UserEnabled(upstreamUser); !exists || enabled {
		t.Fatalf("upstream user should be kept but disabled: exists=%v enabled=%v", exists, enabled)
	}
	if erin.tryLogin("erin@example.com", password) == 200 {
		t.Fatal("deleted user can still sign in")
	}
	if status, _ := erin.call(http.MethodGet, "/api/me/overview", "", nil); status != http.StatusUnauthorized {
		t.Fatal("deleted user's session survived")
	}
	h.expectError(http.MethodDelete, "/api/admin/users/"+strconv.FormatInt(user.ID, 10), "", http.StatusNotFound, "user_not_found")
	// The email is free again.
	h.createUser(`{"email":"erin@example.com"}`)

	// A user with a subscription is kept (disable instead).
	frank, _ := h.createUser(`{"email":"frank@example.com"}`)
	h.mustOK(http.MethodPost, "/api/admin/users/"+strconv.FormatInt(frank.ID, 10)+"/grant", `{"kind":"concurrency","quantity":1,"duration_days":30,"note":"测试"}`, nil)
	h.expectError(http.MethodDelete, "/api/admin/users/"+strconv.FormatInt(frank.ID, 10), "", http.StatusConflict, "user_has_records")

	// The last admin cannot be removed by another path either.
	other, _ := h.createUser(`{"email":"gina@example.com","role":"admin"}`)
	h.mustOK(http.MethodPatch, "/api/admin/users/"+strconv.FormatInt(other.ID, 10), `{"role":"user"}`, nil)
	h.mustOK(http.MethodDelete, "/api/admin/users/"+strconv.FormatInt(other.ID, 10), "", nil)
}
