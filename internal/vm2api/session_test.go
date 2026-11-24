package vm2api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
)

func TestMemberSessionCachesAndRefreshesToken(t *testing.T) {
	ctx := context.Background()
	fake := NewFake()
	owner := fakeTestUser(t, fake, "owner")
	other := fakeTestUser(t, fake, "other")
	var logins atomic.Int32
	panel := fake.Handler("master")
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/panel/login" {
			logins.Add(1)
		}
		panel.ServeHTTP(w, r)
	}))
	defer server.Close()
	client := NewHTTPClient(server.URL, "master", server.Client())
	users, err := client.ListUsers(ctx)
	if err != nil || len(users) != 2 || users[0] != owner {
		t.Fatalf("list users=%+v, %v", users, err)
	}
	keys, err := client.ForUser(ctx, owner.ID, owner.Username, "password")
	if err != nil {
		t.Fatal(err)
	}
	key, err := keys.CreateKey(ctx, CreateKeyInput{UserID: owner.ID, Scope: KeyScope{All: true}})
	if err != nil {
		t.Fatal(err)
	}
	for _, route := range []func() error{
		func() error { _, err := client.GetKey(ctx, key.ID); return err },
		func() error { return client.UpdateKey(ctx, key.ID, UpdateKeyInput{}) },
		func() error { return client.DeleteKey(ctx, key.ID) },
		func() error { _, err := client.KeyStats(ctx, key.ID); return err },
	} {
		if err := route(); !IsStatus(err, 404) {
			t.Fatalf("master can access a member key: %v", err)
		}
	}
	if _, err := client.ForUser(ctx, owner.ID, owner.Username, "password"); err != nil || logins.Load() != 1 {
		t.Fatalf("login not cached: count=%d err=%v", logins.Load(), err)
	}
	fake.mu.Lock()
	fake.revokePanelLocked(owner.ID)
	fake.mu.Unlock()
	if _, err := keys.GetKey(ctx, key.ID); err != nil || logins.Load() != 2 {
		t.Fatalf("session not refreshed: count=%d err=%v", logins.Load(), err)
	}
	otherKeys, err := client.ForUser(ctx, other.ID, other.Username, "password")
	if err != nil {
		t.Fatal(err)
	}
	if err := otherKeys.DeleteKey(ctx, key.ID); !IsStatus(err, 404) {
		t.Fatalf("foreign session can delete key: %v", err)
	}
	if err := client.SetUserPassword(ctx, owner.ID, "replacement"); err != nil {
		t.Fatal(err)
	}
	if _, err := keys.GetKey(ctx, key.ID); !IsStatus(err, 401) {
		t.Fatalf("old password still works: %v", err)
	}
	if _, err := client.ForUser(ctx, owner.ID, owner.Username, "replacement"); err != nil {
		t.Fatal(err)
	}
	if err := client.SetUserEnabled(ctx, owner.ID, false); err != nil {
		t.Fatal(err)
	}
	if _, err := keys.ListKeys(ctx, owner.ID); !IsStatus(err, 401) || fake.ProtocolKeyActive(key.Secret) {
		t.Fatalf("disabled member can authenticate: %v", err)
	}
}

func TestCreateMemberKeyRejectsUnexpectedOwner(t *testing.T) {
	var deleted atomic.Bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var payload string
		switch {
		case r.URL.Path == "/api/panel/login":
			payload = `{"ok":true,"token":"kin-panel-fixture"}`
		case r.Method == http.MethodPost && r.URL.Path == "/api/panel/api-keys":
			var body map[string]any
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil || r.Header.Get("Authorization") != "Bearer kin-panel-fixture" {
				t.Errorf("unexpected create request: %v", err)
			}
			if _, present := body["user_id"]; present {
				t.Error("member request sent user_id")
			}
			payload = `{"ok":true,"item":{"id":"key-fixture","key":"sk-vm-fixture","user_id":null}}`
		case r.Method == http.MethodDelete && r.URL.Path == "/api/panel/api-keys/key-fixture":
			deleted.Store(true)
			payload = `{"ok":true}`
		default:
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusBadRequest)
		}
		_, _ = w.Write([]byte(payload))
	}))
	defer server.Close()
	ctx := context.Background()
	client := NewHTTPClient(server.URL, "master", server.Client())
	keys, err := client.ForUser(ctx, "member", "member", "password")
	if err != nil {
		t.Fatal(err)
	}
	if key, err := keys.CreateKey(ctx, CreateKeyInput{UserID: "member", Scope: KeyScope{All: true}}); err == nil || key.ID != "" || !deleted.Load() {
		t.Fatalf("wrong-owner key was accepted or not cleaned up: %v", err)
	}
}
