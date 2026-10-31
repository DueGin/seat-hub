package vm2api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"strings"
	"testing"
	"time"
)

type panelCallTest struct {
	name     string
	method   string
	path     string
	query    url.Values
	body     string
	response string
	invoke   func(*HTTPClient) error
}

func panelCallTests() []panelCallTest {
	ctx := context.Background()
	ownerID := "user-one"
	name, active, concurrency := "renamed", false, 500
	from := time.Date(2026, 10, 31, 8, 0, 0, 0, time.FixedZone("offset", 8*3600))
	until := from.Add(time.Hour)
	return []panelCallTest{
		{
			name: "ping", method: http.MethodGet, path: "/version", response: `{"ok":true,"data":{"current":"1.2.3"}}`,
			invoke: func(client *HTTPClient) error {
				value, err := client.Ping(ctx)
				if err == nil && (!value.OK || value.Version != "1.2.3" || value.LatencyMS < 0) {
					return fmt.Errorf("unexpected health: %+v", value)
				}
				return err
			},
		},
		{
			name: "create user", method: http.MethodPost, path: "/users",
			body:     `{"username":"alice","password":"password","enabled":true,"role":"user","vm_create_quota":0}`,
			response: `{"ok":true,"data":{"item":{"id":"user-one","username":"alice","enabled":true}}}`,
			invoke: func(client *HTTPClient) error {
				value, err := client.CreateUser(ctx, "alice", "password")
				if err == nil && value != (User{ID: ownerID, Username: "alice", Enabled: true}) {
					return fmt.Errorf("unexpected user: %+v", value)
				}
				return err
			},
		},
		{
			name: "disable user", method: http.MethodPatch, path: "/users/user-one", body: `{"enabled":false}`,
			invoke: func(client *HTTPClient) error { return client.SetUserEnabled(ctx, ownerID, false) },
		},
		{
			name: "create key", method: http.MethodPost, path: "/api-keys",
			body:     `{"name":"tenant","user_id":"user-one","max_concurrency":0}`,
			response: `{"ok":true,"item":{"id":"key-one","key":"sk-vm-0123456789abcdef","key_prefix":"sk-vm-0123"}}`,
			invoke: func(client *HTTPClient) error {
				value, err := client.CreateKey(ctx, CreateKeyInput{Name: "tenant", UserID: ownerID, MaxConcurrency: -1})
				if err == nil && value != (Key{ID: "key-one", Secret: "sk-vm-0123456789abcdef", Prefix: "sk-vm-0123"}) {
					return fmt.Errorf("unexpected key: %+v", value)
				}
				return err
			},
		},
		{
			name: "update key", method: http.MethodPatch, path: "/api-keys/key-one",
			body: `{"name":"renamed","status":"disabled","max_concurrency":128}`,
			invoke: func(client *HTTPClient) error {
				return client.UpdateKey(ctx, "key-one", UpdateKeyInput{Name: &name, Active: &active, MaxConcurrency: &concurrency})
			},
		},
		{
			name: "delete key", method: http.MethodDelete, path: "/api-keys/key-one",
			invoke: func(client *HTTPClient) error { return client.DeleteKey(ctx, "key-one") },
		},
		{
			name: "key stats", method: http.MethodGet, path: "/api-keys/key-one/stats",
			response: `{"ok":true,"item":{"id":"key-one"},"usage_stats":{"history":[{"day":"2026-10-31","requests":2,"input_tokens":10,"output_tokens":20,"cache_read_tokens":30,"cache_creation_tokens":40,"total_cost":0.25}],"models":[{"name":"claude","requests":2,"tokens":100,"total_cost":0.25}]}}`,
			invoke: func(client *HTTPClient) error {
				value, err := client.KeyStats(ctx, "key-one")
				expected := UsageBreakdown{Daily: []DailyUsage{{Date: "2026-10-31", Requests: 2, Tokens: 100, CostUSD: 0.25}}, ByModel: []ModelUsage{{Model: "claude", Requests: 2, Tokens: 100, CostUSD: 0.25}}}
				if err == nil && !reflect.DeepEqual(value, expected) {
					return fmt.Errorf("unexpected stats: %+v", value)
				}
				return err
			},
		},
		{
			name: "list vms", method: http.MethodGet, path: "/vms",
			response: `{"ok":true,"data":{"items":[{"id":"vm-one","name":"Claude slot","platform":"anthropic","owner_user_id":"user-one","origin":"admin_assigned","account_tier":"max","email":"alice@example.test","has_token":true,"cred_status":"ok","schedule_state":"unschedulable","unschedulable_reason":"oauth_revoked"},{"id":"other","platform":"anthropic","owner_user_id":"user-two"},{"id":"codex","platform":"openai","owner_user_id":"user-one"}]}}`,
			invoke: func(client *HTTPClient) error {
				value, err := client.ListVMs(ctx, VMFilter{OwnerUserID: &ownerID, ClaudeOnly: true})
				if err == nil && (len(value) != 1 || value[0].ID != "vm-one" || value[0].Label != "Claude slot" || value[0].Tier != "max" || value[0].EmailMasked != "a***@example.test" || !value[0].HasCredential || !value[0].CredentialRejected) {
					return fmt.Errorf("unexpected vms: %+v", value)
				}
				return err
			},
		},
		{
			name: "account usage", method: http.MethodGet, path: "/vms/vm-one",
			response: `{"ok":true,"data":{"vm":{"id":"vm-one","name":"slot","platform":"anthropic","schedule_state":"restricted","restriction_text":"冷却中","seats_used":2,"seats_max":5,"inflight":3,"utilization_5h":0.25,"reset_5h":"2026-10-31T01:00:00Z","utilization_7d":0.5,"reset_7d":"2026-11-01T01:00:00Z","utilization_7d_oi":0.1,"reset_7d_oi":"2026-11-01T01:00:00Z","estimate_5h":{"status":"ok","spent_usd":1,"total_usd":4,"remaining_usd":3,"used_ratio":0.25},"estimate_7d":{"status":"low_usage","spent_usd":2}},"billing":{"today":{"requests":2,"input_tokens":10,"output_tokens":20,"cache_read_tokens":30,"cache_creation_tokens":40,"total_cost":0.5},"window_5h":{"requests":1,"total_cost":0.25},"window_7d":{"requests":3,"total_cost":0.75},"usage_stats":{"history":[{"day":"2026-10-31","requests":2,"input_tokens":10,"output_tokens":20,"total_cost":0.5}],"models":[{"name":"claude","requests":2,"tokens":30,"total_cost":0.5}]}}}}`,
			invoke: func(client *HTTPClient) error {
				value, err := client.GetAccountUsage(ctx, "vm-one")
				if err != nil {
					return err
				}
				if value.VM.ScheduleState != "restricted" || value.VM.RestrictionText != "冷却中" || value.VM.SeatsUsed != 2 || value.VM.SeatsMax != 5 || value.VM.Inflight != 3 || value.VM.CredentialRejected {
					return fmt.Errorf("unexpected vm usage: %+v", value.VM)
				}
				if value.VM.FiveHour == nil || value.VM.FiveHour.Utilization != 25 || value.VM.FiveHour.ResetsAt == nil || value.VM.FiveHour.ResetsAt.Format(time.RFC3339) != "2026-10-31T01:00:00Z" || value.VM.SevenDay == nil || value.VM.SevenDay.Utilization != 50 || value.VM.SevenDayFable == nil || value.VM.SevenDayFable.Utilization != 10 {
					return fmt.Errorf("unexpected windows: %+v", value.VM)
				}
				if value.Estimate5h == nil || value.Estimate5h.TotalUSD == nil || *value.Estimate5h.TotalUSD != 4 || value.Estimate5h.RemainingUSD == nil || *value.Estimate5h.RemainingUSD != 3 || value.Estimate5h.UsedRatio == nil || *value.Estimate5h.UsedRatio != 0.25 || value.Estimate7d == nil || value.Estimate7d.Status != "low_usage" {
					return fmt.Errorf("unexpected estimates: %+v", value)
				}
				if value.Today == nil || value.Today.Tokens != 100 || value.Today.CostUSD != 0.5 || value.Window5h == nil || value.Window5h.CostUSD != 0.25 || value.Window7d == nil || value.Window7d.CostUSD != 0.75 || len(value.Daily) != 1 || value.Daily[0].Tokens != 30 || len(value.ByModel) != 1 || value.ByModel[0].Model != "claude" || value.FetchedAt.Location() != time.UTC || time.Since(value.FetchedAt) > time.Minute {
					return fmt.Errorf("unexpected account usage: %+v", value)
				}
				return nil
			},
		},
		{
			name: "create vm", method: http.MethodPost, path: "/vms/create",
			body:     `{"name":"hosted","runtime":"container","nested":{"raw":9007199254740993},"auto_allocate_proxy":false}`,
			response: `{"ok":true,"data":{"vm":{"id":"vm-one","name":"hosted","platform":"anthropic","origin":"platform"}}}`,
			invoke: func(client *HTTPClient) error {
				value, err := client.CreateHostedVM(ctx, "hosted", json.RawMessage(`{"runtime":"container","nested":{"raw":9007199254740993},"auto_allocate_proxy":false}`))
				if err == nil && (value.ID != "vm-one" || value.Label != "hosted") {
					return fmt.Errorf("unexpected created vm: %+v", value)
				}
				return err
			},
		},
		{
			name: "assign owner", method: http.MethodPatch, path: "/vms/vm-one/owner", body: `{"user_id":"user-one"}`,
			invoke: func(client *HTTPClient) error { return client.SetVMOwner(ctx, "vm-one", &ownerID) },
		},
		{
			name: "clear owner", method: http.MethodPatch, path: "/vms/vm-one/owner", body: `{"user_id":null}`,
			invoke: func(client *HTTPClient) error { return client.SetVMOwner(ctx, "vm-one", nil) },
		},
		{
			name: "allocate proxy", method: http.MethodPost, path: "/vms/vm-one/allocate-proxy", body: `{}`,
			invoke: func(client *HTTPClient) error { return client.AllocateProxy(ctx, "vm-one") },
		},
		{
			name: "set schedulable", method: http.MethodPost, path: "/vms/vm-one/schedulable", body: `{"schedulable":true}`,
			invoke: func(client *HTTPClient) error { return client.SetSchedulable(ctx, "vm-one", true) },
		},
		{
			name: "delete vm", method: http.MethodDelete, path: "/vms/vm-one",
			invoke: func(client *HTTPClient) error { return client.DeleteVM(ctx, "vm-one") },
		},
		{
			name: "generate auth url", method: http.MethodPost, path: "/vms/vm-one/oauth/generate-auth-url", body: `{"flavor":"claude_code"}`,
			response: `{"ok":true,"data":{"auth_url":"https://example.test/authorize","session_id":"session-one","expires_at":1793406600000}}`,
			invoke: func(client *HTTPClient) error {
				value, err := client.GenerateAuthURL(ctx, "vm-one")
				if err == nil && (value.URL != "https://example.test/authorize" || value.SessionID != "session-one" || !value.ExpiresAt.Equal(time.UnixMilli(1793406600000)) || value.ExpiresAt.Location() != time.UTC) {
					return fmt.Errorf("unexpected auth url: %+v", value)
				}
				return err
			},
		},
		{
			name: "exchange code", method: http.MethodPost, path: "/vms/vm-one/oauth/exchange-code",
			body: `{"session_id":"session-one","code":"authorization-code","flavor":"claude_code"}`,
			invoke: func(client *HTTPClient) error {
				return client.ExchangeCode(ctx, "vm-one", "session-one", "authorization-code")
			},
		},
		{
			name: "bootstrap status", method: http.MethodGet, path: "/vms/vm-one/official-cc-bootstrap",
			response: `{"ok":true,"data":{"vm_id":"vm-one","status":{"status":"ok","error":null}}}`,
			invoke: func(client *HTTPClient) error {
				value, err := client.BootstrapStatus(ctx, "vm-one")
				if err == nil && value != (Bootstrap{State: "succeeded"}) {
					return fmt.Errorf("unexpected bootstrap: %+v", value)
				}
				return err
			},
		},
		{
			name: "user billing", method: http.MethodGet, path: "/billing",
			query:    url.Values{"user_id": {ownerID}, "from": {"2026-10-31T00:00:00Z"}, "until": {"2026-10-31T01:00:00Z"}},
			response: `{"ok":true,"data":{"totals":{"requests":2,"tokens_in":10,"tokens_out":20,"cost_usd":0.25},"items":[{"vm_id":"vm-one","api_key_id":"key-one","requests":2,"tokens_in":10,"tokens_out":20,"cost_usd":0.25}]}}`,
			invoke: func(client *HTTPClient) error {
				value, err := client.UserBilling(ctx, ownerID, from, until)
				if err == nil && (value.Totals.Requests != 2 || value.Totals.Tokens != 30 || value.Totals.InputTokens != 10 || value.Totals.OutputTokens != 20 || value.Totals.CostUSD != 0.25 || len(value.ByVM) != 1 || value.ByVM[0].VMID != "vm-one" || len(value.ByKey) != 1 || value.ByKey[0].KeyID != "key-one" || value.ByKey[0].Tokens != 30) {
					return fmt.Errorf("unexpected billing: %+v", value)
				}
				return err
			},
		},
		{
			name: "user logs", method: http.MethodGet, path: "/request-logs",
			query:    url.Values{"user_id": {ownerID}, "since": {"2026-10-31T00:00:00Z"}, "until": {"2026-10-31T01:00:00Z"}, "limit": {"10"}, "offset": {"20"}, "api_key_id": {"key-one"}, "status": {"error"}, "model": {"claude"}},
			response: `{"ok":true,"total":31,"limit":10,"offset":20,"items":[{"request_id":"req-one","created_at":"2026-10-31T08:00:00+08:00","model":"claude","api_key_id":"key-one","vm_id":"vm-one","stream":true,"status":429,"error_code":"rate_limit","input_tokens":10,"output_tokens":20,"cache_read_tokens":30,"cache_creation_tokens":40,"total_cost":0.25,"duration_ms":300,"first_token_ms":100}]}`,
			invoke: func(client *HTTPClient) error {
				value, err := client.UserRequestLogs(ctx, ownerID, LogQuery{From: from, Until: until, Page: 3, PageSize: 10, Model: "claude", KeyID: "key-one", Status: "error"})
				if err != nil {
					return err
				}
				expected := LogRow{RequestID: "req-one", CreatedAt: from.UTC(), Model: "claude", KeyID: "key-one", VMID: "vm-one", Stream: true, StatusCode: 429, ErrorLabel: "rate_limit", InputTokens: 10, OutputTokens: 20, CacheReadTokens: 30, CacheCreationTokens: 40, CostUSD: 0.25, DurationMS: 300}
				firstToken := int64(100)
				expected.FirstTokenMS = &firstToken
				if value.Total != 31 || value.Page != 3 || value.PageSize != 10 || !reflect.DeepEqual(value.Items, []LogRow{expected}) {
					return fmt.Errorf("unexpected log page: %+v", value)
				}
				return nil
			},
		},
		{
			name: "user stats", method: http.MethodGet, path: "/request-logs/stats",
			query:    url.Values{"user_id": {ownerID}, "since": {"2026-10-31T00:00:00Z"}, "until": {"2026-10-31T01:00:00Z"}},
			response: `{"ok":true,"totals":{"requests":999},"window":{"requests":10,"errors":2,"sla":0.8,"ttft":{"avg_ms":150,"p95_ms":300}}}`,
			invoke: func(client *HTTPClient) error {
				value, err := client.UserRequestStats(ctx, ownerID, from, until)
				if err == nil && value != (RequestStats{Requests: 10, ErrorCount: 2, SuccessRate: 0.8, AvgTTFTMS: 150, P95TTFTMS: 300}) {
					return fmt.Errorf("unexpected request stats: %+v", value)
				}
				return err
			},
		},
	}
}

