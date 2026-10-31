// Package api serves /api/*: the portal's own HTTP surface for the SPA, admin
// console, and payment callbacks. The protocol gateway lives in internal/gateway.
//
// Handlers depend on the narrow interfaces in deps.go, implemented by
// internal/app adapters, so this package never imports catalog/order/provision.
package api

import (
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"
)

const (
	maxJSONBody = 1 << 20 // 1 MiB for portal JSON; the gateway has its own limit
)

type Server struct {
	Deps   Deps
	Log    *slog.Logger
	Secure bool
}

// Deps is everything the HTTP layer needs. Built once in internal/app.
type Deps struct {
	Users    UserStore
	Settings SettingsStore
	Audit    AuditLog
	Auth     AuthService

	Catalog  CatalogService
	Orders   OrderService
	Payments PaymentService
	Accounts AccountService
	Usage    UsageService
	Keys     KeyService
	Admin    AdminService

	Site func(r *http.Request) SiteConfig
	Dev  bool
}

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()

	// Public
	mux.HandleFunc("GET /api/public/config", s.handlePublicConfig)
	mux.HandleFunc("GET /api/public/products", s.handlePublicProducts)

	// Auth
	mux.HandleFunc("POST /api/auth/register", s.handleRegister)
	mux.HandleFunc("POST /api/auth/login", s.handleLogin)
	mux.HandleFunc("POST /api/auth/logout", s.handleLogout)
	mux.HandleFunc("GET /api/auth/me", s.handleMe)
	mux.HandleFunc("POST /api/auth/password", s.handleChangePassword)

	// User
	mux.HandleFunc("GET /api/me/overview", s.handleOverview)
	mux.HandleFunc("GET /api/me/subscriptions", s.handleSubscriptions)

	// Orders and payments
	mux.HandleFunc("POST /api/orders", s.handleCreateOrder)
	mux.HandleFunc("GET /api/orders", s.handleListOrders)
	mux.HandleFunc("GET /api/orders/{order_no}", s.handleGetOrder)
	mux.HandleFunc("POST /api/orders/{order_no}/cancel", s.handleCancelOrder)
	mux.HandleFunc("POST /api/orders/{order_no}/mock-pay", s.handleMockPay)
	mux.HandleFunc("/api/payments/{provider}/notify", s.handlePaymentNotify)
	mux.HandleFunc("GET /api/payments/{provider}/return", s.handlePaymentReturn)

	// Accounts
	mux.HandleFunc("GET /api/accounts", s.handleListAccounts)
	mux.HandleFunc("GET /api/accounts/{id}", s.handleGetAccount)
	mux.HandleFunc("PATCH /api/accounts/{id}", s.handlePatchAccount)

	// Hosted onboarding (OAuth authorization link only)
	mux.HandleFunc("POST /api/hosted", s.handleCreateHosted)
	mux.HandleFunc("GET /api/hosted/{id}/onboarding", s.handleOnboarding)
	mux.HandleFunc("POST /api/hosted/{id}/auth-url", s.handleRegenerateAuthURL)
	mux.HandleFunc("POST /api/hosted/{id}/auth-code", s.handleSubmitAuthCode)
	mux.HandleFunc("POST /api/hosted/{id}/retry", s.handleRetryHosted)
	mux.HandleFunc("DELETE /api/hosted/{id}", s.handleDeleteHosted)

	// API keys
	mux.HandleFunc("GET /api/keys", s.handleListKeys)
	mux.HandleFunc("POST /api/keys", s.handleCreateKey)
	mux.HandleFunc("PATCH /api/keys/{id}", s.handlePatchKey)
	mux.HandleFunc("DELETE /api/keys/{id}", s.handleDeleteKey)
	mux.HandleFunc("GET /api/keys/{id}/stats", s.handleKeyStats)

	// Usage
	mux.HandleFunc("GET /api/usage/summary", s.handleUsageSummary)
	mux.HandleFunc("GET /api/usage/logs", s.handleUsageLogs)

	// Admin
	mux.HandleFunc("GET /api/admin/dashboard", s.admin(s.handleAdminDashboard))
	mux.HandleFunc("GET /api/admin/products", s.admin(s.handleAdminListProducts))
	mux.HandleFunc("POST /api/admin/products", s.admin(s.handleAdminCreateProduct))
	mux.HandleFunc("PATCH /api/admin/products/{id}", s.admin(s.handleAdminPatchProduct))
	mux.HandleFunc("DELETE /api/admin/products/{id}", s.admin(s.handleAdminDeleteProduct))
	mux.HandleFunc("GET /api/admin/orders", s.admin(s.handleAdminOrders))
	mux.HandleFunc("POST /api/admin/orders/{order_no}/mark-paid", s.admin(s.handleAdminMarkPaid))
	mux.HandleFunc("POST /api/admin/orders/{order_no}/cancel", s.admin(s.handleAdminCancelOrder))
	mux.HandleFunc("POST /api/admin/orders/{order_no}/refulfill", s.admin(s.handleAdminRefulfill))
	mux.HandleFunc("GET /api/admin/users", s.admin(s.handleAdminUsers))
	mux.HandleFunc("GET /api/admin/users/{id}", s.admin(s.handleAdminUserDetail))
	mux.HandleFunc("PATCH /api/admin/users/{id}", s.admin(s.handleAdminPatchUser))
	mux.HandleFunc("POST /api/admin/users/{id}/grant", s.admin(s.handleAdminGrant))
	mux.HandleFunc("PATCH /api/admin/subscriptions/{id}", s.admin(s.handleAdminPatchSubscription))
	mux.HandleFunc("GET /api/admin/rental-pool", s.admin(s.handleAdminRentalPool))
	mux.HandleFunc("GET /api/admin/rental-pool/candidates", s.admin(s.handleAdminRentalCandidates))
	mux.HandleFunc("POST /api/admin/rental-pool", s.admin(s.handleAdminRentalAdd))
	mux.HandleFunc("PATCH /api/admin/rental-pool/{vm_id}", s.admin(s.handleAdminRentalPatch))
	mux.HandleFunc("DELETE /api/admin/rental-pool/{vm_id}", s.admin(s.handleAdminRentalRemove))
	mux.HandleFunc("GET /api/admin/accounts", s.admin(s.handleAdminAccounts))
	mux.HandleFunc("POST /api/admin/accounts/{id}/retry", s.admin(s.handleAdminRetryAccount))
	mux.HandleFunc("POST /api/admin/accounts/{id}/release", s.admin(s.handleAdminReleaseAccount))
	mux.HandleFunc("GET /api/admin/jobs", s.admin(s.handleAdminJobs))
	mux.HandleFunc("POST /api/admin/jobs/{id}/retry", s.admin(s.handleAdminRetryJob))
	mux.HandleFunc("GET /api/admin/settings", s.admin(s.handleAdminGetSettings))
	mux.HandleFunc("PUT /api/admin/settings", s.admin(s.handleAdminPutSettings))

	return s.middleware(mux)
}

