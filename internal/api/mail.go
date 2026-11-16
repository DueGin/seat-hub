package api

import (
	"context"
	"errors"
	"net/http"

	"github.com/DueGin/seat-hub/internal/mail"
)

type MailService interface {
	Settings(context.Context) (mail.Settings, error)
	SaveSettings(context.Context, mail.Settings) (mail.Settings, error)
	SendTest(context.Context, string) error
}

func (s *Server) registerMailRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/admin/settings/mail", s.admin(s.handleMailSettings))
	mux.HandleFunc("PUT /api/admin/settings/mail", s.admin(s.handleMailSave))
	mux.HandleFunc("POST /api/admin/settings/mail/test", s.admin(s.handleMailTest))
}
func (s *Server) handleMailSettings(w http.ResponseWriter, r *http.Request) {
	cfg, err := s.Deps.Mail.Settings(r.Context())
	if err != nil {
		s.fail(w, r, err)
		return
	}
	writeOK(w, cfg)
}
func (s *Server) handleMailSave(w http.ResponseWriter, r *http.Request) {
	var body mail.Settings
	if err := readJSON(r, &body); err != nil {
		s.fail(w, r, err)
		return
	}
	cfg, err := s.Deps.Mail.SaveSettings(r.Context(), body)
	if errors.Is(err, mail.ErrInvalid) {
		writeErr(w, 422, "invalid_mail_settings", "请检查邮件服务器、端口、加密方式和邮箱地址")
		return
	}
	if err != nil {
		s.fail(w, r, err)
		return
	}
	admin, _ := s.currentUser(r)
	s.Deps.Audit.Write(r.Context(), admin.ID, "mail.settings", "mail", map[string]any{"enabled": cfg.Enabled})
	writeOK(w, cfg)
}
func (s *Server) handleMailTest(w http.ResponseWriter, r *http.Request) {
	var body struct {
		To string `json:"to"`
	}
	if err := readJSON(r, &body); err != nil {
		s.fail(w, r, err)
		return
	}
	if !mail.Address(body.To) {
		writeErr(w, 422, "invalid_email", "请填写有效的收件邮箱")
		return
	}
	err := s.Deps.Mail.SendTest(r.Context(), body.To)
	admin, _ := s.currentUser(r)
	s.Deps.Audit.Write(r.Context(), admin.ID, "mail.test", "mail", map[string]any{"to": body.To, "success": err == nil})
	if errors.Is(err, mail.ErrNotConfigured) {
		writeErr(w, 422, "mail_not_configured", "请先启用并配置邮件发送")
		return
	}
	if err != nil {
		writeErr(w, 502, "smtp_error", err.Error())
		return
	}
	writeOK(w, map[string]any{})
}
