package api

import (
	"context"
	"encoding/csv"
	"net/http"
	"strconv"
	"strings"

	"github.com/DueGin/seat-hub/internal/reservation"
)

type ReservationService interface {
	Create(context.Context, int64, *int64, string, string, string) error
	List(context.Context, reservation.Filter) (reservation.Page, error)
	Export(context.Context, reservation.Filter) ([]reservation.Item, error)
	Delete(context.Context, int64) error
	Notify(context.Context, int64, string, string) (int64, error)
}

func (s *Server) registerReservationRoutes(mux *http.ServeMux) {
	mux.HandleFunc("POST /api/public/products/{id}/reservations", s.handleReserve)
	mux.HandleFunc("GET /api/admin/reservations", s.admin(s.handleReservations))
	mux.HandleFunc("GET /api/admin/reservations/export", s.admin(s.handleReservationExport))
	mux.HandleFunc("DELETE /api/admin/reservations/{id}", s.admin(s.handleReservationDelete))
	mux.HandleFunc("POST /api/admin/products/{id}/reservations/notify", s.admin(s.handleReservationNotify))
}
func (s *Server) handleReserve(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Email string `json:"email"`
		Note  string `json:"note"`
	}
	if err := readJSON(r, &body); err != nil {
		s.fail(w, r, err)
		return
	}
	var userID *int64
	if user, ok := s.currentUser(r); ok {
		userID = &user.ID
	}
	if err := s.Deps.Reservations.Create(r.Context(), pathID(r), userID, body.Email, body.Note, clientIP(r)); err != nil {
		s.fail(w, r, err)
		return
	}
	writeOK(w, map[string]any{})
}
func reservationFilter(r *http.Request) (reservation.Filter, error) {
	page, size := adminPageParams(r)
	f := reservation.Filter{Status: r.URL.Query().Get("status"), Email: strings.TrimSpace(r.URL.Query().Get("email")), Page: page, PageSize: size}
	if raw := r.URL.Query().Get("product_id"); raw != "" {
		id, err := strconv.ParseInt(raw, 10, 64)
		if err != nil || id <= 0 {
			return f, bad("invalid_product", "商品编号不正确")
		}
		f.ProductID = id
	}
	if (f.Status != "" && f.Status != "waiting" && f.Status != "notified") || len(f.Email) > 254 {
		return f, bad("invalid_filter", "预约筛选条件不正确")
	}
	return f, nil
}
func (s *Server) handleReservations(w http.ResponseWriter, r *http.Request) {
	f, err := reservationFilter(r)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	page, err := s.Deps.Reservations.List(r.Context(), f)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	writeOK(w, page)
}

// csvCell prevents spreadsheet formula execution from untrusted text.
func csvCell(value string) string {
	trimmed := strings.TrimLeft(value, " \t\r\n")
	if strings.HasPrefix(value, "\t") || strings.HasPrefix(value, "\r") || strings.HasPrefix(value, "\n") || strings.HasPrefix(trimmed, "=") || strings.HasPrefix(trimmed, "+") || strings.HasPrefix(trimmed, "-") || strings.HasPrefix(trimmed, "@") {
		return "'" + value
	}
	return value
}
func (s *Server) handleReservationExport(w http.ResponseWriter, r *http.Request) {
	f, err := reservationFilter(r)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	items, err := s.Deps.Reservations.Export(r.Context(), f)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	w.Header().Set("Content-Type", "text/csv; charset=utf-8")
	w.Header().Set("Content-Disposition", `attachment; filename="reservations.csv"`)
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write([]byte("\xef\xbb\xbf"))
	writer := csv.NewWriter(w)
	writer.UseCRLF = true
	_ = writer.Write([]string{"编号", "商品", "邮箱", "备注", "状态", "预约时间", "通知时间"})
	for _, item := range items {
		notified := ""
		if item.NotifiedAt != nil {
			notified = *item.NotifiedAt
		}
		status := "待通知"
		if item.Status == "notified" {
			status = "已通知"
		}
		_ = writer.Write([]string{strconv.FormatInt(item.ID, 10), csvCell(item.ProductName), csvCell(item.Email), csvCell(item.Note), status, item.CreatedAt, notified})
	}
	writer.Flush()
}
func (s *Server) handleReservationDelete(w http.ResponseWriter, r *http.Request) {
	id, ok := adminRequestID(w, r)
	if !ok {
		return
	}
	if err := s.Deps.Reservations.Delete(r.Context(), id); err != nil {
		s.fail(w, r, err)
		return
	}
	admin, _ := s.currentUser(r)
	s.Deps.Audit.Write(r.Context(), admin.ID, "reservation.delete", strconv.FormatInt(id, 10), nil)
	writeOK(w, map[string]any{})
}
func (s *Server) handleReservationNotify(w http.ResponseWriter, r *http.Request) {
	id, ok := adminRequestID(w, r)
	if !ok {
		return
	}
	var body struct {
		Subject string `json:"subject"`
		Body    string `json:"body"`
	}
	if err := readJSON(r, &body); err != nil {
		s.fail(w, r, err)
		return
	}
	n, err := s.Deps.Reservations.Notify(r.Context(), id, body.Subject, body.Body)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	admin, _ := s.currentUser(r)
	s.Deps.Audit.Write(r.Context(), admin.ID, "reservation.notify", strconv.FormatInt(id, 10), map[string]any{"count": n})
	writeOK(w, map[string]any{"count": n})
}