// middleware enforces the JSON content-type on mutating portal calls (a cheap
// CSRF defence next to SameSite=Lax), logs, and recovers panics.
func (s *Server) middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		sw := &statusWriter{ResponseWriter: w, status: 200}
		defer func() {
			if rec := recover(); rec != nil {
				s.Log.Error("panic", "path", r.URL.Path, "panic", rec)
				writeErr(sw, http.StatusInternalServerError, "internal_error", "服务器内部错误")
			}
			s.Log.Info("api", "method", r.Method, "path", r.URL.Path, "status", sw.status,
				"ms", time.Since(start).Milliseconds(), "ip", clientIP(r))
		}()
		if needsJSON(r.Method) && !isJSONContentType(r.Header.Get("Content-Type")) &&
			!strings.Contains(r.URL.Path, "/notify") && !strings.Contains(r.URL.Path, "/return") {
			writeErr(sw, http.StatusUnsupportedMediaType, "unsupported_media_type", "请求需为 application/json")
			return
		}
		next.ServeHTTP(sw, r)
	})
}

// isJSONContentType accepts application/json (optionally with parameters).
// Anything else, including text/json and application/jsonish, is rejected: the
// check doubles as the CSRF guard, so it must not be spoofable by a browser form.
func isJSONContentType(value string) bool {
	mediaType, _, _ := strings.Cut(strings.ToLower(strings.TrimSpace(value)), ";")
	return strings.TrimSpace(mediaType) == "application/json"
}

