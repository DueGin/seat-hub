package app

import (
	"context"
	"path/filepath"

	"github.com/DueGin/seat-hub/internal/api"
	"github.com/DueGin/seat-hub/internal/ticket"
)

func newTicketAdapter(deps featureDeps) api.TicketService {
	service := ticket.New(deps.DB, filepath.Join(filepath.Dir(deps.Cfg.DBPath), "attachments"), deps.Cfg.PublicBaseURL, deps.Mail, deps.Log)
	go service.RunCleanup(context.Background())
	return service
}
