package main

import (
	"log"
	"net/http"
	"os"
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
	fake.AddVM(vm2api.VM{
		ID: "mock-claude-001", Label: "模拟 Claude 账号", Platform: "anthropic",
		Origin: "platform", Tier: "pro", CredStatus: "valid", HasCredential: true,
		ScheduleState: "on", ProxyConfigured: true, SeatsMax: 2,
		FiveHour: &vm2api.Window{Status: "allowed"}, SevenDay: &vm2api.Window{Status: "allowed"},
	})
	server := &http.Server{Addr: listen, Handler: fake.Handler(masterKey), ReadHeaderTimeout: 5 * time.Second}
	log.Printf("模拟 vm2api 服务监听 %s", listen)
	log.Fatal(server.ListenAndServe())
}
