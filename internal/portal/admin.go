package portal

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/DueGin/seat-hub/internal/api"
	"github.com/DueGin/seat-hub/internal/privacy"
	"github.com/DueGin/seat-hub/internal/provision"
	"github.com/DueGin/seat-hub/internal/secret"
	"github.com/DueGin/seat-hub/internal/store"
	"github.com/DueGin/seat-hub/internal/vm2api"
)

var _ api.AdminCoreService = (*Service)(nil)

func adminError(status int, code, message string) error {
	return &api.UserError{Status: status, Code: code, Message: message}
}

func (service *Service) Dashboard(ctx context.Context) (api.AdminDashboardDTO, error) {
	var result api.AdminDashboardDTO
	now := service.now()
	err := service.db.QueryRowContext(ctx, `SELECT
		(SELECT COUNT(*) FROM users),
		COUNT(DISTINCT CASE WHEN provider<>'grant' AND amount_cents>0 THEN user_id END), COALESCE(SUM(amount_cents),0),
		(SELECT COUNT(*) FROM orders WHERE status='pending')
		FROM orders WHERE status IN ('paid','fulfilled')
		AND julianday(paid_at)>=julianday(?) AND julianday(paid_at)<=julianday(?)`,
		store.FmtTime(now.AddDate(0, 0, -30)), store.FmtTime(now)).Scan(
		&result.Users, &result.PayingUsers, &result.RevenueCents, &result.OrdersPending)
	if err != nil {
		return result, fmt.Errorf("查询付费统计失败: %w", err)
	}
	err = service.db.QueryRowContext(ctx, `SELECT
		COALESCE(SUM(kind='hosted' AND status='active'),0),
		COALESCE(SUM(kind='rental' AND status='active'),0),
		COALESCE(SUM(status='failed'),0) FROM resources`).Scan(
		&result.Accounts.HostedActive, &result.Accounts.RentalActive, &result.Accounts.Failed)
	if err != nil {
		return result, fmt.Errorf("查询账号统计失败: %w", err)
	}
	err = service.db.QueryRowContext(ctx, `SELECT COUNT(*), COALESCE(SUM(status='available'),0),
		COALESCE(SUM(status='leased'),0), COALESCE(SUM(status='maintenance'),0) FROM rental_pool`).Scan(
		&result.RentalPool.Total, &result.RentalPool.Available, &result.RentalPool.Leased, &result.RentalPool.Maintenance)
	if err != nil {
		return result, fmt.Errorf("查询租赁池统计失败: %w", err)
	}
	health, err := service.client.Ping(ctx)
	result.VM2API = api.VM2APIHealth{OK: health.OK, Version: health.Version, LatencyMS: health.LatencyMS}
	if err != nil {
		result.VM2API.OK = false
		result.VM2API.Error = "连接上游服务失败"
	} else if !health.OK {
		result.VM2API.Error = "上游服务暂不可用"
	}
	return result, nil
}

func adminWindow(window *vm2api.Window) *api.WindowUsage {
	if window == nil {
		return nil
	}
	result := &api.WindowUsage{Utilization: window.Utilization, Status: window.Status}
	if window.ResetsAt != nil {
		result.ResetsAt = store.FmtTime(*window.ResetsAt)
	}
	return result
}

