package api

import (
	"context"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/DueGin/seat-hub/internal/ticket"
)

// TicketService enforces ownership and transactions below the HTTP layer.
type TicketService interface {
	Create(context.Context, ticket.Actor, ticket.Input) (ticket.Detail, error)
	List(context.Context, ticket.Actor, ticket.Filter) (ticket.Page, error)
	Get(context.Context, ticket.Actor, int64) (ticket.Detail, error)
	Reply(context.Context, ticket.Actor, int64, ticket.ReplyInput) (ticket.Detail, error)
	SetStatus(context.Context, ticket.Actor, int64, string) (ticket.Ticket, error)
	Unread(context.Context, ticket.Actor) (int64, error)
	Upload(context.Context, ticket.Actor, string, io.Reader) (ticket.Attachment, error)
	OpenAttachment(context.Context, ticket.Actor, int64) (ticket.Attachment, *os.File, error)
}

var (
	_ = Err(ticket.ErrNotFound, 404, "ticket_not_found", "工单或图片不存在")
	_ = Err(ticket.ErrForbidden, 403, "ticket_forbidden", "无权查看或关联此内容")
	_ = Err(ticket.ErrInvalid, 422, "ticket_invalid", "请检查工单分类、标题、正文和状态")
	_ = Err(ticket.ErrAttachment, 422, "ticket_attachment_invalid", "每条消息最多 6 张图片，请使用自己上传且未使用的图片")
	_ = Err(ticket.ErrImageType, 422, "ticket_image_type", "只支持 PNG、JPEG、GIF 和 WebP 图片")
	_ = Err(ticket.ErrImageSize, 413, "ticket_image_size", "图片不能为空，且不能超过 5 MB")
)

