package vm2api

import (
	"context"
	"errors"
	"fmt"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"
	"time"
)

func TestFakePoolRules(t *testing.T) {
	ctx := context.Background()
	fake := NewFake()
	fake.AddVM(VM{ID: "a"})
	fake.AddVM(VM{ID: "b"})
	pool, err := fake.CreatePool(ctx, "  first  ", []string{"b", "a", "a"})
	if err != nil || pool.Name != "first" || !pool.Enabled || !regexp.MustCompile(`^pool_[a-f0-9]{8}$`).MatchString(pool.ID) || len(pool.VMIDs) != 2 || pool.VMIDs[0] != "a" {
		t.Fatalf("pool=%+v err=%v", pool, err)
	}
	for _, tc := range []struct {
		name   string
		ids    []string
		status int
		code   string
	}{
		{"first", nil, 409, "vm_pool_name_taken"},
		{"second", []string{"a"}, 409, "vm_in_other_pool"},
		{"unknown", []string{"missing"}, 400, "vm_pool_vm_unknown"},
		{" ", nil, 400, "invalid_vm_pool_name"},
	} {
		_, err := fake.CreatePool(ctx, tc.name, tc.ids)
		if !IsStatus(err, tc.status) || !IsCode(err, tc.code) {
			t.Errorf("%s: %v", tc.name, err)
		}
	}
	if err := fake.DeleteVM(ctx, "a"); err != nil {
		t.Fatal(err)
	}
	pools, err := fake.ListPools(ctx)
	if err != nil || len(pools) != 1 || len(pools[0].VMIDs) != 1 || pools[0].VMIDs[0] != "b" {
		t.Fatalf("pools=%+v err=%v", pools, err)
	}
	owner, _ := fake.CreateUser(ctx, "owner", "password")
	key, err := fake.CreateKey(ctx, CreateKeyInput{UserID: owner.ID, Scope: KeyScope{PoolID: pool.ID}})
	if err != nil {
		t.Fatal(err)
	}
	active := false
	if err := fake.UpdateKey(ctx, key.ID, UpdateKeyInput{Active: &active}); err != nil {
		t.Fatal(err)
	}
	if err := fake.DeletePool(ctx, pool.ID); !IsStatus(err, 409) || !IsCode(err, "vm_pool_in_use") {
		t.Fatalf("referenced pool: %v", err)
	}
	if err := fake.DeleteKey(ctx, key.ID); err != nil {
		t.Fatal(err)
	}
	if err := fake.DeletePool(ctx, pool.ID); err != nil {
		t.Fatal(err)
	}
	if err := fake.DeletePool(ctx, pool.ID); !IsStatus(err, 404) || !IsCode(err, "vm_pool_not_found") {
		t.Fatalf("missing pool: %v", err)
	}
}