func TestHTTPClientMethods(t *testing.T) {
	for _, test := range panelCallTests() {
		t.Run(test.name, func(t *testing.T) {
			calls := 0
			server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
				calls++
				if request.Method != test.method || request.URL.Path != "/api/panel"+test.path {
					t.Errorf("request = %s %s, want %s %s", request.Method, request.URL.Path, test.method, "/api/panel"+test.path)
				}
				if request.Header.Get("Authorization") != "Bearer master-secret" || request.Header.Get("Accept") != "application/json" {
					t.Error("missing panel authorization or accept header")
				}
				query := request.URL.Query()
				if test.name == "user billing" {
					expectedGroup := "vm"
					if calls == 2 {
						expectedGroup = "key"
					}
					if query.Get("group_by") != expectedGroup {
						t.Errorf("billing group = %q, want %q", query.Get("group_by"), expectedGroup)
					}
					query.Del("group_by")
				}
				if query.Encode() != test.query.Encode() {
					t.Errorf("query = %s, want %s", query.Encode(), test.query.Encode())
				}
				body, err := io.ReadAll(request.Body)
				if err != nil {
					t.Errorf("read request body: %v", err)
				}
				if test.body == "" {
					if len(body) != 0 {
						t.Errorf("unexpected request body: %s", body)
					}
				} else {
					if request.Header.Get("Content-Type") != "application/json" {
						t.Error("missing json content type")
					}
					if !reflect.DeepEqual(decodeTestJSON(t, string(body)), decodeTestJSON(t, test.body)) {
						t.Errorf("body = %s, want %s", body, test.body)
					}
				}
				response := test.response
				if response == "" {
					response = `{"ok":true}`
				}
				writer.Header().Set("Content-Type", "application/json")
				if test.name == "create user" || test.name == "create key" {
					writer.WriteHeader(http.StatusCreated)
				}
				_, _ = io.WriteString(writer, response)
			}))
			defer server.Close()
			if err := test.invoke(NewHTTPClient(server.URL+"/", "master-secret", server.Client())); err != nil {
				t.Fatal(err)
			}
			expectedCalls := 1
			if test.name == "user billing" {
				expectedCalls = 2
			}
			if calls != expectedCalls {
				t.Errorf("calls = %d, want %d", calls, expectedCalls)
			}
		})
	}
}