func (s *Server) registerTicketRoutes(mux *http.ServeMux) {
	if s.Deps.Tickets == nil {
		return
	}
	mux.HandleFunc("GET /api/tickets", s.handleTicketsList)
	mux.HandleFunc("POST /api/tickets", s.handleTicketCreate)
	mux.HandleFunc("GET /api/tickets/unread", s.handleTicketUnread)
	mux.HandleFunc("GET /api/tickets/{id}", s.handleTicketGet)
	mux.HandleFunc("POST /api/tickets/{id}/messages", s.handleTicketReply)
	mux.HandleFunc("POST /api/tickets/{id}/close", s.handleTicketStatus)
	mux.HandleFunc("POST /api/tickets/attachments", s.handleTicketUpload)
	mux.HandleFunc("GET /api/tickets/attachments/{id}", s.handleTicketAttachment)
	mux.HandleFunc("GET /api/admin/tickets", s.admin(s.handleTicketsList))
	mux.HandleFunc("GET /api/admin/tickets/unread", s.admin(s.handleTicketUnread))
	mux.HandleFunc("GET /api/admin/tickets/{id}", s.admin(s.handleTicketGet))
	mux.HandleFunc("POST /api/admin/tickets/{id}/messages", s.admin(s.handleTicketReply))
	mux.HandleFunc("PATCH /api/admin/tickets/{id}", s.admin(s.handleTicketStatus))
}
func (s *Server) ticketActor(w http.ResponseWriter, r *http.Request) (ticket.Actor, bool) {
	u, ok := s.currentUser(r)
	if !ok {
		writeErr(w, 401, "unauthorized", "请先登录")
		return ticket.Actor{}, false
	}
	return ticket.Actor{ID: u.ID, Admin: strings.HasPrefix(r.URL.Path, "/api/admin/") && u.Role == "admin"}, true
}
func (s *Server) handleTicketsList(w http.ResponseWriter, r *http.Request) {
	a, ok := s.ticketActor(w, r)
	if !ok {
		return
	}
	page, size := pageParams(r)
	f := ticket.Filter{Status: r.URL.Query().Get("status"), Page: page, PageSize: size}
	if a.Admin {
		f.Category = r.URL.Query().Get("category")
		f.Query = r.URL.Query().Get("q")
		if user := r.URL.Query().Get("user_id"); user != "" {
			var err error
			f.UserID, err = strconv.ParseInt(user, 10, 64)
			if err != nil || f.UserID <= 0 {
				s.fail(w, r, ticket.ErrInvalid)
				return
			}
		}
	}
	data, err := s.Deps.Tickets.List(r.Context(), a, f)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	writeOK(w, data)
}
func (s *Server) handleTicketCreate(w http.ResponseWriter, r *http.Request) {
	a, ok := s.ticketActor(w, r)
	if !ok {
		return
	}
	var in ticket.Input
	if err := readJSON(r, &in); err != nil {
		s.fail(w, r, err)
		return
	}
	d, err := s.Deps.Tickets.Create(r.Context(), a, in)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	writeCreated(w, d)
}
func (s *Server) handleTicketGet(w http.ResponseWriter, r *http.Request) {
	a, ok := s.ticketActor(w, r)
	if !ok {
		return
	}
	d, err := s.Deps.Tickets.Get(r.Context(), a, pathID(r))
	if err != nil {
		s.fail(w, r, err)
		return
	}
	writeOK(w, d)
}
func (s *Server) handleTicketReply(w http.ResponseWriter, r *http.Request) {
	a, ok := s.ticketActor(w, r)
	if !ok {
		return
	}
	var in ticket.ReplyInput
	if err := readJSON(r, &in); err != nil {
		s.fail(w, r, err)
		return
	}
	d, err := s.Deps.Tickets.Reply(r.Context(), a, pathID(r), in)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	writeCreated(w, d)
}
func (s *Server) handleTicketStatus(w http.ResponseWriter, r *http.Request) {
	a, ok := s.ticketActor(w, r)
	if !ok {
		return
	}
	status := "closed"
	if a.Admin {
		var in struct {
			Status string `json:"status"`
		}
		if err := readJSON(r, &in); err != nil {
			s.fail(w, r, err)
			return
		}
		status = in.Status
	}
	t, err := s.Deps.Tickets.SetStatus(r.Context(), a, pathID(r), status)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	writeOK(w, map[string]any{"ticket": t})
}
func (s *Server) handleTicketUnread(w http.ResponseWriter, r *http.Request) {
	a, ok := s.ticketActor(w, r)
	if !ok {
		return
	}
	count, err := s.Deps.Tickets.Unread(r.Context(), a)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	writeOK(w, map[string]int64{"count": count})
}
func (s *Server) handleTicketUpload(w http.ResponseWriter, r *http.Request) {
	a, ok := s.ticketActor(w, r)
	if !ok {
		return
	}
	if r.Header.Get("X-Seat-Hub-Upload") != "1" {
		writeErr(w, 403, "ticket_upload_header", "上传图片需要专用请求头")
		return
	}
	contentType, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || contentType != "multipart/form-data" {
		writeErr(w, 415, "unsupported_media_type", "请使用图片上传表单")
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, ticket.MaxAttachmentSize+(64<<10))
	if err := r.ParseMultipartForm(ticket.MaxAttachmentSize); err != nil {
		var limit *http.MaxBytesError
		if errors.As(err, &limit) {
			s.fail(w, r, ticket.ErrImageSize)
		} else {
			writeErr(w, 422, "invalid_body", "图片上传表单无效")
		}
		return
	}
	defer r.MultipartForm.RemoveAll()
	files := r.MultipartForm.File["file"]
	if len(files) != 1 || len(r.MultipartForm.File) != 1 || len(r.MultipartForm.Value) > 0 {
		writeErr(w, 422, "invalid_body", "每次请选择一张图片")
		return
	}
	file, err := files[0].Open()
	if err != nil {
		s.fail(w, r, fmt.Errorf("open upload: %w", err))
		return
	}
	defer file.Close()
	d, err := s.Deps.Tickets.Upload(r.Context(), a, files[0].Filename, file)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	writeCreated(w, d)
}
func (s *Server) handleTicketAttachment(w http.ResponseWriter, r *http.Request) {
	a, ok := s.ticketActor(w, r)
	if !ok {
		return
	}
	u, _ := s.currentUser(r)
	a.Admin = u.Role == "admin"
	att, file, err := s.Deps.Tickets.OpenAttachment(r.Context(), a, pathID(r))
	if err != nil {
		s.fail(w, r, err)
		return
	}
	defer file.Close()
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Content-Security-Policy", "sandbox")
	w.Header().Set("Cache-Control", "private")
	w.Header().Set("Content-Type", att.ContentType)
	w.Header().Set("Content-Disposition", mime.FormatMediaType("inline", map[string]string{"filename": att.Name}))
	http.ServeContent(w, r, att.Name, time.Time{}, file)
}
