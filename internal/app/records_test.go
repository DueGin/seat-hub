package app

import (
	"net/http"
	"testing"
)

func TestUsageLogsAccountFilterValidation(t *testing.T) {
	h := newHarness(t)
	h.login("admin@example.com", "admin-pass-123")
	for _, value := range []string{"abc", "0", "-3", "1.5"} {
		status, env := h.call(http.MethodGet, "/api/usage/logs?account_id="+value, "", nil)
		if status != http.StatusUnprocessableEntity || env.Error == nil || env.Error.Code != "invalid_request" {
			t.Fatalf("account_id=%s: %d %+v", value, status, env.Error)
		}
	}
	// Someone else's or a missing account is a plain 404, never a 500.
	status, env := h.call(http.MethodGet, "/api/usage/logs?account_id=999", "", nil)
	if status != http.StatusNotFound || env.Error == nil || env.Error.Code != "not_found" {
		t.Fatalf("missing account: %d %+v", status, env.Error)
	}
}
