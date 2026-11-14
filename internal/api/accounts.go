package api

import (
	"net/http"
	"strconv"
)

func pageParams(r *http.Request) (page, size int) {
	page = queryInt(r, "page", 1)
	if page < 1 {
		page = 1
	}
	size = queryInt(r, "page_size", 20)
	if size < 1 {
		size = 20
	}
	if size > 100 {
		size = 100
	}
	return page, size
}

func pageEnvelope[T any](items []T, total int64, page, size int) map[string]any {
	if items == nil {
		items = []T{}
	}
	return map[string]any{"items": items, "total": total, "page": page, "page_size": size}
}

func (s *Server) handleListAccounts(w http.ResponseWriter, r *http.Request) {
	u, ok := s.currentUser(r)
	if !ok {
		writeErr(w, http.StatusUnauthorized, "unauthorized", "请先登录")
		return
	}
	includeReleased := r.URL.Query().Get("include_released") == "1"
	items, err := s.Deps.Accounts.List(r.Context(), u.ID, includeReleased)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	writeOK(w, map[string]any{"items": items})
}

func (s *Server) handleGetAccount(w http.ResponseWriter, r *http.Request) {
	u, ok := s.currentUser(r)
	if !ok {
		writeErr(w, http.StatusUnauthorized, "unauthorized", "请先登录")
		return
	}
	acc, daily, models, err := s.Deps.Accounts.Get(r.Context(), u.ID, pathID(r))
	if err != nil {
		s.fail(w, r, err)
		return
	}
	writeOK(w, map[string]any{"account": acc, "daily": daily, "by_model": models})
}

func (s *Server) handlePatchAccount(w http.ResponseWriter, r *http.Request) {
	u, ok := s.currentUser(r)
	if !ok {
		writeErr(w, http.StatusUnauthorized, "unauthorized", "请先登录")
		return
	}
	var body struct {
		Label string `json:"label"`
	}
	if err := readJSON(r, &body); err != nil {
		s.fail(w, r, err)
		return
	}
	if len([]rune(body.Label)) > 40 {
		writeErr(w, http.StatusUnprocessableEntity, "invalid_label", "备注最多 40 个字符")
		return
	}
	if err := s.Deps.Accounts.Rename(r.Context(), u.ID, pathID(r), body.Label); err != nil {
		s.fail(w, r, err)
		return
	}
	writeOK(w, map[string]any{})
}

// --- hosted onboarding ---

func (s *Server) handleCreateHosted(w http.ResponseWriter, r *http.Request) {
	u, ok := s.currentUser(r)
	if !ok {
		writeErr(w, http.StatusUnauthorized, "unauthorized", "请先登录")
		return
	}
	var body struct {
		Label string `json:"label"`
	}
	if err := readJSON(r, &body); err != nil {
		s.fail(w, r, err)
		return
	}
	if len([]rune(body.Label)) > 40 {
		writeErr(w, http.StatusUnprocessableEntity, "invalid_label", "备注最多 40 个字符")
		return
	}
	acc, ob, err := s.Deps.Accounts.CreateHosted(r.Context(), u.ID, body.Label)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	writeCreated(w, map[string]any{"account": acc, "onboarding": ob})
}

func (s *Server) handleOnboarding(w http.ResponseWriter, r *http.Request) {
	u, ok := s.currentUser(r)
	if !ok {
		writeErr(w, http.StatusUnauthorized, "unauthorized", "请先登录")
		return
	}
	ob, err := s.Deps.Accounts.Onboarding(r.Context(), u.ID, pathID(r))
	if err != nil {
		s.fail(w, r, err)
		return
	}
	writeOK(w, ob)
}

func (s *Server) handleRegenerateAuthURL(w http.ResponseWriter, r *http.Request) {
	u, ok := s.currentUser(r)
	if !ok {
		writeErr(w, http.StatusUnauthorized, "unauthorized", "请先登录")
		return
	}
	ob, err := s.Deps.Accounts.RegenerateAuthURL(r.Context(), u.ID, pathID(r))
	if err != nil {
		s.fail(w, r, err)
		return
	}
	writeOK(w, ob)
}

func (s *Server) handleSubmitAuthCode(w http.ResponseWriter, r *http.Request) {
	u, ok := s.currentUser(r)
	if !ok {
		writeErr(w, http.StatusUnauthorized, "unauthorized", "请先登录")
		return
	}
	var body struct {
		Code string `json:"code"`
	}
	if err := readJSON(r, &body); err != nil {
		s.fail(w, r, err)
		return
	}
	if len(body.Code) < 8 || len(body.Code) > 8192 {
		writeErr(w, http.StatusUnprocessableEntity, "invalid_code", "授权码格式不正确，请粘贴授权页面给出的完整代码")
		return
	}
	ob, err := s.Deps.Accounts.SubmitAuthCode(r.Context(), u.ID, pathID(r), body.Code)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	writeOK(w, ob)
}

