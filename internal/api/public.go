package api

import "net/http"

func (s *Server) handlePublicConfig(w http.ResponseWriter, r *http.Request) {
	site := s.Deps.Site(r)
	site.Dev = s.Deps.Dev
	site.PaymentProviders = s.Deps.Payments.Providers(r.Context())
	if site.PaymentProviders == nil {
		site.PaymentProviders = []PaymentProviderDTO{}
	}
	writeOK(w, site)
}

func (s *Server) handlePublicProducts(w http.ResponseWriter, r *http.Request) {
	items, err := s.Deps.Catalog.Public(r.Context())
	if err != nil {
		s.fail(w, r, err)
		return
	}
	if items == nil {
		items = []ProductDTO{}
	}
	writeOK(w, map[string]any{"items": items})
}
