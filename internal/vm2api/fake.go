// Fake keeps panel state only in memory: it never starts VMs, contacts OAuth,
// allocates real proxies, or forwards inference requests. Usage is empty unless
// seeded with AddRequestLog. OAuth accepts any nonempty code for a live session;
// bootstrap advances on reads instead of running an asynchronous worker.
// Unlike upstream api-keys.mjs, key plaintext is never stored or revealable.
// Handler implements only the panel routes needed by Client, plus redacted key
// and user lists. Wire envelopes, field names, and millisecond OAuth expiry match
// panel-routes.mjs; VM template runtime settings are intentionally ignored.
package vm2api

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

type Fake struct {
	mu        sync.Mutex
	sequence  uint64
	users     map[string]User
	keys      map[string]fakeKey
	vms       map[string]VM
	sessions  map[string]fakeSession
	bootstrap map[string]int
	logs      []fakeLog
	now       func() time.Time

	// Self-check state (fake_selfcheck.go).
	disabledProxies map[string]bool
	exitOverride    map[string]ExitProbe
	exitErr         map[string]error
	proxyQueue      []ProxyView
	resetCalls      map[string]int
	// hostileSelfcheck makes every slot share one fingerprint and one exit, so
	// a self-check can never pass no matter how many times it rebuilds. Only
	// cmd/mock-vm2api enables it, to exercise the rebuild budget end to end.
	hostileSelfcheck bool
}

type fakeKey struct {
	key                  Key
	name, userID, suffix string
	active               bool
	maxConcurrency       int
	createdAt, updatedAt time.Time
}

type fakeSession struct {
	vmID      string
	expiresAt time.Time
}

type fakeLog struct {
	ownerID string
	row     LogRow
}

var _ Client = (*Fake)(nil)

func NewFake() *Fake {
	return &Fake{}
}

func (fake *Fake) initLocked() {
	if fake.users == nil {
		fake.users = make(map[string]User)
		fake.keys = make(map[string]fakeKey)
		fake.vms = make(map[string]VM)
		fake.sessions = make(map[string]fakeSession)
		fake.bootstrap = make(map[string]int)
	}
	if fake.now == nil {
		fake.now = time.Now
	}
}

func (fake *Fake) begin(ctx context.Context) error {
	fake.mu.Lock()
	if err := ctx.Err(); err != nil {
		fake.mu.Unlock()
		return fmt.Errorf("fake request canceled: %w", err)
	}
	fake.initLocked()
	return nil
}

func (fake *Fake) nextID(prefix string) string {
	fake.sequence++
	return fmt.Sprintf("%s_%06d", prefix, fake.sequence)
}

func fakeError(status int, code, message string) error {
	return &Error{Status: status, Code: code, Message: message, Op: "fake"}
}

func fakeMissing(kind string) error {
	return fakeError(http.StatusNotFound, kind+"_not_found", kind+" not found")
}

func fakeClampConcurrency(value int) int {
	return min(128, max(0, value))
}

func fakeCloneWindow(window *Window) *Window {
	if window == nil {
		return nil
	}
	copyWindow := *window
	if window.ResetsAt != nil {
		reset := window.ResetsAt.UTC()
		copyWindow.ResetsAt = &reset
	}
	return &copyWindow
}

func fakeCloneVM(vm VM) VM {
	vm.CredentialRejected = credentialRejected(vm.UnschedulableCode)
	if vm.Proxy != nil {
		proxy := *vm.Proxy
		vm.Proxy = &proxy
	}
	vm.FiveHour = fakeCloneWindow(vm.FiveHour)
	vm.SevenDay = fakeCloneWindow(vm.SevenDay)
	vm.SevenDayFable = fakeCloneWindow(vm.SevenDayFable)
	return vm
}

func fakeCloneLog(row LogRow) LogRow {
	row.CreatedAt = row.CreatedAt.UTC()
	if row.FirstTokenMS != nil {
		latency := *row.FirstTokenMS
		row.FirstTokenMS = &latency
	}
	return row
}

func (fake *Fake) AddVM(vm VM) {
	fake.mu.Lock()
	defer fake.mu.Unlock()
	fake.initLocked()
	if vm.ID == "" {
		vm.ID = fake.newVMIDLocked()
	}
	if vm.Platform == "" {
		vm.Platform = "anthropic"
	}
	if vm.Origin == "" {
		vm.Origin = "platform"
		if vm.OwnerUserID != "" {
			vm.Origin = "admin_assigned"
		}
	}
	if vm.ScheduleState == "" {
		vm.ScheduleState = "on"
	}
	if vm.Fingerprint == (Fingerprint{}) {
		vm.Fingerprint = fake.newFingerprintLocked()
	}
	fake.vms[vm.ID] = fakeCloneVM(vm)
}

func (fake *Fake) AddRequestLog(ownerID string, row LogRow) error {
	fake.mu.Lock()
	defer fake.mu.Unlock()
	fake.initLocked()
	if _, exists := fake.users[ownerID]; !exists {
		return fakeMissing("user")
	}
	if row.RequestID == "" {
		row.RequestID = fake.nextID("req")
	}
	if row.CreatedAt.IsZero() {
		row.CreatedAt = fake.now().UTC()
	}
	fake.logs = append(fake.logs, fakeLog{ownerID: ownerID, row: fakeCloneLog(row)})
	return nil
}

func (fake *Fake) Ping(ctx context.Context) (Health, error) {
	if err := fake.begin(ctx); err != nil {
		return Health{}, err
	}
	defer fake.mu.Unlock()
	return Health{OK: true, Version: "mock-vm2api"}, nil
}

func (fake *Fake) CreateUser(ctx context.Context, username, password string) (User, error) {
	if err := fake.begin(ctx); err != nil {
		return User{}, err
	}
	defer fake.mu.Unlock()
	username = strings.TrimSpace(username)
	if username == "" || password == "" {
		return User{}, fakeError(400, "invalid_user", "username and password required")
	}
	for _, user := range fake.users {
		if user.Username == username {
			return User{}, fakeError(409, "username_exists", "username already exists")
		}
	}
	user := User{ID: fake.nextID("usr"), Username: username, Enabled: true}
	fake.users[user.ID] = user
	return user, nil
}

func (fake *Fake) SetUserEnabled(ctx context.Context, userID string, enabled bool) error {
	if err := fake.begin(ctx); err != nil {
		return err
	}
	defer fake.mu.Unlock()
	user, exists := fake.users[userID]
	if !exists {
		return fakeMissing("user")
	}
	user.Enabled = enabled
	fake.users[userID] = user
	return nil
}

