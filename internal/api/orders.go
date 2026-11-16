package api

import (
	"context"
	"errors"
	"net/http"
)

func (s *Server) handleCreateOrder(w http.ResponseWriter, r *http.Request) {
	u, ok := s.currentUser(r)
	if !ok {
		writeErr(w, http.StatusUnauthorized, "unauthorized", "请先登录")
		return
	}
	var body struct {
		ProductID           int64  `json:"product_id"`
		Quantity            int    `json:"quantity"`
		Provider            string `json:"provider"`
		RenewSubscriptionID *int64 `json:"renew_subscription_id"`
	}
	if err := readJSON(r, &body); err != nil {
		s.fail(w, r, err)
		return
	}
	if body.ProductID <= 0 {
		writeErr(w, http.StatusUnprocessableEntity, "invalid_product", "请选择商品")
		return
	}
	available := false
	for _, provider := range s.Deps.Payments.Providers(r.Context()) {
		if provider.ID == body.Provider {
			available = true
			break
		}
	}
	if !available {
		writeErr(w, 422, "payment_provider_unavailable", "当前不支持该收款方式")
		return
	}
	o, err := s.Deps.Orders.Create(r.Context(), u.ID, body.ProductID, body.Quantity, body.Provider, body.RenewSubscriptionID)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	payment, err := s.Deps.Payments.CreatePayment(r.Context(), o)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	writeCreated(w, map[string]any{"order": o, "payment": payment})
}

func (s *Server) handleListOrders(w http.ResponseWriter, r *http.Request) {
	u, ok := s.currentUser(r)
	if !ok {
		writeErr(w, http.StatusUnauthorized, "unauthorized", "请先登录")
		return
	}
	page, size := pageParams(r)
	items, total, err := s.Deps.Orders.ListByUser(r.Context(), u.ID, page, size)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	writeOK(w, pageEnvelope(items, total, page, size))
}

func (s *Server) handleGetOrder(w http.ResponseWriter, r *http.Request) {
	u, ok := s.currentUser(r)
	if !ok {
		writeErr(w, http.StatusUnauthorized, "unauthorized", "请先登录")
		return
	}
	o, err := s.Deps.Orders.Get(r.Context(), r.PathValue("order_no"))
	if err != nil {
		s.fail(w, r, err)
		return
	}
	if o.UserID != 0 && o.UserID != u.ID && u.Role != "admin" {
		writeErr(w, http.StatusNotFound, "not_found", "订单不存在")
		return
	}
	if o.Status == "pending" && o.Provider == "alipay_f2f" {
		if query, ok := s.Deps.Payments.(interface {
			QueryPending(context.Context, OrderDTO) error
		}); ok {
			if err := query.QueryPending(r.Context(), o); err != nil {
				s.Log.Warn("payment query", "order_no", o.OrderNo, "err", err)
			}
			updated, err := s.Deps.Orders.Get(r.Context(), o.OrderNo)
			if err == nil {
				o = updated
			}
		}
	}
	writeOK(w, map[string]any{"order": o})
}

func (s *Server) handleCancelOrder(w http.ResponseWriter, r *http.Request) {
	u, ok := s.currentUser(r)
	if !ok {
		writeErr(w, http.StatusUnauthorized, "unauthorized", "请先登录")
		return
	}
	o, err := s.Deps.Orders.Get(r.Context(), r.PathValue("order_no"))
	if err != nil {
		s.fail(w, r, err)
		return
	}
	if o.UserID != 0 && o.UserID != u.ID && u.Role != "admin" {
		writeErr(w, http.StatusNotFound, "not_found", "订单不存在")
		return
	}
	if err := s.Deps.Orders.Cancel(r.Context(), o.OrderNo); err != nil {
		s.fail(w, r, err)
		return
	}
	writeOK(w, map[string]any{})
}

// handleMockPay is only reachable when the deployment runs with PORTAL_DEV=1;
// Handler() refuses it otherwise.
func (s *Server) handleMockPay(w http.ResponseWriter, r *http.Request) {
	if !s.Deps.Dev {
		writeErr(w, http.StatusNotFound, "not_found", "未找到")
		return
	}
	u, ok := s.currentUser(r)
	if !ok {
		writeErr(w, http.StatusUnauthorized, "unauthorized", "请先登录")
		return
	}
	o, err := s.Deps.Orders.Get(r.Context(), r.PathValue("order_no"))
	if err != nil {
		s.fail(w, r, err)
		return
	}
	if o.UserID != 0 && o.UserID != u.ID {
		writeErr(w, http.StatusNotFound, "not_found", "订单不存在")
		return
	}
	if err := s.Deps.Payments.MockPay(r.Context(), u.ID, o.OrderNo); err != nil {
		s.fail(w, r, err)
		return
	}
	writeOK(w, map[string]any{})
}

func (s *Server) handlePaymentNotify(w http.ResponseWriter, r *http.Request) {
	provider := r.PathValue("provider")
	status, body, err := s.Deps.Payments.Notify(r.Context(), provider, r)
	if err != nil {
		s.Log.Warn("payment notify", "provider", provider, "err", err)
		// Providers retry on non-success bodies; still use the provider's own
		// failure body when the adapter produced one.
		if body == "" {
			body = "fail"
		}
		if status == 0 {
			status = http.StatusBadRequest
		}
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(status)
	_, _ = w.Write([]byte(body))
}

func (s *Server) handlePaymentReturn(w http.ResponseWriter, r *http.Request) {
	provider := r.PathValue("provider")
	target, err := s.Deps.Payments.ReturnURL(r.Context(), provider, r)
	if err != nil {
		s.Log.Warn("payment return", "provider", provider, "err", err)
		http.Redirect(w, r, "/orders", http.StatusFound)
		return
	}
	http.Redirect(w, r, target, http.StatusFound)
}

var errNotImplemented = errors.New("not implemented")