func (s *Server) handleRetryHosted(w http.ResponseWriter, r *http.Request) {
	u, ok := s.currentUser(r)
	if !ok {
		writeErr(w, http.StatusUnauthorized, "unauthorized", "请先登录")
		return
	}
	ob, err := s.Deps.Accounts.RetryHosted(r.Context(), u.ID, pathID(r))
	if err != nil {
		s.fail(w, r, err)
		return
	}
	writeOK(w, ob)
}

func (s *Server) handleDeleteHosted(w http.ResponseWriter, r *http.Request) {
	u, ok := s.currentUser(r)
	if !ok {
		writeErr(w, http.StatusUnauthorized, "unauthorized", "请先登录")
		return
	}
	if err := s.Deps.Accounts.DeleteHosted(r.Context(), u.ID, pathID(r)); err != nil {
		s.fail(w, r, err)
		return
	}
	writeOK(w, map[string]any{})
}

// --- keys ---

func (s *Server) handleListKeys(w http.ResponseWriter, r *http.Request) {
	u, ok := s.currentUser(r)
	if !ok {
		writeErr(w, http.StatusUnauthorized, "unauthorized", "请先登录")
		return
	}
	items, err := s.Deps.Keys.List(r.Context(), u.ID)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	if items == nil {
		items = []KeyDTO{}
	}
	writeOK(w, map[string]any{"items": items})
}

func (s *Server) handleCreateKey(w http.ResponseWriter, r *http.Request) {
	u, ok := s.currentUser(r)
	if !ok {
		writeErr(w, http.StatusUnauthorized, "unauthorized", "请先登录")
		return
	}
	var body struct {
		Name  string         `json:"name"`
		Scope *KeyScopeInput `json:"scope"`
	}
	if err := readJSON(r, &body); err != nil {
		s.fail(w, r, err)
		return
	}
	name := body.Name
	if name == "" {
		name = "默认"
	}
	if len([]rune(name)) > 40 {
		writeErr(w, http.StatusUnprocessableEntity, "invalid_name", "名称最多 40 个字符")
		return
	}
	input := KeyInput{Name: name}
	if body.Scope != nil {
		input.Scope = *body.Scope
	}
	k, secret, err := s.Deps.Keys.Create(r.Context(), u.ID, input)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	s.Deps.Audit.Write(r.Context(), u.ID, "key.create", k.Prefix, map[string]any{"name": name, "scope": k.Scope})
	writeCreated(w, map[string]any{"key": k, "secret": secret})
}

func (s *Server) handlePatchKey(w http.ResponseWriter, r *http.Request) {
	u, ok := s.currentUser(r)
	if !ok {
		writeErr(w, http.StatusUnauthorized, "unauthorized", "请先登录")
		return
	}
	var body struct {
		Name   *string        `json:"name"`
		Status *string        `json:"status"`
		Scope  *KeyScopeInput `json:"scope"`
	}
	if err := readJSON(r, &body); err != nil {
		s.fail(w, r, err)
		return
	}
	if body.Name != nil && len([]rune(*body.Name)) > 40 {
		writeErr(w, http.StatusUnprocessableEntity, "invalid_name", "名称最多 40 个字符")
		return
	}
	if body.Status != nil && *body.Status != "active" && *body.Status != "disabled" {
		writeErr(w, http.StatusUnprocessableEntity, "invalid_status", "状态只能是 active 或 disabled")
		return
	}
	k, err := s.Deps.Keys.Update(r.Context(), u.ID, pathID(r), KeyPatch{Name: body.Name, Status: body.Status, Scope: body.Scope})
	if err != nil {
		s.fail(w, r, err)
		return
	}
	if body.Scope != nil {
		s.Deps.Audit.Write(r.Context(), u.ID, "key.scope", k.Prefix, map[string]any{"scope": k.Scope})
	}
	writeOK(w, map[string]any{"key": k})
}

func (s *Server) handleDeleteKey(w http.ResponseWriter, r *http.Request) {
	u, ok := s.currentUser(r)
	if !ok {
		writeErr(w, http.StatusUnauthorized, "unauthorized", "请先登录")
		return
	}
	if err := s.Deps.Keys.Delete(r.Context(), u.ID, pathID(r)); err != nil {
		s.fail(w, r, err)
		return
	}
	writeOK(w, map[string]any{})
}

func (s *Server) handleKeyStats(w http.ResponseWriter, r *http.Request) {
	u, ok := s.currentUser(r)
	if !ok {
		writeErr(w, http.StatusUnauthorized, "unauthorized", "请先登录")
		return
	}
	daily, models, err := s.Deps.Usage.KeyStats(r.Context(), u.ID, pathID(r))
	if err != nil {
		s.fail(w, r, err)
		return
	}
	writeOK(w, map[string]any{"daily": daily, "by_model": models})
}

func parseInt64(s string) int64 {
	n, _ := strconv.ParseInt(s, 10, 64)
	return n
}
