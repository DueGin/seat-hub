package app

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"os"
	"path/filepath"
	"testing"

	"github.com/DueGin/seat-hub/internal/ticket"
)

func TestTicketsHTTP(t *testing.T) {
	h := newHarness(t)
	h.mustOK(http.MethodPost, "/api/auth/register", `{"email":"ticket-user@example.com","password":"ticket-password"}`, nil)
	var detail ticket.Detail
	h.mustOK(http.MethodPost, "/api/tickets", `{"category":"api","title":"调用问题","body":"请检查调用记录"}`, &detail)
	id := detail.Ticket.ID
	if id == 0 || detail.Ticket.Status != "open" {
		t.Fatalf("create=%+v", detail)
	}
	path := fmt.Sprintf("/api/tickets/%d", id)
	image, _ := base64.StdEncoding.DecodeString("iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAQAAAC1HAwCAAAAC0lEQVR42mP8/x8AAwMCAO+jZl8AAAAASUVORK5CYII=")
	var form bytes.Buffer
	writer := multipart.NewWriter(&form)
	part, err := writer.CreateFormFile("file", "screen.png")
	if err != nil {
		t.Fatal(err)
	}
	part.Write(image)
	writer.Close()
	req, err := http.NewRequest(http.MethodPost, h.portal.URL+"/api/tickets/attachments", &form)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", writer.FormDataContentType())
	req.Header.Set("X-Seat-Hub-Upload", "1")
	response, err := h.client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	var upload struct {
		OK   bool              `json:"ok"`
		Data ticket.Attachment `json:"data"`
	}
	err = json.NewDecoder(response.Body).Decode(&upload)
	response.Body.Close()
	if err != nil || response.StatusCode != 201 || !upload.OK {
		t.Fatalf("upload status=%d err=%v", response.StatusCode, err)
	}
	entries, err := os.ReadDir(filepath.Join(filepath.Dir(h.app.Cfg.DBPath), "attachments"))
	if err != nil || len(entries) != 1 {
		t.Fatalf("attachment directory=%v %v", entries, err)
	}
	h.mustOK(http.MethodPost, path+"/messages", fmt.Sprintf(`{"body":"![截图](%s)","attachment_ids":[%d]}`, upload.Data.URL, upload.Data.ID), nil)
	h.mustOK(http.MethodPost, "/api/auth/logout", `{}`, nil)
	h.mustOK(http.MethodPost, "/api/auth/register", `{"email":"other-ticket@example.com","password":"ticket-password"}`, nil)
	for _, url := range []string{path, upload.Data.URL} {
		status, env := h.call(http.MethodGet, url, "", nil)
		if status != 403 || env.Error.Code != "ticket_forbidden" {
			t.Fatalf("ownership %s=%d %+v", url, status, env.Error)
		}
	}
	h.mustOK(http.MethodPost, "/api/auth/logout", `{}`, nil)
	h.login("admin@example.com", "admin-pass-123")
	h.mustOK(http.MethodGet, "/api/admin/tickets", "", nil)
	h.mustOK(http.MethodPost, fmt.Sprintf("/api/admin/tickets/%d/messages", id), `{"body":"已处理，请重试"}`, &detail)
	if detail.Ticket.Status != "replied" {
		t.Fatal(detail.Ticket.Status)
	}
	h.mustOK(http.MethodPost, "/api/auth/logout", `{}`, nil)
	h.login("ticket-user@example.com", "ticket-password")
	var unread struct {
		Count int `json:"count"`
	}
	h.mustOK(http.MethodGet, "/api/tickets/unread", "", &unread)
	if unread.Count != 1 {
		t.Fatal(unread.Count)
	}
	h.mustOK(http.MethodGet, path, "", &detail)
	h.mustOK(http.MethodGet, "/api/tickets/unread", "", &unread)
	if unread.Count != 0 {
		t.Fatal(unread.Count)
	}
	response, err = h.client.Get(h.portal.URL + upload.Data.URL)
	if err != nil {
		t.Fatal(err)
	}
	body, err := io.ReadAll(response.Body)
	response.Body.Close()
	if err != nil || !bytes.Equal(body, image) {
		t.Fatal("image body mismatch", err)
	}
	h.mustOK(http.MethodPost, path+"/close", `{}`, nil)
	h.mustOK(http.MethodPost, path+"/messages", `{"body":"还有一个问题"}`, &detail)
	if detail.Ticket.Status != "open" {
		t.Fatal(detail.Ticket.Status)
	}
}
