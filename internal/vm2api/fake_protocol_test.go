package vm2api

import (
	"context"
	"net/http/httptest"
	"testing"
	"time"
)

func TestFakeRecordProtocolCall(t *testing.T) {
	ctx := context.Background()
	fake := NewFake()
	owner, err := fake.CreateUser(ctx, "owner", "password")
	if err != nil {
		t.Fatal(err)
	}
	fake.AddVM(VM{ID: "slot-a", OwnerUserID: owner.ID})
	pool, err := fake.CreatePool(ctx, "owner", []string{"slot-a"})
	if err != nil {
		t.Fatal(err)
	}
	key, err := fake.CreateKey(ctx, CreateKeyInput{UserID: owner.ID, Name: "k", Scope: KeyScope{PoolID: pool.ID}})
	if err != nil {
		t.Fatal(err)
	}
	if _, known := fake.RecordProtocolCall(ProtocolCall{Secret: "sk-vm-unknown"}); known {
		t.Fatal("unknown key accepted")
	}
	// No signed-in slot yet: logged as a failure without a slot.
	if status, known := fake.RecordProtocolCall(ProtocolCall{Secret: key.Secret, Model: "m"}); !known || status != 503 {
		t.Fatalf("no slot: status=%d known=%v", status, known)
	}
	fake.AddVM(VM{ID: "slot-a", OwnerUserID: owner.ID, HasCredential: true})
	fake.AddVM(VM{ID: "slot-b", OwnerUserID: "someone-else", HasCredential: true})
	if status, _ := fake.RecordProtocolCall(ProtocolCall{Secret: key.Secret, Model: "m", InputTokens: 100, OutputTokens: 10, DurationMS: 50}); status != 200 {
		t.Fatalf("status=%d", status)
	}
	page, err := fake.UserRequestLogs(ctx, owner.ID, LogQuery{From: time.Now().Add(-time.Hour), Until: time.Now().Add(time.Hour)})
	if err != nil || page.Total != 2 {
		t.Fatalf("page=%+v err=%v", page, err)
	}
	ok := page.Items[0]
	if ok.VMID != "slot-a" || ok.KeyID != key.ID || ok.InputTokens != 100 || ok.CostUSD <= 0 || ok.FirstTokenMS == nil {
		t.Fatalf("served row=%+v", ok)
	}
	if failed := page.Items[1]; failed.VMID != "" || failed.StatusCode != 503 || failed.CostUSD != 0 {
		t.Fatalf("failed row=%+v", failed)
	}
	byVM, err := fake.UserRequestLogs(ctx, owner.ID, LogQuery{From: time.Now().Add(-time.Hour), Until: time.Now().Add(time.Hour), VMID: "slot-a"})
	if err != nil || byVM.Total != 1 {
		t.Fatalf("vm filter=%+v err=%v", byVM, err)
	}
}

