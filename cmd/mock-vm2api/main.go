package main

import (
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/DueGin/seat-hub/internal/vm2api"
)

func main() {
	listen := os.Getenv("PORTAL_MOCK_VM2API_LISTEN")
	if listen == "" {
		listen = ":8788"
	}
	masterKey := os.Getenv("PORTAL_MOCK_VM2API_MASTER_KEY")
	if masterKey == "" {
		masterKey = "mock-master-key"
	}
	fake := vm2api.NewFake()
	// PORTAL_MOCK_SELFCHECK=hostile makes every slot share one fingerprint and
	// exit, so self-check can never pass: useful to watch a slot exhaust its
	// rebuild budget in a smoke test.
	if os.Getenv("PORTAL_MOCK_SELFCHECK") == "hostile" {
		fake.SetHostileSelfcheck(true)
		log.Printf("模拟 vm2api 进入敌意自检模式：所有槽位共用指纹与出口")
	}
	// Platform accounts for the rental pool. Each sits behind its own socks5
	// exit and holds its own Claude account, so pool admission self-check passes.
	for i, tier := range []string{"pro", "max", "pro"} {
		n := fmt.Sprintf("%03d", i+1)
		fake.AddVM(vm2api.VM{
			ID: "mock-claude-" + n, Label: "模拟 Claude 账号 " + n, Platform: "anthropic",
			Origin: "platform", Tier: tier, CredStatus: "valid", HasCredential: true,
			ScheduleState: "on", ProxyConfigured: true, SeatsMax: 2,
			EmailMasked: "ren***" + n + "@example.com", Email: "rental" + n + "@example.com",
			AccountUUID: "acct-mock-" + n, Timezone: "Asia/Tokyo",
			Proxy: &vm2api.ProxyView{
				ID: "px-mock-" + n, Scheme: "socks5", Status: "ok", Enabled: true,
				GeoIP: fmt.Sprintf("198.51.100.%d", i+11), GeoTimezone: "Asia/Tokyo",
			},
			FiveHour: &vm2api.Window{Status: "allowed"}, SevenDay: &vm2api.Window{Status: "allowed"},
		})
	}
	// The panel side is the Fake; /v1 is a small echo so the gateway can be
	// exercised locally without a real vm2api (the Fake has no protocol layer).
	mux := http.NewServeMux()
	mux.Handle("/v1/", devProtocol(masterKey, fake))
	// Register the panel handler for its real prefix too: a catch-all "/" does
	// not match a more specific "/api/" pattern, so it would shadow /api/panel.
	mux.Handle("/api/", fake.Handler(masterKey))
	mux.Handle("/", fake.Handler(masterKey))
	server := &http.Server{Addr: listen, Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	log.Printf("模拟 vm2api 服务监听 %s", listen)
	log.Fatal(server.ListenAndServe())
}

// devProtocol answers protocol calls with a fixed Anthropic-shaped body and
// rejects anything that does not present a vm2api key, so a locally issued
// portal key can be checked all the way through the gateway.
func devProtocol(masterKey string, fake *vm2api.Fake) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		key := r.Header.Get("x-api-key")
		if bearer, found := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer "); found {
			key = bearer
		}
		// Accept the master key and any portal-issued vm2api key; the real
		// service validates the latter against its own key store.
		if key != masterKey && !strings.HasPrefix(key, "sk-vm-") {
			writeProtocolError(w, http.StatusUnauthorized, "authentication_error", "invalid api key (mock)")
			return
		}
		if r.URL.Path == "/v1/models" {
			w.Header().Set("Content-Type", "application/json; charset=utf-8")
			_, _ = w.Write([]byte(`{"data":[{"id":"claude-sonnet-5","type":"model"}]}`))
			return
		}
		started := time.Now()
		body, _ := io.ReadAll(io.LimitReader(r.Body, 8<<20))
		var request struct {
			Model  string `json:"model"`
			Stream bool   `json:"stream"`
		}
		_ = json.Unmarshal(body, &request)
		if request.Model == "" {
			request.Model = "claude-sonnet-5"
		}
		// Portal-issued keys are logged against one of their owner's slots,
		// so the portal's usage and records pages have real rows locally.
		if key != masterKey {
			status, known := fake.RecordProtocolCall(vm2api.ProtocolCall{
				Secret: key, Model: request.Model, Stream: request.Stream,
				InputTokens: int64(max(1, len(body)/4)), OutputTokens: 12,
				DurationMS: max(1, time.Since(started).Milliseconds()) + 420,
			})
			if !known {
				writeProtocolError(w, http.StatusUnauthorized, "authentication_error", "invalid api key (mock)")
				return
			}
			if status != http.StatusOK {
				writeProtocolError(w, status, "overloaded_error", "no account available for this key (mock)")
				return
			}
		}
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		reply, _ := json.Marshal(map[string]any{
			"id": "msg_mock", "type": "message", "role": "assistant", "model": request.Model,
			"content":     []map[string]string{{"type": "text", "text": "mock reply"}},
			"stop_reason": "end_turn", "usage": map[string]int{"input_tokens": max(1, len(body)/4), "output_tokens": 12},
		})
		_, _ = w.Write(reply)
	})
}

func writeProtocolError(w http.ResponseWriter, status int, kind, message string) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	body, _ := json.Marshal(map[string]any{"type": "error", "error": map[string]string{"type": kind, "message": message}})
	_, _ = w.Write(body)
}
