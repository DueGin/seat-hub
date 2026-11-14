package api

import (
	"crypto/rand"
	"math/big"
	"net/http"
	"strings"
	"unicode/utf8"
)

// Admin user management. Every change is audited; the acting admin can never
// lock themselves out (disable, demote, reset, sign out or delete themselves),
// and the last active admin cannot be removed.

const (
	userNoteMax          = 500
	generatedPasswordLen = 16
	// No 0/O/1/l/I: generated passwords are read aloud and retyped.
	passwordAlphabet = "abcdefghijkmnopqrstuvwxyzABCDEFGHJKLMNPQRSTUVWXYZ23456789"
)

func generatePassword() (string, error) {
	out := make([]byte, generatedPasswordLen)
	limit := big.NewInt(int64(len(passwordAlphabet)))
	for index := range out {
		n, err := rand.Int(rand.Reader, limit)
		if err != nil {
			return "", err
		}
		out[index] = passwordAlphabet[n.Int64()]
	}
	return string(out), nil
}

func validEmail(email string) bool {
	at := strings.IndexByte(email, '@')
	return at > 0 && at < len(email)-1 && len(email) <= 254 && !strings.ContainsAny(email, " \t\r\n")
}

func normalizeEmail(email string) string { return strings.ToLower(strings.TrimSpace(email)) }

// choosePassword returns the password to set and whether it was generated.
func (s *Server) choosePassword(given string) (password, hash string, generated bool, err error) {
	password = given
	if password == "" {
		if password, err = generatePassword(); err != nil {
			return "", "", false, err
		}
		generated = true
	}
	hash, err = s.Deps.Auth.HashPassword(password)
	if err != nil {
		return "", "", false, badRequest{code: "invalid_password", msg: err.Error()}
	}
	return password, hash, generated, nil
}

func (s *Server) handleAdminUsers(w http.ResponseWriter, r *http.Request) {
	page, size := adminPageParams(r)
	query := r.URL.Query()
	filter := AdminUserFilter{Q: strings.TrimSpace(query.Get("q")), Role: query.Get("role"), Status: query.Get("status")}
	if filter.Role != "" && filter.Role != "user" && filter.Role != "admin" {
		writeErr(w, http.StatusUnprocessableEntity, "invalid_role", "role 只能是 user 或 admin")
		return
	}
	if filter.Status != "" && filter.Status != "active" && filter.Status != "disabled" {
		writeErr(w, http.StatusUnprocessableEntity, "invalid_status", "status 只能是 active 或 disabled")
		return
	}
	items, total, err := s.Deps.Users.AdminList(r.Context(), filter, page, size)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	writeOK(w, adminPageEnvelope(items, total, page, size))
}

func (s *Server) handleAdminCreateUser(w http.ResponseWriter, r *http.Request) {
	admin, _ := s.currentUser(r)
	var body struct {
		Email    string `json:"email"`
		Password string `json:"password"`
		Role     string `json:"role"`
		Note     string `json:"note"`
	}
	if err := readJSON(r, &body); err != nil {
		s.fail(w, r, err)
		return
	}
	email := normalizeEmail(body.Email)
	if !validEmail(email) {
		writeErr(w, http.StatusUnprocessableEntity, "invalid_email", "请输入有效邮箱")
		return
	}
	if body.Role == "" {
		body.Role = "user"
	}
	if body.Role != "user" && body.Role != "admin" {
		writeErr(w, http.StatusUnprocessableEntity, "invalid_role", "role 只能是 user 或 admin")
		return
	}
	note := strings.TrimSpace(body.Note)
	if utf8.RuneCountInString(note) > userNoteMax {
		writeErr(w, http.StatusUnprocessableEntity, "invalid_note", "备注最多 500 个字")
		return
	}
	password, hash, generated, err := s.choosePassword(body.Password)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	created, err := s.Deps.Users.Create(r.Context(), email, hash, body.Role)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	if note != "" {
		if err := s.Deps.Users.SetNote(r.Context(), created.ID, note); err != nil {
			s.fail(w, r, err)
			return
		}
	}
	s.Deps.Audit.Write(r.Context(), admin.ID, "user.create", created.Email, map[string]any{"id": created.ID, "role": body.Role, "generated_password": generated})
	user, err := s.Deps.Users.AdminDetail(r.Context(), created.ID)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	out := map[string]any{"user": user}
	if generated {
		out["password"] = password // shown once; never stored in plain text
	}
	writeJSON(w, http.StatusCreated, map[string]any{"ok": true, "data": out})
}