func TestFakeProtocolEnforcesOwnerAndScope(t *testing.T) {
	for _, tc := range []struct {
		name                   string
		pool                   bool
		members                []string
		disabled, missing, off bool
		owner                  string
		status                 int
		code                   string
	}{
		{name: "own vm in pool", pool: true, members: []string{"own"}, owner: "owner", status: 200},
		{name: "foreign vm in pool", pool: true, members: []string{"foreign"}, owner: "owner", status: 503, code: "pool_unavailable"},
		{name: "empty pool", pool: true, owner: "owner", status: 403, code: "vm_pool_unavailable"},
		{name: "disabled pool", pool: true, members: []string{"own"}, disabled: true, owner: "owner", status: 403, code: "vm_pool_unavailable"},
		{name: "missing pool", pool: true, members: []string{"own"}, missing: true, owner: "owner", status: 403, code: "vm_pool_unavailable"},
		{name: "allowlist own vm", members: []string{"own"}, owner: "owner", status: 200},
		{name: "allowlist excludes own vm", members: []string{"foreign"}, owner: "owner", status: 503, code: "pool_unavailable"},
		{name: "stopped vm", members: []string{"own"}, off: true, owner: "owner", status: 503, code: "pool_unavailable"},
		{name: "ownerless cannot schedule owned vm", members: []string{"foreign"}, status: 503, code: "pool_unavailable"},
		{name: "ownerless schedules platform vm", members: []string{"platform"}, status: 200},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			fake := NewFake()
			owner, _ := fake.CreateUser(ctx, "owner", "password")
			fake.AddVM(VM{ID: "own", OwnerUserID: owner.ID, HasCredential: true})
			fake.AddVM(VM{ID: "foreign", OwnerUserID: "other", HasCredential: true})
			fake.AddVM(VM{ID: "platform", HasCredential: true})
			scope := KeyScope{AllowedVMs: tc.members}
			if tc.pool {
				pool, err := fake.CreatePool(ctx, "pool", tc.members)
				if err != nil {
					t.Fatal(err)
				}
				scope = KeyScope{PoolID: pool.ID}
			}
			ownerID := ""
			if tc.owner != "" {
				ownerID = owner.ID
			}
			key, err := fake.CreateKey(ctx, CreateKeyInput{UserID: ownerID, Scope: scope})
			if err != nil {
				t.Fatal(err)
			}
			if tc.disabled {
				fakeTestRaw(t, fake.Handler("master"), "PATCH", "/api/panel/vm-pools/"+scope.PoolID, "master", `{"enabled":false}`, 200)
			}
			if tc.missing {
				fake.mu.Lock()
				delete(fake.pools, scope.PoolID)
				fake.mu.Unlock()
			}
			if tc.off {
				if err := fake.SetSchedulable(ctx, "own", false); err != nil {
					t.Fatal(err)
				}
			}
			status, code, known := fake.RecordProtocolCallWithCode(ProtocolCall{Secret: key.Secret})
			if !known || status != tc.status || code != tc.code {
				t.Fatalf("status=%d code=%q known=%v", status, code, known)
			}
			logs, err := fake.UserRequestLogs(ctx, "", LogQuery{KeyID: key.ID})
			logCode := tc.code
			if logCode == "pool_unavailable" {
				logCode = "account_pool_exhausted"
			}
			if err != nil || logs.Total != 1 || logs.Items[0].ErrorLabel != logCode {
				t.Fatalf("logs=%+v err=%v", logs, err)
			}
			if status == 200 && (ownerID != "" && logs.Items[0].VMID != "own" || ownerID == "" && logs.Items[0].VMID != "platform") {
				t.Fatalf("wrong vm: %+v", logs.Items[0])
			}
		})
	}
}

func TestFakeProtocolReadsLiveScope(t *testing.T) {
	ctx := context.Background()
	fake := NewFake()
	owner, _ := fake.CreateUser(ctx, "owner", "password")
	fake.AddVM(VM{ID: "a", OwnerUserID: owner.ID, HasCredential: true})
	fake.AddVM(VM{ID: "b", OwnerUserID: owner.ID, HasCredential: true})
	pool, _ := fake.CreatePool(ctx, "pool", []string{"a"})
	key, err := fake.CreateKey(ctx, CreateKeyInput{UserID: owner.ID, Scope: KeyScope{PoolID: pool.ID}})
	if err != nil {
		t.Fatal(err)
	}
	if status, _ := fake.RecordProtocolCall(ProtocolCall{Secret: key.Secret}); status != 200 {
		t.Fatalf("first call=%d", status)
	}
	if _, err := fake.SetPoolMembers(ctx, pool.ID, []string{"b"}); err != nil {
		t.Fatal(err)
	}
	if err := fake.SetVMOwner(ctx, "a", nil); err != nil {
		t.Fatal(err)
	}
	if status, _ := fake.RecordProtocolCall(ProtocolCall{Secret: key.Secret}); status != 200 {
		t.Fatalf("second call=%d", status)
	}
	logs, _ := fake.UserRequestLogs(ctx, "", LogQuery{KeyID: key.ID})
	if logs.Total != 2 || logs.Items[0].VMID != "b" || logs.Items[1].VMID != "a" {
		t.Fatalf("logs=%+v", logs)
	}
	if err := fake.SetVMOwner(ctx, "b", nil); err != nil {
		t.Fatal(err)
	}
	if status, _ := fake.RecordProtocolCall(ProtocolCall{Secret: key.Secret}); status != 503 {
		t.Fatalf("reassigned vm accepted: %d", status)
	}
}

