// Package gateway proxies /v1/* to vm2api after swapping the portal API key for
// the user's vm2api key, and enforces the user's purchased concurrency.
//
// It never meters usage: billing and usage data live in vm2api.
package gateway

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"path"
	"strings"
	"sync"
	"time"
)

type Config struct {
	Upstream     *url.URL
	Resolver     Resolver
	Transport    http.RoundTripper
	MaxBodyBytes int64
	// QueueTimeout is how long a request waits for a free concurrency slot
	// before 429. Zero means fail fast.
	QueueTimeout time.Duration
}

type Gateway struct {
	cfg     Config
	proxy   *httputil.ReverseProxy
	limiter *limiter
}

func New(cfg Config) *Gateway {
	if cfg.Transport == nil {
		cfg.Transport = &http.Transport{
			Proxy:                 http.ProxyFromEnvironment,
			DialContext:           (&net.Dialer{Timeout: 15 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
			MaxIdleConns:          256,
			MaxIdleConnsPerHost:   256,
			IdleConnTimeout:       90 * time.Second,
			ExpectContinueTimeout: 1 * time.Second,
			// SSE through ReverseProxy must not be buffered; ResponseHeaderTimeout
			// is left at zero because the first upstream event can take minutes.
			ForceAttemptHTTP2: false,
		}
	}
	g := &Gateway{cfg: cfg, limiter: newLimiter()}
	g.proxy = &httputil.ReverseProxy{
		Rewrite:        g.rewrite,
		Transport:      cfg.Transport,
		FlushInterval:  -1, // flush every write: SSE must not be buffered
		ModifyResponse: stripHopByHopResponse,
		ErrorHandler:   g.onProxyError,
	}
	return g
}

func (g *Gateway) Inflight(userID int64) int { return g.limiter.inflight(userID) }

func (g *Gateway) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	// Normalize before routing: net/http hands us the raw path, so
	// "/v1/../api/panel/login" would otherwise pass isProxiedPath and vm2api
	// would clean it into its own admin surface on the other side.
	cleanPath, ok := canonicalPath(r.URL.Path, r.URL.RawPath)
	if !ok {
		writeErr(w, r, http.StatusBadRequest, "invalid_path", "请求路径不合法")
		return
	}
	if cleanPath != r.URL.Path {
		u := *r.URL
		u.Path = cleanPath
		u.RawPath = "" // force EscapedPath to re-derive from Path
		r = r.Clone(r.Context())
		r.URL = &u
	}
	if !isProxiedPath(r.URL.Path) {
		http.NotFound(w, r)
		return
	}
	key := presentedKey(r)
	if key == "" {
		writeErr(w, r, http.StatusUnauthorized, "missing_api_key", "缺少 API Key：请用 Authorization: Bearer 或 x-api-key 传入")
		return
	}
	p, err := g.cfg.Resolver.Resolve(r.Context(), key)
	if err != nil {
		writeErr(w, r, http.StatusBadGateway, "resolver_error", "鉴权服务暂时不可用，请稍后重试")
		return
	}
	if p == nil {
		writeErr(w, r, http.StatusUnauthorized, "invalid_api_key", "API Key 无效或已删除")
		return
	}
	if !p.Active {
		msg := "该用户已被停用"
		switch p.Reason {
		case "no_active_accounts":
			msg = "你还没有可用的账号：请先购买托管席位并添加账号，或租用账号"
		case "key_disabled":
			msg = "该 API Key 已停用"
		case "key_no_accounts":
			msg = "该密钥可用的账号都已失效，请在「API 密钥」里重新选择账号"
		}
		writeErr(w, r, http.StatusForbidden, p.Reason, msg)
		return
	}

	release, ok := g.limiter.acquire(r.Context(), p.UserID, p.Concurrency, g.cfg.QueueTimeout)
	if !ok {
		if errors.Is(r.Context().Err(), context.Canceled) {
			return
		}
		if p.Concurrency <= 0 {
			// Zero slots is a hard wall, not "unlimited": wait for a purchase.
			writeErr(w, r, http.StatusTooManyRequests, "concurrency_limit_exceeded",
				"当前没有可用的并发额度（0 路）：请先购买并发套餐后再调用")
			return
		}
		w.Header().Set("Retry-After", "1")
		writeErr(w, r, http.StatusTooManyRequests, "concurrency_limit_exceeded",
			"并发已用满，请提高并发额度或稍后重试")
		return
	}
	defer release()
	if g.cfg.Resolver != nil {
		g.cfg.Resolver.Touch(p.KeyID)
	}

	if g.cfg.MaxBodyBytes > 0 && r.Body != nil {
		r.Body = http.MaxBytesReader(w, r.Body, g.cfg.MaxBodyBytes)
	}
	r = r.WithContext(context.WithValue(r.Context(), principalKey, p))
	g.proxy.ServeHTTP(w, r)
}

type ctxKey string

const principalKey ctxKey = "principal"

func principalFrom(ctx context.Context) *Principal {
	p, _ := ctx.Value(principalKey).(*Principal)
	return p
}

// rewrite strips everything that could leak the client's identity or reach
// vm2api internals, then installs the user's upstream key.
func (g *Gateway) rewrite(pr *httputil.ProxyRequest) {
	pr.SetURL(g.cfg.Upstream)
	pr.Out.URL.Path = pr.In.URL.Path
	pr.Out.URL.RawPath = "" // ServeHTTP already canonicalized Path
	pr.Out.URL.RawQuery = pr.In.URL.RawQuery
	pr.Out.Host = g.cfg.Upstream.Host

	for _, h := range []string{
		"Authorization", "X-Api-Key", "Cookie", "Proxy-Authorization",
		"X-Forwarded-For",
		"X-Forwarded-Host", "X-Forwarded-Proto", "X-Real-Ip",
	} {
		pr.Out.Header.Del(h)
	}
	for h := range pr.Out.Header {
		if strings.HasPrefix(strings.ToLower(h), "x-kin-") {
			delete(pr.Out.Header, h)
		}
	}
	if p := principalFrom(pr.In.Context()); p != nil {
		pr.Out.Header.Set("Authorization", "Bearer "+p.UpstreamSecret)
	}
	// Preserve the real client IP for vm2api's logs without letting the caller
	// choose it.
	if ip := clientIP(pr.In); ip != "" {
		pr.Out.Header.Set("X-Forwarded-For", ip)
	}
	pr.SetXForwarded()
	if ip := clientIP(pr.In); ip != "" {
		// SetXForwarded would have appended the address we just set; pin ours.
		pr.Out.Header.Set("X-Forwarded-For", ip)
	}
}

func stripHopByHopResponse(resp *http.Response) error {
	// A 101 must reach handleUpgradeResponse intact: it re-reads the Upgrade
	// type from the Connection header, so stripping Connection turns every
	// WebSocket upgrade into "backend tried to switch protocol \"\"".
	if resp.StatusCode == http.StatusSwitchingProtocols {
		return nil
	}
	for _, h := range []string{"Connection", "Keep-Alive", "Proxy-Authenticate", "Proxy-Authorization",
		"Te", "Trailer", "Transfer-Encoding", "Upgrade"} {
		resp.Header.Del(h)
	}
	return nil
}

func (g *Gateway) onProxyError(w http.ResponseWriter, r *http.Request, err error) {
	var maxErr *http.MaxBytesError
	if errors.As(err, &maxErr) {
		writeErr(w, r, http.StatusRequestEntityTooLarge, "body_too_large", "请求体超过上限")
		return
	}
	if errors.Is(r.Context().Err(), context.Canceled) {
		return // client went away; nothing to write
	}
	writeErr(w, r, http.StatusBadGateway, "upstream_unavailable", "上游服务暂时不可用")
}

// isProxiedPath limits the gateway to the vm2api protocol surface.
func isProxiedPath(p string) bool {
	return p == "/v1" || strings.HasPrefix(p, "/v1/")
}

// canonicalPath cleans a request path and rejects dot segments outright.
//
// Cleaning alone is not enough: path.Clean("/v1/../api/panel/login") is
// "/api/panel/login", which is outside the gateway surface, but a path such as
// "/v1/./messages" would silently normalize into an allowed route. Rejecting
// any "." or ".." segment keeps one meaning for both Path and RawPath and
// makes an encoded attempt (%2e%2e) fail the same way as the plain form.
func canonicalPath(p, rawPath string) (string, bool) {
	if hasDotSegment(p) || hasDotSegment(rawPath) {
		return "", false
	}
	if p == "" {
		return "/", true
	}
	trailingSlash := strings.HasSuffix(p, "/") && p != "/"
	clean := path.Clean(p)
	if !strings.HasPrefix(clean, "/") {
		clean = "/" + clean
	}
	if clean == "/" {
		return clean, true
	}
	if trailingSlash {
		clean += "/"
	}
	return clean, true
}

// hasDotSegment reports whether any "/"-separated segment is "." or "..".
// It also inspects percent-decoded segments so "/v1/%2e%2e/x" is caught even
// when the caller only guards RawPath.
func hasDotSegment(p string) bool {
	if p == "" {
		return false
	}
	for _, seg := range strings.Split(p, "/") {
		if seg == "." || seg == ".." {
			return true
		}
		if unescaped, err := url.PathUnescape(seg); err == nil && (unescaped == "." || unescaped == "..") {
			return true
		}
	}
	return false
}

func presentedKey(r *http.Request) string {
	if h := r.Header.Get("Authorization"); h != "" {
		if after, ok := strings.CutPrefix(h, "Bearer "); ok {
			return strings.TrimSpace(after)
		}
		if after, ok := strings.CutPrefix(h, "bearer "); ok {
			return strings.TrimSpace(after)
		}
	}
	return strings.TrimSpace(r.Header.Get("x-api-key"))
}

func clientIP(r *http.Request) string {
	if host, _, err := net.SplitHostPort(r.RemoteAddr); err == nil {
		return host
	}
	return r.RemoteAddr
}

// isOpenAIShaped reports whether the path takes OpenAI error envelopes.
func isOpenAIShaped(p string) bool {
	switch {
	case p == "/v1/chat/completions", p == "/v1/completions", p == "/v1/responses":
		return true
	}
	return false
}

// writeErr emits the Anthropic or OpenAI error envelope, matching vm2api so
// existing SDKs surface the message the same way.
func writeErr(w http.ResponseWriter, r *http.Request, status int, code, msg string) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	if isOpenAIShaped(r.URL.Path) {
		typ := "invalid_request_error"
		switch status {
		case http.StatusUnauthorized:
			typ = "authentication_error"
		case http.StatusForbidden:
			typ = "permission_error"
		case http.StatusTooManyRequests:
			typ = "rate_limit_error"
		case http.StatusBadGateway:
			typ = "api_error"
		}
		_, _ = w.Write([]byte(`{"error":{"message":` + jsonString(msg) + `,"type":"` + typ + `","code":"` + code + `"}}`))
		return
	}
	typ := "invalid_request_error"
	switch status {
	case http.StatusUnauthorized:
		typ = "authentication_error"
	case http.StatusForbidden:
		typ = "permission_error"
	case http.StatusTooManyRequests:
		typ = "rate_limit_error"
	case http.StatusBadGateway:
		typ = "api_error"
	}
	_, _ = w.Write([]byte(`{"type":"error","error":{"type":"` + typ + `","message":` + jsonString(msg) + `}}`))
}

