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
			name: "list users", method: http.MethodGet, path: "/users",
			response: `{"ok":true,"data":{"items":[{"id":"user-one","username":"alice","role":"user","enabled":false}]}}`,
			invoke: func(client *HTTPClient) error {
				users, err := client.ListUsers(ctx)
				if err == nil && !reflect.DeepEqual(users, []User{{ID: ownerID, Username: "alice", Role: "user"}}) {
					return fmt.Errorf("unexpected users: %+v", users)
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
			body:     `{"name":"tenant","max_concurrency":0,"group_type":"anthropic","vm_pool_id":"pool_1234abcd"}`,
			response: `{"ok":true,"item":{"id":"key-one","key":"sk-vm-0123456789abcdef","key_prefix":"sk-vm-0123"}}`,
			invoke: func(client *HTTPClient) error {
				value, err := client.CreateKey(ctx, CreateKeyInput{Name: "tenant", MaxConcurrency: -1, Scope: KeyScope{PoolID: "pool_1234abcd"}})
				if err == nil && value != (Key{ID: "key-one", Secret: "sk-vm-0123456789abcdef", Prefix: "sk-vm-0123"}) {
					return fmt.Errorf("unexpected key: %+v", value)
				}
				return err
			},
		},
		{
			name: "create all accounts key", method: http.MethodPost, path: "/api-keys",
			body:     `{"name":"all","max_concurrency":0,"group_type":"all","allowed_vms":[],"vm_pool_id":null}`,
			response: `{"ok":true,"item":{"id":"key-one","key":"sk-vm-0123456789abcdef"}}`,
			invoke: func(client *HTTPClient) error {
				_, err := client.CreateKey(ctx, CreateKeyInput{Name: "all", Scope: KeyScope{All: true}})
				return err
			},
		},
		{
			name: "update all accounts key", method: http.MethodPatch, path: "/api-keys/key-one",
			body: `{"group_type":"all","allowed_vms":[],"vm_pool_id":null}`,
			invoke: func(client *HTTPClient) error {
				return client.UpdateKey(ctx, "key-one", UpdateKeyInput{Scope: &KeyScope{All: true}})
			},
		},
		{
			name: "create allowlist key", method: http.MethodPost, path: "/api-keys",
			body:     `{"name":"selected","max_concurrency":2,"group_type":"anthropic","allowed_vms":["vm-one","vm-two"],"vm_pool_id":null}`,
			response: `{"ok":true,"item":{"id":"key-one","key":"sk-vm-0123456789abcdef","key_prefix":"sk-vm-0123"}}`,
			invoke: func(client *HTTPClient) error {
				_, err := client.CreateKey(ctx, CreateKeyInput{Name: "selected", MaxConcurrency: 2, Scope: KeyScope{AllowedVMs: []string{"vm-one", "vm-two"}}})
				return err
			},
		},
		{
			name: "update pool key", method: http.MethodPatch, path: "/api-keys/key-one",
			body: `{"group_type":"anthropic","vm_pool_id":"pool_1234abcd"}`,
			invoke: func(client *HTTPClient) error {
				return client.UpdateKey(ctx, "key-one", UpdateKeyInput{Scope: &KeyScope{PoolID: "pool_1234abcd"}})
			},
		},
		{
			name: "update allowlist key", method: http.MethodPatch, path: "/api-keys/key-one",
			body: `{"group_type":"anthropic","allowed_vms":["vm-one"],"vm_pool_id":null}`,
			invoke: func(client *HTTPClient) error {
				return client.UpdateKey(ctx, "key-one", UpdateKeyInput{Scope: &KeyScope{AllowedVMs: []string{"vm-one"}}})
			},
		},
		{
			name: "get key", method: http.MethodGet, path: "/api-keys/key-one/stats",
			response: `{"ok":true,"item":{"id":"key-one","name":"name","user_id":null,"status":"active","max_concurrency":7,"group_type":"anthropic","allowed_vms":[],"vm_pool_id":"pool_1234abcd"}}`,
			invoke: func(client *HTTPClient) error {
				key, err := client.GetKey(ctx, "key-one")
				want := KeyInfo{ID: "key-one", Name: "name", Active: true, MaxConcurrency: 7, GroupType: "anthropic", PoolID: "pool_1234abcd", AllowedVMs: []string{}}
				if err == nil && !reflect.DeepEqual(key, want) {
					return fmt.Errorf("key=%+v, want=%+v", key, want)
				}
				return err
			},
		},
		{
			name: "list keys", method: http.MethodGet, path: "/api-keys",
			response: `{"ok":true,"keys":[{"id":"key-one","user_id":null,"status":"disabled","group_type":"anthropic","allowed_vms":["vm-one"],"vm_pool_id":null},{"id":"other","user_id":"user-two","status":"active","group_type":"all","allowed_vms":[]}]}`,
			invoke: func(client *HTTPClient) error {
				keys, err := client.ListKeys(ctx, "")
				if err == nil && (len(keys) != 1 || keys[0].ID != "key-one" || keys[0].Active || keys[0].PoolID != "" || !reflect.DeepEqual(keys[0].AllowedVMs, []string{"vm-one"})) {
					return fmt.Errorf("keys=%+v", keys)
				}
				return err
			},
		},
		{
			name: "list all keys", method: http.MethodGet, path: "/api-keys",
			response: `{"ok":true,"keys":[{"id":"master","user_id":null,"status":"active","group_type":"all","allowed_vms":[]},{"id":"tenant","user_id":"user-one","status":"disabled","group_type":"anthropic","allowed_vms":["vm-one"]}]}`,
			invoke: func(client *HTTPClient) error {
				keys, err := client.ListKeys(ctx, "")
				if err == nil && (len(keys) != 1 || keys[0].UserID != "" || !keys[0].Active) {
					return fmt.Errorf("keys=%+v", keys)
				}
				return err
			},
		},
		{
			name: "list pools", method: http.MethodGet, path: "/vm-pools",
			response: `{"ok":true,"data":{"pools":[{"id":"pool_1234abcd","name":"name","enabled":true,"vm_ids":["vm-one"]},{"id":"pool_abcd1234","name":"empty","enabled":false,"vm_ids":null}]}}`,
			invoke: func(client *HTTPClient) error {
				pools, err := client.ListPools(ctx)
				if err == nil && (len(pools) != 2 || !pools[0].Enabled || !reflect.DeepEqual(pools[0].VMIDs, []string{"vm-one"}) || pools[1].Enabled || pools[1].VMIDs == nil) {
					return fmt.Errorf("pools=%+v", pools)
				}
				return err
			},
		},
		{
			name: "create pool", method: http.MethodPost, path: "/vm-pools", body: `{"name":"name","vm_ids":["vm-one"],"enabled":true}`,
			response: `{"ok":true,"data":{"pool":{"id":"pool_1234abcd","name":"name","enabled":true,"vm_ids":["vm-one"]}}}`,
			invoke: func(client *HTTPClient) error {
				pool, err := client.CreatePool(ctx, "name", []string{"vm-one"})
				if err == nil && (pool.ID != "pool_1234abcd" || pool.Name != "name" || !pool.Enabled || !reflect.DeepEqual(pool.VMIDs, []string{"vm-one"})) {
					return fmt.Errorf("pool=%+v", pool)
				}
				return err
			},
		},
		{
			name: "set pool members", method: http.MethodPatch, path: "/vm-pools/pool_1234abcd", body: `{"vm_ids":[],"enabled":true}`,
			response: `{"ok":true,"data":{"pool":{"id":"pool_1234abcd","name":"name","enabled":true,"vm_ids":[]}}}`,
			invoke: func(client *HTTPClient) error {
				pool, err := client.SetPoolMembers(ctx, "pool_1234abcd", nil)
				if err == nil && (!pool.Enabled || pool.VMIDs == nil || len(pool.VMIDs) != 0) {
					return fmt.Errorf("pool=%+v", pool)
				}
				return err
			},
		},
		{
			name: "delete pool", method: http.MethodDelete, path: "/vm-pools/pool_1234abcd",
			invoke: func(client *HTTPClient) error { return client.DeletePool(ctx, "pool_1234abcd") },
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
				if err == nil && (len(value) != 1 || value[0].ID != "vm-one" || value[0].Label != "Claude slot" || value[0].Tier != "max" || value[0].EmailMasked != "al***@***.test" || !value[0].HasCredential || !value[0].CredentialRejected) {
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
			name: "collect identity", method: http.MethodPost, path: "/vms/vm-one/collect-identity", body: `{}`,
			response: `{"ok":true,"fingerprint":{"device_id":"wrong-envelope"},"data":{"fingerprint":{"device_id":"device-one","guest_machine_id":"machine-one","hostname":"host-one","timezone":"Asia/Tokyo","locale":"en-US","collected_at":"2026-10-31T00:00:00Z"}}}`,
			invoke: func(client *HTTPClient) error {
				value, err := client.CollectIdentity(ctx, "vm-one")
				expected := Fingerprint{DeviceID: "device-one", GuestMachineID: "machine-one", Hostname: "host-one", Timezone: "Asia/Tokyo", Locale: "en-US", CollectedAt: "2026-10-31T00:00:00Z"}
				if err == nil && value != expected {
					return fmt.Errorf("unexpected fingerprint: %+v", value)
				}
				return err
			},
		},
		{
			name: "probe exit", method: http.MethodPost, path: "/vms/vm-one/exit-probe", body: `{}`,
			response: `{"ok":true,"via":"wrong-envelope","data":{"via":"socks5","ip":"wrong-level","timezone":"UTC","geo":{"ip":"203.0.113.10","country_code":"JP","timezone":"Asia/Tokyo"}}}`,
			invoke: func(client *HTTPClient) error {
				value, err := client.ProbeExit(ctx, "vm-one")
				if err == nil && value != (ExitProbe{Via: "socks5", IP: "203.0.113.10", CountryCode: "JP", Timezone: "Asia/Tokyo"}) {
					return fmt.Errorf("unexpected exit probe: %+v", value)
				}
				return err
			},
		},
		{
			name: "reset vm", method: http.MethodPost, path: "/vms/vm-one/reset", body: `{"mode":"traits"}`,
			response: `{"ok":true,"data":{"vm":{"id":"vm-one"},"recreated":true,"mode":"traits"}}`,
			invoke:   func(client *HTTPClient) error { return client.ResetVM(ctx, "vm-one") },
		},
		{
			name: "unbind proxy", method: http.MethodPost, path: "/proxies/proxy-one/unbind", body: `{"vm_id":"vm-one"}`,
			response: `{"ok":true,"data":{"id":"proxy-one","bound_vm_ids":[]}}`,
			invoke:   func(client *HTTPClient) error { return client.UnbindProxy(ctx, "proxy-one", "vm-one") },
		},
		{
			name: "disable proxy", method: http.MethodPost, path: "/proxies/proxy-one/disable", body: `{}`,
			response: `{"ok":true,"data":{"id":"proxy-one","enabled":false}}`,
			invoke:   func(client *HTTPClient) error { return client.DisableProxy(ctx, "proxy-one") },
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
			query:    url.Values{"user_id": {ownerID}, "since": {"2026-10-31T00:00:00Z"}, "until": {"2026-10-31T01:00:00Z"}, "limit": {"10"}, "offset": {"20"}, "api_key_id": {"key-one"}, "vm_id": {"vm-one"}, "status": {"error"}, "model": {"claude"}},
			response: `{"ok":true,"total":31,"limit":10,"offset":20,"items":[{"request_id":"req-one","created_at":"2026-10-31T08:00:00+08:00","model":"claude","api_key_id":"key-one","vm_id":"vm-one","stream":true,"status":429,"error_code":"rate_limit","input_tokens":10,"output_tokens":20,"cache_read_tokens":30,"cache_creation_tokens":40,"total_cost":0.25,"duration_ms":300,"first_token_ms":100}]}`,
			invoke: func(client *HTTPClient) error {
				value, err := client.UserRequestLogs(ctx, ownerID, LogQuery{From: from, Until: until, Page: 3, PageSize: 10, Model: "claude", KeyID: "key-one", VMID: "vm-one", Status: "error"})
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
				if test.name == "create user" || test.name == "create key" || test.name == "create allowlist key" || test.name == "create pool" {
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

func TestHTTPClientRejectsInvalidScopeWithoutRequest(t *testing.T) {
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		t.Error("invalid scope sent upstream")
		_, _ = io.WriteString(w, `{"ok":true}`)
	}))
	defer server.Close()
	client := NewHTTPClient(server.URL, "master", server.Client())
	for _, scope := range []KeyScope{{}, {PoolID: "pool_1234abcd", AllowedVMs: []string{"vm-one"}}} {
		if _, err := client.CreateKey(context.Background(), CreateKeyInput{Scope: scope}); !errors.Is(err, ErrInvalidScope) {
			t.Fatalf("create scope=%+v: %v", scope, err)
		}
		if err := client.UpdateKey(context.Background(), "key-one", UpdateKeyInput{Scope: &scope}); !errors.Is(err, ErrInvalidScope) {
			t.Fatalf("update scope=%+v: %v", scope, err)
		}
	}
	if calls != 0 {
		t.Fatalf("calls=%d", calls)
	}
}

func TestHTTPClientPoolAndKeyErrors(t *testing.T) {
	for _, tc := range []struct {
		code   string
		status int
		invoke func(*HTTPClient) error
	}{
		{"vm_pool_name_taken", 409, func(c *HTTPClient) error { _, err := c.CreatePool(context.Background(), "pool", nil); return err }},
		{"vm_in_other_pool", 409, func(c *HTTPClient) error {
			_, err := c.SetPoolMembers(context.Background(), "pool_1234abcd", []string{"vm-one"})
			return err
		}},
		{"vm_pool_in_use", 409, func(c *HTTPClient) error { return c.DeletePool(context.Background(), "pool_1234abcd") }},
		{"vm_pool_not_found", 404, func(c *HTTPClient) error {
			_, err := c.SetPoolMembers(context.Background(), "pool_1234abcd", nil)
			return err
		}},
		{"vm_pool_not_found", 404, func(c *HTTPClient) error { return c.DeletePool(context.Background(), "pool_1234abcd") }},
		{"vm_pool_vm_unknown", 400, func(c *HTTPClient) error {
			_, err := c.CreatePool(context.Background(), "pool", []string{"unknown"})
			return err
		}},
		{"", 404, func(c *HTTPClient) error { _, err := c.GetKey(context.Background(), "missing"); return err }},
	} {
		t.Run(tc.code, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(tc.status)
				_ = json.NewEncoder(w).Encode(map[string]any{"ok": false, "error": map[string]string{"code": tc.code, "message": "请求失败"}})
			}))
			defer server.Close()
			err := fmt.Errorf("wrapped: %w", tc.invoke(NewHTTPClient(server.URL, "master", server.Client())))
			if !IsStatus(err, tc.status) || ErrorCode(err) != tc.code {
				t.Fatalf("err=%v code=%q", err, ErrorCode(err))
			}
		})
	}
	if ErrorCode(nil) != "" || ErrorCode(errors.New("not upstream")) != "" {
		t.Fatal("unexpected code on ordinary error")
	}
}

