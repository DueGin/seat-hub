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
	key, err := fake.CreateKey(ctx, CreateKeyInput{UserID: owner.ID, Name: "k"})
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
