package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
)

var adminUserPatchMu sync.Mutex

func (s *Server) handleAdminDashboard(w http.ResponseWriter, r *http.Request) {
	dashboard, err := s.Deps.Admin.Dashboard(r.Context())
	if err != nil {
		s.fail(w, r, err)
		return
	}
	writeOK(w, dashboard)
}

func (s *Server) handleAdminListProducts(w http.ResponseWriter, r *http.Request) {
	items, err := s.Deps.Catalog.All(r.Context())
	if err != nil {
		s.fail(w, r, err)
		return
	}
	if items == nil {
		items = []ProductDTO{}
	}
	writeOK(w, map[string]any{"items": items})
}

func (s *Server) handleAdminCreateProduct(w http.ResponseWriter, r *http.Request) {
	var body ProductInput
	if err := readJSON(r, &body); err != nil {
		s.fail(w, r, err)
		return
	}
	product, err := s.Deps.Catalog.Create(r.Context(), body)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	writeCreated(w, map[string]any{"product": product})
}

func (s *Server) handleAdminPatchProduct(w http.ResponseWriter, r *http.Request) {
	id, ok := adminRequestID(w, r)
	if !ok {
		return
	}
	product, err := s.Deps.Catalog.Get(r.Context(), id)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	body := ProductInput{
		Kind: product.Kind, Name: product.Name, Description: product.Description,
		Badge: product.Badge, UnitQuantity: product.UnitQuantity,
		DurationDays: product.DurationDays, PriceCents: product.PriceCents,
		Currency: product.Currency, MinQty: product.MinQty, MaxQty: product.MaxQty,
		Enabled: &product.Enabled, SortOrder: product.SortOrder,
	}
	if err := readJSON(r, &body); err != nil {
		s.fail(w, r, err)
		return
	}
	updated, err := s.Deps.Catalog.Update(r.Context(), id, body)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	writeOK(w, map[string]any{"product": updated})
}

func (s *Server) handleAdminDeleteProduct(w http.ResponseWriter, r *http.Request) {
	id, ok := adminRequestID(w, r)
	if !ok {
		return
	}
	if err := s.Deps.Catalog.Delete(r.Context(), id); err != nil {
		s.fail(w, r, err)
		return
	}
	writeOK(w, map[string]any{})
}

func (s *Server) handleAdminOrders(w http.ResponseWriter, r *http.Request) {
	userID, ok := adminQueryUserID(w, r)
	if !ok {
		return
	}
	status := r.URL.Query().Get("status")
	if status != "" && status != "pending" && status != "paid" && status != "fulfilled" &&
		status != "fulfill_failed" && status != "cancelled" && status != "expired" && status != "refunded" {
		writeErr(w, http.StatusUnprocessableEntity, "invalid_status", "订单状态不正确")
		return
	}
	page, size := adminPageParams(r)
	items, total, err := s.Deps.Orders.AdminList(r.Context(), status, userID, page, size)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	writeOK(w, adminPageEnvelope(items, total, page, size))
}

func (s *Server) handleAdminMarkPaid(w http.ResponseWriter, r *http.Request) {
	var body struct {
		TradeNo string `json:"trade_no"`
	}
	if err := readJSON(r, &body); err != nil {
		s.fail(w, r, err)
		return
	}
	if err := s.Deps.Orders.MarkPaidByAdmin(r.Context(), r.PathValue("order_no"), body.TradeNo); err != nil {
		s.fail(w, r, err)
		return
	}
	writeOK(w, map[string]any{})
}

func (s *Server) handleAdminCancelOrder(w http.ResponseWriter, r *http.Request) {
	if err := s.Deps.Orders.Cancel(r.Context(), r.PathValue("order_no")); err != nil {
		s.fail(w, r, err)
		return
	}
	writeOK(w, map[string]any{})
}

func (s *Server) handleAdminRefulfill(w http.ResponseWriter, r *http.Request) {
	if err := s.Deps.Orders.Refulfill(r.Context(), r.PathValue("order_no")); err != nil {
		s.fail(w, r, err)
		return
	}
	writeOK(w, map[string]any{})
}