func TestHTTPClientMasterUsageQuery(t *testing.T) {
	calls := 0
	from := time.Date(2026, 11, 14, 0, 0, 0, 0, time.UTC)
	until := from.Add(time.Hour)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		q := r.URL.Query()
		if _, exists := q["user_id"]; exists {
			t.Errorf("master view must omit user_id: %s", r.URL)
		}
		if r.Method != http.MethodGet {
			t.Errorf("method=%s", r.Method)
		}
		startKey := "since"
		if r.URL.Path == "/api/panel/billing" {
			startKey = "from"
			if q.Get("group_by") != "vm" && q.Get("group_by") != "key" {
				t.Errorf("group=%q", q.Get("group_by"))
			}
		} else if r.URL.Path == "/api/panel/request-logs" {
			if q.Get("api_key_id") != "key-one" || q.Get("vm_id") != "vm-one" || q.Get("limit") != "10" || q.Get("offset") != "10" {
				t.Errorf("log query=%s", q.Encode())
			}
		} else if r.URL.Path != "/api/panel/request-logs/stats" {
			t.Errorf("path=%s", r.URL.Path)
		}
		if q.Get(startKey) != from.Format(time.RFC3339) || q.Get("until") != until.Format(time.RFC3339) {
			t.Errorf("time query=%s", q.Encode())
		}
		_, _ = io.WriteString(w, `{"ok":true,"data":{}}`)
	}))
	defer server.Close()
	c := NewHTTPClient(server.URL, "master", server.Client())
	if _, err := c.UserBilling(context.Background(), "", from, until); err != nil {
		t.Fatal(err)
	}
	if _, err := c.UserRequestLogs(context.Background(), "", LogQuery{From: from, Until: until, KeyID: "key-one", VMID: "vm-one", Page: 2, PageSize: 10}); err != nil {
		t.Fatal(err)
	}
	if _, err := c.UserRequestStats(context.Background(), "", from, until); err != nil {
		t.Fatal(err)
	}
	if calls != 4 {
		t.Fatalf("calls=%d", calls)
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
	key, err := NewHTTPClient(server.URL, "master", server.Client()).CreateKey(context.Background(), CreateKeyInput{Scope: KeyScope{PoolID: "pool_1234abcd"}})
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

func TestHTTPClientVMIdentityAndProxyFields(t *testing.T) {
	fingerprint := Fingerprint{
		DeviceID: "device-one", GuestMachineID: "machine-one", Hostname: "host-one",
		Timezone: "Asia/Tokyo", Locale: "en-US", CollectedAt: "2026-10-31T00:00:00Z",
	}
	for _, test := range []struct {
		name  string
		raw   string
		proxy *ProxyView
	}{
		{
			name: "enabled proxy",
			raw:  `{"id":"proxy-one","scheme":"socks5","status":"ok","enabled":true,"blocked_reason":"","geo":{"ip":"203.0.113.10","timezone":"Asia/Tokyo"}}`,
			proxy: &ProxyView{ID: "proxy-one", Scheme: "socks5", Status: "ok", Enabled: true,
				GeoIP: "203.0.113.10", GeoTimezone: "Asia/Tokyo"},
		},
		{
			name: "disabled blocked proxy",
			raw:  `{"id":"proxy-one","scheme":"socks5","status":"dead","enabled":false,"blocked_reason":"probe_failed","geo":{"ip":"203.0.113.11","timezone":"America/New_York"}}`,
			proxy: &ProxyView{ID: "proxy-one", Scheme: "socks5", Status: "dead", BlockedReason: "probe_failed",
				GeoIP: "203.0.113.11", GeoTimezone: "America/New_York"},
		},
		{
			name: "enabled omitted and geo null", raw: `{"id":"proxy-one","scheme":"local","status":"unknown","geo":null}`,
			proxy: &ProxyView{ID: "proxy-one", Scheme: "local", Status: "unknown", Enabled: true},
		},
		{
			name: "enabled null and geo omitted", raw: `{"id":"proxy-one","enabled":null}`,
			proxy: &ProxyView{ID: "proxy-one", Enabled: true},
		},
		{name: "unbound proxy", raw: `null`},
	} {
		t.Run(test.name, func(t *testing.T) {
			for _, path := range []string{"/vms", "/vms/vm-one"} {
				t.Run(path, func(t *testing.T) {
					server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
						if request.Method != http.MethodGet || request.URL.Path != "/api/panel"+path || request.URL.RawQuery != "" {
							t.Errorf("unexpected request: %s %s", request.Method, request.URL)
						}
						vm := `{"id":"vm-one","fingerprint":{"device_id":"device-one","guest_machine_id":"machine-one","hostname":"host-one","timezone":"Asia/Tokyo","locale":"en-US","collected_at":"2026-10-31T00:00:00Z"},"timezone":"Asia/Singapore","locale":"zh-CN","email":"alice@example.test","account_uuid":"account-one","proxy":` + test.raw + `}`
						if path == "/vms" {
							_, _ = io.WriteString(writer, `{"ok":true,"items":[],"data":{"items":[`+vm+`]}}`)
						} else {
							_, _ = io.WriteString(writer, `{"ok":true,"vm":{"id":"wrong-envelope"},"data":{"vm":`+vm+`}}`)
						}
					}))
					defer server.Close()
					client := NewHTTPClient(server.URL, "master", server.Client())
					var vm VM
					if path == "/vms" {
						items, err := client.ListVMs(context.Background(), VMFilter{})
						if err != nil || len(items) != 1 {
							t.Fatalf("list vms = %+v, %v", items, err)
						}
						vm = items[0]
					} else {
						usage, err := client.GetAccountUsage(context.Background(), "vm-one")
						if err != nil {
							t.Fatal(err)
						}
						vm = usage.VM
					}
					if vm.ID != "vm-one" || vm.Fingerprint != fingerprint || vm.Timezone != "Asia/Singapore" || vm.Locale != "zh-CN" || vm.Email != "alice@example.test" || vm.EmailMasked != "al***@***.test" || vm.AccountUUID != "account-one" {
						t.Fatalf("unexpected vm identity: %+v", vm)
					}
					if !reflect.DeepEqual(vm.Proxy, test.proxy) {
						t.Fatalf("proxy = %+v, want %+v", vm.Proxy, test.proxy)
					}
				})
			}
		})
	}
}

