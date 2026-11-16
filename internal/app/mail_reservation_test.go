package app

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/DueGin/seat-hub/internal/api"
	"github.com/DueGin/seat-hub/internal/mail"
	"github.com/DueGin/seat-hub/internal/reservation"
)

func TestMailAndReservationsHTTP(t *testing.T) {
	h := newHarness(t)
	h.login("admin@example.com", "admin-pass-123")
	var created struct {
		Product api.ProductDTO `json:"product"`
	}
	h.mustOK(http.MethodPost, "/api/admin/products", `{"kind":"rental_account","name":"预约方案","unit_quantity":1,"duration_days":30,"price_cents":100,"min_qty":1,"max_qty":5,"enabled":true,"reservation_only":true}`, &created)
	if !created.Product.ReservationOnly {
		t.Fatal("missing reservation flag")
	}
	id := created.Product.ID
	h.mustOK(http.MethodPatch, fmt.Sprintf("/api/admin/products/%d", id), `{"badge":"新品"}`, &created)
	if !created.Product.ReservationOnly {
		t.Fatal("partial update lost reservation flag")
	}
	status, env := h.call(http.MethodPost, "/api/orders", fmt.Sprintf(`{"product_id":%d,"quantity":1,"provider":"manual"}`, id), nil)
	if status != 422 || env.Error.Code != "product_reservation_only" {
		t.Fatalf("order status=%d err=%+v", status, env.Error)
	}
	var cfg mail.Settings
	h.mustOK(http.MethodGet, "/api/admin/settings/mail", "", &cfg)
	cfg = mail.Settings{Enabled: true, Host: "localhost", Port: 25, Security: "none", FromAddress: "sender@example.com", Password: "secret-test"}
	body, _ := json.Marshal(cfg)
	h.mustOK(http.MethodPut, "/api/admin/settings/mail", string(body), &cfg)
	if cfg.Password != "***" {
		t.Fatal("smtp password leaked")
	}
	h.mustOK(http.MethodPost, "/api/auth/logout", `{}`, nil)
	reservePath := fmt.Sprintf("/api/public/products/%d/reservations", id)
	h.mustOK(http.MethodPost, reservePath, `{"email":"ONE@example.com","note":"=HYPERLINK(\"bad\")"}`, nil)
	h.mustOK(http.MethodPost, reservePath, `{"email":"one@example.com"}`, nil)
	status, _ = h.call(http.MethodGet, "/api/admin/reservations", "", nil)
	if status != 401 {
		t.Fatalf("admin leaked status=%d", status)
	}
	h.login("admin@example.com", "admin-pass-123")
	var list reservation.Page
	h.mustOK(http.MethodGet, "/api/admin/reservations", "", &list)
	if list.Total != 1 || list.Items[0].Email != "one@example.com" || list.Items[0].UserID != nil {
		t.Fatalf("list=%+v", list)
	}
	response, err := h.client.Get(h.portal.URL + "/api/admin/reservations/export")
	if err != nil {
		t.Fatal(err)
	}
	csv, _ := io.ReadAll(response.Body)
	response.Body.Close()
	if !strings.HasPrefix(string(csv), "\xef\xbb\xbf") || !strings.Contains(string(csv), "'=HYPERLINK") {
		t.Fatalf("unsafe csv: %q", csv)
	}
	var notified struct {
		Count int64 `json:"count"`
	}
	h.mustOK(http.MethodPost, fmt.Sprintf("/api/admin/products/%d/reservations/notify", id), `{"subject":"开售","body":"欢迎购买"}`, &notified)
	if notified.Count != 1 {
		t.Fatal(notified.Count)
	}
	h.mustOK(http.MethodPost, fmt.Sprintf("/api/admin/products/%d/reservations/notify", id), `{"subject":"开售","body":"欢迎购买"}`, &notified)
	if notified.Count != 0 {
		t.Fatal("duplicate notification")
	}
	h.mustOK(http.MethodDelete, fmt.Sprintf("/api/admin/reservations/%d", list.Items[0].ID), `{}`, nil)
}