func decodeTestJSON(t *testing.T, raw string) any {
	t.Helper()
	decoder := json.NewDecoder(strings.NewReader(raw))
	decoder.UseNumber()
	var value any
	if err := decoder.Decode(&value); err != nil {
		t.Errorf("decode test json: %v", err)
	}
	return value
}

func TestHTTPClientMethodErrors(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.WriteHeader(http.StatusConflict)
		_, _ = io.WriteString(writer, `{"ok":false,"error":{"code":"credential_unavailable","message":"credential rejected"}}`)
	}))
	defer server.Close()
	client := NewHTTPClient(server.URL, "master", server.Client())
	for _, test := range panelCallTests() {
		t.Run(test.name, func(t *testing.T) {
			err := test.invoke(client)
			var panelErr *Error
			if !errors.As(err, &panelErr) || panelErr.Status != 409 || panelErr.Code != "credential_unavailable" || panelErr.Message != "credential rejected" || panelErr.Op != test.method+" "+test.path {
				t.Fatalf("unexpected error: %v", err)
			}
			if !IsCode(err, "credential_unavailable") || !IsStatus(err, 409) {
				t.Fatal("error helpers did not preserve panel error")
			}
		})
	}
}

type panelRoundTripFunc func(*http.Request) (*http.Response, error)