func (s *Server) handleAdminUsers(w http.ResponseWriter, r *http.Request) {
	page, size := adminPageParams(r)
	items, total, err := s.Deps.Users.AdminList(r.Context(), r.URL.Query().Get("q"), page, size)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	writeOK(w, adminPageEnvelope(items, total, page, size))
}

func (s *Server) handleAdminUserDetail(w http.ResponseWriter, r *http.Request) {
	id, ok := adminRequestID(w, r)
	if !ok {
		return
	}
	user, err := s.Deps.Users.AdminDetail(r.Context(), id)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	subscriptions, err := s.Deps.Usage.Subscriptions(r.Context(), id, "all")
	if err != nil {
		s.fail(w, r, err)
		return
	}
	accounts, err := s.Deps.Accounts.List(r.Context(), id, true)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	keys, err := s.Deps.Keys.List(r.Context(), id)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	orders := []OrderDTO{}
	for page := 1; ; page++ {
		items, total, err := s.Deps.Orders.ListByUser(r.Context(), id, page, 100)
		if err != nil {
			s.fail(w, r, err)
			return
		}
		orders = append(orders, items...)
		if len(items) == 0 || int64(len(orders)) >= total {
			break
		}
	}
	if accounts == nil {
		accounts = []AccountDTO{}
	}
	if keys == nil {
		keys = []KeyDTO{}
	}
	writeOK(w, map[string]any{
		"user": user, "subscriptions": subscriptionItems(subscriptions),
		"accounts": accounts, "keys": keys, "orders": orders,
	})
}

func (s *Server) handleAdminPatchUser(w http.ResponseWriter, r *http.Request) {
	id, ok := adminRequestID(w, r)
	if !ok {
		return
	}
	var body struct {
		Status *string `json:"status"`
		Role   *string `json:"role"`
	}
	if err := readJSON(r, &body); err != nil {
		s.fail(w, r, err)
		return
	}
	if body.Status != nil && *body.Status != "active" && *body.Status != "disabled" {
		writeErr(w, http.StatusUnprocessableEntity, "invalid_status", "status 只能是 active 或 disabled")
		return
	}
	if body.Role != nil && *body.Role != "user" && *body.Role != "admin" {
		writeErr(w, http.StatusUnprocessableEntity, "invalid_role", "role 只能是 user 或 admin")
		return
	}
	if body.Status == nil && body.Role == nil {
		writeErr(w, http.StatusUnprocessableEntity, "invalid_body", "请提供 status 或 role")
		return
	}
	adminUserPatchMu.Lock()
	defer adminUserPatchMu.Unlock()
	user, err := s.Deps.Users.ByID(r.Context(), id)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	removingAdmin := body.Status != nil && *body.Status == "disabled" || body.Role != nil && *body.Role == "user"
	if user.Role == "admin" && user.Status == "active" && removingAdmin {
		admins, err := s.Deps.Users.CountAdmins(r.Context())
		if err != nil {
			s.fail(w, r, err)
			return
		}
		if admins <= 1 {
			writeErr(w, http.StatusConflict, "last_admin", "不能停用或降级最后一个管理员")
			return
		}
	}
	if body.Status != nil && *body.Status != user.Status {
		if err := s.Deps.Users.SetStatus(r.Context(), id, *body.Status); err != nil {
			s.fail(w, r, err)
			return
		}
		user.Status = *body.Status
	}
	if body.Role != nil && *body.Role != user.Role {
		if err := s.Deps.Users.SetRole(r.Context(), id, *body.Role); err != nil {
			s.fail(w, r, err)
			return
		}
		user.Role = *body.Role
	}
	writeOK(w, map[string]any{"user": user})
}

