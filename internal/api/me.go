package api

import "net/http"

func (s *Server) handleOverview(w http.ResponseWriter, r *http.Request) {
	user, ok := s.currentUser(r)
	if !ok {
		writeErr(w, http.StatusUnauthorized, "unauthorized", "请先登录")
		return
	}
	overview, err := s.Deps.Usage.Overview(r.Context(), user.ID)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	overview.BaseURL = s.Deps.Site(r).BaseURL
	overview.ExpiringSoon = subscriptionItems(overview.ExpiringSoon)
	writeOK(w, overview)
}

func (s *Server) handleSubscriptions(w http.ResponseWriter, r *http.Request) {
	user, ok := s.currentUser(r)
	if !ok {
		writeErr(w, http.StatusUnauthorized, "unauthorized", "请先登录")
		return
	}
	status := r.URL.Query().Get("status")
	if status == "" {
		status = "active"
	}
	if status != "active" && status != "expired" && status != "all" {
		writeErr(w, http.StatusUnprocessableEntity, "invalid_status", "status 只能是 active、expired 或 all")
		return
	}
	items, err := s.Deps.Usage.Subscriptions(r.Context(), user.ID, status)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	writeOK(w, map[string]any{"items": subscriptionItems(items)})
}

func subscriptionItems(items []SubscriptionDTO) []SubscriptionDTO {
	if items == nil {
		return []SubscriptionDTO{}
	}
	for index := range items {
		if items[index].RenewProductIDs == nil {
			items[index].RenewProductIDs = []int64{}
		}
	}
	return items
}
