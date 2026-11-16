package app

import (
	"database/sql"
	"log/slog"

	"github.com/DueGin/seat-hub/internal/config"
	"github.com/DueGin/seat-hub/internal/mail"
	"github.com/DueGin/seat-hub/internal/secret"
	"github.com/DueGin/seat-hub/internal/store"
)

// featureDeps is what the v0.5 features (mail, reservations, tickets) get from
// the wiring. Each feature's constructor lives in its own adapters_*.go file.
type featureDeps struct {
	DB       *sql.DB
	Box      *secret.Box
	Mail     *mail.Service
	Settings *store.Settings
	Audit    *store.Audit
	Cfg      config.Config
	Log      *slog.Logger
}
