package main

import (
	"log"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/DueGin/vm2api-portal/internal/vm2api"
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
	fake.AddVM(vm2api.VM{
		ID: "mock-claude-001", Label: "模拟 Claude 账号", Platform: "anthropic",
		Origin: "platform", Tier: "pro", CredStatus: "valid", HasCredential: true,
		ScheduleState: "on", ProxyConfigured: true, SeatsMax: 2,
		FiveHour: &vm2api.Window{Status: "allowed"}, SevenDay: &vm2api.Window{Status: "allowed"},
	})
	// The panel side is the Fake; /v1 is a small echo so the gateway can be
	// exercised locally without a real vm2api (the Fake has no protocol layer).
	mux := http.NewServeMux()
	mux.Handle("/v1/", devProtocol(masterKey))
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
func devProtocol(masterKey string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		key := r.Header.Get("x-api-key")
		if bearer, found := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer "); found {
			key = bearer
		}
		// Accept the master key and any portal-issued vm2api key; the real
		// service validates the latter against its own key store.
		if key != masterKey && !strings.HasPrefix(key, "sk-vm-") {
			w.Header().Set("Content-Type", "application/json; charset=utf-8")
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"type":"error","error":{"type":"authentication_error","message":"invalid api key (mock)"}}`))
			return
		}
		if r.URL.Path == "/v1/models" {
			w.Header().Set("Content-Type", "application/json; charset=utf-8")
			_, _ = w.Write([]byte(`{"data":[{"id":"claude-sonnet-5","type":"model"}]}`))
			return
		}
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		_, _ = w.Write([]byte(`{"id":"msg_mock","type":"message","role":"assistant","model":"claude-sonnet-5",` +
			`"content":[{"type":"text","text":"mock reply"}],"stop_reason":"end_turn",` +
			`"usage":{"input_tokens":3,"output_tokens":2}}`))
	})
}
