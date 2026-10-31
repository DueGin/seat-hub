package gateway

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type fakeResolver struct {
	p        *Principal
	touched  int64
	resolved []string
	mu       sync.Mutex
}

func (f *fakeResolver) Resolve(_ context.Context, key string) (*Principal, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.resolved = append(f.resolved, key)
	if f.p == nil || key != "sk-pt-good" {
		return nil, nil
	}
	return f.p, nil
}

func (f *fakeResolver) Touch(keyID int64) { f.touched = keyID }

func newGateway(t *testing.T, p *Principal, upstream http.Handler) (*Gateway, *httptest.Server) {
	t.Helper()
	up := httptest.NewServer(upstream)
	t.Cleanup(up.Close)
	u, _ := url.Parse(up.URL)
	return New(Config{Upstream: u, Resolver: &fakeResolver{p: p}, MaxBodyBytes: 1024}), up
}

func TestMissingKey(t *testing.T) {
	g, _ := newGateway(t, &Principal{}, http.NotFoundHandler())
	rec := httptest.NewRecorder()
	g.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader("{}")))
	if rec.Code != 401 {
		t.Fatalf("status = %d, want 401", rec.Code)
	}
	var body struct {
		Type  string `json:"type"`
		Error struct {
			Message string `json:"message"`
			Type    string `json:"type"`
		} `json:"error"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &body)
	if body.Type != "error" || body.Error.Type != "authentication_error" {
		t.Fatalf("unexpected anthropic error envelope: %s", rec.Body.String())
	}
}

func TestInvalidKeyOpenAIShape(t *testing.T) {
	g, _ := newGateway(t, nil, http.NotFoundHandler())
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader("{}"))
	req.Header.Set("x-api-key", "sk-pt-bad")
	rec := httptest.NewRecorder()
	g.ServeHTTP(rec, req)
	if rec.Code != 401 {
		t.Fatalf("status = %d", rec.Code)
	}
	var body map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &body)
	if _, ok := body["error"]; !ok {
		t.Fatalf("want OpenAI envelope, got %s", rec.Body.String())
	}
}

func TestSwapsAuthorizationAndStripsClientHeaders(t *testing.T) {
	var got *http.Request
	upstreamSeen := make(chan *http.Request, 1)
	g, _ := newGateway(t, &Principal{UserID: 1, KeyID: 2, UpstreamSecret: "sk-vm-up", Concurrency: 5, Active: true},
		http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			got = r.Clone(context.Background())
			upstreamSeen <- got
			w.WriteHeader(200)
			_, _ = w.Write([]byte(`{"ok":true}`))
		}))
	req := httptest.NewRequest(http.MethodPost, "/v1/messages?beta=1", strings.NewReader(`{"model":"x"}`))
	req.Header.Set("Authorization", "Bearer sk-pt-good")
	req.Header.Set("X-Kin-Debug", "1")
	req.Header.Set("Cookie", "portal_session=secret")
	req.Header.Set("X-Session-Id", "conv-1")
	rec := httptest.NewRecorder()
	g.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	r := <-upstreamSeen
	if auth := r.Header.Get("Authorization"); auth != "Bearer sk-vm-up" {
		t.Fatalf("authorization = %q", auth)
	}
	for _, h := range []string{"X-Kin-Debug", "Cookie"} {
		if v := r.Header.Get(h); v != "" {
			t.Fatalf("%s leaked upstream: %q", h, v)
		}
	}
	if r.Header.Get("X-Session-Id") != "conv-1" {
		t.Fatal("sticky header must pass through")
	}
	if r.URL.Query().Get("beta") != "1" {
		t.Fatal("query lost")
	}
}

func TestInactiveUser(t *testing.T) {
	g, _ := newGateway(t, &Principal{Active: false, Reason: "no_active_accounts"}, http.NotFoundHandler())
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader("{}"))
	req.Header.Set("Authorization", "Bearer sk-pt-good")
	rec := httptest.NewRecorder()
	g.ServeHTTP(rec, req)
	if rec.Code != 403 {
		t.Fatalf("status = %d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "托管席位") {
		t.Fatalf("helpful message missing: %s", rec.Body.String())
	}
}

func TestConcurrencyLimit(t *testing.T) {
	release := make(chan struct{})
	entered := make(chan struct{}, 4)
	g, _ := newGateway(t, &Principal{UserID: 7, KeyID: 1, UpstreamSecret: "sk-vm-x", Concurrency: 1, Active: true},
		http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			entered <- struct{}{}
			<-release
			w.WriteHeader(200)
		}))
	go func() {
		req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader("{}"))
		req.Header.Set("Authorization", "Bearer sk-pt-good")
		g.ServeHTTP(httptest.NewRecorder(), req)
	}()
	<-entered

	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader("{}"))
	req.Header.Set("Authorization", "Bearer sk-pt-good")
	rec := httptest.NewRecorder()
	g.ServeHTTP(rec, req)
	if rec.Code != 429 {
		t.Fatalf("second request status = %d, want 429", rec.Code)
	}
	if rec.Header().Get("Retry-After") == "" {
		t.Fatal("missing Retry-After")
	}
	close(release)
	waitInflight(t, g, 7, 0)
}

func TestConcurrencyQueueTimeout(t *testing.T) {
	release := make(chan struct{})
	entered := make(chan struct{}, 1)
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		entered <- struct{}{}
		<-release
		w.WriteHeader(200)
	}))
	defer up.Close()
	u, _ := url.Parse(up.URL)
	g := New(Config{Upstream: u, Resolver: &fakeResolver{p: &Principal{UserID: 9, KeyID: 1, UpstreamSecret: "k", Concurrency: 1, Active: true}},
		QueueTimeout: 50 * time.Millisecond})
	go func() {
		req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader("{}"))
		req.Header.Set("Authorization", "Bearer sk-pt-good")
		g.ServeHTTP(httptest.NewRecorder(), req)
	}()
	<-entered
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader("{}"))
	req.Header.Set("Authorization", "Bearer sk-pt-good")
	rec := httptest.NewRecorder()
	start := time.Now()
	g.ServeHTTP(rec, req)
	if rec.Code != 429 {
		t.Fatalf("status = %d", rec.Code)
	}
	if time.Since(start) < 40*time.Millisecond {
		t.Fatal("returned before queue timeout")
	}
	close(release)
}

func TestBodyLimit(t *testing.T) {
	g, _ := newGateway(t, &Principal{UserID: 1, KeyID: 1, UpstreamSecret: "k", Concurrency: 5, Active: true},
		http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			var b [4096]byte
			for {
				if _, err := r.Body.Read(b[:]); err != nil {
					break
				}
			}
			w.WriteHeader(200)
		}))
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(strings.Repeat("x", 4096)))
	req.Header.Set("Authorization", "Bearer sk-pt-good")
	rec := httptest.NewRecorder()
	g.ServeHTTP(rec, req)
	if rec.Code != 413 {
		t.Fatalf("status = %d, want 413", rec.Code)
	}
}

func TestUpstreamError(t *testing.T) {
	g, _ := newGateway(t, &Principal{UserID: 1, KeyID: 1, UpstreamSecret: "k", Concurrency: 5, Active: true},
		http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { panic(http.ErrAbortHandler) }))
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader("{}"))
	req.Header.Set("Authorization", "Bearer sk-pt-good")
	rec := httptest.NewRecorder()
	g.ServeHTTP(rec, req)
	if rec.Code != 502 {
		t.Fatalf("status = %d, want 502", rec.Code)
	}
}

func TestNonV1PathRejected(t *testing.T) {
	g, _ := newGateway(t, &Principal{}, http.NotFoundHandler())
	rec := httptest.NewRecorder()
	g.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/panel/vms", nil))
	if rec.Code != 404 {
		t.Fatalf("status = %d, want 404", rec.Code)
	}
}

func waitInflight(t *testing.T, g *Gateway, userID int64, want int) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if g.Inflight(userID) == want {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("inflight(%d) = %d, want %d", userID, g.Inflight(userID), want)
}

func TestRejectsDotSegments(t *testing.T) {
	var upstreamHits int32
	g, _ := newGateway(t, &Principal{UserID: 1, KeyID: 1, UpstreamSecret: "k", Concurrency: 5, Active: true},
		http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			atomic.AddInt32(&upstreamHits, 1)
			w.WriteHeader(200)
		}))
	for _, target := range []string{
		"/v1/../api/panel/login",
		"/v1/%2e%2e/api/panel/users",
		"/v1/./messages",
		"/v1/messages/../../api/panel/login",
	} {
		req := httptest.NewRequest(http.MethodPost, target, strings.NewReader("{}"))
		req.Header.Set("Authorization", "Bearer sk-pt-good")
		rec := httptest.NewRecorder()
		g.ServeHTTP(rec, req)
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("%s: status = %d, want 400", target, rec.Code)
		}
		if !strings.Contains(rec.Body.String(), "请求路径不合法") {
			t.Fatalf("%s: body = %s", target, rec.Body.String())
		}
	}
	if n := atomic.LoadInt32(&upstreamHits); n != 0 {
		t.Fatalf("upstream saw %d traversal requests", n)
	}
}

func TestCanonicalPathForwarded(t *testing.T) {
	seen := make(chan string, 2)
	g, _ := newGateway(t, &Principal{UserID: 1, KeyID: 1, UpstreamSecret: "k", Concurrency: 5, Active: true},
		http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			seen <- r.URL.Path
			w.WriteHeader(200)
		}))
	for _, tc := range []struct{ in, want string }{
		{"/v1//messages", "/v1/messages"},
		{"/v1/messages/", "/v1/messages/"},
		{"/v1", "/v1"},
	} {
		req := httptest.NewRequest(http.MethodPost, tc.in, strings.NewReader("{}"))
		req.Header.Set("Authorization", "Bearer sk-pt-good")
		rec := httptest.NewRecorder()
		g.ServeHTTP(rec, req)
		if rec.Code != 200 {
			t.Fatalf("%s: status = %d", tc.in, rec.Code)
		}
		if got := <-seen; got != tc.want {
			t.Fatalf("%s: upstream path = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// TestWebSocketUpgradePassthrough runs a real 101 through the proxy: the
// hijacked connection is required for a protocol switch, so an
// httptest.ResponseRecorder cannot stand in for the upstream.
func TestWebSocketUpgradePassthrough(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, brw, err := w.(http.Hijacker).Hijack()
		if err != nil {
			t.Errorf("hijack: %v", err)
			return
		}
		defer conn.Close()
		if _, err := brw.WriteString("HTTP/1.1 101 Switching Protocols\r\n" +
			"Upgrade: websocket\r\n" +
			"Connection: Upgrade\r\n" +
			"X-Test-Upstream: yes\r\n\r\n"); err != nil {
			t.Errorf("write 101: %v", err)
			return
		}
		_ = brw.Flush()
		_, _ = brw.ReadString('\n') // wait for the client to hang up
	}))
	defer up.Close()
	u, _ := url.Parse(up.URL)
	g := New(Config{Upstream: u, Resolver: &fakeResolver{p: &Principal{UserID: 3, KeyID: 1, UpstreamSecret: "k", Concurrency: 5, Active: true}}})

	gwSrv := httptest.NewServer(g)
	defer gwSrv.Close()

	conn, err := net.Dial("tcp", gwSrv.Listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	req := "GET /v1/responses HTTP/1.1\r\n" +
		"Host: " + gwSrv.Listener.Addr().String() + "\r\n" +
		"Authorization: Bearer sk-pt-good\r\n" +
		"Upgrade: websocket\r\n" +
		"Connection: Upgrade\r\n\r\n"
	if _, err := conn.Write([]byte(req)); err != nil {
		t.Fatal(err)
	}
	_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	br := bufio.NewReader(conn)
	resp, err := http.ReadResponse(br, nil)
	if err != nil {
		t.Fatalf("read response: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusSwitchingProtocols {
		t.Fatalf("status = %d, want 101", resp.StatusCode)
	}
	if got := resp.Header.Get("Upgrade"); !strings.EqualFold(got, "websocket") {
		t.Fatalf("Upgrade = %q", got)
	}
	if got := resp.Header.Get("Connection"); !strings.EqualFold(got, "Upgrade") {
		t.Fatalf("Connection = %q", got)
	}
	if resp.Header.Get("X-Test-Upstream") != "yes" {
		t.Fatal("upstream header did not pass through")
	}
}

func TestZeroConcurrencyRejected(t *testing.T) {
	g, _ := newGateway(t, &Principal{UserID: 4, KeyID: 1, UpstreamSecret: "k", Concurrency: 0, Active: true},
		http.NotFoundHandler())
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader("{}"))
	req.Header.Set("Authorization", "Bearer sk-pt-good")
	rec := httptest.NewRecorder()
	g.ServeHTTP(rec, req)
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want 429", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "concurrency_limit_exceeded") {
		t.Fatalf("code missing: %s", rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "购买并发") {
		t.Fatalf("purchase hint missing: %s", rec.Body.String())
	}
	if g.Inflight(4) != 0 {
		t.Fatalf("inflight = %d, want 0", g.Inflight(4))
	}
}

func TestNegativeConcurrencyTreatedAsZero(t *testing.T) {
	l := newLimiter()
	if _, ok := l.acquire(context.Background(), 1, -3, time.Second); ok {
		t.Fatal("negative concurrency acquired a slot")
	}
	if l.inflight(1) != 0 {
		t.Fatalf("inflight = %d, want 0", l.inflight(1))
	}
}

// TestLimiterNoSlotLeakUnderCancellation hammers the acquire/release hand-over
// race: a queued waiter that loses the select to ctx.Done after release()
// already transferred the slot must hand it back, never leak it.
func TestLimiterNoSlotLeakUnderCancellation(t *testing.T) {
	for _, limit := range []int{1, 2, 8} {
		limit := limit
		t.Run(fmt.Sprintf("limit=%d", limit), func(t *testing.T) {
			l := newLimiter()
			const users = 4
			var wg sync.WaitGroup
			for u := int64(1); u <= users; u++ {
				wg.Add(1)
				go func(userID int64) {
					defer wg.Done()
					for i := 0; i < 400; i++ {
						release, ok := l.acquire(context.Background(), userID, limit, 2*time.Millisecond)
						if !ok {
							continue
						}
						if i%3 == 0 {
							// Let handle-over happen while a cancel race is live.
							time.Sleep(time.Millisecond)
						}
						release()
					}
				}(u)
			}
			for u := int64(1); u <= users; u++ {
				wg.Add(1)
				go func(userID int64) {
					defer wg.Done()
					for i := 0; i < 400; i++ {
						ctx, cancel := context.WithTimeout(context.Background(), 3*time.Millisecond)
						release, ok := l.acquire(ctx, userID, limit, 10*time.Millisecond)
						cancel()
						if ok {
							time.Sleep(time.Millisecond)
							release()
						}
					}
				}(u)
			}
			wg.Wait()
			for u := int64(1); u <= users; u++ {
				if got := l.inflight(u); got != 0 {
					t.Fatalf("user %d leaked %d slots", u, got)
				}
			}
		})
	}
}

func TestLimiterQueueHandoffCounts(t *testing.T) {
	l := newLimiter()
	rel, ok := l.acquire(context.Background(), 1, 1, 0)
	if !ok {
		t.Fatal("first acquire failed")
	}
	got := make(chan func(), 1)
	go func() {
		r, ok := l.acquire(context.Background(), 1, 1, time.Second)
		if !ok {
			got <- nil
			return
		}
		got <- r
	}()
	time.Sleep(20 * time.Millisecond)
	rel()
	r := <-got
	if r == nil {
		t.Fatal("queued waiter was not handed the slot")
	}
	if n := l.inflight(1); n != 1 {
		t.Fatalf("inflight after hand-over = %d, want 1", n)
	}
	r()
	if n := l.inflight(1); n != 0 {
		t.Fatalf("inflight after release = %d, want 0", n)
	}
}
