package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/DueGin/seat-hub/internal/vm2api"
)

func TestDevProtocolEnforcesLiveKeyScope(t *testing.T) {
	ctx := context.Background()
	fake := vm2api.NewFake()
	owner, err := fake.CreateUser(ctx, "owner", "password")
	if err != nil {
		t.Fatal(err)
	}
	fake.AddVM(vm2api.VM{ID: "own", OwnerUserID: owner.ID, HasCredential: true})
	fake.AddVM(vm2api.VM{ID: "foreign", OwnerUserID: "other", HasCredential: true})
	pool, err := fake.CreatePool(ctx, "pool", []string{"own"})
	if err != nil {
		t.Fatal(err)
	}
	key, err := fake.CreateKey(ctx, vm2api.CreateKeyInput{UserID: owner.ID, Scope: vm2api.KeyScope{PoolID: pool.ID}})
	if err != nil {
		t.Fatal(err)
	}
	handler := devProtocol("master", fake)
	request := func(secret, path string, status int, code string) {
		t.Helper()
		r := httptest.NewRequest(http.MethodPost, path, strings.NewReader(`{"model":"claude","messages":[]}`))
		r.Header.Set("x-api-key", secret)
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, r)
		if w.Code != status {
			t.Fatalf("%s: status=%d body=%s", path, w.Code, w.Body.String())
		}
		if code != "" {
			var body struct {
				Error struct {
					Code string `json:"code"`
				} `json:"error"`
			}
			if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil || body.Error.Code != code {
				t.Fatalf("code=%q err=%v body=%s", code, err, w.Body.String())
			}
		}
	}
	request("sk-vm-unknown", "/v1/messages", 401, "")
	request("sk-vm-unknown", "/v1/models", 401, "")
	request(key.Secret, "/v1/messages", 200, "")
	request(key.Secret, "/v1/models", 200, "")
	if _, err := fake.SetPoolMembers(ctx, pool.ID, []string{"foreign"}); err != nil {
		t.Fatal(err)
	}
	request(key.Secret, "/v1/messages", 503, "pool_unavailable")
	if _, err := fake.SetPoolMembers(ctx, pool.ID, nil); err != nil {
		t.Fatal(err)
	}
	request(key.Secret, "/v1/messages", 403, "vm_pool_unavailable")
	selected := vm2api.KeyScope{AllowedVMs: []string{"foreign"}}
	if err := fake.UpdateKey(ctx, key.ID, vm2api.UpdateKeyInput{Scope: &selected}); err != nil {
		t.Fatal(err)
	}
	request(key.Secret, "/v1/messages", 503, "pool_unavailable")
	selected.AllowedVMs = []string{"own"}
	if err := fake.UpdateKey(ctx, key.ID, vm2api.UpdateKeyInput{Scope: &selected}); err != nil {
		t.Fatal(err)
	}
	request(key.Secret, "/v1/messages", 200, "")
	active := false
	if err := fake.UpdateKey(ctx, key.ID, vm2api.UpdateKeyInput{Active: &active}); err != nil {
		t.Fatal(err)
	}
	request(key.Secret, "/v1/messages", 401, "")
	request(key.Secret, "/v1/models", 401, "")
}
