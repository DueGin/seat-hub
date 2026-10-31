package auth

import (
	"context"
	"errors"
	"net/http"
	"time"

	"github.com/DueGin/vm2api-portal/internal/secret"
	"github.com/DueGin/vm2api-portal/internal/store"
)

const (
	CookieName  = "portal_session"
	SessionTTL  = 7 * 24 * time.Hour
	ctxUserKey  = ctxKey("user")
	ctxTokenKey = ctxKey("session_token_hash")
)

type ctxKey string

type Service struct {
	Box      *secret.Box
	Sessions *store.Sessions
	Users    *store.Users
	Secure   bool // set Secure on the cookie
}

type LoginResult struct {
	User  store.User
	Token string // raw cookie value
}

// Login verifies credentials and issues a session. It returns the user even on
// failure so callers can distinguish disabled accounts.
func (s *Service) Login(ctx context.Context, email, password, ip, ua string) (LoginResult, error) {
	u, err := s.Users.GetByEmail(ctx, email)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return LoginResult{}, ErrInvalidCredentials
		}
		return LoginResult{}, err
	}
	if !VerifyPassword(u.PasswordHash, password) {
		return LoginResult{}, ErrInvalidCredentials
	}
	if u.Status != "active" {
		return LoginResult{}, ErrUserDisabled
	}
	token, err := secret.RandomToken(32)
	if err != nil {
		return LoginResult{}, err
	}
	if err := s.Sessions.Create(ctx, s.Box.MAC(token), u.ID, time.Now().Add(SessionTTL), ip, ua); err != nil {
		return LoginResult{}, err
	}
	return LoginResult{User: u, Token: token}, nil
}

var (
	ErrInvalidCredentials = errors.New("invalid credentials")
	ErrUserDisabled       = errors.New("user disabled")
)

func (s *Service) SetCookie(w http.ResponseWriter, token string) {
	http.SetCookie(w, &http.Cookie{
		Name:     CookieName,
		Value:    token,
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		Secure:   s.Secure,
		MaxAge:   int(SessionTTL / time.Second),
	})
}

func (s *Service) ClearCookie(w http.ResponseWriter) {
	http.SetCookie(w, &http.Cookie{
		Name: CookieName, Value: "", Path: "/", HttpOnly: true,
		SameSite: http.SameSiteLaxMode, Secure: s.Secure, MaxAge: -1,
	})
}

// Middleware loads the session (when present) into the request context and
// leaves the request unauthenticated otherwise.
func (s *Service) Middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := r.Cookie(CookieName)
		if err != nil || c.Value == "" {
			next.ServeHTTP(w, r)
			return
		}
		hash := s.Box.MAC(c.Value)
		userID, err := s.Sessions.Lookup(r.Context(), hash)
		if err != nil {
			next.ServeHTTP(w, r)
			return
		}
		u, err := s.Users.GetByID(r.Context(), userID)
		if err != nil || u.Status != "active" {
			next.ServeHTTP(w, r)
			return
		}
		ctx := context.WithValue(r.Context(), ctxUserKey, &u)
		ctx = context.WithValue(ctx, ctxTokenKey, hash)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

func UserFrom(ctx context.Context) (*store.User, bool) {
	u, ok := ctx.Value(ctxUserKey).(*store.User)
	return u, ok && u != nil
}

func TokenHashFrom(ctx context.Context) string {
	h, _ := ctx.Value(ctxTokenKey).(string)
	return h
}

// RequireUser rejects unauthenticated requests with 401 unauthorized.
func RequireUser(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if _, ok := UserFrom(r.Context()); !ok {
			writeUnauthorized(w)
			return
		}
		next(w, r)
	}
}

// RequireAdmin rejects non-admin users with 403 forbidden.
func RequireAdmin(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		u, ok := UserFrom(r.Context())
		if !ok {
			writeUnauthorized(w)
			return
		}
		if u.Role != "admin" {
			writeJSONError(w, http.StatusForbidden, "forbidden", "需要管理员权限")
			return
		}
		next(w, r)
	}
}

// writeUnauthorized / writeJSONError are thin shims so this package does not
// depend on internal/api and create an import cycle. The JSON shape matches
// docs/PORTAL_API.md.
func writeUnauthorized(w http.ResponseWriter) {
	writeJSONError(w, http.StatusUnauthorized, "unauthorized", "请先登录")
}

func writeJSONError(w http.ResponseWriter, status int, code, msg string) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_, _ = w.Write([]byte(`{"ok":false,"error":{"code":"` + code + `","message":"` + msg + `"}}`))
}

// VerifyPassword checks a password for an existing user without issuing a session.
func (s *Service) VerifyPassword(userID int64, password string) bool {
	u, err := s.Users.GetByID(context.Background(), userID)
	if err != nil {
		return false
	}
	return VerifyPassword(u.PasswordHash, password)
}