func (s *Server) handleAdminGrant(w http.ResponseWriter, r *http.Request) {
	admin, ok := s.currentUser(r)
	if !ok {
		writeErr(w, http.StatusUnauthorized, "unauthorized", "请先登录")
		return
	}
	id, ok := adminRequestID(w, r)
	if !ok {
		return
	}
	var body struct {
		Kind         string `json:"kind"`
		Quantity     int    `json:"quantity"`
		DurationDays int    `json:"duration_days"`
		Note         string `json:"note"`
	}
	if err := readJSON(r, &body); err != nil {
		s.fail(w, r, err)
		return
	}
	if body.Kind != "concurrency" && body.Kind != "hosted_seat" && body.Kind != "rental_account" {
		writeErr(w, http.StatusUnprocessableEntity, "invalid_kind", "订阅类型不正确")
		return
	}
	if body.Quantity <= 0 || body.DurationDays <= 0 {
		writeErr(w, http.StatusUnprocessableEntity, "invalid_quantity", "数量和有效天数必须大于 0")
		return
	}
	if err := s.Deps.Admin.GrantSubscription(r.Context(), admin.ID, id, body.Kind, body.Quantity, body.DurationDays, body.Note); err != nil {
		s.fail(w, r, err)
		return
	}
	writeCreated(w, map[string]any{})
}

func (s *Server) handleAdminPatchSubscription(w http.ResponseWriter, r *http.Request) {
	id, ok := adminRequestID(w, r)
	if !ok {
		return
	}
	var body struct {
		ExpiresAt *string `json:"expires_at"`
		Status    *string `json:"status"`
	}
	if err := readJSON(r, &body); err != nil {
		s.fail(w, r, err)
		return
	}
	if body.ExpiresAt == nil && body.Status == nil {
		writeErr(w, http.StatusUnprocessableEntity, "invalid_body", "请提供 expires_at 或 status")
		return
	}
	if body.Status != nil && *body.Status != "cancelled" {
		writeErr(w, http.StatusUnprocessableEntity, "invalid_status", "status 只能是 cancelled")
		return
	}
	if body.ExpiresAt != nil {
		expiresAt, err := time.Parse(time.RFC3339, *body.ExpiresAt)
		if err != nil {
			writeErr(w, http.StatusUnprocessableEntity, "invalid_expires_at", "expires_at 必须是 RFC3339 时间")
			return
		}
		normalized := expiresAt.UTC().Format(time.RFC3339)
		body.ExpiresAt = &normalized
	}
	status := ""
	if body.Status != nil {
		status = *body.Status
	}
	if err := s.Deps.Admin.PatchSubscription(r.Context(), id, body.ExpiresAt, status); err != nil {
		s.fail(w, r, err)
		return
	}
	writeOK(w, map[string]any{})
}

func (s *Server) handleAdminRentalPool(w http.ResponseWriter, r *http.Request) {
	items, err := s.Deps.Admin.RentalPool(r.Context())
	if err != nil {
		s.fail(w, r, err)
		return
	}
	if items == nil {
		items = []RentalPoolRowDTO{}
	}
	writeOK(w, map[string]any{"items": items})
}

func (s *Server) handleAdminRentalCandidates(w http.ResponseWriter, r *http.Request) {
	items, err := s.Deps.Admin.RentalCandidates(r.Context())
	if err != nil {
		s.fail(w, r, err)
		return
	}
	if items == nil {
		items = []RentalCandidateDTO{}
	}
	writeOK(w, map[string]any{"items": items})
}

func (s *Server) handleAdminRentalAdd(w http.ResponseWriter, r *http.Request) {
	var body struct {
		VMIDs []string `json:"vm_ids"`
		Note  string   `json:"note"`
	}
	if err := readJSON(r, &body); err != nil {
		s.fail(w, r, err)
		return
	}
	if len(body.VMIDs) == 0 {
		writeErr(w, http.StatusUnprocessableEntity, "invalid_vm_ids", "请至少选择一个 VM")
		return
	}
	for _, vmID := range body.VMIDs {
		if strings.TrimSpace(vmID) == "" {
			writeErr(w, http.StatusUnprocessableEntity, "invalid_vm_ids", "VM ID 不能为空")
			return
		}
	}
	if err := s.Deps.Admin.RentalAdd(r.Context(), body.VMIDs, body.Note); err != nil {
		s.fail(w, r, err)
		return
	}
	writeCreated(w, map[string]any{})
}