func TestFakePoolsHTTPRoundTrip(t *testing.T) {
	ctx := context.Background()
	fake := NewFake()
	fake.AddVM(VM{ID: "a"})
	fake.AddVM(VM{ID: "b"})
	server := httptest.NewServer(fake.Handler("master"))
	defer server.Close()
	client := NewHTTPClient(server.URL, "master", server.Client())
	owner := fakeTestUser(t, client, "owner")
	pool, err := client.CreatePool(ctx, "  owner  ", []string{"a"})
	if err != nil || pool.Name != "owner" || !pool.Enabled {
		t.Fatalf("pool=%+v err=%v", pool, err)
	}
	if _, err := client.CreatePool(ctx, "owner", nil); !IsStatus(err, 409) || ErrorCode(err) != "vm_pool_name_taken" {
		t.Fatalf("duplicate: %v", err)
	}
	other, err := client.CreatePool(ctx, "other", []string{"b"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := client.SetPoolMembers(ctx, pool.ID, []string{"b"}); !IsStatus(err, 409) || ErrorCode(err) != "vm_in_other_pool" {
		t.Fatalf("overlap: %v", err)
	}
	if _, err := client.SetPoolMembers(ctx, pool.ID, []string{"missing"}); !IsStatus(err, 400) || ErrorCode(err) != "vm_pool_vm_unknown" {
		t.Fatalf("unknown vm: %v", err)
	}
	key, err := client.CreateKey(ctx, CreateKeyInput{UserID: owner.ID, Name: "pool key", MaxConcurrency: 8, Scope: KeyScope{PoolID: pool.ID}})
	if err != nil {
		t.Fatal(err)
	}
	info, err := client.GetKey(ctx, key.ID)
	if err != nil || info.PoolID != pool.ID || info.UserID != owner.ID || !info.Active || info.GroupType != "anthropic" || info.MaxConcurrency != 8 {
		t.Fatalf("info=%+v err=%v", info, err)
	}
	if err := client.DeletePool(ctx, pool.ID); !IsStatus(err, 409) || ErrorCode(err) != "vm_pool_in_use" {
		t.Fatalf("in use: %v", err)
	}
	fakeTestRaw(t, fake.Handler("master"), "PATCH", "/api/panel/vm-pools/"+pool.ID, "master", `{"enabled":false}`, 200)
	pool, err = client.SetPoolMembers(ctx, pool.ID, nil)
	if err != nil || !pool.Enabled || pool.VMIDs == nil || len(pool.VMIDs) != 0 {
		t.Fatalf("cleared pool=%+v err=%v", pool, err)
	}
	selected := KeyScope{AllowedVMs: []string{"a"}}
	if err := client.UpdateKey(ctx, key.ID, UpdateKeyInput{Scope: &selected}); err != nil {
		t.Fatal(err)
	}
	info, err = client.GetKey(ctx, key.ID)
	if err != nil || info.PoolID != "" || len(info.AllowedVMs) != 1 || info.AllowedVMs[0] != "a" {
		t.Fatalf("selected info=%+v err=%v", info, err)
	}
	all, err := client.ListKeys(ctx, "")
	if err != nil || len(all) != 1 || all[0].ID != key.ID {
		t.Fatalf("keys=%+v err=%v", all, err)
	}
	if _, err := client.ListKeys(ctx, "other"); err != nil {
		t.Fatal(err)
	}
	if err := client.DeletePool(ctx, pool.ID); err != nil {
		t.Fatal(err)
	}
	if err := client.DeletePool(ctx, other.ID); err != nil {
		t.Fatal(err)
	}
	pools, err := client.ListPools(ctx)
	if err != nil || pools == nil || len(pools) != 0 {
		t.Fatalf("pools=%+v err=%v", pools, err)
	}
	if _, err := client.SetPoolMembers(ctx, pool.ID, nil); !IsStatus(err, 404) || ErrorCode(err) != "vm_pool_not_found" {
		t.Fatalf("missing pool: %v", err)
	}
	if err := client.DeleteKey(ctx, key.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := client.GetKey(ctx, key.ID); !IsStatus(err, 404) {
		t.Fatalf("missing key: %v", err)
	}
}

func TestFakePoolBoundariesAndIsolation(t *testing.T) {
	ctx := context.Background()
	fake := NewFake()
	fake.AddVM(VM{ID: "a"})
	fake.AddVM(VM{ID: "b"})
	if _, err := fake.CreatePool(ctx, strings.Repeat("x", 41), nil); !IsCode(err, "invalid_vm_pool_name") {
		t.Fatalf("long name: %v", err)
	}
	pool, err := fake.CreatePool(ctx, strings.Repeat("x", 40), []string{"a"})
	if err != nil {
		t.Fatal(err)
	}
	pool.VMIDs[0] = "b"
	pools, err := fake.ListPools(ctx)
	if err != nil || pools[0].VMIDs[0] != "a" {
		t.Fatalf("mutated members: %+v, %v", pools, err)
	}
	pools[0].VMIDs[0] = "b"
	pools, err = fake.ListPools(ctx)
	if err != nil || pools[0].VMIDs[0] != "a" {
		t.Fatalf("mutated list: %+v, %v", pools, err)
	}
	if _, err := fake.SetPoolMembers(ctx, pool.ID, []string{"missing"}); !IsCode(err, "vm_pool_vm_unknown") {
		t.Fatalf("invalid update: %v", err)
	}
	pools, _ = fake.ListPools(ctx)
	if pools[0].VMIDs[0] != "a" {
		t.Fatal("invalid update changed members")
	}
}

func TestFakeMasterUsageFilters(t *testing.T) {
	ctx := context.Background()
	fake := NewFake()
	a := fakeTestUser(t, fake, "a")
	b := fakeTestUser(t, fake, "b")
	from := time.Now().Add(-time.Hour)
	until := time.Now().Add(time.Hour)
	for i, owner := range []User{a, a, b} {
		keyID := fmt.Sprintf("key-%d", i)
		fakeTestSeedLog(t, fake, owner.ID, LogRow{KeyID: keyID, VMID: "vm", StatusCode: 200, InputTokens: 10, OutputTokens: 2, CostUSD: 0.5})
	}
	server := httptest.NewServer(fake.Handler("master"))
	defer server.Close()
	for _, client := range []Client{fake, NewHTTPClient(server.URL, "master", server.Client())} {
		billing, err := client.UserBilling(ctx, "", from, until)
		if err != nil || billing.Totals.Requests != 3 || billing.Totals.CostUSD != 1.5 || len(billing.ByKey) != 3 {
			t.Fatalf("billing=%+v err=%v", billing, err)
		}
		logs, err := client.UserRequestLogs(ctx, "", LogQuery{From: from, Until: until, KeyID: "key-2"})
		if err != nil || logs.Total != 1 || logs.Items[0].KeyID != "key-2" {
			t.Fatalf("logs=%+v err=%v", logs, err)
		}
		stats, err := client.UserRequestStats(ctx, "", from, until)
		if err != nil || stats.Requests != 3 || stats.ErrorCount != 0 {
			t.Fatalf("stats=%+v err=%v", stats, err)
		}
		logs, err = client.UserRequestLogs(ctx, a.ID, LogQuery{From: from, Until: until, KeyID: "key-2"})
		if err != nil || logs.Total != 0 {
			t.Fatalf("owner filter escaped: %+v, %v", logs, err)
		}
	}
}

func TestFakeKeyScopeValidation(t *testing.T) {
	ctx := context.Background()
	fake := NewFake()
	owner, _ := fake.CreateUser(ctx, "owner", "password")
	fake.AddVM(VM{ID: "a"})
	fake.AddVM(VM{ID: "openai", Platform: "openai"})
	pool, err := fake.CreatePool(ctx, "first", nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, scope := range []KeyScope{{}, {PoolID: pool.ID, AllowedVMs: []string{"a"}}} {
		if _, err := fake.CreateKey(ctx, CreateKeyInput{UserID: owner.ID, Scope: scope}); !errors.Is(err, ErrInvalidScope) {
			t.Fatalf("invalid scope=%+v: %v", scope, err)
		}
	}
	for _, tc := range []struct {
		scope KeyScope
		code  string
	}{
		{KeyScope{PoolID: "pool_00000000"}, "vm_pool_unknown"},
		{KeyScope{AllowedVMs: []string{"missing"}}, "key_vm_unknown"},
		{KeyScope{AllowedVMs: []string{" "}}, "key_vms_required"},
		{KeyScope{AllowedVMs: []string{"openai"}}, "key_vm_mismatch"},
	} {
		if _, err := fake.CreateKey(ctx, CreateKeyInput{UserID: owner.ID, Scope: tc.scope}); !IsStatus(err, 400) || !IsCode(err, tc.code) {
			t.Fatalf("scope=%+v: %v", tc.scope, err)
		}
	}
	key, err := fake.CreateKey(ctx, CreateKeyInput{Name: "key", UserID: owner.ID, MaxConcurrency: 500, Scope: KeyScope{PoolID: pool.ID}})
	if err != nil {
		t.Fatal(err)
	}
	invalid := KeyScope{}
	if err := fake.UpdateKey(ctx, key.ID, UpdateKeyInput{Scope: &invalid}); !errors.Is(err, ErrInvalidScope) {
		t.Fatalf("invalid update: %v", err)
	}
	for _, tc := range []struct {
		scope KeyScope
		code  string
	}{
		{KeyScope{PoolID: "pool_00000000"}, "vm_pool_unknown"},
		{KeyScope{AllowedVMs: []string{"missing"}}, "key_vm_unknown"},
	} {
		if err := fake.UpdateKey(ctx, key.ID, UpdateKeyInput{Scope: &tc.scope}); !IsStatus(err, 400) || !IsCode(err, tc.code) {
			t.Fatalf("scope update=%+v: %v", tc.scope, err)
		}
	}
	unchanged, err := fake.GetKey(ctx, key.ID)
	if err != nil || unchanged.PoolID != pool.ID {
		t.Fatalf("failed update changed scope: %+v %v", unchanged, err)
	}
	selected := KeyScope{AllowedVMs: []string{"a"}}
	if err := fake.UpdateKey(ctx, key.ID, UpdateKeyInput{Scope: &selected}); err != nil {
		t.Fatal(err)
	}
	info, err := fake.GetKey(ctx, key.ID)
	if err != nil || info.Name != "key" || info.UserID != owner.ID || !info.Active || info.MaxConcurrency != 128 || info.GroupType != "anthropic" || info.PoolID != "" || len(info.AllowedVMs) != 1 {
		t.Fatalf("info=%+v err=%v", info, err)
	}
	info.AllowedVMs[0] = "mutated"
	keys, err := fake.ListKeys(ctx, "")
	if err != nil || len(keys) != 1 || keys[0].AllowedVMs[0] != "a" {
		t.Fatalf("keys=%+v err=%v", keys, err)
	}
	keys, err = fake.ListKeys(ctx, "someone-else")
	if err != nil || len(keys) != 0 {
		t.Fatalf("filtered keys=%+v err=%v", keys, err)
	}
}

// vm2api stores a key created without any scope field as group_type=all.
// The Go client never sends one, but the mock must accept it so an upgrade
// from a release that did can be rehearsed against cmd/mock-vm2api.
func TestFakeHTTPAcceptsLegacyUnscopedKey(t *testing.T) {
	ctx := context.Background()
	fake := NewFake()
	user, err := fake.CreateUser(ctx, "legacy", "password")
	if err != nil {
		t.Fatal(err)
	}
	payload := fakeTestRaw(t, fake.Handler("master"), "POST", "/api/panel/api-keys", "master", `{"name":"old","user_id":"`+user.ID+`","max_concurrency":2}`, 201)
	item, _ := payload["item"].(map[string]any)
	id, _ := item["id"].(string)
	info, err := fake.GetKey(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if info.GroupType != "all" || info.PoolID != "" || len(info.AllowedVMs) != 0 {
		t.Fatalf("legacy key=%+v", info)
	}
	if _, ok := info.Scope(); ok {
		t.Fatal("an unscoped key must not read as a portal scope")
	}
	// A body that names a scope is still validated.
	fakeTestRaw(t, fake.Handler("master"), "POST", "/api/panel/api-keys", "master", `{"name":"bad","user_id":"`+user.ID+`","group_type":"anthropic"}`, 400)
}