func (fake *Fake) CreateKey(ctx context.Context, in CreateKeyInput) (Key, error) {
	if err := fake.begin(ctx); err != nil {
		return Key{}, err
	}
	defer fake.mu.Unlock()
	if _, exists := fake.users[in.UserID]; !exists {
		return Key{}, fakeMissing("user")
	}
	secretBytes := make([]byte, 32)
	if _, err := rand.Read(secretBytes); err != nil {
		return Key{}, fmt.Errorf("generate key: %w", err)
	}
	secret := "sk-vm-" + hex.EncodeToString(secretBytes)
	key := Key{ID: fake.nextID("key"), Prefix: secret[:10]}
	name := strings.TrimSpace(in.Name)
	if name == "" {
		name = "default"
	}
	stamp := fake.now().UTC()
	fake.keys[key.ID] = fakeKey{
		key: key, name: string([]rune(name)[:min(100, len([]rune(name)))]),
		userID: in.UserID, suffix: secret[len(secret)-4:], active: true,
		maxConcurrency: fakeClampConcurrency(in.MaxConcurrency), createdAt: stamp, updatedAt: stamp,
	}
	key.Secret = secret
	return key, nil
}

func (fake *Fake) UpdateKey(ctx context.Context, keyID string, in UpdateKeyInput) error {
	if err := fake.begin(ctx); err != nil {
		return err
	}
	defer fake.mu.Unlock()
	record, exists := fake.keys[keyID]
	if !exists {
		return fakeMissing("api_key")
	}
	if in.Name != nil {
		name := []rune(strings.TrimSpace(*in.Name))
		if len(name) != 0 {
			record.name = string(name[:min(100, len(name))])
		}
	}
	if in.Active != nil {
		record.active = *in.Active
	}
	if in.MaxConcurrency != nil {
		record.maxConcurrency = fakeClampConcurrency(*in.MaxConcurrency)
	}
	record.updatedAt = fake.now().UTC()
	fake.keys[keyID] = record
	return nil
}

func (fake *Fake) DeleteKey(ctx context.Context, keyID string) error {
	if err := fake.begin(ctx); err != nil {
		return err
	}
	defer fake.mu.Unlock()
	if _, exists := fake.keys[keyID]; !exists {
		return fakeMissing("api_key")
	}
	delete(fake.keys, keyID)
	return nil
}

func (fake *Fake) KeyStats(ctx context.Context, keyID string) (UsageBreakdown, error) {
	if err := fake.begin(ctx); err != nil {
		return UsageBreakdown{}, err
	}
	defer fake.mu.Unlock()
	key, exists := fake.keys[keyID]
	if !exists {
		return UsageBreakdown{}, fakeMissing("api_key")
	}
	return fakeBreakdown(fake.logsLocked(key.userID, keyID, "", time.Time{}, time.Time{})), nil
}

func (fake *Fake) ListVMs(ctx context.Context, filter VMFilter) ([]VM, error) {
	if err := fake.begin(ctx); err != nil {
		return nil, err
	}
	defer fake.mu.Unlock()
	result := make([]VM, 0)
	for _, vm := range fake.vms {
		if filter.OwnerUserID != nil && vm.OwnerUserID != *filter.OwnerUserID {
			continue
		}
		if filter.Unowned && (vm.OwnerUserID != "" || vm.Origin != "platform") || filter.ClaudeOnly && vm.Platform != "anthropic" {
			continue
		}
		result = append(result, fakeCloneVM(vm))
	}
	sort.Slice(result, func(left, right int) bool { return result[left].ID < result[right].ID })
	return result, nil
}

func (fake *Fake) GetAccountUsage(ctx context.Context, vmID string) (AccountUsage, error) {
	if err := fake.begin(ctx); err != nil {
		return AccountUsage{}, err
	}
	defer fake.mu.Unlock()
	vm, exists := fake.vms[vmID]
	if !exists {
		return AccountUsage{}, fakeMissing("vm")
	}
	now := fake.now().UTC()
	shanghai := time.FixedZone("Asia/Shanghai", 8*60*60)
	local := now.In(shanghai)
	dayStart := time.Date(local.Year(), local.Month(), local.Day(), 0, 0, 0, 0, shanghai)
	breakdown := fakeBreakdown(fake.logsLocked("", "", vmID, time.Time{}, now))
	today := fakeSpend(fake.logsLocked("", "", vmID, dayStart, now))
	window5h := fakeSpend(fake.logsLocked("", "", vmID, now.Add(-5*time.Hour), now))
	window7d := fakeSpend(fake.logsLocked("", "", vmID, now.Add(-7*24*time.Hour), now))
	return AccountUsage{
		VM: fakeCloneVM(vm), Today: &today, Window5h: &window5h, Window7d: &window7d,
		Estimate5h: &Estimate{Status: "no_data", SpentUSD: window5h.CostUSD},
		Estimate7d: &Estimate{Status: "no_data", SpentUSD: window7d.CostUSD},
		Daily:      breakdown.Daily, ByModel: breakdown.ByModel, FetchedAt: now,
	}, nil
}

func (fake *Fake) newVMIDLocked() string {
	for {
		id := fake.nextID("vm")
		if _, exists := fake.vms[id]; !exists {
			return id
		}
	}
}

func (fake *Fake) CreateHostedVM(ctx context.Context, label string, template json.RawMessage) (VM, error) {
	if err := fake.begin(ctx); err != nil {
		return VM{}, err
	}
	defer fake.mu.Unlock()
	if len(template) != 0 {
		var settings map[string]json.RawMessage
		if err := json.Unmarshal(template, &settings); err != nil || settings == nil {
			return VM{}, fakeError(400, "invalid_template", "template must be a json object")
		}
	}
	vm := VM{
		ID: fake.newVMIDLocked(), Label: label, Platform: "anthropic", Origin: "platform",
		CredStatus: "missing", ScheduleState: "unschedulable", UnschedulableCode: "no_credential", SeatsMax: 2,
	}
	vm.Fingerprint = fake.newFingerprintLocked()
	fake.vms[vm.ID] = vm
	return vm, nil
}