func (s *Server) handleAdminPatchUser(w http.ResponseWriter, r *http.Request) {
	admin, _ := s.currentUser(r)
	id, ok := adminRequestID(w, r)
	if !ok {
		return
	}
	var body struct {
		Email  *string `json:"email"`
		Status *string `json:"status"`
		Role   *string `json:"role"`
		Note   *string `json:"note"`
	}
	if err := readJSON(r, &body); err != nil {
		s.fail(w, r, err)
		return
	}
	if body.Email == nil && body.Status == nil && body.Role == nil && body.Note == nil {
		writeErr(w, http.StatusUnprocessableEntity, "invalid_body", "请至少修改一项")
		return
	}
	if body.Status != nil && *body.Status != "active" && *body.Status != "disabled" {
		writeErr(w, http.StatusUnprocessableEntity, "invalid_status", "status 只能是 active 或 disabled")
		return
	}
	if body.Role != nil && *body.Role != "user" && *body.Role != "admin" {
		writeErr(w, http.StatusUnprocessableEntity, "invalid_role", "role 只能是 user 或 admin")
		return
	}
	var email, note string
	if body.Email != nil {
		email = normalizeEmail(*body.Email)
		if !validEmail(email) {
			writeErr(w, http.StatusUnprocessableEntity, "invalid_email", "请输入有效邮箱")
			return
		}
	}
	if body.Note != nil {
		note = strings.TrimSpace(*body.Note)
		if utf8.RuneCountInString(note) > userNoteMax {
			writeErr(w, http.StatusUnprocessableEntity, "invalid_note", "备注最多 500 个字")
			return
		}
	}
	adminUserPatchMu.Lock()
	defer adminUserPatchMu.Unlock()
	user, err := s.Deps.Users.ByID(r.Context(), id)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	if id == admin.ID && (body.Status != nil && *body.Status != user.Status || body.Role != nil && *body.Role != user.Role) {
		writeErr(w, http.StatusConflict, "self_lockout", "不能停用自己或取消自己的管理员身份，请让其他管理员操作")
		return
	}
	removingAdmin := body.Status != nil && *body.Status == "disabled" || body.Role != nil && *body.Role == "user"
	if user.Role == "admin" && user.Status == "active" && removingAdmin {
		if err := s.ensureNotLastAdmin(r); err != nil {
			s.fail(w, r, err)
			return
		}
	}
	changes := map[string]any{}
	if body.Email != nil && email != user.Email {
		if err := s.Deps.Users.SetEmail(r.Context(), id, email); err != nil {
			s.fail(w, r, err)
			return
		}
		changes["email"] = map[string]string{"from": user.Email, "to": email}
	}
	if body.Note != nil {
		if err := s.Deps.Users.SetNote(r.Context(), id, note); err != nil {
			s.fail(w, r, err)
			return
		}
		changes["note"] = true
	}
	if body.Role != nil && *body.Role != user.Role {
		if err := s.Deps.Users.SetRole(r.Context(), id, *body.Role); err != nil {
			s.fail(w, r, err)
			return
		}
		changes["role"] = *body.Role
	}
	if body.Status != nil && *body.Status != user.Status {
		// Last: it also syncs vm2api, the step most likely to fail. The local
		// changes above stand; the admin can retry the status change.
		if err := s.Deps.Users.SetStatus(r.Context(), id, *body.Status); err != nil {
			s.Deps.Audit.Write(r.Context(), admin.ID, "user.update", user.Email, changes)
			s.fail(w, r, err)
			return
		}
		changes["status"] = *body.Status
	}
	if len(changes) > 0 {
		s.Deps.Audit.Write(r.Context(), admin.ID, "user.update", user.Email, changes)
	}
	updated, err := s.Deps.Users.AdminDetail(r.Context(), id)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	writeOK(w, map[string]any{"user": updated})
}

func (s *Server) ensureNotLastAdmin(r *http.Request) error {
	admins, err := s.Deps.Users.CountAdmins(r.Context())
	if err != nil {
		return err
	}
	if admins <= 1 {
		return &UserError{Status: http.StatusConflict, Code: "last_admin", Message: "不能停用、降级或删除最后一个管理员"}
	}
	return nil
}

