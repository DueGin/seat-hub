package api

import (
	"context"
	"errors"
	"net/http"
	"testing"
)

type fakeSystem struct{ applied []string }

func (f *fakeSystem) UpdateStatus(ctx context.Context, refresh bool) UpdateStatusDTO {
	return UpdateStatusDTO{Current: "v1.0.0", CanUpdate: true, UpdateAvailable: true, Latest: &ReleaseDTO{Version: "v1.1.0"}, Changelog: []ReleaseDTO{}}
}

func (f *fakeSystem) ApplyUpdate(ctx context.Context, version string) (UpdateJobDTO, error) {
	if version != "v1.1.0" {
		return UpdateJobDTO{}, &UserError{Status: 409, Code: "version_not_latest", Message: "只能更新到最新发布的版本"}
	}
	f.applied = append(f.applied, version)
	return UpdateJobDTO{Kind: "update", Version: version, Stage: "downloading"}, nil
}

func (f *fakeSystem) Rollback(ctx context.Context) (UpdateJobDTO, error) {
	return UpdateJobDTO{}, Err(errors.New("none"), 409, "no_rollback", "没有可以回滚到的上一个版本")
}

func TestSystemUpdateIsAdminOnly(t *testing.T) {
	for _, role := range []string{"", "user"} {
		srv := newTestServer(t, role)
		for _, call := range []struct{ method, path, body string }{
			{http.MethodGet, "/api/admin/system/update", ""},
			{http.MethodPost, "/api/admin/system/update", `{"version":"v1.1.0"}`},
			{http.MethodPost, "/api/admin/system/rollback", `{}`},
		} {
			resp, _ := doJSON(t, call.method, srv.URL+call.path, call.body)
			if resp.StatusCode != http.StatusUnauthorized && resp.StatusCode != http.StatusForbidden {
				t.Errorf("role %q %s %s: status=%d", role, call.method, call.path, resp.StatusCode)
			}
		}
	}
}

func TestSystemUpdateEndpoints(t *testing.T) {
	srv := newTestServer(t, "admin")
	resp, body := doJSON(t, http.MethodGet, srv.URL+"/api/admin/system/update?refresh=1", "")
	data, _ := body["data"].(map[string]any)
	if resp.StatusCode != http.StatusOK || data["current"] != "v1.0.0" || data["update_available"] != true {
		t.Fatalf("status: %d %v", resp.StatusCode, body)
	}
	resp, body = doJSON(t, http.MethodPost, srv.URL+"/api/admin/system/update", `{"version":"v1.1.0"}`)
	if data, _ := body["data"].(map[string]any); resp.StatusCode != http.StatusAccepted || data["stage"] != "downloading" {
		t.Fatalf("apply: %d %v", resp.StatusCode, body)
	}
	for _, testcase := range []struct {
		body   string
		status int
		code   string
	}{
		{`{}`, http.StatusUnprocessableEntity, "invalid_version"},
		{`{"version":"v1.0.1"}`, http.StatusConflict, "version_not_latest"},
		{`{"version":"v1.1.0","extra":1}`, http.StatusUnprocessableEntity, ""},
	} {
		resp, body := doJSON(t, http.MethodPost, srv.URL+"/api/admin/system/update", testcase.body)
		errBody, _ := body["error"].(map[string]any)
		if resp.StatusCode != testcase.status || testcase.code != "" && errBody["code"] != testcase.code {
			t.Errorf("%s: %d %v", testcase.body, resp.StatusCode, body)
		}
	}
	resp, body = doJSON(t, http.MethodPost, srv.URL+"/api/admin/system/rollback", `{}`)
	if errBody, _ := body["error"].(map[string]any); resp.StatusCode != http.StatusConflict || errBody["code"] != "no_rollback" {
		t.Fatalf("rollback: %d %v", resp.StatusCode, body)
	}
}

func TestPublicConfigReportsVersion(t *testing.T) {
	srv := newTestServer(t, "")
	_, body := doJSON(t, http.MethodGet, srv.URL+"/api/public/config", "")
	if data, _ := body["data"].(map[string]any); data["version"] != "dev" {
		t.Fatalf("config=%v", body)
	}
}