func (fake *Fake) SetVMOwner(ctx context.Context, vmID string, userID *string) error {
	if err := fake.begin(ctx); err != nil {
		return err
	}
	defer fake.mu.Unlock()
	vm, exists := fake.vms[vmID]
	if !exists {
		return fakeMissing("vm")
	}
	owner := ""
	if userID != nil {
		owner = strings.TrimSpace(*userID)
	}
	if owner != "" {
		if _, exists := fake.users[owner]; !exists {
			return fakeMissing("user")
		}
	} else if vm.Origin == "user_created" {
		return fakeError(400, "cannot_unassign_user_created", "cannot unassign user-created vm")
	}
	vm.OwnerUserID = owner
	if owner == "" {
		vm.Origin = "platform"
	} else if vm.Origin != "user_created" {
		vm.Origin = "admin_assigned"
	}
	fake.vms[vmID] = vm
	return nil
}

func (fake *Fake) AllocateProxy(ctx context.Context, vmID string) error {
	if err := fake.begin(ctx); err != nil {
		return err
	}
	defer fake.mu.Unlock()
	vm, exists := fake.vms[vmID]
	if !exists {
		return fakeMissing("vm")
	}
	if vm.Proxy == nil {
		proxy := fake.nextProxyLocked()
		vm.Proxy = &proxy
		// vm2api follows the proxy's timezone unless the slot pins one.
		if vm.Timezone == "" {
			vm.Timezone = proxy.GeoTimezone
		}
	}
	vm.ProxyConfigured = true
	fake.vms[vmID] = vm
	return nil
}

func (fake *Fake) SetSchedulable(ctx context.Context, vmID string, schedulable bool) error {
	if err := fake.begin(ctx); err != nil {
		return err
	}
	defer fake.mu.Unlock()
	vm, exists := fake.vms[vmID]
	if !exists {
		return fakeMissing("vm")
	}
	if schedulable && (vm.CredentialRejected || credentialRejected(vm.UnschedulableCode)) {
		return fakeError(409, "credential_unavailable", "credential unavailable")
	}
	vm.ScheduleState = "off"
	if schedulable {
		vm.ScheduleState = "on"
	}
	fake.vms[vmID] = vm
	return nil
}

func (fake *Fake) DeleteVM(ctx context.Context, vmID string) error {
	if err := fake.begin(ctx); err != nil {
		return err
	}
	defer fake.mu.Unlock()
	if _, exists := fake.vms[vmID]; !exists {
		return fakeMissing("vm")
	}
	delete(fake.vms, vmID)
	delete(fake.bootstrap, vmID)
	fake.clearSessionsLocked(vmID)
	return nil
}

func (fake *Fake) clearSessionsLocked(vmID string) {
	for sessionID, session := range fake.sessions {
		if session.vmID == vmID {
			delete(fake.sessions, sessionID)
		}
	}
}

func (fake *Fake) GenerateAuthURL(ctx context.Context, vmID string) (AuthURL, error) {
	if err := fake.begin(ctx); err != nil {
		return AuthURL{}, err
	}
	defer fake.mu.Unlock()
	vm, exists := fake.vms[vmID]
	if !exists {
		return AuthURL{}, fakeMissing("vm")
	}
	if vm.Platform != "anthropic" {
		return AuthURL{}, fakeError(400, "unsupported_platform", "claude vm required")
	}
	if !vm.ProxyConfigured {
		return AuthURL{}, fakeError(400, "proxy_required", "proxy required")
	}
	fake.clearSessionsLocked(vmID)
	sessionID := fake.nextID("oauth")
	expiresAt := fake.now().UTC().Add(30 * time.Minute)
	fake.sessions[sessionID] = fakeSession{vmID: vmID, expiresAt: expiresAt}
	return AuthURL{
		URL:       "https://claude.com/oauth/authorize?flavor=claude_code&state=" + url.QueryEscape(sessionID),
		SessionID: sessionID, ExpiresAt: expiresAt,
	}, nil
}

func (fake *Fake) ExchangeCode(ctx context.Context, vmID, sessionID, code string) error {
	if err := fake.begin(ctx); err != nil {
		return err
	}
	defer fake.mu.Unlock()
	vm, exists := fake.vms[vmID]
	if !exists {
		return fakeMissing("vm")
	}
	if strings.TrimSpace(code) == "" {
		return fakeError(400, "code_required", "authorization code required")
	}
	session, exists := fake.sessions[sessionID]
	if !exists || !fake.now().Before(session.expiresAt) {
		delete(fake.sessions, sessionID)
		return fakeError(400, "session_expired", "authorization session expired")
	}
	if session.vmID != vmID {
		return fakeError(400, "session_vm_mismatch", "authorization session belongs to another vm")
	}
	if !vm.ProxyConfigured {
		return fakeError(400, "proxy_required", "proxy required")
	}
	delete(fake.sessions, sessionID)
	fake.bootstrap[vmID] = 0
	vm.HasCredential = true
	vm.CredentialRejected = false
	vm.CredStatus = "valid"
	if vm.ScheduleState == "unschedulable" && vm.UnschedulableCode == "no_credential" {
		vm.ScheduleState = "on"
	}
	vm.UnschedulableCode = ""
	vm.RestrictionText = ""
	if vm.Tier == "" {
		vm.Tier = "pro"
	}
	fake.vms[vmID] = vm
	return nil
}

func (fake *Fake) BootstrapStatus(ctx context.Context, vmID string) (Bootstrap, error) {
	if err := fake.begin(ctx); err != nil {
		return Bootstrap{}, err
	}
	defer fake.mu.Unlock()
	if _, exists := fake.vms[vmID]; !exists {
		return Bootstrap{}, fakeMissing("vm")
	}
	step, started := fake.bootstrap[vmID]
	if !started {
		return Bootstrap{State: "pending"}, nil
	}
	states := [...]string{"pending", "running", "succeeded"}
	fake.bootstrap[vmID] = min(step+1, len(states)-1)
	return Bootstrap{State: states[step]}, nil
}

func (fake *Fake) logsLocked(ownerID, keyID, vmID string, from, until time.Time) []LogRow {
	rows := make([]LogRow, 0)
	for _, entry := range fake.logs {
		row := entry.row
		if ownerID != "" && entry.ownerID != ownerID || keyID != "" && row.KeyID != keyID || vmID != "" && row.VMID != vmID {
			continue
		}
		if !from.IsZero() && row.CreatedAt.Before(from) || !until.IsZero() && row.CreatedAt.After(until) {
			continue
		}
		rows = append(rows, fakeCloneLog(row))
	}
	return rows
}

func fakeRowTokens(row LogRow) int64 {
	return row.InputTokens + row.OutputTokens + row.CacheReadTokens + row.CacheCreationTokens
}

