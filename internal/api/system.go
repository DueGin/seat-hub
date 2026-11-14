package api

import (
	"context"
	"net/http"
	"time"

	"github.com/DueGin/seat-hub/internal/buildinfo"
)

// SystemService is the in-site update surface (admin only).
type SystemService interface {
	UpdateStatus(ctx context.Context, refresh bool) UpdateStatusDTO
	ApplyUpdate(ctx context.Context, version string) (UpdateJobDTO, error)
	Rollback(ctx context.Context) (UpdateJobDTO, error)
}

type ReleaseDTO struct {
	Version     string    `json:"version"`
	Name        string    `json:"name"`
	Notes       string    `json:"notes"`
	URL         string    `json:"url"`
	PublishedAt time.Time `json:"published_at"`
}

type UpdateJobDTO struct {
	Kind      string    `json:"kind"`
	Version   string    `json:"version"`
	Stage     string    `json:"stage"`
	Done      int64     `json:"done"`
	Total     int64     `json:"total"`
	Error     string    `json:"error,omitempty"`
	StartedAt time.Time `json:"started_at"`
}

type InstalledDTO struct {
	Version string `json:"version"`
	Source  string `json:"source"`
}

type UpdateFailureDTO struct {
	Version string `json:"version"`
	Error   string `json:"error"`
	At      string `json:"at"`
}

type UpdateStatusDTO struct {
	Current         string            `json:"current"`
	Commit          string            `json:"commit"`
	BuildTime       string            `json:"build_time"`
	Supervisor      string            `json:"supervisor"`
	DrainSeconds    int               `json:"drain_seconds"`
	CanUpdate       bool              `json:"can_update"`
	Reason          string            `json:"reason,omitempty"`
	Latest          *ReleaseDTO       `json:"latest"`
	Changelog       []ReleaseDTO      `json:"changelog"`
	UpdateAvailable bool              `json:"update_available"`
	CheckedAt       *time.Time        `json:"checked_at"`
	CheckError      string            `json:"check_error,omitempty"`
	Job             *UpdateJobDTO     `json:"job"`
	Pending         bool              `json:"pending"`
	Rollback        *InstalledDTO     `json:"rollback"`
	LastFailure     *UpdateFailureDTO `json:"last_failure"`
}

func (s *Server) handleAdminUpdateStatus(w http.ResponseWriter, r *http.Request) {
	refresh := r.URL.Query().Get("refresh") == "1"
	writeOK(w, s.Deps.System.UpdateStatus(r.Context(), refresh))
}

func (s *Server) handleAdminApplyUpdate(w http.ResponseWriter, r *http.Request) {
	admin, _ := s.currentUser(r)
	var body struct {
		Version string `json:"version"`
	}
	if err := readJSON(r, &body); err != nil {
		s.fail(w, r, err)
		return
	}
	if body.Version == "" {
		writeErr(w, http.StatusUnprocessableEntity, "invalid_version", "请指定要更新到的版本")
		return
	}
	job, err := s.Deps.System.ApplyUpdate(r.Context(), body.Version)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	s.Deps.Audit.Write(r.Context(), admin.ID, "system.update", body.Version, map[string]any{"from": buildinfo.Current().Version})
	writeJSON(w, http.StatusAccepted, map[string]any{"ok": true, "data": job})
}

func (s *Server) handleAdminRollback(w http.ResponseWriter, r *http.Request) {
	admin, _ := s.currentUser(r)
	var body struct{}
	if err := readJSON(r, &body); err != nil {
		s.fail(w, r, err)
		return
	}
	job, err := s.Deps.System.Rollback(r.Context())
	if err != nil {
		s.fail(w, r, err)
		return
	}
	s.Deps.Audit.Write(r.Context(), admin.ID, "system.rollback", job.Version, nil)
	writeJSON(w, http.StatusAccepted, map[string]any{"ok": true, "data": job})
}
