package api

import (
	"net/http"
	"time"
)

func (s *Server) handleUsageSummary(w http.ResponseWriter, r *http.Request) {
	u, ok := s.currentUser(r)
	if !ok {
		writeErr(w, http.StatusUnauthorized, "unauthorized", "请先登录")
		return
	}
	until := time.Now().UTC()
	from := until.AddDate(0, 0, -7)
	if v := r.URL.Query().Get("from"); v != "" {
		t, err := time.Parse(time.RFC3339, v)
		if err != nil {
			writeErr(w, http.StatusUnprocessableEntity, "invalid_range", "from 必须是 RFC3339 时间")
			return
		}
		from = t.UTC()
	}
	if v := r.URL.Query().Get("until"); v != "" {
		t, err := time.Parse(time.RFC3339, v)
		if err != nil {
			writeErr(w, http.StatusUnprocessableEntity, "invalid_range", "until 必须是 RFC3339 时间")
			return
		}
		until = t.UTC()
	}
	if !until.After(from) {
		writeErr(w, http.StatusUnprocessableEntity, "invalid_range", "结束时间必须晚于开始时间")
		return
	}
	out, err := s.Deps.Usage.Summary(r.Context(), u.ID, from, until)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	writeOK(w, out)
}

func (s *Server) handleUsageLogs(w http.ResponseWriter, r *http.Request) {
	u, ok := s.currentUser(r)
	if !ok {
		writeErr(w, http.StatusUnauthorized, "unauthorized", "请先登录")
		return
	}
	page, size := pageParams(r)
	until := time.Now().UTC()
	from := until.AddDate(0, 0, -7)
	if v := r.URL.Query().Get("from"); v != "" {
		if t, err := time.Parse(time.RFC3339, v); err == nil {
			from = t.UTC()
		} else {
			writeErr(w, http.StatusUnprocessableEntity, "invalid_range", "from 必须是 RFC3339 时间")
			return
		}
	}
	if v := r.URL.Query().Get("until"); v != "" {
		if t, err := time.Parse(time.RFC3339, v); err == nil {
			until = t.UTC()
		} else {
			writeErr(w, http.StatusUnprocessableEntity, "invalid_range", "until 必须是 RFC3339 时间")
			return
		}
	}
	status := r.URL.Query().Get("status")
	if status != "" && status != "success" && status != "error" {
		writeErr(w, http.StatusUnprocessableEntity, "invalid_status", "status 只能是 success 或 error")
		return
	}
	out, err := s.Deps.Usage.Logs(r.Context(), u.ID, UsageLogQuery{
		Page:     page,
		PageSize: size,
		From:     from,
		Until:    until,
		Model:    r.URL.Query().Get("model"),
		KeyID:    parseInt64(r.URL.Query().Get("key_id")),
		Status:   status,
	})
	if err != nil {
		s.fail(w, r, err)
		return
	}
	if out.Items == nil {
		out.Items = []UsageLogRowDTO{}
	}
	writeOK(w, out)
}
