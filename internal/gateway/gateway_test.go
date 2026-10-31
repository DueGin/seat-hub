package gateway

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
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