func (roundTrip panelRoundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return roundTrip(request)
}

func TestHTTPClientTransportFailures(t *testing.T) {
	transportErr := errors.New("connection refused")
	client := NewHTTPClient("http://panel.invalid", "master", &http.Client{Transport: panelRoundTripFunc(func(request *http.Request) (*http.Response, error) {
		return nil, transportErr
	})})
	_, err := client.Ping(context.Background())
	if !errors.Is(err, ErrUnavailable) || !errors.Is(err, transportErr) {
		t.Fatalf("transport error = %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	client = NewHTTPClient("http://panel.invalid", "master", &http.Client{Transport: panelRoundTripFunc(func(request *http.Request) (*http.Response, error) {
		return nil, request.Context().Err()
	})})
	_, err = client.Ping(ctx)
	if !errors.Is(err, ErrUnavailable) || !errors.Is(err, context.Canceled) {
		t.Fatalf("cancellation error = %v", err)
	}
}

func TestHTTPClientMalformedResponses(t *testing.T) {
	for _, test := range []struct {
		name   string
		status int
		body   string
		code   string
	}{
		{name: "plain non-2xx", status: 502, body: "bad gateway"},
		{name: "failure envelope at 200", status: 200, body: `{"ok":false,"error":{"code":"denied","message":"denied"}}`, code: "denied"},
		{name: "malformed success", status: 200, body: `{`},
		{name: "missing envelope", status: 200, body: `{}`},
	} {
		t.Run(test.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
				writer.WriteHeader(test.status)
				_, _ = io.WriteString(writer, test.body)
			}))
			defer server.Close()
			_, err := NewHTTPClient(server.URL, "master", server.Client()).Ping(context.Background())
			if err == nil {
				t.Fatal("expected response error")
			}
			if test.status != 200 || test.code != "" {
				var panelErr *Error
				if !errors.As(err, &panelErr) || panelErr.Status != test.status || panelErr.Code != test.code {
					t.Fatalf("unexpected panel error: %v", err)
				}
			}
		})
	}
}