func (s *Server) handleAdminRentalPatch(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Status string  `json:"status"`
		Note   *string `json:"note"`
	}
	if err := readJSON(r, &body); err != nil {
		s.fail(w, r, err)
		return
	}
	if body.Status != "available" && body.Status != "maintenance" {
		writeErr(w, http.StatusUnprocessableEntity, "invalid_status", "status 只能是 available 或 maintenance")
		return
	}
	vmID := r.PathValue("vm_id")
	note := ""
	if body.Note == nil {
		items, err := s.Deps.Admin.RentalPool(r.Context())
		if err != nil {
			s.fail(w, r, err)
			return
		}
		for _, item := range items {
			if item.VMID == vmID {
				note = item.Note
				break
			}
		}
	} else {
		note = *body.Note
	}
	if err := s.Deps.Admin.RentalPatch(r.Context(), vmID, body.Status, note); err != nil {
		s.fail(w, r, err)
		return
	}
	writeOK(w, map[string]any{})
}

func (s *Server) handleAdminRentalRemove(w http.ResponseWriter, r *http.Request) {
	if err := s.Deps.Admin.RentalRemove(r.Context(), r.PathValue("vm_id")); err != nil {
		s.fail(w, r, err)
		return
	}
	writeOK(w, map[string]any{})
}

func (s *Server) handleAdminRentalSelfcheck(w http.ResponseWriter, r *http.Request) {
	vmID := r.PathValue("vm_id")
	if strings.TrimSpace(vmID) == "" {
		writeErr(w, http.StatusUnprocessableEntity, "invalid_request", "VM ID 不能为空")
		return
	}
	result, err := s.Deps.Admin.RunPoolSelfcheck(r.Context(), vmID)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	writeOK(w, result)
}

func (s *Server) handleAdminAccountSelfcheck(w http.ResponseWriter, r *http.Request) {
	id, ok := adminSelfcheckRequestID(w, r)
	if !ok {
		return
	}
	body := &struct {
		Rebuild json.RawMessage `json:"rebuild"`
	}{}
	if err := readJSON(r, &body); err != nil || body == nil {
		writeErr(w, http.StatusUnprocessableEntity, "invalid_request", "请求体须为 JSON 对象，rebuild 须为布尔值")
		return
	}
	rebuild := false
	if len(body.Rebuild) > 0 {
		switch string(body.Rebuild) {
		case "true":
			rebuild = true
		case "false":
		default:
			writeErr(w, http.StatusUnprocessableEntity, "invalid_request", "rebuild 须为布尔值")
			return
		}
	}
	result, err := s.Deps.Admin.RunSelfcheck(r.Context(), id, rebuild)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	writeOK(w, result)
}

func (s *Server) handleAdminAccountSelfchecks(w http.ResponseWriter, r *http.Request) {
	id, ok := adminSelfcheckRequestID(w, r)
	if !ok {
		return
	}
	limit := 20
	if values, exists := r.URL.Query()["limit"]; exists {
		if len(values) != 1 {
			writeErr(w, http.StatusUnprocessableEntity, "invalid_request", "limit 须为 1 到 100 的整数，且只能提供一次")
			return
		}
		value, err := strconv.Atoi(values[0])
		if err != nil || value < 1 || value > 100 {
			writeErr(w, http.StatusUnprocessableEntity, "invalid_request", "limit 须为 1 到 100 的整数")
			return
		}
		limit = value
	}
	items, err := s.Deps.Admin.SelfcheckHistory(r.Context(), id, limit)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	if items == nil {
		items = []SelfcheckReportDTO{}
	}
	writeOK(w, map[string]any{"items": items})
}

func adminSelfcheckRequestID(w http.ResponseWriter, r *http.Request) (int64, bool) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil || id <= 0 {
		writeErr(w, http.StatusUnprocessableEntity, "invalid_request", "ID 必须是正整数")
		return 0, false
	}
	return id, true
}