func fakeSpend(rows []LogRow) Spend {
	var spend Spend
	for _, row := range rows {
		spend.Requests++
		spend.InputTokens += row.InputTokens
		spend.OutputTokens += row.OutputTokens
		spend.CacheReadTokens += row.CacheReadTokens
		spend.CacheCreationTokens += row.CacheCreationTokens
		spend.Tokens += fakeRowTokens(row)
		spend.CostUSD += row.CostUSD
	}
	return spend
}

func fakeBreakdown(rows []LogRow) UsageBreakdown {
	daily := make(map[string]DailyUsage)
	models := make(map[string]ModelUsage)
	zone := time.FixedZone("Asia/Shanghai", 8*60*60)
	for _, row := range rows {
		date := row.CreatedAt.In(zone).Format("2006-01-02")
		day := daily[date]
		day.Date = date
		day.Requests++
		day.Tokens += fakeRowTokens(row)
		day.CostUSD += row.CostUSD
		daily[date] = day
		model := models[row.Model]
		model.Model = row.Model
		model.Requests++
		model.Tokens += fakeRowTokens(row)
		model.CostUSD += row.CostUSD
		models[row.Model] = model
	}
	result := UsageBreakdown{Daily: make([]DailyUsage, 0, len(daily)), ByModel: make([]ModelUsage, 0, len(models))}
	for _, day := range daily {
		result.Daily = append(result.Daily, day)
	}
	for _, model := range models {
		result.ByModel = append(result.ByModel, model)
	}
	sort.Slice(result.Daily, func(left, right int) bool { return result.Daily[left].Date < result.Daily[right].Date })
	sort.Slice(result.ByModel, func(left, right int) bool { return result.ByModel[left].Model < result.ByModel[right].Model })
	return result
}

func (fake *Fake) UserBilling(ctx context.Context, userID string, from, until time.Time) (UserBilling, error) {
	if err := fake.begin(ctx); err != nil {
		return UserBilling{}, err
	}
	defer fake.mu.Unlock()
	if _, exists := fake.users[userID]; !exists {
		return UserBilling{}, fakeMissing("user")
	}
	rows := fake.logsLocked(userID, "", "", from, until)
	result := UserBilling{
		Totals: fakeSpend(rows), Daily: []DailyUsage{}, ByModel: []ModelUsage{},
		ByKey: []KeyUsage{}, ByVM: []VMUsage{},
	}
	result.Totals.InputTokens += result.Totals.CacheReadTokens + result.Totals.CacheCreationTokens
	result.Totals.CacheReadTokens = 0
	result.Totals.CacheCreationTokens = 0
	keys := make(map[string]KeyUsage)
	vms := make(map[string]VMUsage)
	for _, row := range rows {
		key := keys[row.KeyID]
		key.KeyID = row.KeyID
		key.Requests++
		key.Tokens += fakeRowTokens(row)
		key.CostUSD += row.CostUSD
		keys[row.KeyID] = key
		vm := vms[row.VMID]
		vm.VMID = row.VMID
		vm.Requests++
		vm.Tokens += fakeRowTokens(row)
		vm.CostUSD += row.CostUSD
		vms[row.VMID] = vm
	}
	for _, key := range keys {
		result.ByKey = append(result.ByKey, key)
	}
	for _, vm := range vms {
		result.ByVM = append(result.ByVM, vm)
	}
	sort.Slice(result.ByKey, func(left, right int) bool { return result.ByKey[left].KeyID < result.ByKey[right].KeyID })
	sort.Slice(result.ByVM, func(left, right int) bool { return result.ByVM[left].VMID < result.ByVM[right].VMID })
	return result, nil
}

func (fake *Fake) UserRequestLogs(ctx context.Context, userID string, query LogQuery) (LogPage, error) {
	if err := fake.begin(ctx); err != nil {
		return LogPage{}, err
	}
	defer fake.mu.Unlock()
	if _, exists := fake.users[userID]; !exists {
		return LogPage{}, fakeMissing("user")
	}
	if query.Status != "" && query.Status != "success" && query.Status != "error" {
		return LogPage{}, fakeError(400, "invalid_status", "invalid log status")
	}
	rows := fake.filteredLogsLocked(userID, query)
	page, pageSize := max(1, query.Page), query.PageSize
	if pageSize <= 0 {
		pageSize = 50
	}
	pageSize = min(500, pageSize)
	start := len(rows)
	if page-1 <= len(rows)/pageSize {
		start = min((page-1)*pageSize, len(rows))
	}
	end := start + min(pageSize, len(rows)-start)
	return LogPage{Items: rows[start:end], Total: int64(len(rows)), Page: page, PageSize: pageSize}, nil
}

func (fake *Fake) filteredLogsLocked(userID string, query LogQuery) []LogRow {
	rows := make([]LogRow, 0)
	for _, row := range fake.logsLocked(userID, query.KeyID, "", query.From, query.Until) {
		if query.Model != "" && row.Model != query.Model {
			continue
		}
		success := row.StatusCode >= 200 && row.StatusCode < 400
		if query.Status == "success" && !success || query.Status == "error" && success {
			continue
		}
		rows = append(rows, row)
	}
	sort.SliceStable(rows, func(left, right int) bool { return rows[left].CreatedAt.After(rows[right].CreatedAt) })
	return rows
}

func (fake *Fake) UserRequestStats(ctx context.Context, userID string, from, until time.Time) (RequestStats, error) {
	if err := fake.begin(ctx); err != nil {
		return RequestStats{}, err
	}
	defer fake.mu.Unlock()
	if _, exists := fake.users[userID]; !exists {
		return RequestStats{}, fakeMissing("user")
	}
	rows := fake.logsLocked(userID, "", "", from, until)
	result := RequestStats{Requests: int64(len(rows)), SuccessRate: 1}
	latencies := make([]int64, 0)
	for _, row := range rows {
		if row.StatusCode < 200 || row.StatusCode >= 400 {
			result.ErrorCount++
		}
		if row.FirstTokenMS != nil {
			latencies = append(latencies, *row.FirstTokenMS)
			result.AvgTTFTMS += float64(*row.FirstTokenMS)
		}
	}
	if len(rows) != 0 {
		result.SuccessRate = float64(result.Requests-result.ErrorCount) / float64(result.Requests)
	}
	if len(latencies) != 0 {
		slices.Sort(latencies)
		result.AvgTTFTMS /= float64(len(latencies))
		result.P95TTFTMS = float64(latencies[int(math.Ceil(float64(len(latencies))*0.95))-1])
	}
	return result, nil
}