func (service *Service) RentalPool(ctx context.Context) ([]api.RentalPoolRowDTO, error) {
	rows, err := service.db.QueryContext(ctx, `SELECT pool.vm_id,pool.tier,pool.status,
		resource.user_id,COALESCE(user.email,''),pool.note,pool.selfcheck_state,pool.selfcheck_checked_at FROM rental_pool AS pool
		LEFT JOIN resources AS resource ON resource.id=pool.leased_resource_id
		LEFT JOIN users AS user ON user.id=resource.user_id ORDER BY pool.vm_id`)
	if err != nil {
		return nil, fmt.Errorf("查询租赁池失败: %w", err)
	}
	defer rows.Close()
	items := make([]api.RentalPoolRowDTO, 0)
	for rows.Next() {
		var item api.RentalPoolRowDTO
		var state string
		var checkedAt sql.NullString
		if err := rows.Scan(&item.VMID, &item.Tier, &item.Status, &item.LeasedToUserID, &item.LeasedToEmail, &item.Note, &state, &checkedAt); err != nil {
			return nil, fmt.Errorf("读取租赁池账号失败: %w", err)
		}
		item.Selfcheck = &api.SelfcheckDTO{State: state}
		if checkedAt.Valid {
			item.Selfcheck.CheckedAt = &checkedAt.String
		}
		items = append(items, item)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("读取租赁池失败: %w", err)
	}
	rows.Close()
	if len(items) == 0 {
		return items, nil
	}
	vms, err := service.client.ListVMs(ctx, vm2api.VMFilter{})
	if err != nil {
		return nil, fmt.Errorf("获取上游账号失败: %w", err)
	}
	byID := make(map[string]vm2api.VM, len(vms))
	for _, vm := range vms {
		byID[vm.ID] = vm
	}
	for index := range items {
		if vm, exists := byID[items[index].VMID]; exists {
			items[index].VM = &api.RentalVMDTO{
				Label: vm.Label, EmailMasked: privacy.MaskEmail(vm.EmailMasked), CredStatus: vm.CredStatus,
				ScheduleState: vm.ScheduleState, Tier: vm.Tier,
				FiveHour: adminWindow(vm.FiveHour), SevenDay: adminWindow(vm.SevenDay),
			}
		}
	}
	return items, nil
}

func adminRentable(vm vm2api.VM) bool {
	return vm.Platform == "anthropic" && vm.OwnerUserID == "" && vm.Origin == "platform"
}

func (service *Service) RentalCandidates(ctx context.Context) ([]api.RentalCandidateDTO, error) {
	vms, err := service.client.ListVMs(ctx, vm2api.VMFilter{Unowned: true, ClaudeOnly: true})
	if err != nil {
		return nil, fmt.Errorf("获取候选账号失败: %w", err)
	}
	rows, err := service.db.QueryContext(ctx, `SELECT vm_id FROM rental_pool
		UNION SELECT vm_id FROM resources WHERE vm_id IS NOT NULL AND status<>'released'`)
	if err != nil {
		return nil, fmt.Errorf("查询已归属账号失败: %w", err)
	}
	defer rows.Close()
	excluded := make(map[string]bool)
	for rows.Next() {
		var vmID string
		if err := rows.Scan(&vmID); err != nil {
			return nil, fmt.Errorf("读取已归属账号失败: %w", err)
		}
		excluded[vmID] = true
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("读取已归属账号失败: %w", err)
	}
	items := make([]api.RentalCandidateDTO, 0)
	for _, vm := range vms {
		if adminRentable(vm) && !excluded[vm.ID] {
			items = append(items, api.RentalCandidateDTO{
				VMID: vm.ID, Label: vm.Label, EmailMasked: privacy.MaskEmail(vm.EmailMasked),
				CredStatus: vm.CredStatus, ScheduleState: vm.ScheduleState, Tier: vm.Tier,
			})
		}
	}
	sort.Slice(items, func(left, right int) bool { return items[left].VMID < items[right].VMID })
	return items, nil
}

