package api

import (
	"errors"
	"net/http"
	"strings"
	"sync"

	"github.com/DueGin/vm2api-portal/internal/auth"
	"github.com/DueGin/vm2api-portal/internal/store"
)

// firstUserMu serialises the "first user becomes admin" check and create.
var firstUserMu sync.Mutex

var (
	ErrEmailTaken         = Err(errors.New("email taken"), http.StatusConflict, "email_taken", "该邮箱已注册")
	ErrRegistrationClosed = Err(errors.New("registration closed"), http.StatusForbidden, "registration_closed", "当前未开放注册")
	ErrInvalidCredentials = Err(errors.New("invalid credentials"), http.StatusUnauthorized, "invalid_credentials", "邮箱或密码不正确")
	ErrUserDisabled       = Err(errors.New("user disabled"), http.StatusForbidden, "user_disabled", "账号已被停用")
	ErrTooManyAttempts    = Err(errors.New("too many attempts"), http.StatusTooManyRequests, "too_many_attempts", "尝试次数过多，请稍后再试")
	ErrInvalidPassword    = Err(errors.New("invalid password"), http.StatusUnprocessableEntity, "invalid_password", "旧密码不正确")
	ErrBadPassword        = Err(errors.New("bad password"), http.StatusUnprocessableEntity, "invalid_password", "密码长度须为 8–128")
)

func (s *Server) handleRegister(w http.ResponseWriter, r *http.Request) {
	site := s.Deps.Site(r)
	if !site.RegistrationOpen {
		writeErr(w, http.StatusForbidden, "registration_closed", "当前未开放注册")
		return
	}
	var body struct {
		Email    string `json:"email"`
		Password string `json:"password"`
	}
	if err := readJSON(r, &body); err != nil {
		s.fail(w, r, err)
		return
	}
	email := strings.ToLower(strings.TrimSpace(body.Email))
	if !strings.Contains(email, "@") || len(email) > 254 {
		writeErr(w, http.StatusUnprocessableEntity, "invalid_email", "请输入有效邮箱")
		return
	}
	hash, err := s.Deps.Auth.HashPassword(body.Password)
	if err != nil {
		writeErr(w, http.StatusUnprocessableEntity, "invalid_password", err.Error())
		return
	}
	// The first registered user becomes the admin when the deployment has no
	// users yet. Serialised because two concurrent first registrations would
	// otherwise both observe an empty table and both become admin.
	role := "user"
	firstUserMu.Lock()
	if n, err := s.Deps.Users.Count(r.Context()); err == nil && n == 0 {
		role = "admin"
	}
	u, err := s.Deps.Users.Create(r.Context(), email, hash, role)
	firstUserMu.Unlock()
	if err != nil {
		out := ErrEmailTaken
		if !strings.Contains(err.Error(), "email already registered") {
			out = err
		}
		s.fail(w, r, out)
		return
	}
	token, _, err := s.Deps.Auth.Login(r.Context(), email, body.Password, clientIP(r), r.UserAgent())
	if err != nil {
		s.fail(w, r, err)
		return
	}
	s.Deps.Auth.SetCookie(w, token)
	s.Deps.Audit.Write(r.Context(), u.ID, "user.register", u.Email, nil)
	writeCreated(w, map[string]any{"user": u})
}

func (s *Server) handleLogin(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Email    string `json:"email"`
		Password string `json:"password"`
	}
	if err := readJSON(r, &body); err != nil {
		s.fail(w, r, err)
		return
	}
	email := strings.ToLower(strings.TrimSpace(body.Email))
	if !s.Deps.Auth.Allowed(clientIP(r), email) {
		writeErr(w, http.StatusTooManyRequests, "too_many_attempts", "尝试次数过多，请稍后再试")
		return
	}
	token, u, err := s.Deps.Auth.Login(r.Context(), email, body.Password, clientIP(r), r.UserAgent())
	if err != nil {
		// auth.ErrUserDisabled is a different sentinel from this package's
		// ErrUserDisabled; the contract wants 403 user_disabled for a disabled
		// account, so match the service's error explicitly.
		mapped := ErrInvalidCredentials
		switch {
		case errors.Is(err, auth.ErrUserDisabled), errors.Is(err, ErrUserDisabled):
			mapped = ErrUserDisabled
		case errors.Is(err, store.ErrNotFound), errors.Is(err, auth.ErrInvalidCredentials),
			errors.Is(err, ErrInvalidCredentials):
			mapped = ErrInvalidCredentials
		}
		s.fail(w, r, mapped)
		return
	}
	s.Deps.Auth.SetCookie(w, token)
	writeOK(w, map[string]any{"user": u})
}

func (s *Server) handleLogout(w http.ResponseWriter, r *http.Request) {
	if h := s.Deps.Auth.TokenHashFrom(r.Context()); h != "" {
		_ = s.Deps.Auth.Logout(r.Context(), h)
	}
	s.Deps.Auth.ClearCookie(w)
	writeOK(w, map[string]any{})
}

func (s *Server) handleMe(w http.ResponseWriter, r *http.Request) {
	u, ok := s.currentUser(r)
	if !ok {
		writeErr(w, http.StatusUnauthorized, "unauthorized", "请先登录")
		return
	}
	writeOK(w, map[string]any{"user": u})
}

func (s *Server) handleChangePassword(w http.ResponseWriter, r *http.Request) {
	u, ok := s.currentUser(r)
	if !ok {
		writeErr(w, http.StatusUnauthorized, "unauthorized", "请先登录")
		return
	}
	var body struct {
		OldPassword string `json:"old_password"`
		NewPassword string `json:"new_password"`
	}
	if err := readJSON(r, &body); err != nil {
		s.fail(w, r, err)
		return
	}
	if !verifyPassword(s, u.ID, body.OldPassword) {
		writeErr(w, http.StatusUnprocessableEntity, "invalid_password", "旧密码不正确")
		return
	}
	hash, err := s.Deps.Auth.HashPassword(body.NewPassword)
	if err != nil {
		writeErr(w, http.StatusUnprocessableEntity, "invalid_password", err.Error())
		return
	}
	if err := s.Deps.Users.SetPassword(r.Context(), u.ID, hash); err != nil {
		s.fail(w, r, err)
		return
	}
	// PORTAL_API.md: changing the password revokes the user's other sessions.
	// The current one is kept so the caller is not logged out mid-flow (the
	// cookie stays valid). Rotating a password after a compromise is useless
	// if the attacker's session outlives it.
	keep := s.Deps.Auth.TokenHashFrom(r.Context())
	if revoker, ok := s.Deps.Auth.(SessionRevoker); ok {
		if err := revoker.RevokeOthers(r.Context(), u.ID, keep); err != nil {
			s.fail(w, r, err)
			return
		}
	} else {
		s.Log.Warn("auth service cannot revoke other sessions", "user_id", u.ID)
	}
	s.Deps.Audit.Write(r.Context(), u.ID, "user.password", u.Email, nil)
	writeOK(w, map[string]any{})
}

// verifyPassword checks the current password through the auth service.
func verifyPassword(s *Server, userID int64, password string) bool {
	return s.Deps.Auth.VerifyPassword(userID, password)
}