func fakeUserWire(user User) map[string]any {
	return map[string]any{
		"id": user.ID, "username": user.Username, "enabled": user.Enabled,
		"role": "user", "vm_create_quota": 0,
	}
}

func fakeProxyWire(proxy *ProxyView) any {
	if proxy == nil {
		return nil
	}
	return map[string]any{
		"id": proxy.ID, "scheme": proxy.Scheme, "status": proxy.Status,
		"enabled": proxy.Enabled, "blocked_reason": proxy.BlockedReason,
		"geo": map[string]any{"ip": proxy.GeoIP, "timezone": proxy.GeoTimezone},
	}
}

func fakeVMWire(vm VM) map[string]any {
	var owner any
	if vm.OwnerUserID != "" {
		owner = vm.OwnerUserID
	}
	reason := vm.UnschedulableCode
	if vm.CredentialRejected && reason == "" {
		reason = "oauth_revoked"
	}
	result := map[string]any{
		"id": vm.ID, "name": vm.Label, "platform": vm.Platform, "owner_user_id": owner,
		"origin": vm.Origin, "email": vm.Email, "email_masked": vm.EmailMasked, "account_tier": vm.Tier,
		"account_uuid": vm.AccountUUID, "fingerprint": vm.Fingerprint, "timezone": vm.Timezone, "locale": vm.Locale,
		"has_token": vm.HasCredential, "cred_status": map[string]any{"key": vm.CredStatus, "text": vm.CredStatus},
		"schedule_state": vm.ScheduleState, "unschedulable_reason": reason,
		"restriction_text": vm.RestrictionText, "proxy": fakeProxyWire(vm.Proxy), "proxy_configured": vm.ProxyConfigured,
		"seats_used": vm.SeatsUsed, "seats_max": vm.SeatsMax, "inflight": vm.Inflight,
	}
	for _, pair := range []struct {
		suffix string
		window *Window
	}{{"5h", vm.FiveHour}, {"7d", vm.SevenDay}, {"7d_oi", vm.SevenDayFable}} {
		var utilization, reset, status any
		if pair.window != nil {
			utilization = pair.window.Utilization / 100
			status = pair.window.Status
			if pair.window.ResetsAt != nil {
				reset = pair.window.ResetsAt.UTC().Format(time.RFC3339Nano)
			}
		}
		result["utilization_"+pair.suffix] = utilization
		result["reset_"+pair.suffix] = reset
		result["status_"+pair.suffix] = status
	}
	return result
}

func (fake *Fake) keyWire(keyID string) (map[string]any, error) {
	fake.mu.Lock()
	defer fake.mu.Unlock()
	record, exists := fake.keys[keyID]
	if !exists {
		return nil, fakeMissing("api_key")
	}
	status := "disabled"
	if record.active {
		status = "active"
	}
	return map[string]any{
		"id": keyID, "name": record.name, "key": record.key.Prefix[:8] + "…" + record.suffix,
		"key_prefix": record.key.Prefix, "revealable": false, "status": status,
		"user_id": record.userID, "max_concurrency": record.maxConcurrency,
		"created_at": record.createdAt, "updated_at": record.updatedAt, "category": "oauth",
		"requests": 0, "tokens_in": 0, "tokens_out": 0, "quota_used": 0,
	}, nil
}

func fakeSpendWire(spend *Spend) any {
	if spend == nil {
		return nil
	}
	return map[string]any{
		"requests": spend.Requests, "input_tokens": spend.InputTokens, "output_tokens": spend.OutputTokens,
		"cache_read_tokens": spend.CacheReadTokens, "cache_creation_tokens": spend.CacheCreationTokens,
		"tokens": spend.Tokens, "total_cost": spend.CostUSD,
	}
}

func fakeUsageWire(usage UsageBreakdown, rows []LogRow) map[string]any {
	history := make([]any, 0, len(usage.Daily))
	zone := time.FixedZone("Asia/Shanghai", 8*60*60)
	for _, day := range usage.Daily {
		dayRows := make([]LogRow, 0)
		for _, row := range rows {
			if row.CreatedAt.In(zone).Format("2006-01-02") == day.Date {
				dayRows = append(dayRows, row)
			}
		}
		spend := fakeSpend(dayRows)
		wire := fakeSpendWire(&spend).(map[string]any)
		wire["day"] = day.Date
		history = append(history, wire)
	}
	models := make([]any, 0, len(usage.ByModel))
	for _, model := range usage.ByModel {
		models = append(models, map[string]any{
			"name": model.Model, "requests": model.Requests, "tokens": model.Tokens, "total_cost": model.CostUSD,
		})
	}
	return map[string]any{"days": 30, "history": history, "models": models, "vms": []any{}}
}

func fakeLogWire(row LogRow) map[string]any {
	return map[string]any{
		"request_id": row.RequestID, "created_at": row.CreatedAt, "model": row.Model,
		"api_key_id": row.KeyID, "vm_id": row.VMID, "stream": row.Stream, "status": row.StatusCode,
		"error_code": row.ErrorLabel, "input_tokens": row.InputTokens, "output_tokens": row.OutputTokens,
		"cache_read_tokens": row.CacheReadTokens, "cache_creation_tokens": row.CacheCreationTokens,
		"total_cost": row.CostUSD, "duration_ms": row.DurationMS, "first_token_ms": row.FirstTokenMS,
	}
}

func fakeOK(data any) map[string]any {
	return map[string]any{"ok": true, "data": data}
}

func fakeDecodeBody[T any](request *http.Request) (T, error) {
	var body T
	decoder := json.NewDecoder(request.Body)
	if err := decoder.Decode(&body); err != nil {
		return body, fakeError(400, "invalid_json", "invalid json body")
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		return body, fakeError(400, "invalid_json", "invalid json body")
	}
	return body, nil
}

func fakeQueryTime(request *http.Request, keys ...string) (time.Time, error) {
	for _, key := range keys {
		if value := request.URL.Query().Get(key); value != "" {
			stamp, err := time.Parse(time.RFC3339Nano, value)
			if err != nil {
				return time.Time{}, fakeError(400, "invalid_time", "invalid "+key)
			}
			return stamp.UTC(), nil
		}
	}
	return time.Time{}, nil
}

