package vm2api

import (
	"context"
	"net/http"
	"net/http/httptest"
)

func (fake *Fake) revokePanelLocked(id string) {
	for token, owner := range fake.panels {
		if owner == id {
			delete(fake.panels, token)
		}
	}
}

func (fake *Fake) SetUserPassword(ctx context.Context, id, password string) error {
	if err := fake.begin(ctx); err != nil {
		return err
	}
	defer fake.mu.Unlock()
	if _, ok := fake.users[id]; !ok {
		return fakeMissing("user")
	}
	if password == "" {
		return fakeError(400, "invalid_password", "password required")
	}
	fake.passwords[id] = password
	fake.revokePanelLocked(id)
	return nil
}

func (fake *Fake) panelLogin(request *http.Request) (int, any, error) {
	body, err := fakeDecodeBody[struct{ Username, Password string }](request)
	if err != nil {
		return 0, nil, err
	}
	if err := fake.begin(request.Context()); err != nil {
		return 0, nil, err
	}
	defer fake.mu.Unlock()
	for id, user := range fake.users {
		if user.Enabled && user.Username == body.Username && fake.passwords[id] == body.Password {
			token := fake.nextID("kin-panel-fake")
			fake.panels[token] = id
			return 200, map[string]any{"ok": true, "token": token, "expires_in": 604800}, nil
		}
	}
	return 0, nil, fakeError(401, "unauthorized", "invalid panel credentials")
}

func (fake *Fake) validatePanelScope(owner string, scope KeyScope) error {
	if owner == "" {
		return nil
	}
	fake.mu.Lock()
	defer fake.mu.Unlock()
	if role := fake.users[owner].Role; role == "admin" || role == "super" {
		return nil
	}
	if scope.PoolID != "" {
		return fakeError(403, "key_pool_forbidden", "tenant cannot set a pool")
	}
	for _, id := range scope.AllowedVMs {
		if fake.vms[id].OwnerUserID != owner {
			return fakeError(400, "key_vm_unknown", "所选槽位不存在")
		}
	}
	return nil
}

type fakePanelTransport struct{ handler http.Handler }

func (transport fakePanelTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	if err := request.Context().Err(); err != nil {
		return nil, err
	}
	response := httptest.NewRecorder()
	transport.handler.ServeHTTP(response, request)
	return response.Result(), nil
}

func (fake *Fake) ForUser(ctx context.Context, id, username, password string) (KeyClient, error) {
	client := NewHTTPClient("http://fake-panel", "", &http.Client{Transport: fakePanelTransport{handler: fake.Handler("")}})
	return client.ForUser(ctx, id, username, password)
}

// DriftKeyOwner simulates an out-of-band owner change in a test fixture.
func (fake *Fake) DriftKeyOwner(id, owner string) {
	fake.mu.Lock()
	defer fake.mu.Unlock()
	if key, ok := fake.keys[id]; ok {
		key.userID = owner
		fake.keys[id] = key
	}
}