func (service *Service) RentalAdd(ctx context.Context, vmIDs []string, note string) error {
	if len(vmIDs) == 0 {
		return adminError(http.StatusUnprocessableEntity, "invalid_vm_ids", "请至少选择一个账号")
	}
	seen := make(map[string]bool, len(vmIDs))
	for _, vmID := range vmIDs {
		if strings.TrimSpace(vmID) == "" || seen[vmID] {
			return adminError(http.StatusUnprocessableEntity, "invalid_vm_ids", "账号标识不能为空或重复")
		}
		seen[vmID] = true
	}
	service.mu.Lock()
	defer service.mu.Unlock()
	vms, err := service.client.ListVMs(ctx, vm2api.VMFilter{})
	if err != nil {
		return fmt.Errorf("获取上游账号失败: %w", err)
	}
	byID := make(map[string]vm2api.VM, len(vms))
	for _, vm := range vms {
		byID[vm.ID] = vm
	}
	for _, vmID := range vmIDs {
		vm, exists := byID[vmID]
		if !exists {
			return adminError(http.StatusNotFound, "vm_not_found", "上游账号不存在")
		}
		if !adminRentable(vm) {
			return adminError(http.StatusConflict, "vm_not_available", "账号已归属用户或不符合入池条件")
		}
		var unavailable bool
		if err := service.db.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM rental_pool WHERE vm_id=?)
			OR EXISTS(SELECT 1 FROM resources WHERE vm_id=? AND status<>'released')`, vmID, vmID).Scan(&unavailable); err != nil {
			return fmt.Errorf("check rental vm availability: %w", err)
		}
		if unavailable {
			return adminError(http.StatusConflict, "vm_not_available", "账号已入池或已归属用户")
		}
	}
	checked := make(map[string]provision.SelfcheckState, len(vmIDs))
	for _, vmID := range vmIDs {
		state, err := service.provisioning.AdmitPoolVM(ctx, vmID)
		if err != nil || state.State != "pass" {
			codes := make([]string, 0)
			for _, check := range state.Checks {
				if !check.OK && !check.Skipped {
					codes = append(codes, string(check.Code))
				}
			}
			detail := vmID
			if len(codes) > 0 {
				detail += " [" + strings.Join(codes, ", ") + "]"
			}
			if err == nil {
				err = provision.ErrSelfcheckFailed
			} else if !errors.Is(err, provision.ErrSelfcheckFailed) {
				err = fmt.Errorf("%w: %w", provision.ErrSelfcheckFailed, err)
			}
			return fmt.Errorf("check rental vm %s: %w", detail, err)
		}
		checked[vmID] = state
	}
	tx, err := service.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("开始入池事务失败: %w", err)
	}
	defer tx.Rollback()
	now := store.FmtTime(service.now())
	for _, vmID := range vmIDs {
		var inserted string
		err := tx.QueryRowContext(ctx, `INSERT INTO rental_pool(vm_id,tier,status,note,selfcheck_state,selfcheck_checked_at,created_at,updated_at)
			SELECT ?,?,'available',?,'pass',?,?,? WHERE NOT EXISTS
			(SELECT 1 FROM resources WHERE vm_id=? AND status<>'released')
			ON CONFLICT(vm_id) DO NOTHING RETURNING vm_id`, vmID, byID[vmID].Tier, note, checked[vmID].CheckedAt, now, now, vmID).Scan(&inserted)
		if errors.Is(err, sql.ErrNoRows) {
			return adminError(http.StatusConflict, "vm_not_available", "账号已入池或已归属用户")
		}
		if err != nil {
			return fmt.Errorf("添加租赁池账号失败: %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("提交入池事务失败: %w", err)
	}
	return nil
}

func (service *Service) adminPoolMutationError(ctx context.Context, vmID string, err error) error {
	if err == nil {
		return nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("修改租赁池失败: %w", err)
	}
	var exists int
	if err := service.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM rental_pool WHERE vm_id=?`, vmID).Scan(&exists); err != nil {
		return fmt.Errorf("查询租赁池账号失败: %w", err)
	}
	if exists == 0 {
		return adminError(http.StatusNotFound, "pool_vm_not_found", "租赁池账号不存在")
	}
	return adminError(http.StatusConflict, "vm_leased", "账号正在出租，无法修改或移除")
}

func (service *Service) RentalPatch(ctx context.Context, vmID, status, note string) error {
	if status != "available" && status != "maintenance" {
		return adminError(http.StatusUnprocessableEntity, "invalid_status", "租赁池状态只能是可用或维护")
	}
	service.mu.Lock()
	defer service.mu.Unlock()
	var updated string
	err := service.db.QueryRowContext(ctx, `UPDATE rental_pool SET status=?,note=?,updated_at=?
		WHERE vm_id=? AND status<>'leased' AND leased_resource_id IS NULL RETURNING vm_id`,
		status, note, store.FmtTime(service.now()), vmID).Scan(&updated)
	return service.adminPoolMutationError(ctx, vmID, err)
}

func (service *Service) RentalRemove(ctx context.Context, vmID string) error {
	service.mu.Lock()
	defer service.mu.Unlock()
	var removed string
	err := service.db.QueryRowContext(ctx, `DELETE FROM rental_pool
		WHERE vm_id=? AND status<>'leased' AND leased_resource_id IS NULL RETURNING vm_id`, vmID).Scan(&removed)
	return service.adminPoolMutationError(ctx, vmID, err)
}