func jsonString(s string) string {
	b := strings.Builder{}
	b.WriteByte('"')
	for _, r := range s {
		switch r {
		case '"':
			b.WriteString(`\"`)
		case '\\':
			b.WriteString(`\\`)
		case '\n':
			b.WriteString(`\n`)
		default:
			b.WriteRune(r)
		}
	}
	b.WriteByte('"')
	return b.String()
}

// --- per-user concurrency ---

type limiter struct {
	mu      sync.Mutex
	active  map[int64]int
	waiters map[int64][]chan struct{}
}

func newLimiter() *limiter {
	return &limiter{active: map[int64]int{}, waiters: map[int64][]chan struct{}{}}
}

func (l *limiter) inflight(userID int64) int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.active[userID]
}

// acquire reserves one slot for the user. limit <= 0 means the user has no
// concurrency at all (not "unlimited"), which the caller turns into a 429.
//
// Every successful acquire returns a release func, and every path that can
// race with a hand-over still returns one: release() transfers the slot to the
// waiter by closing its channel *and* incrementing active, so a waiter that
// loses the select race to ctx.Done must hand the slot back instead of
// dropping it.
func (l *limiter) acquire(ctx context.Context, userID int64, limit int, queueTimeout time.Duration) (func(), bool) {
	if limit <= 0 {
		return nil, false
	}
	l.mu.Lock()
	if l.active[userID] < limit {
		l.active[userID]++
		l.mu.Unlock()
		return func() { l.release(userID) }, true
	}
	if queueTimeout <= 0 {
		l.mu.Unlock()
		return nil, false
	}
	ch := make(chan struct{})
	l.waiters[userID] = append(l.waiters[userID], ch)
	l.mu.Unlock()

	timer := time.NewTimer(queueTimeout)
	defer timer.Stop()
	select {
	case <-ch:
		return func() { l.release(userID) }, true
	case <-timer.C:
		if l.dequeue(userID, ch) {
			return nil, false
		}
		// release() already took us out of the queue and handed the slot
		// over; take ownership of it.
		return func() { l.release(userID) }, true
	case <-ctx.Done():
		if l.dequeue(userID, ch) {
			return nil, false
		}
		// Same race as above: the slot was already transferred to us.
		return func() { l.release(userID) }, true
	}
}

// dequeue removes ch from the waiter queue and reports whether it was still
// queued. false means release() already claimed it.
func (l *limiter) dequeue(userID int64, ch chan struct{}) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	ws := l.waiters[userID]
	for i, w := range ws {
		if w == ch {
			l.waiters[userID] = append(ws[:i], ws[i+1:]...)
			if len(l.waiters[userID]) == 0 {
				delete(l.waiters, userID)
			}
			return true
		}
	}
	if len(ws) == 0 {
		delete(l.waiters, userID)
	}
	return false
}

func (l *limiter) release(userID int64) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if ws := l.waiters[userID]; len(ws) > 0 {
		w := ws[0]
		if len(ws) == 1 {
			delete(l.waiters, userID)
		} else {
			l.waiters[userID] = ws[1:]
		}
		// Hand the slot straight to the waiter: active is neither decremented
		// here nor incremented there, the count simply transfers.
		close(w)
		return
	}
	if l.active[userID] > 1 {
		l.active[userID]--
	} else {
		delete(l.active, userID)
	}
}
