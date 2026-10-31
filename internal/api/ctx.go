package api

import (
	"net/http"
	"strings"
)

// currentUser returns the session user for the request.
func (s *Server) currentUser(r *http.Request) (UserDTO, bool) {
	return s.Deps.Auth.UserFrom(r.Context())
}

// user wraps a handler so it only runs for a logged-in user.
func (s *Server) user(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if _, ok := s.currentUser(r); !ok {
			writeErr(w, http.StatusUnauthorized, "unauthorized", "请先登录")
			return
		}
		h(w, r)
	}
}

func hostOf(r *http.Request) string {
	if h := r.Host; h != "" {
		return h
	}
	return ""
}

func schemeOf(r *http.Request) string {
	if r.TLS != nil {
		return "https"
	}
	if p := r.Header.Get("X-Forwarded-Proto"); p != "" {
		return strings.TrimSpace(strings.Split(p, ",")[0])
	}
	return "http"
}