func (service *Service) Jobs(ctx context.Context, status string, page, pageSize int) ([]api.JobDTO, int64, error) {
	if status != "" && status != "pending" && status != "running" && status != "failed" && status != "succeeded" {
		return nil, 0, adminError(http.StatusUnprocessableEntity, "invalid_status", "任务状态不正确")
	}
	if page < 1 {
		page = 1
	}
	if pageSize < 1 {
		pageSize = 20
	}
	pageSize = min(pageSize, 100)
	if page-1 > int(^uint(0)>>1)/pageSize {
		return nil, 0, adminError(http.StatusUnprocessableEntity, "invalid_page", "页码超出有效范围")
	}
	where := ""
	args := make([]any, 0, 3)
	if status != "" {
		where = " WHERE status=?"
		args = append(args, status)
	}
	var total int64
	if err := service.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM jobs`+where, args...).Scan(&total); err != nil {
		return nil, 0, fmt.Errorf("统计任务失败: %w", err)
	}
	args = append(args, pageSize, (page-1)*pageSize)
	rows, err := service.db.QueryContext(ctx, `SELECT id,type,status,attempts,run_after,
		COALESCE(last_error,''),payload,created_at,updated_at FROM jobs`+where+` ORDER BY id DESC LIMIT ? OFFSET ?`, args...)
	if err != nil {
		return nil, 0, fmt.Errorf("查询任务失败: %w", err)
	}
	defer rows.Close()
	items := make([]api.JobDTO, 0)
	for rows.Next() {
		var item api.JobDTO
		if err := rows.Scan(&item.ID, &item.Type, &item.Status, &item.Attempts, &item.RunAfter,
			&item.LastError, &item.Payload, &item.CreatedAt, &item.UpdatedAt); err != nil {
			return nil, 0, fmt.Errorf("读取任务失败: %w", err)
		}
		items = append(items, item)
	}
	if err := rows.Err(); err != nil {
		return nil, 0, fmt.Errorf("读取任务失败: %w", err)
	}
	return items, total, nil
}

func (service *Service) RetryJob(ctx context.Context, id int64) error {
	service.mu.Lock()
	defer service.mu.Unlock()
	now := store.FmtTime(service.now())
	var updated int64
	err := service.db.QueryRowContext(ctx, `UPDATE jobs SET status='pending',attempts=0,
		locked_until=NULL,last_error=NULL,run_after=?,updated_at=? WHERE id=? AND status='failed'
		AND NOT EXISTS (SELECT 1 FROM jobs AS other WHERE other.id<>jobs.id
		AND other.dedupe_key=jobs.dedupe_key AND other.status IN ('pending','running')) RETURNING id`,
		now, now, id).Scan(&updated)
	if err == nil {
		return nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("重试任务失败: %w", err)
	}
	var count int
	if err := service.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM jobs WHERE id=?`, id).Scan(&count); err != nil {
		return fmt.Errorf("查询任务失败: %w", err)
	}
	if count == 0 {
		return adminError(http.StatusNotFound, "job_not_found", "任务不存在")
	}
	return adminError(http.StatusConflict, "job_not_retryable", "仅可重试失败任务，且同类任务不能正在执行或等待执行")
}

func adminEnqueueEntitlements(ctx context.Context, tx *sql.Tx, userID int64, now string) error {
	payload := fmt.Sprintf(`{"user_id":%d}`, userID)
	if _, err := tx.ExecContext(ctx, `INSERT INTO jobs(type,payload,status,run_after,created_at,updated_at)
		VALUES ('sync_concurrency',?,'pending',?,?,?),('sweep_expiry','{}','pending',?,?,?)`,
		payload, now, now, now, now, now, now); err != nil {
		return fmt.Errorf("创建权益同步任务失败: %w", err)
	}
	return nil
}

