package provision_test

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/DueGin/seat-hub/internal/vm2api"
)

var _ vm2api.Client = (*vm2api.Fake)(nil)

func TestContractHostedProvisioning(t *testing.T) {
	ctx := context.Background()
	var client vm2api.Client = vm2api.NewFake()
	user, err := client.CreateUser(ctx, "contract-user", "password")
	if err != nil {
		t.Fatal(err)
	}
	key, err := client.CreateKey(ctx, vm2api.CreateKeyInput{Name: "托管密钥", UserID: user.ID, MaxConcurrency: 10})
	if err != nil || key.ID == "" || !strings.HasPrefix(key.Secret, "sk-vm-") {
		t.Fatalf("create key = %+v, %v", key, err)
	}
	vm, err := client.CreateHostedVM(ctx, "托管账号", json.RawMessage(`{"platform":"anthropic","runtime":"container"}`))
	if err != nil {
		t.Fatal(err)
	}
	if err := client.SetVMOwner(ctx, vm.ID, &user.ID); err != nil {
		t.Fatal(err)
	}
	if err := client.AllocateProxy(ctx, vm.ID); err != nil {
		t.Fatal(err)
	}
	before := time.Now().UTC()
	auth, err := client.GenerateAuthURL(ctx, vm.ID)
	if err != nil || auth.SessionID == "" || auth.URL == "" || auth.ExpiresAt.Before(before.Add(29*time.Minute)) || auth.ExpiresAt.After(time.Now().UTC().Add(30*time.Minute)) {
		t.Fatalf("auth url = %+v, %v", auth, err)
	}
	if err := client.ExchangeCode(ctx, vm.ID, auth.SessionID, "contract-code"); err != nil {
		t.Fatal(err)
	}
	for _, expected := range []string{"pending", "running", "succeeded", "succeeded"} {
		status, err := client.BootstrapStatus(ctx, vm.ID)
		if err != nil || status.State != expected {
			t.Fatalf("bootstrap state = %+v, %v; want %s", status, err, expected)
		}
	}
	if err := client.SetSchedulable(ctx, vm.ID, true); err != nil {
		t.Fatal(err)
	}
	usage, err := client.GetAccountUsage(ctx, vm.ID)
	if err != nil || usage.VM.OwnerUserID != user.ID || usage.VM.Origin != "admin_assigned" || !usage.VM.HasCredential || !usage.VM.ProxyConfigured || usage.VM.ScheduleState != "on" {
		t.Fatalf("provisioned usage = %+v, %v", usage, err)
	}
	if err := client.SetSchedulable(ctx, vm.ID, false); err != nil {
		t.Fatal(err)
	}
	if err := client.SetVMOwner(ctx, vm.ID, nil); err != nil {
		t.Fatal(err)
	}
	owned, err := client.ListVMs(ctx, vm2api.VMFilter{OwnerUserID: &user.ID})
	if err != nil || len(owned) != 0 {
		t.Fatalf("owned after reclaim = %+v, %v", owned, err)
	}
	pool, err := client.ListVMs(ctx, vm2api.VMFilter{Unowned: true, ClaudeOnly: true})
	if err != nil || len(pool) != 1 || pool[0].ID != vm.ID || pool[0].Origin != "platform" {
		t.Fatalf("reclaimed pool = %+v, %v", pool, err)
	}
}

func TestContractOwnerIsolationAndMissingVM(t *testing.T) {
	ctx := context.Background()
	var client vm2api.Client = vm2api.NewFake()
	first, err := client.CreateUser(ctx, "first", "password")
	if err != nil {
		t.Fatal(err)
	}
	second, err := client.CreateUser(ctx, "second", "password")
	if err != nil {
		t.Fatal(err)
	}
	vm, err := client.CreateHostedVM(ctx, "专属账号", nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := client.SetVMOwner(ctx, vm.ID, &first.ID); err != nil {
		t.Fatal(err)
	}
	foreign, err := client.ListVMs(ctx, vm2api.VMFilter{OwnerUserID: &second.ID})
	if err != nil || len(foreign) != 0 {
		t.Fatalf("foreign vms = %+v, %v", foreign, err)
	}
	if err := client.DeleteVM(ctx, vm.ID); err != nil {
		t.Fatal(err)
	}
	_, err = client.GetAccountUsage(ctx, vm.ID)
	if !vm2api.IsStatus(err, 404) {
		t.Fatalf("deleted vm error = %v, want status 404", err)
	}
}
