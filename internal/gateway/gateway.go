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
		}
		writeErr(w, r, http.StatusForbidden, p.Reason, msg)
		return
	}

	release, ok := g.limiter.acquire(r.Context(), p.UserID, p.Concurrency, g.cfg.QueueTimeout)
	if !ok {
		if errors.Is(r.Context().Err(), context.Canceled) {
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
	pr.Out.URL.RawQuery = pr.In.URL.RawQuery
	pr.Out.Host = g.cfg.Upstream.Host

	for _, h := range []string{
		"Authorization", "X-Api-Key", "Cookie", "Proxy-Authorization",
		"X-Kin-Debug", "X-Kin-Log", "X-Kin-Pin-Vm", "X-Forwarded-For",
		"X-Forwarded-Host", "X-Forwarded-Proto", "X-Real-Ip",
	} {
		pr.Out.Header.Del(h)
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
	for _, h := range []string{"Connection", "Keep-Alive", "Proxy-Authenticate", "Proxy-Authorization",
		"Te", "Trailer", "Transfer-Encoding", "Upgrade"} {
		// Keep Upgrade for 101 responses so the hijack path still works.
		if resp.StatusCode == http.StatusSwitchingProtocols && h == "Upgrade" {
			continue
		}
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

// acquire reserves one slot for the user. limit <= 0 is treated as unlimited.
func (l *limiter) acquire(ctx context.Context, userID int64, limit int, queueTimeout time.Duration) (func(), bool) {
	if limit <= 0 {
		l.mu.Lock()
		l.active[userID]++
		l.mu.Unlock()
		return func() { l.release(userID) }, true
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
		l.mu.Lock()
		removed := false
		ws := l.waiters[userID]
		for i, w := range ws {
			if w == ch {
				l.waiters[userID] = append(ws[:i], ws[i+1:]...)
				removed = true
				break
			}
		}
		l.mu.Unlock()
		// The slot may have been handed over between the select and now.
		if !removed {
			return func() { l.release(userID) }, true
		}
		return nil, false
	case <-ctx.Done():
		l.mu.Lock()
		ws := l.waiters[userID]
		for i, w := range ws {
			if w == ch {
				l.waiters[userID] = append(ws[:i], ws[i+1:]...)
				break
			}
		}
		l.mu.Unlock()
		return nil, false
	}
}

func (l *limiter) release(userID int64) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.active[userID] > 0 {
		l.active[userID]--
	}
	if ws := l.waiters[userID]; len(ws) > 0 {
		w := ws[0]
		l.waiters[userID] = ws[1:]
		l.active[userID]++ // hand the slot straight to the waiter
		close(w)
	}
}