func adminAudit(ctx context.Context, tx *sql.Tx, actorID any, action, target string, detail any, now string) error {
	raw, err := json.Marshal(detail)
	if err != nil {
		return fmt.Errorf("编码审计详情失败: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO audit_log(actor_id,action,target,detail,created_at)
		VALUES (?,?,?,?,?)`, actorID, action, target, string(raw), now); err != nil {
		return fmt.Errorf("写入审计记录失败: %w", err)
	}
	return nil
}

func (service *Service) GrantSubscription(ctx context.Context, adminID, userID int64, kind string, quantity, durationDays int, note string) error {
	if kind != "concurrency" && kind != "hosted_seat" && kind != "rental_account" {
		return adminError(http.StatusUnprocessableEntity, "invalid_kind", "订阅类型不正确")
	}
	if quantity <= 0 || durationDays <= 0 || durationDays > 3652058 {
		return adminError(http.StatusUnprocessableEntity, "invalid_quantity", "数量和有效天数必须为有效正整数")
	}
	now := service.now()
	expires := now.AddDate(0, 0, durationDays)
	if expires.Year() > 9999 || !expires.After(now) {
		return adminError(http.StatusUnprocessableEntity, "invalid_quantity", "订阅有效天数超出范围")
	}
	service.mu.Lock()
	defer service.mu.Unlock()
	var exists int
	if err := service.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM users WHERE id=?`, userID).Scan(&exists); err != nil {
		return fmt.Errorf("查询赠送用户失败: %w", err)
	}
	if exists == 0 {
		return ErrUserNotFound
	}
	token, err := secret.RandomToken(18)
	if err != nil {
		return fmt.Errorf("生成赠送订单号失败: %w", err)
	}
	tx, err := service.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("开始赠送事务失败: %w", err)
	}
	defer tx.Rollback()
	stock, err := service.rentalStock(ctx, kind, quantity)
	if err != nil {
		return err
	}
	stamp := store.FmtTime(now)
	_, err = tx.ExecContext(ctx, `INSERT INTO products(kind,name,unit_quantity,duration_days,price_cents,
		currency,min_qty,max_qty,enabled,created_at,updated_at)
		SELECT ?,'管理员赠送',1,?,0,'CNY',1,?,0,?,? WHERE NOT EXISTS
		(SELECT 1 FROM products WHERE kind=? AND name='管理员赠送' AND enabled=0
		AND unit_quantity=1 AND duration_days=? AND price_cents=0 AND currency='CNY' AND min_qty=1 AND max_qty>=?)`,
		kind, durationDays, quantity, stamp, stamp, kind, durationDays, quantity)
	if err != nil {
		return fmt.Errorf("创建赠送商品失败: %w", err)
	}
	product := api.ProductDTO{Kind: kind, Name: "管理员赠送", UnitQuantity: 1,
		DurationDays: durationDays, Currency: "CNY", MinQty: 1}
	err = tx.QueryRowContext(ctx, `SELECT id,max_qty FROM products WHERE kind=? AND name='管理员赠送'
		AND enabled=0 AND unit_quantity=1 AND duration_days=? AND price_cents=0 AND currency='CNY'
		AND min_qty=1 AND max_qty>=? ORDER BY id LIMIT 1`, kind, durationDays, quantity).Scan(&product.ID, &product.MaxQty)
	if err != nil {
		return fmt.Errorf("读取赠送商品失败: %w", err)
	}
	snapshot, err := json.Marshal(product)
	if err != nil {
		return fmt.Errorf("编码赠送商品失败: %w", err)
	}
	orderNo := "GRANT-" + token
	var orderID int64
	err = tx.QueryRowContext(ctx, `INSERT INTO orders(order_no,user_id,product_id,product_snapshot,kind,
		quantity,units,duration_days,amount_cents,currency,provider,status,created_at,expires_at,paid_at,fulfilled_at,updated_at)
		VALUES (?,?,?,?,?,?,?,?,0,'CNY','grant','fulfilled',?,?,?,?,?) RETURNING id`,
		orderNo, userID, product.ID, string(snapshot), kind, quantity, quantity, durationDays,
		stamp, stamp, stamp, stamp, stamp).Scan(&orderID)
	if err != nil {
		return fmt.Errorf("创建赠送订单失败: %w", err)
	}
	var subscriptionID int64
	err = tx.QueryRowContext(ctx, `INSERT INTO subscriptions(user_id,kind,quantity,status,starts_at,
		expires_at,source_order_id,note,created_at,updated_at) VALUES (?,?,?,'active',?,?,?,?,?,?) RETURNING id`,
		userID, kind, quantity, stamp, store.FmtTime(expires), orderID, note, stamp, stamp).Scan(&subscriptionID)
	if err != nil {
		return fmt.Errorf("创建赠送订阅失败: %w", err)
	}
	if err := adminEnqueueEntitlements(ctx, tx, userID, stamp); err != nil {
		return fmt.Errorf("安排赠送权益同步失败: %w", err)
	}
	detail := map[string]any{"user_id": userID, "subscription_id": subscriptionID,
		"order_no": orderNo, "kind": kind, "quantity": quantity, "duration_days": durationDays, "note": note}
	if err := adminAudit(ctx, tx, adminID, "subscription.grant", fmt.Sprint(subscriptionID), detail, stamp); err != nil {
		return fmt.Errorf("记录赠送审计失败: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("提交赠送事务失败: %w", err)
	}
	// A granted rental subscription entitles the user to real accounts: assign
	// them now, after the entitlement rows are committed. Failing here leaves a
	// subscription without stock, so the error is returned rather than swallowed.
	if kind == "rental_account" && stock {
		if err := service.provisioning.AllocateRental(ctx, subscriptionID); err != nil {
			return err
		}
	}
	return nil
}

// rentalStock verifies a rental grant can be honoured before anything is
// written. It returns whether an allocation is needed at all.
func (service *Service) rentalStock(ctx context.Context, kind string, quantity int) (bool, error) {
	if kind != "rental_account" {
		return false, nil
	}
	var available int
	if err := service.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM rental_pool WHERE status='available' AND selfcheck_state='pass'`).Scan(&available); err != nil {
		return false, fmt.Errorf("查询租赁库存失败: %w", err)
	}
	if available < quantity {
		return false, adminError(http.StatusConflict, "out_of_stock", "租赁池可用账号不足，无法赠送")
	}
	return true, nil
}

func (service *Service) PatchSubscription(ctx context.Context, id int64, expiresAt *string, status string) error {
	if status != "" && status != "cancelled" {
		return adminError(http.StatusUnprocessableEntity, "invalid_status", "订阅状态仅可修改为已取消")
	}
	if expiresAt == nil && status == "" {
		return adminError(http.StatusUnprocessableEntity, "invalid_body", "请提供到期时间或取消状态")
	}
	var expiry any
	if expiresAt != nil {
		parsed, err := time.Parse(time.RFC3339, *expiresAt)
		if err != nil || parsed.UTC().Year() < 1 || parsed.UTC().Year() > 9999 {
			return adminError(http.StatusUnprocessableEntity, "invalid_expires_at", "到期时间必须是有效的标准时间格式")
		}
		expiry = store.FmtTime(parsed)
	}
	service.mu.Lock()
	defer service.mu.Unlock()
	tx, err := service.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("开始修改订阅事务失败: %w", err)
	}
	defer tx.Rollback()
	now := store.FmtTime(service.now())
	var userID int64
	err = tx.QueryRowContext(ctx, `UPDATE subscriptions SET expires_at=COALESCE(?,expires_at),
		status=CASE WHEN status='cancelled' OR ?='cancelled' THEN 'cancelled'
		WHEN julianday(COALESCE(?,expires_at))<=julianday(?) THEN 'expired' ELSE 'active' END,
		updated_at=? WHERE id=? RETURNING user_id`, expiry, status, expiry, now, now, id).Scan(&userID)
	if errors.Is(err, sql.ErrNoRows) {
		return adminError(http.StatusNotFound, "subscription_not_found", "订阅不存在")
	}
	if err != nil {
		return fmt.Errorf("修改订阅失败: %w", err)
	}
	if err := adminEnqueueEntitlements(ctx, tx, userID, now); err != nil {
		return fmt.Errorf("安排订阅权益同步失败: %w", err)
	}
	detail := map[string]any{"user_id": userID, "expires_at": expiry, "status": status}
	if err := adminAudit(ctx, tx, nil, "subscription.patch", fmt.Sprint(id), detail, now); err != nil {
		return fmt.Errorf("记录订阅审计失败: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("提交修改订阅事务失败: %w", err)
	}
	// Extending a rental subscription revives its entitlement. If its accounts
	// were already released, the user would stay entitled to N accounts while
	// holding none, so top the subscription back up.
	var kind, current string
	if err := service.db.QueryRowContext(ctx, `SELECT kind,status FROM subscriptions WHERE id=?`, id).Scan(&kind, &current); err != nil {
		return fmt.Errorf("读取订阅类型失败: %w", err)
	}
	if kind == "rental_account" && current == "active" {
		if err := service.provisioning.AllocateRental(ctx, id); err != nil {
			return err
		}
	}
	return nil
}