func TestHTTPClientRejectsMaskedKey(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		_, _ = io.WriteString(writer, `{"ok":true,"item":{"id":"key","key":"sk-vm-01…cdef"}}`)
	}))
	defer server.Close()
	key, err := NewHTTPClient(server.URL, "master", server.Client()).CreateKey(context.Background(), CreateKeyInput{})
	if err == nil || key.Secret != "" {
		t.Fatalf("masked key escaped as plaintext: %+v, %v", key, err)
	}
}

func TestHTTPClientUnownedFilter(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		_, _ = io.WriteString(writer, `{"ok":true,"data":{"items":[{"id":"pool","platform":"anthropic","origin":"platform"},{"id":"self","platform":"anthropic","origin":"user_created"},{"id":"owned","platform":"anthropic","origin":"admin_assigned","owner_user_id":"user"},{"id":"openai","platform":"openai","origin":"platform"}]}}`)
	}))
	defer server.Close()
	items, err := NewHTTPClient(server.URL, "master", server.Client()).ListVMs(context.Background(), VMFilter{Unowned: true, ClaudeOnly: true})
	if err != nil || len(items) != 1 || items[0].ID != "pool" {
		t.Fatalf("platform filter = %+v, %v", items, err)
	}
}

func TestHTTPClientCredentialRejection(t *testing.T) {
	for _, reason := range []string{"oauth_revoked", "credential_expired", "proxy_required", "no_credential", "rate_limit"} {
		t.Run(reason, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
				_ = json.NewEncoder(writer).Encode(map[string]any{"ok": true, "data": map[string]any{"vm": map[string]any{"unschedulable_reason": reason}}})
			}))
			defer server.Close()
			usage, err := NewHTTPClient(server.URL, "master", server.Client()).GetAccountUsage(context.Background(), "vm-one")
			if err != nil {
				t.Fatal(err)
			}
			expected := strings.HasPrefix(reason, "oauth_") || strings.HasPrefix(reason, "credential_")
			if usage.VM.CredentialRejected != expected {
				t.Fatalf("reason %q: rejected = %v", reason, usage.VM.CredentialRejected)
			}
		})
	}
}