func (s *Server) handleAdminAccounts(w http.ResponseWriter, r *http.Request) {
	userID, ok := adminQueryUserID(w, r)
	if !ok {
		return
	}
	kind := r.URL.Query().Get("kind")
	if kind != "" && kind != "hosted" && kind != "rental" {
		writeErr(w, http.StatusUnprocessableEntity, "invalid_kind", "kind 只能是 hosted 或 rental")
		return
	}
	page, size := adminPageParams(r)
	items, total, err := s.Deps.Accounts.AdminList(r.Context(), kind, r.URL.Query().Get("status"), userID, page, size)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	writeOK(w, adminPageEnvelope(items, total, page, size))
}

func (s *Server) handleAdminRetryAccount(w http.ResponseWriter, r *http.Request) {
	id, ok := adminRequestID(w, r)
	if !ok {
		return
	}
	if err := s.Deps.Accounts.AdminRetry(r.Context(), id); err != nil {
		s.fail(w, r, err)
		return
	}
	writeOK(w, map[string]any{})
}

func (s *Server) handleAdminReleaseAccount(w http.ResponseWriter, r *http.Request) {
	id, ok := adminRequestID(w, r)
	if !ok {
		return
	}
	if err := s.Deps.Accounts.AdminRelease(r.Context(), id); err != nil {
		s.fail(w, r, err)
		return
	}
	writeOK(w, map[string]any{})
}

func (s *Server) handleAdminJobs(w http.ResponseWriter, r *http.Request) {
	status := r.URL.Query().Get("status")
	if status != "" && status != "failed" && status != "pending" && status != "running" {
		writeErr(w, http.StatusUnprocessableEntity, "invalid_status", "status 只能是 failed、pending 或 running")
		return
	}
	page, size := adminPageParams(r)
	items, total, err := s.Deps.Admin.Jobs(r.Context(), status, page, size)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	writeOK(w, adminPageEnvelope(items, total, page, size))
}

func (s *Server) handleAdminRetryJob(w http.ResponseWriter, r *http.Request) {
	id, ok := adminRequestID(w, r)
	if !ok {
		return
	}
	if err := s.Deps.Admin.RetryJob(r.Context(), id); err != nil {
		s.fail(w, r, err)
		return
	}
	writeOK(w, map[string]any{})
}

func (s *Server) handleAdminGetSettings(w http.ResponseWriter, r *http.Request) {
	settings, err := s.Deps.Settings.Get(r.Context())
	if err != nil {
		s.fail(w, r, err)
		return
	}
	settings.Payment, err = adminMaskPayment(settings.Payment)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	writeOK(w, settings)
}

func (s *Server) handleAdminPutSettings(w http.ResponseWriter, r *http.Request) {
	admin, ok := s.currentUser(r)
	if !ok {
		writeErr(w, http.StatusUnauthorized, "unauthorized", "请先登录")
		return
	}
	settings, err := s.Deps.Settings.Get(r.Context())
	if err != nil {
		s.fail(w, r, err)
		return
	}
	previousPayment := settings.Payment
	settings.Payment = append(json.RawMessage(nil), settings.Payment...)
	settings.HostedVMTemplate = append(json.RawMessage(nil), settings.HostedVMTemplate...)
	if err := readJSON(r, &settings); err != nil {
		s.fail(w, r, err)
		return
	}
	if strings.TrimSpace(settings.SiteName) == "" || settings.BaseConcurrency < 0 ||
		settings.ConcurrencyPerHostedSeat < 0 || settings.ConcurrencyPerRental < 0 ||
		settings.HostedGraceHours < 0 || settings.RentalGraceHours < 0 || settings.GatewayQueueTimeoutMS < 0 {
		writeErr(w, http.StatusUnprocessableEntity, "invalid_settings", "站点名称不能为空，配额和超时时间不能为负数")
		return
	}
	var template map[string]json.RawMessage
	if err := json.Unmarshal(settings.HostedVMTemplate, &template); err != nil || template == nil {
		writeErr(w, http.StatusUnprocessableEntity, "invalid_settings", "hosted_vm_template 必须是 JSON 对象")
		return
	}
	settings.Payment, err = adminMergePayment(previousPayment, settings.Payment)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	masked := settings
	masked.Payment, err = adminMaskPayment(settings.Payment)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	if err := s.Deps.Settings.Put(r.Context(), settings, admin.ID); err != nil {
		s.fail(w, r, err)
		return
	}
	s.Deps.Audit.Write(r.Context(), admin.ID, "settings.update", "site", masked)
	writeOK(w, masked)
}