func (s *Server) handleAdminResetPassword(w http.ResponseWriter, r *http.Request) {
	admin, _ := s.currentUser(r)
	id, ok := adminRequestID(w, r)
	if !ok {
		return
	}
	if id == admin.ID {
		writeErr(w, http.StatusConflict, "self_lockout", "修改自己的密码请在「账户安全」页操作")
		return
	}
	var body struct {
		Password string `json:"password"`
	}
	if err := readJSON(r, &body); err != nil {
		s.fail(w, r, err)
		return
	}
	user, err := s.Deps.Users.ByID(r.Context(), id)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	password, hash, generated, err := s.choosePassword(body.Password)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	if err := s.Deps.Users.SetPassword(r.Context(), id, hash); err != nil {
		s.fail(w, r, err)
		return
	}
	// A reset is usually a response to a leaked password: end every session.
	if err := s.Deps.Users.RevokeSessions(r.Context(), id); err != nil {
		s.fail(w, r, err)
		return
	}
	s.Deps.Audit.Write(r.Context(), admin.ID, "user.reset_password", user.Email, map[string]any{"id": id, "generated": generated})
	out := map[string]any{}
	if generated {
		out["password"] = password
	}
	writeOK(w, out)
}

func (s *Server) handleAdminRevokeSessions(w http.ResponseWriter, r *http.Request) {
	admin, _ := s.currentUser(r)
	id, ok := adminRequestID(w, r)
	if !ok {
		return
	}
	if id == admin.ID {
		writeErr(w, http.StatusConflict, "self_lockout", "不能在这里让自己下线，请使用「退出登录」")
		return
	}
	user, err := s.Deps.Users.ByID(r.Context(), id)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	if err := s.Deps.Users.RevokeSessions(r.Context(), id); err != nil {
		s.fail(w, r, err)
		return
	}
	s.Deps.Audit.Write(r.Context(), admin.ID, "user.revoke_sessions", user.Email, map[string]any{"id": id})
	writeOK(w, map[string]any{})
}

func (s *Server) handleAdminDeleteUser(w http.ResponseWriter, r *http.Request) {
	admin, _ := s.currentUser(r)
	id, ok := adminRequestID(w, r)
	if !ok {
		return
	}
	if id == admin.ID {
		writeErr(w, http.StatusConflict, "self_lockout", "不能删除自己的账号")
		return
	}
	adminUserPatchMu.Lock()
	defer adminUserPatchMu.Unlock()
	user, err := s.Deps.Users.ByID(r.Context(), id)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	if user.Role == "admin" && user.Status == "active" {
		if err := s.ensureNotLastAdmin(r); err != nil {
			s.fail(w, r, err)
			return
		}
	}
	if err := s.Deps.Users.Delete(r.Context(), id); err != nil {
		s.fail(w, r, err)
		return
	}
	s.Deps.Audit.Write(r.Context(), admin.ID, "user.delete", user.Email, map[string]any{"id": id, "role": user.Role})
	writeOK(w, map[string]any{})
}

// handleAdminUserKey disables, enables or deletes one of a user's API keys,
// e.g. after a leak.
func (s *Server) handleAdminUserKey(w http.ResponseWriter, r *http.Request) {
	admin, _ := s.currentUser(r)
	id, ok := adminRequestID(w, r)
	if !ok {
		return
	}
	keyID := parseInt64(r.PathValue("key_id"))
	if keyID <= 0 {
		writeErr(w, http.StatusUnprocessableEntity, "invalid_request", "密钥 ID 必须是正整数")
		return
	}
	user, err := s.Deps.Users.ByID(r.Context(), id)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	if r.Method == http.MethodDelete {
		if err := s.Deps.Keys.Delete(r.Context(), id, keyID); err != nil {
			s.fail(w, r, err)
			return
		}
		s.Deps.Audit.Write(r.Context(), admin.ID, "user.key_delete", user.Email, map[string]any{"key_id": keyID})
		writeOK(w, map[string]any{})
		return
	}
	var body struct {
		Status string `json:"status"`
	}
	if err := readJSON(r, &body); err != nil {
		s.fail(w, r, err)
		return
	}
	if body.Status != "active" && body.Status != "disabled" {
		writeErr(w, http.StatusUnprocessableEntity, "invalid_status", "status 只能是 active 或 disabled")
		return
	}
	key, err := s.Deps.Keys.Update(r.Context(), id, keyID, "", body.Status)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	s.Deps.Audit.Write(r.Context(), admin.ID, "user.key_update", user.Email, map[string]any{"key_id": keyID, "status": body.Status})
	writeOK(w, map[string]any{"key": key})
}