func TestHTTPClientBootstrapStates(t *testing.T) {
	for _, test := range []struct{ raw, state, message string }{
		{`null`, "pending", ""},
		{`{"status":"running"}`, "running", ""},
		{`{"status":"error","error":"boot failed"}`, "failed", "boot failed"},
		{`{"status":"ok"}`, "succeeded", ""},
	} {
		t.Run(test.state, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
				_, _ = io.WriteString(writer, `{"ok":true,"data":{"status":`+test.raw+`}}`)
			}))
			defer server.Close()
			status, err := NewHTTPClient(server.URL, "master", server.Client()).BootstrapStatus(context.Background(), "vm-one")
			if err != nil || status != (Bootstrap{State: test.state, Message: test.message}) {
				t.Fatalf("bootstrap = %+v, %v", status, err)
			}
		})
	}
}

func TestHTTPClientWindowStatusAndNullUsage(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		_, _ = io.WriteString(writer, `{"ok":true,"data":{"vm":{"id":"vm-one","utilization_5h":0,"reset_5h":null,"status_5h":"unknown","utilization_7d":1,"reset_7d":1793406600000,"status_7d":"limited","utilization_7d_oi":0.15,"status_7d_oi":"ok"},"billing":{"today":null,"window_5h":null,"window_7d":null,"usage_stats":null}}}`)
	}))
	defer server.Close()
	usage, err := NewHTTPClient(server.URL, "master", server.Client()).GetAccountUsage(context.Background(), "vm-one")
	if err != nil {
		t.Fatal(err)
	}
	if usage.VM.FiveHour == nil || usage.VM.FiveHour.Utilization != 0 || usage.VM.FiveHour.Status != "unknown" || usage.VM.FiveHour.ResetsAt != nil || usage.VM.SevenDay == nil || usage.VM.SevenDay.Status != "limited" || usage.VM.SevenDay.ResetsAt == nil || !usage.VM.SevenDay.ResetsAt.Equal(time.UnixMilli(1793406600000)) || usage.VM.SevenDayFable == nil || usage.VM.SevenDayFable.Status != "ok" {
		t.Fatalf("unexpected windows: %+v", usage.VM)
	}
	if usage.Today != nil || usage.Window5h != nil || usage.Window7d != nil || len(usage.Daily) != 0 || len(usage.ByModel) != 0 {
		t.Fatalf("null usage was synthesized: %+v", usage)
	}
}