func (fake *Fake) Handler(masterKey string) http.Handler {
	return http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Content-Type", "application/json; charset=utf-8")
		writer.Header().Set("Cache-Control", "no-store")
		key := request.Header.Get("x-api-key")
		if bearer, found := strings.CutPrefix(request.Header.Get("Authorization"), "Bearer "); found {
			key = bearer
		}
		status, payload, err := 0, any(nil), error(nil)
		if masterKey == "" || subtle.ConstantTimeCompare([]byte(key), []byte(masterKey)) != 1 {
			err = fakeError(401, "unauthorized", "invalid master key")
		} else {
			request.Body = http.MaxBytesReader(writer, request.Body, 32<<10)
			status, payload, err = fake.panel(request)
		}
		if err != nil {
			var panelError *Error
			status, code, message := http.StatusInternalServerError, "internal_error", "internal error"
			if errors.As(err, &panelError) {
				status, code, message = panelError.Status, panelError.Code, panelError.Message
			}
			writer.WriteHeader(status)
			_ = json.NewEncoder(writer).Encode(map[string]any{"ok": false, "error": map[string]any{"code": code, "message": message}})
			return
		}
		writer.WriteHeader(status)
		_ = json.NewEncoder(writer).Encode(payload)
	})
}

func (fake *Fake) panel(request *http.Request) (int, any, error) {
	ctx, path := request.Context(), request.URL.Path
	method := request.Method
	if path == "/api/panel/version" && method == http.MethodGet {
		health, err := fake.Ping(ctx)
		return 200, fakeOK(map[string]any{"current": health.Version}), err
	}
	if path == "/api/panel/users" {
		if method == http.MethodPost {
			body, err := fakeDecodeBody[struct{ Username, Password string }](request)
			if err != nil {
				return 0, nil, err
			}
			user, err := fake.CreateUser(ctx, body.Username, body.Password)
			return 201, fakeOK(map[string]any{"item": fakeUserWire(user)}), err
		}
		if method == http.MethodGet {
			if err := fake.begin(ctx); err != nil {
				return 0, nil, err
			}
			defer fake.mu.Unlock()
			ids := make([]string, 0, len(fake.users))
			for id := range fake.users {
				ids = append(ids, id)
			}
			slices.Sort(ids)
			items := make([]any, 0, len(ids))
			for _, id := range ids {
				items = append(items, fakeUserWire(fake.users[id]))
			}
			return 200, fakeOK(map[string]any{"items": items}), nil
		}
	}
	if path == "/api/panel/api-keys" {
		if method == http.MethodPost {
			body, err := fakeDecodeBody[struct {
				Name           string `json:"name"`
				UserID         string `json:"user_id"`
				MaxConcurrency int    `json:"max_concurrency"`
			}](request)
			if err != nil {
				return 0, nil, err
			}
			key, err := fake.CreateKey(ctx, CreateKeyInput{Name: body.Name, UserID: body.UserID, MaxConcurrency: body.MaxConcurrency})
			if err != nil {
				return 0, nil, err
			}
			item, err := fake.keyWire(key.ID)
			if err != nil {
				return 0, nil, err
			}
			item["key"] = key.Secret
			return 201, map[string]any{"ok": true, "item": item}, nil
		}
		if method == http.MethodGet {
			if err := fake.begin(ctx); err != nil {
				return 0, nil, err
			}
			ids := make([]string, 0, len(fake.keys))
			for id, key := range fake.keys {
				if owner := request.URL.Query().Get("user_id"); owner == "" || key.userID == owner {
					ids = append(ids, id)
				}
			}
			fake.mu.Unlock()
			slices.Sort(ids)
			items, active := make([]any, 0, len(ids)), 0
			for _, id := range ids {
				if item, err := fake.keyWire(id); err == nil {
					items = append(items, item)
					if item["status"] == "active" {
						active++
					}
				}
			}
			return 200, map[string]any{"ok": true, "keys": items, "total": len(items), "active": active}, nil
		}
	}
	if path == "/api/panel/vms" && method == http.MethodGet {
		filter := VMFilter{Unowned: request.URL.Query().Get("unowned") == "true", ClaudeOnly: request.URL.Query().Get("claude_only") == "true"}
		if owner := request.URL.Query().Get("user_id"); owner != "" {
			filter.OwnerUserID = &owner
		}
		vms, err := fake.ListVMs(ctx, filter)
		items := make([]any, 0, len(vms))
		for _, vm := range vms {
			items = append(items, fakeVMWire(vm))
		}
		return 200, fakeOK(map[string]any{"items": items, "total": len(items)}), err
	}
	if path == "/api/panel/vms/create" && method == http.MethodPost {
		body, err := fakeDecodeBody[map[string]json.RawMessage](request)
		if err != nil {
			return 0, nil, err
		}
		var label string
		if raw, exists := body["name"]; exists {
			if err := json.Unmarshal(raw, &label); err != nil {
				return 0, nil, fakeError(400, "invalid_name", "name must be a string")
			}
		}
		template, err := json.Marshal(body)
		if err != nil {
			return 0, nil, fmt.Errorf("encode template: %w", err)
		}
		vm, err := fake.CreateHostedVM(ctx, label, template)
		return 200, fakeOK(map[string]any{"vm": fakeVMWire(vm), "allocated_proxy": nil}), err
	}
	if (path == "/api/panel/billing" || path == "/api/panel/request-logs" || path == "/api/panel/request-logs/stats") && method == http.MethodGet {
		return fake.panelUsage(request)
	}
	parts := strings.Split(strings.TrimPrefix(path, "/api/panel/"), "/")
	if !strings.HasPrefix(path, "/api/panel/") || len(parts) < 2 {
		return 0, nil, fakeMissing("route")
	}
	id := parts[1]
	if parts[0] == "users" && len(parts) == 2 && method == http.MethodPatch {
		body, err := fakeDecodeBody[struct{ Enabled *bool }](request)
		if err != nil {
			return 0, nil, err
		}
		if body.Enabled == nil {
			return 0, nil, fakeError(400, "invalid_user", "enabled required")
		}
		if err := fake.SetUserEnabled(ctx, id, *body.Enabled); err != nil {
			return 0, nil, err
		}
		fake.mu.Lock()
		user := fake.users[id]
		fake.mu.Unlock()
		return 200, fakeOK(map[string]any{"item": fakeUserWire(user)}), nil
	}
	if parts[0] == "api-keys" {
		return fake.panelKey(request, id, parts[2:])
	}
	if parts[0] == "vms" {
		return fake.panelVM(request, id, parts[2:])
	}
	if parts[0] == "proxies" {
		return fake.panelProxy(request, id, parts[2:])
	}
	return 0, nil, fakeMissing("route")
}