func adminRequestID(w http.ResponseWriter, r *http.Request) (int64, bool) {
	id := pathID(r)
	if id <= 0 {
		writeErr(w, http.StatusUnprocessableEntity, "invalid_id", "ID 必须是正整数")
		return 0, false
	}
	return id, true
}

func adminQueryUserID(w http.ResponseWriter, r *http.Request) (int64, bool) {
	value := r.URL.Query().Get("user_id")
	if value == "" {
		return 0, true
	}
	id, err := strconv.ParseInt(value, 10, 64)
	if err != nil || id <= 0 {
		writeErr(w, http.StatusUnprocessableEntity, "invalid_user_id", "user_id 必须是正整数")
		return 0, false
	}
	return id, true
}

func adminPageParams(r *http.Request) (int, int) {
	page, size := queryInt(r, "page", 1), queryInt(r, "page_size", 20)
	if page < 1 {
		page = 1
	}
	if size < 1 {
		size = 20
	}
	if size > 100 {
		size = 100
	}
	return page, size
}

func adminPageEnvelope[Item any](items []Item, total int64, page, size int) map[string]any {
	if items == nil {
		items = []Item{}
	}
	return map[string]any{"items": items, "total": total, "page": page, "page_size": size}
}

func adminPaymentFields(raw json.RawMessage) (map[string]map[string]json.RawMessage, error) {
	providers := map[string]map[string]json.RawMessage{}
	if len(raw) == 0 {
		return providers, nil
	}
	if err := json.Unmarshal(raw, &providers); err != nil || providers == nil {
		return nil, bad("invalid_settings", "payment 必须是支付提供方配置对象")
	}
	for _, fields := range providers {
		if fields == nil {
			return nil, bad("invalid_settings", "支付提供方配置必须是 JSON 对象")
		}
	}
	return providers, nil
}

func adminMergePayment(previous, patch json.RawMessage) (json.RawMessage, error) {
	providers, err := adminPaymentFields(previous)
	if err != nil {
		return nil, fmt.Errorf("decode stored payment settings: %w", err)
	}
	updates, err := adminPaymentFields(patch)
	if err != nil {
		return nil, err
	}
	for provider, fields := range updates {
		if providers[provider] == nil {
			providers[provider] = map[string]json.RawMessage{}
		}
		for name, value := range fields {
			if name == "key" {
				var secret *string
				if err := json.Unmarshal(value, &secret); err != nil || secret == nil {
					return nil, bad("invalid_settings", "支付密钥必须是字符串")
				}
				if *secret == "" || *secret == "***" {
					if _, exists := providers[provider][name]; !exists {
						providers[provider][name] = json.RawMessage(`""`)
					}
					continue
				}
			}
			providers[provider][name] = value
		}
	}
	merged, err := json.Marshal(providers)
	if err != nil {
		return nil, fmt.Errorf("encode payment settings: %w", err)
	}
	return merged, nil
}

func adminMaskPayment(raw json.RawMessage) (json.RawMessage, error) {
	providers, err := adminPaymentFields(raw)
	if err != nil {
		return nil, err
	}
	for _, fields := range providers {
		value, exists := fields["key"]
		if !exists {
			continue
		}
		var secret *string
		if err := json.Unmarshal(value, &secret); err != nil || secret == nil {
			return nil, bad("invalid_settings", "支付密钥必须是字符串")
		}
		if *secret != "" {
			fields["key"] = json.RawMessage(`"***"`)
		}
	}
	masked, err := json.Marshal(providers)
	if err != nil {
		return nil, fmt.Errorf("encode masked payment settings: %w", err)
	}
	return masked, nil
}