func TestFakeProtocolLegacyAllScopeStillEnforcesOwnership(t *testing.T) {
	ctx := context.Background()
	fake := NewFake()
	owner, _ := fake.CreateUser(ctx, "owner", "password")
	fake.AddVM(VM{ID: "own", OwnerUserID: owner.ID, HasCredential: true})
	fake.AddVM(VM{ID: "foreign", OwnerUserID: "other", HasCredential: true})
	key, err := fake.CreateKey(ctx, CreateKeyInput{UserID: owner.ID, Scope: KeyScope{AllowedVMs: []string{"own"}}})
	if err != nil {
		t.Fatal(err)
	}
	// Existing unrestricted upstream keys can be found by reconciliation.
	fake.mu.Lock()
	record := fake.keys[key.ID]
	record.groupType, record.allowedVMs = "all", nil
	fake.keys[key.ID] = record
	fake.mu.Unlock()
	if status, _ := fake.RecordProtocolCall(ProtocolCall{Secret: key.Secret}); status != 200 {
		t.Fatalf("legacy all status=%d", status)
	}
	if err := fake.SetVMOwner(ctx, "own", nil); err != nil {
		t.Fatal(err)
	}
	if status, _ := fake.RecordProtocolCall(ProtocolCall{Secret: key.Secret}); status != 503 {
		t.Fatalf("legacy all escaped owner: %d", status)
	}
	fake.mu.Lock()
	record.userID = ""
	fake.keys[key.ID] = record
	fake.mu.Unlock()
	if status, _ := fake.RecordProtocolCall(ProtocolCall{Secret: key.Secret}); status != 200 {
		t.Fatalf("master legacy all status=%d", status)
	}
}

// The slot filter must survive the real HTTP round trip, not just the Go call.
func TestFakeHTTPRequestLogsFilterBySlot(t *testing.T) {
	ctx := context.Background()
	fake := NewFake()
	owner, err := fake.CreateUser(ctx, "owner", "password")
	if err != nil {
		t.Fatal(err)
	}
	for index, vmID := range []string{"slot-a", "slot-a", "slot-b"} {
		if err := fake.AddRequestLog(owner.ID, LogRow{VMID: vmID, Model: "m", StatusCode: 200, CreatedAt: time.Now().Add(-time.Duration(index+1) * time.Minute)}); err != nil {
			t.Fatal(err)
		}
	}
	server := httptest.NewServer(fake.Handler("master"))
	defer server.Close()
	client := NewHTTPClient(server.URL, "master", server.Client())
	window := LogQuery{From: time.Now().Add(-time.Hour), Until: time.Now().Add(time.Hour)}
	for vmID, want := range map[string]int64{"": 3, "slot-a": 2, "slot-b": 1, "slot-none": 0} {
		query := window
		query.VMID = vmID
		page, err := client.UserRequestLogs(ctx, owner.ID, query)
		if err != nil || page.Total != want {
			t.Fatalf("vm %q: total=%d err=%v", vmID, page.Total, err)
		}
		for _, row := range page.Items {
			if vmID != "" && row.VMID != vmID {
				t.Fatalf("vm %q returned row from %q", vmID, row.VMID)
			}
		}
	}
}