func TestHTTPClientPartialKeyUpdate(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		var body map[string]any
		if err := json.NewDecoder(request.Body).Decode(&body); err != nil {
			t.Error(err)
		}
		if !reflect.DeepEqual(body, map[string]any{"status": "active", "max_concurrency": float64(0)}) {
			t.Errorf("partial update = %+v", body)
		}
		if request.URL.EscapedPath() != "/api/panel/api-keys/key%20%3F%23" {
			t.Errorf("escaped path = %s", request.URL.EscapedPath())
		}
		_, _ = io.WriteString(writer, `{"ok":true}`)
	}))
	defer server.Close()
	active, concurrency := true, -1
	err := NewHTTPClient(server.URL, "master", server.Client()).UpdateKey(context.Background(), "key ?#", UpdateKeyInput{Active: &active, MaxConcurrency: &concurrency})
	if err != nil {
		t.Fatal(err)
	}
}

func TestHTTPClientInvalidTemplateDoesNotSend(t *testing.T) {
	calls := 0
	client := NewHTTPClient("http://panel.invalid", "master", &http.Client{Transport: panelRoundTripFunc(func(request *http.Request) (*http.Response, error) {
		calls++
		return nil, errors.New("unexpected request")
	})})
	for _, raw := range []string{`{`, `[]`, `null`, `"template"`} {
		_, err := client.CreateHostedVM(context.Background(), "slot", json.RawMessage(raw))
		if err == nil {
			t.Errorf("accepted invalid template %s", raw)
		}
	}
	if calls != 0 {
		t.Fatalf("invalid templates sent %d requests", calls)
	}
}

func TestHTTPClientStructuredCredentialStatus(t *testing.T) {
	for _, test := range []struct{ raw, expected string }{
		{`{"key":"ok","text":"可用","tone":"ok","usable":true}`, "可用"},
		{`{"key":"none","text":"无凭证","usable":false}`, "无凭证"},
		{`{"key":"bad","reason":"oauth_revoked"}`, "bad"},
		{`"legacy status"`, "legacy status"},
		{`null`, ""},
	} {
		t.Run(test.expected, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
				_, _ = io.WriteString(writer, `{"ok":true,"data":{"vm":{"id":"vm-one","cred_status":`+test.raw+`}}}`)
			}))
			defer server.Close()
			usage, err := NewHTTPClient(server.URL, "master", server.Client()).GetAccountUsage(context.Background(), "vm-one")
			if err != nil || usage.VM.CredStatus != test.expected {
				t.Fatalf("credential status = %q, %v; want %q", usage.VM.CredStatus, err, test.expected)
			}
		})
	}
}