func (fake *Fake) panelProxy(request *http.Request, id string, suffix []string) (int, any, error) {
	if request.Method != http.MethodPost || len(suffix) != 1 {
		return 0, nil, fakeMissing("route")
	}
	switch suffix[0] {
	case "unbind":
		body, err := fakeDecodeBody[struct {
			VMID string `json:"vm_id"`
		}](request)
		if err != nil {
			return 0, nil, err
		}
		vmID := strings.TrimSpace(body.VMID)
		if vmID == "" {
			return 0, nil, fakeError(400, "missing_field", "vm_id required")
		}
		return 200, fakeOK(map[string]any{"id": id}), fake.UnbindProxy(request.Context(), id, vmID)
	case "disable":
		return 200, fakeOK(map[string]any{"id": id, "enabled": false}), fake.DisableProxy(request.Context(), id)
	}
	return 0, nil, fakeMissing("route")
}

func (fake *Fake) panelKey(request *http.Request, id string, suffix []string) (int, any, error) {
	ctx := request.Context()
	if len(suffix) == 1 && suffix[0] == "stats" && request.Method == http.MethodGet {
		usage, err := fake.KeyStats(ctx, id)
		if err != nil {
			return 0, nil, err
		}
		item, err := fake.keyWire(id)
		if err != nil {
			return 0, nil, err
		}
		fake.mu.Lock()
		rows := fake.logsLocked(item["user_id"].(string), id, "", time.Time{}, time.Time{})
		fake.mu.Unlock()
		return 200, map[string]any{"ok": true, "item": item, "usage_stats": fakeUsageWire(usage, rows)}, nil
	}
	if len(suffix) == 1 && suffix[0] == "reveal" && request.Method == http.MethodPost {
		if _, err := fake.keyWire(id); err != nil {
			return 0, nil, err
		}
		return 0, nil, fakeError(409, "key_not_revealable", "key plaintext is not retained")
	}
	if len(suffix) != 0 {
		return 0, nil, fakeMissing("route")
	}
	switch request.Method {
	case http.MethodPatch:
		body, err := fakeDecodeBody[struct {
			Name           *string `json:"name"`
			Status         *string `json:"status"`
			MaxConcurrency *int    `json:"max_concurrency"`
		}](request)
		if err != nil {
			return 0, nil, err
		}
		input := UpdateKeyInput{Name: body.Name, MaxConcurrency: body.MaxConcurrency}
		if body.Status != nil {
			if *body.Status != "active" && *body.Status != "disabled" {
				return 0, nil, fakeError(400, "invalid_status", "invalid key status")
			}
			active := *body.Status == "active"
			input.Active = &active
		}
		if err := fake.UpdateKey(ctx, id, input); err != nil {
			return 0, nil, err
		}
		item, err := fake.keyWire(id)
		return 200, map[string]any{"ok": true, "item": item}, err
	case http.MethodDelete:
		return 200, map[string]any{"ok": true}, fake.DeleteKey(ctx, id)
	}
	return 0, nil, fakeMissing("route")
}

func (fake *Fake) panelVM(request *http.Request, id string, suffix []string) (int, any, error) {
	ctx := request.Context()
	if len(suffix) == 0 {
		if request.Method == http.MethodDelete {
			return 200, fakeOK(map[string]any{"id": id}), fake.DeleteVM(ctx, id)
		}
		if request.Method == http.MethodGet {
			usage, err := fake.GetAccountUsage(ctx, id)
			if err != nil {
				return 0, nil, err
			}
			fake.mu.Lock()
			rows := fake.logsLocked("", "", id, time.Time{}, time.Time{})
			fake.mu.Unlock()
			vm := fakeVMWire(usage.VM)
			vm["estimate_5h"], vm["estimate_7d"] = usage.Estimate5h, usage.Estimate7d
			return 200, fakeOK(map[string]any{
				"vm": vm, "account": nil,
				"billing": map[string]any{
					"today": fakeSpendWire(usage.Today), "window_5h": fakeSpendWire(usage.Window5h), "window_7d": fakeSpendWire(usage.Window7d),
					"usage_stats": fakeUsageWire(UsageBreakdown{Daily: usage.Daily, ByModel: usage.ByModel}, rows),
				},
			}), nil
		}
	}
	action := strings.Join(suffix, "/")
	if action == "official-cc-bootstrap" && request.Method == http.MethodGet {
		bootstrap, err := fake.BootstrapStatus(ctx, id)
		status := bootstrap.State
		if status == "succeeded" {
			status = "ok"
		} else if status == "failed" {
			status = "error"
		}
		return 200, fakeOK(map[string]any{"vm_id": id, "status": map[string]any{"status": status, "error": bootstrap.Message}}), err
	}
	if action == "owner" && request.Method == http.MethodPatch {
		body, err := fakeDecodeBody[struct {
			UserID      *string `json:"user_id"`
			OwnerUserID *string `json:"owner_user_id"`
		}](request)
		if err != nil {
			return 0, nil, err
		}
		owner := body.UserID
		if owner == nil {
			owner = body.OwnerUserID
		}
		if err := fake.SetVMOwner(ctx, id, owner); err != nil {
			return 0, nil, err
		}
		usage, err := fake.GetAccountUsage(ctx, id)
		return 200, fakeOK(map[string]any{"vm": fakeVMWire(usage.VM)}), err
	}
	if request.Method != http.MethodPost {
		return 0, nil, fakeMissing("route")
	}
	switch action {
	case "collect-identity":
		fingerprint, err := fake.CollectIdentity(ctx, id)
		return 200, fakeOK(map[string]any{"fingerprint": fingerprint}), err
	case "exit-probe":
		probe, err := fake.ProbeExit(ctx, id)
		return 200, fakeOK(map[string]any{
			"via": probe.Via,
			"geo": map[string]any{"ip": probe.IP, "country_code": probe.CountryCode, "timezone": probe.Timezone},
		}), err
	case "reset":
		body, err := fakeDecodeBody[struct{ Mode string }](request)
		if err != nil {
			return 0, nil, err
		}
		if body.Mode != "traits" {
			return 0, nil, fakeError(400, "unsupported_mode", "traits mode required")
		}
		if err := fake.ResetVM(ctx, id); err != nil {
			return 0, nil, err
		}
		usage, err := fake.GetAccountUsage(ctx, id)
		return 200, fakeOK(map[string]any{"vm": fakeVMWire(usage.VM), "recreated": true, "mode": body.Mode}), err
	case "allocate-proxy":
		if err := fake.AllocateProxy(ctx, id); err != nil {
			return 0, nil, err
		}
		usage, err := fake.GetAccountUsage(ctx, id)
		return 200, fakeOK(map[string]any{"proxy": fakeProxyWire(usage.VM.Proxy)}), err
	case "schedulable":
		body, err := fakeDecodeBody[struct{ Schedulable *bool }](request)
		if err != nil {
			return 0, nil, err
		}
		if body.Schedulable == nil {
			return 0, nil, fakeError(400, "invalid_schedule", "schedulable required")
		}
		return 200, fakeOK(map[string]any{"id": id, "schedulable": *body.Schedulable}), fake.SetSchedulable(ctx, id, *body.Schedulable)
	case "oauth/generate-auth-url":
		body, err := fakeDecodeBody[struct{ Flavor string }](request)
		if err != nil {
			return 0, nil, err
		}
		if body.Flavor != "claude_code" {
			return 0, nil, fakeError(400, "unsupported_flavor", "claude_code flavor required")
		}
		auth, err := fake.GenerateAuthURL(ctx, id)
		return 200, fakeOK(map[string]any{
			"auth_url": auth.URL, "session_id": auth.SessionID, "expires_at": auth.ExpiresAt.UnixMilli(), "flavor": "claude_code",
		}), err
	case "oauth/exchange-code":
		body, err := fakeDecodeBody[struct {
			SessionID string `json:"session_id"`
			Code      string `json:"code"`
			Flavor    string `json:"flavor"`
		}](request)
		if err != nil {
			return 0, nil, err
		}
		if body.Flavor != "claude_code" {
			return 0, nil, fakeError(400, "unsupported_flavor", "claude_code flavor required")
		}
		if err := fake.ExchangeCode(ctx, id, body.SessionID, body.Code); err != nil {
			return 0, nil, err
		}
		usage, err := fake.GetAccountUsage(ctx, id)
		return 200, fakeOK(map[string]any{"vm": fakeVMWire(usage.VM), "official_cc_bootstrap": map[string]any{"scheduled": true}}), err
	}
	return 0, nil, fakeMissing("route")
}

