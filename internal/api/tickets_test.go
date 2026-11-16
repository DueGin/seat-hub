package api

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"log/slog"
	"mime/multipart"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/DueGin/seat-hub/internal/store"
	"github.com/DueGin/seat-hub/internal/ticket"
)

func TestTicketAPIUploadAndOwnership(t *testing.T) {
	db, err := store.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	for _, id := range []int{1, 2, 3} {
		if _, err := db.Exec("INSERT INTO users(id,email,password_hash,role,status,created_at,updated_at) VALUES(?,?,'x','user','active','2026-01-01T00:00:00Z','2026-01-01T00:00:00Z')", id, strings.Repeat("u", id)+"@example.com"); err != nil {
			t.Fatal(err)
		}
	}
	svc := ticket.New(db.DB, t.TempDir(), "", nil, nil)
	d, err := svc.Create(context.Background(), ticket.Actor{ID: 1}, ticket.Input{Category: "api", Title: "问题", Body: "正文"})
	if err != nil {
		t.Fatal(err)
	}
	png, _ := base64.StdEncoding.DecodeString("iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAQAAAC1HAwCAAAAC0lEQVR42mP8/x8AAwMCAO+jZl8AAAAASUVORK5CYII=")
	att, err := svc.Upload(context.Background(), ticket.Actor{ID: 1}, "test.png", bytes.NewReader(png))
	if err != nil {
		t.Fatal(err)
	}
	f := &fakeAuth{user: &UserDTO{ID: 2, Role: "user"}}
	s := &Server{Deps: Deps{Auth: f, Tickets: svc}, Log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	handler := f.Middleware(s.Handler())
	request := func(method, path, contentType string, body io.Reader, header string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(method, path, body)
		r.Header.Set("Content-Type", contentType)
		r.Header.Set("X-Seat-Hub-Upload", header)
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, r)
		return w
	}
	for _, v := range []struct{ method, path string }{{"GET", "/api/tickets/1"}, {"POST", "/api/tickets/1/messages"}, {"POST", "/api/tickets/1/close"}, {"GET", att.URL}, {"GET", "/api/admin/tickets"}} {
		w := request(v.method, v.path, "application/json", strings.NewReader(`{"body":"正文"}`), "")
		if w.Code != 403 {
			t.Fatalf("%s %s: %d %s", v.method, v.path, w.Code, w.Body.String())
		}
	}
	f.user = &UserDTO{ID: 1, Role: "user"}
	var form bytes.Buffer
	writer := multipart.NewWriter(&form)
	part, err := writer.CreateFormFile("file", "../../image.png")
	if err != nil {
		t.Fatal(err)
	}
	part.Write(png)
	writer.Close()
	ct := writer.FormDataContentType()
	for _, v := range []struct {
		path, header string
		want         int
	}{{"/api/tickets/attachments", "", 415}, {"/api/tickets/attachments", "0", 415}, {"/api/tickets/attachments", "1", 201}, {"/api/tickets/1/messages", "1", 415}, {"/api/tickets/attachments/", "1", 415}} {
		w := request("POST", v.path, ct, bytes.NewReader(form.Bytes()), v.header)
		if w.Code != v.want {
			t.Fatalf("upload %s %q: %d %s", v.path, v.header, w.Code, w.Body.String())
		}
	}
	if w := request("POST", "/api/tickets/attachments", "multipart/form-data; boundary=bad", strings.NewReader("malformed"), "1"); w.Code != 422 {
		t.Fatalf("malformed=%d", w.Code)
	}
	if w := request("POST", "/api/tickets/attachments", "application/json", strings.NewReader(`{}`), "1"); w.Code != 415 {
		t.Fatalf("json upload=%d", w.Code)
	}
	w := request("GET", att.URL, "", nil, "")
	if w.Code != 200 || w.Header().Get("X-Content-Type-Options") != "nosniff" || w.Header().Get("Content-Security-Policy") != "sandbox" || w.Header().Get("Cache-Control") != "private" || !bytes.Equal(w.Body.Bytes(), png) {
		t.Fatalf("attachment: %d %v", w.Code, w.Header())
	}
	w = request("GET", "/api/tickets/1", "", nil, "")
	var detail struct {
		Data ticket.Detail `json:"data"`
	}
	json.Unmarshal(w.Body.Bytes(), &detail)
	if w.Code != 200 || detail.Data.Ticket.ID != d.Ticket.ID {
		t.Fatalf("detail=%s", w.Body.String())
	}
	f.user = &UserDTO{ID: 3, Role: "admin"}
	if w := request("GET", att.URL, "", nil, ""); w.Code != 200 {
		t.Fatalf("admin attachment=%d", w.Code)
	}
}

func TestTicketUploadMultipartValidation(t *testing.T) {
	db, err := store.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	_, err = db.Exec("INSERT INTO users(id,email,password_hash,role,status,created_at,updated_at) VALUES(1,'user@example.com','x','user','active','2026-01-01T00:00:00Z','2026-01-01T00:00:00Z')")
	if err != nil {
		t.Fatal(err)
	}
	f := &fakeAuth{user: &UserDTO{ID: 1, Role: "user"}}
	s := &Server{Deps: Deps{Auth: f, Tickets: ticket.New(db.DB, t.TempDir(), "", nil, nil)}, Log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	handler := f.Middleware(s.Handler())
	for _, v := range []struct {
		name, field, body string
		want              int
	}{{"no file", "", "", 422}, {"wrong field", "image", "GIF89a", 422}, {"sniff spoofed file", "file", "<svg></svg>", 422}, {"too large", "file", strings.Repeat("x", ticket.MaxAttachmentSize+1), 413}} {
		t.Run(v.name, func(t *testing.T) {
			var form bytes.Buffer
			writer := multipart.NewWriter(&form)
			if v.field != "" {
				part, err := writer.CreateFormFile(v.field, "fake.png")
				if err != nil {
					t.Fatal(err)
				}
				io.WriteString(part, v.body)
			}
			writer.Close()
			r := httptest.NewRequest("POST", "/api/tickets/attachments", &form)
			r.Header.Set("Content-Type", writer.FormDataContentType())
			r.Header.Set("X-Seat-Hub-Upload", "1")
			w := httptest.NewRecorder()
			handler.ServeHTTP(w, r)
			if w.Code != v.want {
				t.Fatalf("got %d: %s", w.Code, w.Body.String())
			}
		})
	}
}