func TestHTTPClientSelfcheckEscapedPaths(t *testing.T) {
	ctx := context.Background()
	for _, test := range []struct {
		name   string
		path   string
		body   string
		invoke func(*HTTPClient) error
	}{
		{name: "collect identity", path: "/vms/slot%20%3F%23/collect-identity", body: `{}`, invoke: func(client *HTTPClient) error {
			_, err := client.CollectIdentity(ctx, "slot ?#")
			return err
		}},
		{name: "probe exit", path: "/vms/slot%20%3F%23/exit-probe", body: `{}`, invoke: func(client *HTTPClient) error {
			_, err := client.ProbeExit(ctx, "slot ?#")
			return err
		}},
		{name: "reset vm", path: "/vms/slot%20%3F%23/reset", body: `{"mode":"traits"}`, invoke: func(client *HTTPClient) error {
			return client.ResetVM(ctx, "slot ?#")
		}},
		{name: "unbind proxy", path: "/proxies/proxy%20%3F%23/unbind", body: `{"vm_id":"slot ?#"}`, invoke: func(client *HTTPClient) error {
			return client.UnbindProxy(ctx, "proxy ?#", "slot ?#")
		}},
		{name: "disable proxy", path: "/proxies/proxy%20%3F%23/disable", body: `{}`, invoke: func(client *HTTPClient) error {
			return client.DisableProxy(ctx, "proxy ?#")
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
				if request.Method != http.MethodPost || request.URL.EscapedPath() != "/api/panel"+test.path || request.URL.RawQuery != "" {
					t.Errorf("unexpected request: %s %s", request.Method, request.URL)
				}
				body, err := io.ReadAll(request.Body)
				if err != nil {
					t.Error(err)
				}
				if !reflect.DeepEqual(decodeTestJSON(t, string(body)), decodeTestJSON(t, test.body)) {
					t.Errorf("body = %s, want %s", body, test.body)
				}
				_, _ = io.WriteString(writer, `{"ok":true,"data":{}}`)
			}))
			defer server.Close()
			if err := test.invoke(NewHTTPClient(server.URL, "master", server.Client())); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestFakeHTTPSelfcheckRoundTrip(t *testing.T) {
	ctx := context.Background()
	fake := NewFake()
	fake.AddVM(VM{ID: "vm-one", HasCredential: true, CredStatus: "valid", Timezone: "Asia/Tokyo", Locale: "en-US"})
	fake.SetAccount("vm-one", "account-one", "alice@example.test")
	server := httptest.NewServer(fake.Handler("master"))
	defer server.Close()
	client := NewHTTPClient(server.URL, "master", server.Client())
	readVM := func() VM {
		t.Helper()
		usage, err := client.GetAccountUsage(ctx, "vm-one")
		if err != nil {
			t.Fatal(err)
		}
		items, err := client.ListVMs(ctx, VMFilter{})
		if err != nil || len(items) != 1 || !reflect.DeepEqual(items[0], usage.VM) {
			t.Fatalf("list/detail mismatch: %+v, %+v, %v", items, usage.VM, err)
		}
		return usage.VM
	}
	vm := readVM()
	if vm.Fingerprint.DeviceID == "" || vm.Proxy != nil || vm.ProxyConfigured || vm.AccountUUID != "account-one" || vm.Email != "alice@example.test" || vm.EmailMasked != "al***@***.test" || vm.Timezone != "Asia/Tokyo" || vm.Locale != "en-US" {
		t.Fatalf("unexpected initial vm: %+v", vm)
	}
	fingerprint, err := client.CollectIdentity(ctx, vm.ID)
	if err != nil || fingerprint.DeviceID != vm.Fingerprint.DeviceID || fingerprint.GuestMachineID != vm.Fingerprint.GuestMachineID || fingerprint.Hostname != vm.Fingerprint.Hostname || fingerprint.CollectedAt == "" {
		t.Fatalf("collect identity = %+v, %v", fingerprint, err)
	}
	proxy := ProxyView{ID: "proxy-one", Scheme: "socks5", Status: "ok", Enabled: true, GeoIP: "203.0.113.10", GeoTimezone: "Asia/Tokyo"}
	fake.QueueProxies(proxy)
	if err := client.AllocateProxy(ctx, vm.ID); err != nil {
		t.Fatal(err)
	}
	vm = readVM()
	if !vm.ProxyConfigured || !reflect.DeepEqual(vm.Proxy, &proxy) {
		t.Fatalf("allocated proxy = %+v", vm)
	}
	probe, err := client.ProbeExit(ctx, vm.ID)
	if err != nil || probe != (ExitProbe{Via: proxy.Scheme, IP: proxy.GeoIP, Timezone: proxy.GeoTimezone}) {
		t.Fatalf("probe exit = %+v, %v", probe, err)
	}
	if err := client.ResetVM(ctx, vm.ID); err != nil {
		t.Fatal(err)
	}
	reset := readVM()
	if fake.ResetCalls(vm.ID) != 1 || reset.ID != vm.ID || reset.Fingerprint.DeviceID == fingerprint.DeviceID || reset.Fingerprint.GuestMachineID == fingerprint.GuestMachineID || reset.Fingerprint.Hostname == fingerprint.Hostname || reset.AccountUUID != "" || reset.Email != "" || reset.EmailMasked != "" || reset.HasCredential || reset.UnschedulableCode != "no_credential" || !reflect.DeepEqual(reset.Proxy, &proxy) {
		t.Fatalf("unexpected reset vm: %+v", reset)
	}
	if err := client.DisableProxy(ctx, proxy.ID); err != nil {
		t.Fatal(err)
	}
	vm = readVM()
	proxy.Enabled = false
	if !fake.ProxyDisabled(proxy.ID) || !reflect.DeepEqual(vm.Proxy, &proxy) {
		t.Fatalf("disabled proxy = %+v", vm.Proxy)
	}
	if err := client.UnbindProxy(ctx, proxy.ID, vm.ID); err != nil {
		t.Fatal(err)
	}
	vm = readVM()
	if vm.Proxy != nil || vm.ProxyConfigured {
		t.Fatalf("proxy still bound: %+v", vm)
	}
	proxy.Enabled = true
	fake.QueueProxies(proxy)
	if err := client.AllocateProxy(ctx, vm.ID); err != nil {
		t.Fatal(err)
	}
	vm = readVM()
	if vm.Proxy == nil || vm.Proxy.ID == proxy.ID || !vm.Proxy.Enabled {
		t.Fatalf("disabled proxy was reused: %+v", vm.Proxy)
	}
}

func TestFakeHTTPSelfcheckWire(t *testing.T) {
	for _, test := range []struct {
		name string
		path string
		body string
		data string
	}{
		{name: "collect identity", path: "/vms/vm-one/collect-identity", body: `{}`,
			data: `{"fingerprint":{"device_id":"device-one","guest_machine_id":"machine-one","hostname":"host-one","timezone":"Asia/Tokyo","locale":"en-US","collected_at":"2026-10-31T00:00:00Z"}}`},
		{name: "probe exit", path: "/vms/vm-one/exit-probe", body: `{}`,
			data: `{"via":"socks5","geo":{"ip":"203.0.113.10","country_code":"JP","timezone":"Asia/Tokyo"}}`},
		{name: "reset vm", path: "/vms/vm-one/reset", body: `{"mode":"traits"}`},
		{name: "unbind proxy", path: "/proxies/proxy-one/unbind", body: `{"vm_id":"vm-one"}`, data: `{"id":"proxy-one"}`},
		{name: "disable proxy", path: "/proxies/proxy-one/disable", body: `{}`, data: `{"id":"proxy-one","enabled":false}`},
	} {
		t.Run(test.name, func(t *testing.T) {
			fake := NewFake()
			fake.now = func() time.Time { return time.Date(2026, 10, 31, 0, 0, 0, 0, time.UTC) }
			fake.AddVM(VM{ID: "vm-one", Fingerprint: Fingerprint{DeviceID: "device-one", GuestMachineID: "machine-one", Hostname: "host-one", Timezone: "Asia/Tokyo", Locale: "en-US"}})
			fake.SetProxy("vm-one", &ProxyView{ID: "proxy-one", Scheme: "socks5", Status: "ok", Enabled: true})
			fake.SetExitProbe("vm-one", ExitProbe{Via: "socks5", IP: "203.0.113.10", CountryCode: "JP", Timezone: "Asia/Tokyo"}, nil)
			payload := fakeTestRaw(t, fake.Handler("master"), http.MethodPost, "/api/panel"+test.path, "master", test.body, http.StatusOK)
			if payload["ok"] != true || payload["data"] == nil || len(payload) != 2 {
				t.Fatalf("unexpected envelope: %+v", payload)
			}
			if test.data != "" {
				if !reflect.DeepEqual(payload["data"], decodeTestJSON(t, test.data)) {
					t.Fatalf("data = %+v, want %s", payload["data"], test.data)
				}
			} else {
				data := payload["data"].(map[string]any)
				vm, ok := data["vm"].(map[string]any)
				if !ok || vm["id"] != "vm-one" || data["mode"] != "traits" || data["recreated"] != true || fake.ResetCalls("vm-one") != 1 {
					t.Fatalf("unexpected reset response: %+v", data)
				}
			}
		})
	}
}

func TestFakeHTTPSelfcheckErrors(t *testing.T) {
	for _, test := range []struct {
		name   string
		method string
		path   string
		body   string
		status int
		code   string
	}{
		{name: "collect missing vm", path: "/vms/missing/collect-identity", body: `{}`, status: 404, code: "vm_not_found"},
		{name: "probe missing vm", path: "/vms/missing/exit-probe", body: `{}`, status: 404, code: "vm_not_found"},
		{name: "probe without proxy", path: "/vms/unbound/exit-probe", body: `{}`, status: 502, code: "proxy_required"},
		{name: "probe failure", path: "/vms/vm-one/exit-probe", body: `{}`, status: 502, code: "exit_probe_failed"},
		{name: "reset missing vm", path: "/vms/missing/reset", body: `{"mode":"traits"}`, status: 404, code: "vm_not_found"},
		{name: "reset malformed body", path: "/vms/vm-one/reset", body: `{`, status: 400, code: "invalid_json"},
		{name: "reset unsupported mode", path: "/vms/vm-one/reset", body: `{"mode":"unknown"}`, status: 400, code: "unsupported_mode"},
		{name: "unbind missing vm", path: "/proxies/proxy-one/unbind", body: `{"vm_id":"missing"}`, status: 404, code: "vm_not_found"},
		{name: "unbind wrong proxy", path: "/proxies/missing/unbind", body: `{"vm_id":"vm-one"}`, status: 404, code: "proxy_not_found"},
		{name: "unbind missing vm id", path: "/proxies/proxy-one/unbind", body: `{}`, status: 400, code: "missing_field"},
		{name: "unbind malformed body", path: "/proxies/proxy-one/unbind", body: `{`, status: 400, code: "invalid_json"},
		{name: "collect wrong method", method: http.MethodGet, path: "/vms/vm-one/collect-identity", status: 404, code: "route_not_found"},
		{name: "probe wrong method", method: http.MethodGet, path: "/vms/vm-one/exit-probe", status: 404, code: "route_not_found"},
		{name: "reset wrong method", method: http.MethodGet, path: "/vms/vm-one/reset", status: 404, code: "route_not_found"},
		{name: "unbind wrong method", method: http.MethodGet, path: "/proxies/proxy-one/unbind", status: 404, code: "route_not_found"},
		{name: "disable wrong method", method: http.MethodGet, path: "/proxies/proxy-one/disable", status: 404, code: "route_not_found"},
		{name: "disable extra suffix", path: "/proxies/proxy-one/disable/extra", status: 404, code: "route_not_found"},
	} {
		t.Run(test.name, func(t *testing.T) {
			fake := NewFake()
			fake.AddVM(VM{ID: "vm-one"})
			fake.AddVM(VM{ID: "unbound"})
			fake.SetProxy("vm-one", &ProxyView{ID: "proxy-one", Enabled: true})
			fake.SetExitProbe("vm-one", ExitProbe{}, &Error{Status: http.StatusBadGateway, Code: "exit_probe_failed", Message: "probe failed"})
			method := test.method
			if method == "" {
				method = http.MethodPost
			}
			payload := fakeTestRaw(t, fake.Handler("master"), method, "/api/panel"+test.path, "master", test.body, test.status)
			panelError, ok := payload["error"].(map[string]any)
			if payload["ok"] != false || payload["data"] != nil || !ok || panelError["code"] != test.code || panelError["message"] == "" {
				t.Fatalf("unexpected error envelope: %+v", payload)
			}
			if fake.ResetCalls("vm-one") != 0 || fake.ProxyDisabled("proxy-one") {
				t.Fatal("failed request mutated self-check state")
			}
		})
	}
}

// vm2api emits reset times as RFC3339, epoch milliseconds, or (reset_5h,
// copied from Anthropic's headers) epoch seconds as a string. All must parse,
// and an odd value must not fail the response it belongs to.
func TestPanelTimeAcceptsEveryVm2apiShape(t *testing.T) {
	want := time.Date(2026, 11, 13, 13, 50, 0, 0, time.UTC)
	for _, raw := range []string{
		`"2026-11-13T13:50:00Z"`, `"2026-11-13T21:50:00+08:00"`,
		`1794577800`, `"1794577800"`, `1794577800000`, `"1794577800000"`, `1794577800.0`,
	} {
		var value panelTime
		if err := json.Unmarshal([]byte(raw), &value); err != nil || !value.Time.Equal(want) {
			t.Errorf("%s: time=%v err=%v", raw, value.Time, err)
		}
	}
	for _, raw := range []string{`null`, `""`, `"  "`, `0`, `-5`, `"soon"`, `true`, `{}`} {
		var value panelTime
		if err := json.Unmarshal([]byte(raw), &value); err != nil || !value.Time.IsZero() {
			t.Errorf("%s: time=%v err=%v", raw, value.Time, err)
		}
	}
	var vm panelVM
	if err := json.Unmarshal([]byte(`{"id":"vm-1","reset_5h":"1794577800","reset_7d":"later"}`), &vm); err != nil || !vm.Reset5h.Time.Equal(want) || !vm.Reset7d.Time.IsZero() {
		t.Fatalf("vm=%+v err=%v", vm, err)
	}
}