func (fake *Fake) panelUsage(request *http.Request) (int, any, error) {
	ctx, query := request.Context(), request.URL.Query()
	owner := query.Get("user_id")
	if owner == "" {
		return 0, nil, fakeError(400, "user_id_required", "user_id required")
	}
	from, err := fakeQueryTime(request, "since", "from")
	if err != nil {
		return 0, nil, err
	}
	until, err := fakeQueryTime(request, "until")
	if err != nil {
		return 0, nil, err
	}
	switch request.URL.Path {
	case "/api/panel/billing":
		billing, err := fake.UserBilling(ctx, owner, from, until)
		if err != nil {
			return 0, nil, err
		}
		items := make([]any, 0)
		fake.mu.Lock()
		rows := fake.logsLocked(owner, "", "", from, until)
		fake.mu.Unlock()
		groupedRows := make(map[string][]LogRow)
		groupBy := "vm"
		if query.Get("group_by") == "key" {
			groupBy = "key"
		}
		for _, row := range rows {
			id := row.VMID
			if groupBy == "key" {
				id = row.KeyID
			}
			groupedRows[id] = append(groupedRows[id], row)
		}
		if groupBy == "key" {
			for _, key := range billing.ByKey {
				item := fakeBillingWire(groupedRows[key.KeyID])
				item["api_key_id"] = key.KeyID
				items = append(items, item)
			}
		} else {
			for _, vm := range billing.ByVM {
				item := fakeBillingWire(groupedRows[vm.VMID])
				item["vm_id"] = vm.VMID
				items = append(items, item)
			}
		}
		return 200, fakeOK(map[string]any{
			"group_by": groupBy, "items": items,
			"totals": fakeBillingWire(rows),
		}), nil
	case "/api/panel/request-logs/stats":
		stats, err := fake.UserRequestStats(ctx, owner, from, until)
		return 200, map[string]any{"ok": true, "window": map[string]any{
			"requests": stats.Requests, "errors": stats.ErrorCount, "sla": stats.SuccessRate,
			"ttft": map[string]any{"avg_ms": stats.AvgTTFTMS, "p95_ms": stats.P95TTFTMS},
		}}, err
	case "/api/panel/request-logs":
		limit, offset := 50, 0
		for _, pair := range []struct {
			name   string
			target *int
		}{{"limit", &limit}, {"offset", &offset}} {
			if raw := query.Get(pair.name); raw != "" {
				value, err := strconv.Atoi(raw)
				if err != nil || value < 0 || pair.name == "limit" && value == 0 {
					return 0, nil, fakeError(400, "invalid_pagination", "invalid "+pair.name)
				}
				*pair.target = value
			}
		}
		limit = min(limit, 500)
		if err := fake.begin(ctx); err != nil {
			return 0, nil, err
		}
		defer fake.mu.Unlock()
		if _, exists := fake.users[owner]; !exists {
			return 0, nil, fakeMissing("user")
		}
		if status := query.Get("status"); status != "" && status != "success" && status != "error" {
			return 0, nil, fakeError(400, "invalid_status", "invalid log status")
		}
		rows := fake.filteredLogsLocked(owner, LogQuery{
			From: from, Until: until,
			KeyID: query.Get("api_key_id"), Model: query.Get("model"), Status: query.Get("status"),
		})
		total := len(rows)
		rows = rows[min(offset, len(rows)):]
		rows = rows[:min(limit, len(rows))]
		items := make([]any, 0, len(rows))
		for _, row := range rows {
			items = append(items, fakeLogWire(row))
		}
		return 200, map[string]any{"ok": true, "mode": "normal", "items": items, "total": total, "limit": limit, "offset": offset}, nil
	}
	return 0, nil, fakeMissing("route")
}

func fakeBillingWire(rows []LogRow) map[string]any {
	spend := fakeSpend(rows)
	success := 0
	for _, row := range rows {
		if row.StatusCode >= 200 && row.StatusCode < 400 {
			success++
		}
	}
	return map[string]any{
		"requests": spend.Requests, "ok": success, "fail": len(rows) - success,
		"tokens_in": spend.Tokens - spend.OutputTokens, "tokens_out": spend.OutputTokens,
		"cost_usd": spend.CostUSD, "official_cost_usd": spend.CostUSD,
	}
}