func needsJSON(method string) bool {
	switch method {
	case http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete:
		return true
	}
	return false
}

type statusWriter struct {
	http.ResponseWriter
	status int
}

func (w *statusWriter) WriteHeader(code int) {
	w.status = code
	w.ResponseWriter.WriteHeader(code)
}

func (w *statusWriter) Flush() {
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// --- envelopes ---

// ErrorDTO is the public error shape, reused both inside the failure envelope
// and in data payloads such as onboarding results.
type ErrorDTO struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

type apiError = ErrorDTO

func writeOK(w http.ResponseWriter, data any) {
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "data": data})
}

func writeCreated(w http.ResponseWriter, data any) {
	writeJSON(w, http.StatusCreated, map[string]any{"ok": true, "data": data})
}

func writeErr(w http.ResponseWriter, status int, code, message string) {
	writeJSON(w, status, map[string]any{"ok": false, "error": apiError{Code: code, Message: message}})
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(v)
}

// --- request helpers ---

var errBadRequest = errors.New("bad request")

type badRequest struct {
	code string
	msg  string
}

func (e badRequest) Error() string { return e.msg }

func bad(code, msg string) error { return badRequest{code: code, msg: msg} }

func readJSON(r *http.Request, into any) error {
	if r.Body == nil {
		return nil
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, maxJSONBody))
	if err != nil {
		return bad("invalid_body", "读取请求体失败")
	}
	if len(strings.TrimSpace(string(body))) == 0 {
		return nil
	}
	dec := json.NewDecoder(strings.NewReader(string(body)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(into); err != nil {
		return bad("invalid_body", "请求体不是合法的 JSON")
	}
	return nil
}

// fail maps an error to a response. Known sentinel errors carry a code/message;
// anything else becomes a 500 and is logged by the caller.
func (s *Server) fail(w http.ResponseWriter, r *http.Request, err error) {
	var br badRequest
	if errors.As(err, &br) {
		writeErr(w, http.StatusUnprocessableEntity, br.code, br.msg)
		return
	}
	if mapped := AsUserError(err); mapped != nil {
		writeErr(w, mapped.Status, mapped.Code, mapped.Message)
		return
	}
	s.Log.Error("handler error", "path", r.URL.Path, "err", err)
	writeErr(w, http.StatusInternalServerError, "internal_error", "服务器内部错误")
}

// admin guards a handler behind role=admin.
func (s *Server) admin(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		u, ok := s.currentUser(r)
		if !ok {
			writeErr(w, http.StatusUnauthorized, "unauthorized", "请先登录")
			return
		}
		if u.Role != "admin" {
			writeErr(w, http.StatusForbidden, "forbidden", "需要管理员权限")
			return
		}
		h(w, r)
	}
}

// clientIP returns the caller's address. Behind nginx the TCP peer is the
// proxy (usually 127.0.0.1), and trusting it blindly would collapse every
// caller into one rate-limit bucket. X-Forwarded-For is only honoured when the
// direct peer is a loopback or private address, and the last hop is used: that
// is the value our own proxy appended or wrote with $remote_addr, so a client
// cannot spoof it by sending its own header.
func clientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	peer := net.ParseIP(host)
	if peer == nil || !(peer.IsLoopback() || peer.IsPrivate()) {
		return host
	}
	hops := r.Header.Values("X-Forwarded-For")
	if len(hops) == 0 {
		return host
	}
	var last string
	for _, part := range strings.Split(strings.Join(hops, ","), ",") {
		if candidate := net.ParseIP(strings.TrimSpace(part)); candidate != nil {
			last = candidate.String()
		}
	}
	if last == "" {
		return host
	}
	return last
}

func queryInt(r *http.Request, key string, def int) int {
	v := r.URL.Query().Get(key)
	if v == "" {
		return def
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return def
	}
	return n
}

func pathID(r *http.Request) int64 {
	n, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	return n
}
